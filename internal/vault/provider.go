package vault

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ProviderCloudflare is the only provider credential scope implemented today.
// The scope is deliberately typed: it is not an application secret namespace
// and cannot be reached through the app/environment secret APIs.
const ProviderCloudflare = "cloudflare"

const (
	MaxProviderZones   = 200
	MaxProviderZoneLen = 253
)

var (
	providerPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	providerZonePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	providerNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])$`)
)

// ProviderScope identifies one provider credential. Provider and zone ID are
// both required; a zone name is metadata and never an identity.
type ProviderScope struct {
	Provider string `json:"provider"`
	ZoneID   string `json:"zone_id"`
}

// NewProviderScope validates and returns a canonical provider scope.
func NewProviderScope(provider, zoneID string) (ProviderScope, error) {
	scope := ProviderScope{
		Provider: strings.ToLower(strings.TrimSpace(provider)),
		ZoneID:   strings.ToLower(strings.TrimSpace(zoneID)),
	}
	if err := scope.Validate(); err != nil {
		return ProviderScope{}, err
	}
	return scope, nil
}

// Validate checks the typed provider identity without inspecting any secret.
func (p ProviderScope) Validate() error {
	if !providerPattern.MatchString(p.Provider) {
		return contract.NewFault(contract.ValidationFailed, "invalid provider credential scope", "Use a lowercase provider name such as cloudflare.")
	}
	if p.Provider != ProviderCloudflare {
		return contract.NewFault(contract.ValidationFailed, "unsupported provider credential scope", "Only Cloudflare provider credentials are supported.")
	}
	if !providerZonePattern.MatchString(p.ZoneID) {
		return contract.NewFault(contract.ValidationFailed, "invalid provider zone ID", "Use the lowercase 32-character Cloudflare zone identifier.")
	}
	return nil
}

// Key returns a stable, non-secret diagnostic label.
func (p ProviderScope) Key() string { return p.Provider + "/" + p.ZoneID }

func (p ProviderScope) String() string { return p.Key() }

// ValidateProviderZoneName validates the non-secret zone name stored beside a
// provider credential.
func ValidateProviderZoneName(zoneName string) error {
	if len(zoneName) == 0 || len(zoneName) > MaxProviderZoneLen ||
		!providerNamePattern.MatchString(zoneName) || !strings.Contains(zoneName, ".") ||
		strings.Contains(zoneName, "..") {
		return contract.NewFault(contract.ValidationFailed, "invalid provider zone name", "Use a lowercase DNS name such as example.com.")
	}
	return nil
}

// ProviderMetadata is secret-free provider credential state. It can be placed
// in API results, logs and status output.
type ProviderMetadata struct {
	Provider       string         `json:"provider"`
	ZoneID         string         `json:"zone_id"`
	ZoneName       string         `json:"zone_name"`
	LatestRevision uint64         `json:"latest_revision"`
	Revision       RevisionRef    `json:"revision"`
	CreatedBy      contract.Actor `json:"created_by"`
	UpdatedBy      contract.Actor `json:"updated_by"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// Scope returns the typed identity.
func (m ProviderMetadata) Scope() ProviderScope {
	return ProviderScope{Provider: m.Provider, ZoneID: m.ZoneID}
}

// Validate rejects malformed provider metadata before a caller trusts it.
func (m ProviderMetadata) Validate() error {
	if err := m.Scope().Validate(); err != nil {
		return err
	}
	if err := ValidateProviderZoneName(m.ZoneName); err != nil {
		return err
	}
	if m.LatestRevision == 0 || m.Revision.Revision != m.LatestRevision ||
		!isHexDigest(m.Revision.Hash) || m.Revision.KeyID == "" ||
		m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() ||
		m.CreatedBy.Validate() != nil || m.UpdatedBy.Validate() != nil {
		return contract.NewFault(contract.Unavailable, "API returned invalid provider credential metadata", "Check binary compatibility before retrying a provider mutation.")
	}
	return nil
}

