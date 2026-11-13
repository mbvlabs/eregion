package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/vault"
)

// CloudflareCredentialStore adapts the vault provider scope to the DNS zone and
// credential interfaces. It reads PostgreSQL on every call; no process caches a
// provider token or zone inventory across operations or starts.
type CloudflareCredentialStore struct {
	vault *vault.Service
}

func NewCloudflareCredentialStore(service *vault.Service) *CloudflareCredentialStore {
	return &CloudflareCredentialStore{vault: service}
}

func (s *CloudflareCredentialStore) DNSZone(ctx context.Context, zoneID, zoneName string) (integrations.DNSZone, bool, error) {
	if s == nil || s.vault == nil {
		return integrations.DNSZone{}, false, nil
	}
	zoneID = strings.ToLower(strings.TrimSpace(zoneID))
	zoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneName), "."))
	if zoneID != "" {
		scope, err := vault.NewProviderScope(vault.ProviderCloudflare, zoneID)
		if err != nil {
			return integrations.DNSZone{}, false, err
		}
		metadata, err := s.vault.GetProviderCredential(ctx, scope)
		if err != nil && contract.Classify(err).Code == contract.NotFound {
			return integrations.DNSZone{}, false, nil
		}
		if err != nil {
			return integrations.DNSZone{}, false, err
		}
		if zoneName != "" && zoneName != metadata.ZoneName {
			return integrations.DNSZone{}, false, nil
		}
		return integrations.DNSZone{ZoneID: metadata.ZoneID, ZoneName: metadata.ZoneName}, true, nil
	}
	if zoneName == "" {
		return integrations.DNSZone{}, false, nil
	}
	zones, err := s.DNSZones(ctx)
	if err != nil {
		return integrations.DNSZone{}, false, err
	}
	for _, zone := range zones {
		if zone.ZoneID == zoneName || strings.EqualFold(zone.ZoneName, zoneName) {
			return zone, true, nil
		}
	}
	return integrations.DNSZone{}, false, nil
}

func (s *CloudflareCredentialStore) DNSZones(ctx context.Context) ([]integrations.DNSZone, error) {
	if s == nil || s.vault == nil {
		return nil, nil
	}
	metadata, err := s.vault.ListProviderCredentials(ctx, vault.ProviderCloudflare, vault.MaxProviderZones)
	if err != nil {
		return nil, err
	}
	zones := make([]integrations.DNSZone, 0, len(metadata))
	for _, provider := range metadata {
		zones = append(zones, integrations.DNSZone{ZoneID: provider.ZoneID, ZoneName: provider.ZoneName})
	}
	return zones, nil
}

func (s *CloudflareCredentialStore) DNSCredential(ctx context.Context, provider, zoneID string) ([]byte, error) {
	if s == nil || s.vault == nil {
		return nil, contract.NewFault(contract.InputRequired, "the vault is not configured on this control plane", "Set EREGION_VAULT_KEY_FILE on the node before configuring or using Cloudflare DNS.")
	}
	if provider != integrations.DNSProviderCloudflare {
		return nil, contract.NewFault(contract.ValidationFailed, "unsupported DNS provider", "Only Cloudflare DNS is supported.")
	}
	scope, err := vault.NewProviderScope(vault.ProviderCloudflare, zoneID)
	if err != nil {
		return nil, err
	}
	_, token, err := s.vault.ResolveProviderCredential(ctx, scope)
	if err != nil {
		return nil, err
	}
	return token, nil
}

var _ integrations.DNSZoneSource = (*CloudflareCredentialStore)(nil)
var _ integrations.DNSCredentialSource = (*CloudflareCredentialStore)(nil)

// CloudflareSetup owns the bounded human setup path: validate a supplied token
// with a read-only DNS list, then atomically append an encrypted revision and
// audit the non-secret outcome. It is not a platform action and never enters
// River.
type CloudflareSetup struct {
	vault     *vault.Service
	baseURL   string
	client    *http.Client
	now       func() time.Time
	auditKind string
}