// SetProviderCredential appends one immutable encrypted provider revision under
// the caller's transaction. expectedRevision is 0 only for the first
// configuration; every replacement must name the observed latest revision.
func (s *Store) SetProviderCredential(ctx context.Context, tx ProviderTx, scope ProviderScope, zoneName string, value []byte, expectedRevision uint64, actor contract.Actor) (ProviderMetadata, error) {
	if tx == nil {
		return ProviderMetadata{}, contract.NewFault(contract.Internal, "provider credential mutation has no transaction", "Integrate provider credential storage through an explicit PostgreSQL transaction.")
	}
	if err := scope.Validate(); err != nil {
		return ProviderMetadata{}, err
	}
	zoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneName), "."))
	if err := ValidateProviderZoneName(zoneName); err != nil {
		return ProviderMetadata{}, err
	}
	if err := ValidateValue(value); err != nil {
		return ProviderMetadata{}, err
	}
	if err := validateActor(actor); err != nil {
		return ProviderMetadata{}, err
	}

	state, exists, err := tx.LockProviderCredential(ctx, scope)
	if err != nil {
		return ProviderMetadata{}, providerStorageError(err)
	}
	if exists && state.ZoneName != zoneName {
		return ProviderMetadata{}, contract.NewFault(contract.Conflict, "provider zone name cannot change for an existing zone ID", "Keep the configured zone name or remove the binding deliberately before using a different name.")
	}
	revision, create, err := providerNextRevision(scope, exists, state.LatestRevision, expectedRevision)
	if err != nil {
		return ProviderMetadata{}, err
	}
	ciphertext, keyID, err := s.ring.SealProvider(scope, revision, value)
	if err != nil {
		return ProviderMetadata{}, err
	}
	ciphertextHash := CiphertextSHA256(ciphertext)
	revisionHash := ProviderRevisionHash(scope, revision, keyID, ciphertext)

	if err := applyProviderRevision(ctx, tx, scope, zoneName, revision, expectedRevision, state.LatestRevision, create, actor); err != nil {
		return ProviderMetadata{}, err
	}
	inserted, err := tx.InsertProviderCredentialRevision(ctx, ProviderRevisionInput{
		Scope: scope, Revision: revision, KeyID: keyID, Ciphertext: ciphertext,
		CiphertextSHA256: ciphertextHash, RevisionHash: revisionHash, Actor: actor,
	})
	if err != nil {
		return ProviderMetadata{}, providerStorageError(err)
	}
	if !inserted {
		return ProviderMetadata{}, providerRevisionConflict(scope, expectedRevision, state.LatestRevision)
	}
	metadata, found, err := tx.ProviderMetadata(ctx, scope)
	if err != nil {
		return ProviderMetadata{}, providerStorageError(err)
	}
	if !found {
		return ProviderMetadata{}, providerNotFound(scope)
	}
	return metadata, nil
}

// applyProviderRevision writes the provider pointer for one already-sealed
// revision: first state for a new scope, or the locked CAS update.
func applyProviderRevision(ctx context.Context, tx ProviderTx, scope ProviderScope, zoneName string, revision, expectedRevision, latest uint64, create bool, actor contract.Actor) error {
	if !create {
		updated, err := tx.UpdateProviderCredentialRevision(ctx, scope, expectedRevision, revision, actor)
		if err != nil {
			return providerStorageError(err)
		}
		if !updated {
			return providerRevisionConflict(scope, expectedRevision, latest)
		}
		return nil
	}
	inserted, err := tx.InsertProviderCredential(ctx, ProviderCredentialInput{Scope: scope, ZoneName: zoneName, Actor: actor})
	if err != nil {
		return providerStorageError(err)
	}
	if !inserted {
		return providerUniqueConflict(scope)
	}
	return nil
}

// GetProviderCredential reads secret-free provider metadata.
func (s *Store) GetProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, error) {
	if err := scope.Validate(); err != nil {
		return ProviderMetadata{}, err
	}
	if s == nil || s.providers == nil {
		return ProviderMetadata{}, providerStorageNotConfigured()
	}
	metadata, found, err := s.providers.ProviderMetadata(ctx, scope)
	if err != nil {
		return ProviderMetadata{}, providerStorageError(err)
	}
	if !found {
		return ProviderMetadata{}, providerNotFound(scope)
	}
	return metadata, nil
}