func NewCloudflareSetup(service *vault.Service, baseURL string, client *http.Client) *CloudflareSetup {
	return &CloudflareSetup{vault: service, baseURL: baseURL, client: client, now: time.Now, auditKind: "cloudflare.configure"}
}

// Preview validates the token and returns the exact current revision the
// operator must confirm. It changes no credential, metadata or provider record.
func (s *CloudflareSetup) PreviewCloudflareCredential(ctx context.Context, actor contract.Actor, zoneID, zoneName string, token []byte) (api.CloudflareCredentialPreview, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return api.CloudflareCredentialPreview{}, err
	}
	scope, zone, trimmed, err := s.prepare(zoneID, zoneName, token)
	if err != nil {
		return api.CloudflareCredentialPreview{}, err
	}
	if err := s.validateToken(ctx, zone, trimmed); err != nil {
		return api.CloudflareCredentialPreview{}, err
	}
	current, err := s.current(ctx, scope, zone)
	if err != nil {
		return api.CloudflareCredentialPreview{}, err
	}
	next := uint64(1)
	if current != 0 {
		next = current + 1
	}
	return api.CloudflareCredentialPreview{
		Provider: scope.Provider, ZoneID: scope.ZoneID, ZoneName: zone.ZoneName,
		CurrentRevision: current, NextRevision: next,
		ReadValidated: true,
		Validation:    "read-validated with a bounded DNS list for this zone; this does not prove DNS write capability",
		Warnings: []string{
			"The API token is not shown again after this command.",
			"The existing credential is replaced only after this preview's current revision is committed; a concurrent rotation is refused as a conflict.",
		},
	}, nil
}

// ConfigureCloudflareCredential validates the token again and commits the
// metadata, immutable encrypted revision and non-secret audit entry in one
// transaction guarded by expectedRevision. A validation, CAS or audit failure
// leaves the previous credential active.
func (s *CloudflareSetup) ConfigureCloudflareCredential(ctx context.Context, actor contract.Actor, zoneID, zoneName string, expectedRevision uint64, token []byte) (vault.ProviderMetadata, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return vault.ProviderMetadata{}, err
	}
	scope, zone, trimmed, err := s.prepare(zoneID, zoneName, token)
	if err != nil {
		return vault.ProviderMetadata{}, err
	}
	if err := s.validateToken(ctx, zone, trimmed); err != nil {
		return vault.ProviderMetadata{}, err
	}
	tx, err := s.vault.Store().Pool().Begin(ctx)
	if err != nil {
		return vault.ProviderMetadata{}, contract.NewFault(contract.Unavailable, "provider credential storage is unavailable", "Check PostgreSQL connectivity and retry; the existing credential was not changed.")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	metadata, err := s.vault.Store().SetProviderCredential(ctx, tx, scope, zone.ZoneName, []byte(trimmed), expectedRevision, actor)
	if err != nil {
		return vault.ProviderMetadata{}, err
	}
	entry := integrations.AuditEntry{
		At: s.now().UTC(), Actor: actor.Subject, Action: s.auditKind,
		Target: scope.Provider + "/" + scope.ZoneID, Outcome: "succeeded",
		Detail: fmt.Sprintf("zone=%s revision=%d read_validated=true", zone.ZoneName, metadata.LatestRevision),
	}
	if err := recordAuditTx(ctx, tx, entry); err != nil {
		return vault.ProviderMetadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return vault.ProviderMetadata{}, providerCommitFault(err)
	}
	return metadata, nil
}

// providerCommitFault keeps an ambiguous PostgreSQL commit failure safe. A
// definite rollback means the previous credential is untouched; any other
// error may have committed, so the operator must inspect the observed revision
// before retrying. The cause is never attached, avoiding DB error details and
// any credential material reaching the CLI.
func providerCommitFault(err error) *contract.Fault {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return contract.NewFault(contract.Unavailable,
			"provider credential commit was rolled back",
			"The existing credential was not changed. Run eregion domain cloudflare status to read the observed revision, then retry with that revision.")
	}
	return contract.NewFault(contract.Unavailable,
		"provider credential commit outcome is unknown",
		"Run eregion domain cloudflare status and inspect the observed revision before retrying; do not assume the previous credential is still active.")
}

func (s *CloudflareSetup) ListCloudflareCredentials(ctx context.Context, actor contract.Actor, zoneRef string) ([]vault.ProviderMetadata, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return nil, err
	}
	if s == nil || s.vault == nil {
		return nil, contract.NewFault(contract.InputRequired, "the vault is not configured on this control plane", "Set EREGION_VAULT_KEY_FILE on the node before configuring Cloudflare DNS.")
	}
	metadata, err := s.vault.ListProviderCredentials(ctx, vault.ProviderCloudflare, vault.MaxProviderZones)
	if err != nil {
		return nil, err
	}
	zoneRef = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneRef), "."))
	if zoneRef == "" {
		return metadata, nil
	}
	filtered := make([]vault.ProviderMetadata, 0, 1)
	for _, provider := range metadata {
		if provider.ZoneID == zoneRef || strings.EqualFold(provider.ZoneName, zoneRef) {
			filtered = append(filtered, provider)
		}
	}
	return filtered, nil
}

func (s *CloudflareSetup) prepare(zoneID, zoneName string, token []byte) (vault.ProviderScope, integrations.DNSZone, string, error) {
	if s == nil || s.vault == nil {
		return vault.ProviderScope{}, integrations.DNSZone{}, "", contract.NewFault(contract.InputRequired, "the vault is not configured on this control plane", "Set EREGION_VAULT_KEY_FILE on the node before configuring Cloudflare DNS.")
	}
	scope, err := vault.NewProviderScope(vault.ProviderCloudflare, zoneID)
	if err != nil {
		return vault.ProviderScope{}, integrations.DNSZone{}, "", err
	}
	zoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneName), "."))
	if err := vault.ValidateProviderZoneName(zoneName); err != nil {
		return vault.ProviderScope{}, integrations.DNSZone{}, "", err
	}
	trimmed := strings.TrimRight(string(token), "\r\n")
	if trimmed == "" {
		return vault.ProviderScope{}, integrations.DNSZone{}, "", contract.NewFault(contract.InputRequired, "a Cloudflare API token is required", "Enter it at the hidden terminal prompt; there is no token flag, environment variable, file or stdin fallback.")
	}
	return scope, integrations.DNSZone{ZoneID: scope.ZoneID, ZoneName: zoneName}, trimmed, nil
}

func (s *CloudflareSetup) validateToken(ctx context.Context, zone integrations.DNSZone, token string) error {
	client, err := integrations.NewCloudflare(integrations.CloudflareConfig{
		APIToken: token, ZoneID: zone.ZoneID, ZoneName: zone.ZoneName,
		BaseURL: s.baseURL, HTTPClient: s.client,
	})
	if err != nil {
		return err
	}
	return client.ValidateToken(ctx)
}

func (s *CloudflareSetup) current(ctx context.Context, scope vault.ProviderScope, zone integrations.DNSZone) (uint64, error) {
	metadata, err := s.vault.GetProviderCredential(ctx, scope)
	if err != nil && contract.Classify(err).Code == contract.NotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if metadata.ZoneName != zone.ZoneName {
		return 0, contract.NewFault(contract.Conflict, "provider zone name cannot change for an existing zone ID", "Keep the configured zone name or remove the binding deliberately before using a different name.")
	}
	return metadata.LatestRevision, nil
}

func requireCloudflareOperator(actor contract.Actor) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if actor.Kind != "operator" {
		return contract.NewFault(contract.Forbidden, "Cloudflare setup uses the trusted SSH operator boundary", "CI, webhook and system actors cannot configure provider credentials.")
	}
	return nil
}

func recordAuditTx(ctx context.Context, tx pgx.Tx, entry integrations.AuditEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode the provider audit entry", "Do not activate the credential; report the binary version.")
	}
	if err := integrations.InsertAuditTx(ctx, tx, raw); err != nil {
		return contract.NewFault(contract.Unavailable, "durable integration audit is unavailable", "The provider credential was not activated; repair database connectivity and retry.")
	}
	return nil
}

var _ api.CloudflareSetupService = (*CloudflareSetup)(nil)