// ListProviderCredentials reads secret-free metadata for one provider. The
// list is bounded and ordered by zone name so a name reference is unambiguous.
func (s *Store) ListProviderCredentials(ctx context.Context, provider string, limit int) ([]ProviderMetadata, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !providerPattern.MatchString(provider) || provider != ProviderCloudflare {
		return nil, contract.NewFault(contract.ValidationFailed, "unsupported provider credential scope", "Only Cloudflare provider credentials are supported.")
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > MaxProviderZones {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid provider credential list limit", "Use a limit from 1 to 200.")
	}
	if s == nil || s.providers == nil {
		return nil, providerStorageNotConfigured()
	}
	metadata, err := s.providers.ListProviderMetadata(ctx, provider, limit)
	if err != nil {
		return nil, providerStorageError(err)
	}
	return metadata, nil
}

// ResolveProviderCredential decrypts the current revision for one operation.
// It always reads PostgreSQL; no caller may cache the plaintext across
// operations or process starts.
func (s *Store) ResolveProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, []byte, error) {
	if err := scope.Validate(); err != nil {
		return ProviderMetadata{}, nil, err
	}
	if s == nil || s.providers == nil {
		return ProviderMetadata{}, nil, providerStorageNotConfigured()
	}
	encrypted, found, err := s.providers.ProviderEncryptedRevision(ctx, scope)
	if err != nil {
		return ProviderMetadata{}, nil, providerStorageError(err)
	}
	if !found {
		return ProviderMetadata{}, nil, providerNotFound(scope)
	}
	metadata, found, err := s.providers.ProviderMetadata(ctx, scope)
	if err != nil {
		return ProviderMetadata{}, nil, providerStorageError(err)
	}
	if !found {
		return ProviderMetadata{}, nil, providerNotFound(scope)
	}
	if metadata.LatestRevision != encrypted.Revision ||
		metadata.LatestRevision != encrypted.LatestRevision ||
		metadata.ZoneName != encrypted.ZoneName ||
		metadata.Revision.Hash != encrypted.RevisionHash ||
		metadata.Revision.KeyID != encrypted.KeyID {
		return ProviderMetadata{}, nil, contract.NewFault(contract.Unavailable, "provider credential changed while it was read", "Retry the read; provider revisions are immutable but the latest pointer may advance concurrently.")
	}
	row := providerEncryptedRow{
		Metadata:       metadata,
		KeyID:          encrypted.KeyID,
		Ciphertext:     encrypted.Ciphertext,
		CiphertextHash: encrypted.CiphertextSHA256,
		RevisionHash:   encrypted.RevisionHash,
	}
	plaintext, err := decryptProviderRow(s.ring, scope, &row)
	if err != nil {
		return ProviderMetadata{}, nil, err
	}
	return row.Metadata, plaintext, nil
}

// Service wrappers keep the setup and DNS adapters on the existing vault
// boundary without introducing a second credential system.

func (s *Service) GetProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, error) {
	return s.store.GetProviderCredential(ctx, scope)
}

func (s *Service) ListProviderCredentials(ctx context.Context, provider string, limit int) ([]ProviderMetadata, error) {
	return s.store.ListProviderCredentials(ctx, provider, limit)
}

func (s *Service) ResolveProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, []byte, error) {
	return s.store.ResolveProviderCredential(ctx, scope)
}

type providerEncryptedRow struct {
	Metadata       ProviderMetadata
	KeyID          string
	Ciphertext     []byte
	CiphertextHash string
	RevisionHash   string
}

func decryptProviderRow(ring *KeyRing, scope ProviderScope, row *providerEncryptedRow) ([]byte, error) {
	if row.CiphertextHash != CiphertextSHA256(row.Ciphertext) {
		return nil, contract.NewFault(contract.PreconditionFailed, "provider credential ciphertext integrity check failed", "Restore the revision from backup; do not trust or re-encrypt modified ciphertext.")
	}
	expectedHash := ProviderRevisionHash(scope, row.Metadata.Revision.Revision, row.KeyID, row.Ciphertext)
	if expectedHash != row.RevisionHash {
		return nil, contract.NewFault(contract.PreconditionFailed, "provider credential revision binding check failed", "Restore the revision from backup; ciphertext cannot be moved between provider scopes or revisions.")
	}
	plaintext, keyID, err := ring.OpenProvider(scope, row.Metadata.Revision.Revision, row.Ciphertext)
	if err != nil {
		if IsCiphertextError(err) {
			return nil, contract.NewFault(contract.PreconditionFailed, "provider credential ciphertext failed authentication", "Restore the revision from backup and verify the master-key file; never log or retry modified ciphertext.")
		}
		return nil, err
	}
	if keyID != row.KeyID {
		clearBytes(plaintext)
		return nil, contract.NewFault(contract.PreconditionFailed, "provider credential key identity check failed", "Restore the revision from backup and verify the master-key file.")
	}
	row.Metadata.Revision.Hash = expectedHash
	return plaintext, nil
}

func providerNotFound(scope ProviderScope) *contract.FaultError {
	fault := contract.NewFault(contract.NotFound, "provider credential was not found", "Configure it with eregion dns configure --provider cloudflare, then retry.")
	fault.Details = map[string]any{"provider": scope.Provider, "zone_id": scope.ZoneID}
	return fault
}

func providerRevisionConflict(scope ProviderScope, expected, current uint64) *contract.FaultError {
	fault := contract.NewFault(contract.Conflict, "provider credential revision does not match", "Re-read the provider credential status and retry with its latest revision; the existing credential was not changed.")
	fault.Details = map[string]any{
		"provider": scope.Provider, "zone_id": scope.ZoneID,
		"expected_revision": expected, "current_revision": current,
	}
	return fault
}

func providerUniqueConflict(scope ProviderScope) *contract.FaultError {
	fault := contract.NewFault(contract.Conflict, "provider zone is already configured", "Read the provider credential status; a zone ID and zone name are both configured once.")
	fault.Details = map[string]any{"provider": scope.Provider, "zone_id": scope.ZoneID}
	return fault
}

func providerStorageNotConfigured() *contract.FaultError {
	return contract.NewFault(contract.Internal, "provider credential storage is not configured", "Construct the vault from the typed entities store before using provider credentials.")
}

func providerStorageError(err error) error {
	var fault *contract.FaultError
	if errors.As(err, &fault) {
		return fault
	}
	return databaseFault(err)
}

// ProviderRevisionHash binds provider ciphertext to its exact scope and
// revision without using plaintext. It is deterministic across processes and
// safe to persist or log.
func ProviderRevisionHash(scope ProviderScope, revision uint64, keyID string, ciphertext []byte) string {
	hash := sha256.New()
	hash.Write([]byte("eregion-provider-credential-revision/v1"))
	writeHashField(hash, scope.Provider)
	writeHashField(hash, scope.ZoneID)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], revision)
	hash.Write(number[:])
	writeHashField(hash, keyID)
	writeHashField(hash, string(ciphertext))
	return hex.EncodeToString(hash.Sum(nil))
}

// providerNextRevision is the CAS decision used under the row lock. expected 0
// creates only when no row exists; otherwise the immutable latest revision is
// the guard. Revision numbers are bounded by the signed bigint column and never
// wrap.
func providerNextRevision(scope ProviderScope, exists bool, latest, expected uint64) (uint64, bool, error) {
	if !exists {
		if expected != 0 {
			return 0, false, providerRevisionConflict(scope, expected, 0)
		}
		return 1, true, nil
	}
	if expected == 0 || expected != latest {
		return 0, false, providerRevisionConflict(scope, expected, latest)
	}
	if latest >= math.MaxInt64 {
		return 0, false, contract.NewFault(contract.Conflict, "provider credential revision space is exhausted", "Report the binary version; revision numbers cannot wrap.")
	}
	return latest + 1, false, nil
}
