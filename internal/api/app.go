package api

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/vault"
)

// AppService is the transport-neutral application boundary shared by the HTTP
// API and node RPC. It knows no authentication, HTTP or SSH; callers resolve an
// actor before using it.
type AppService struct {
	Operations operations.Service
	Vault      VaultService
	ReadyCheck func(context.Context) error
	// Retention is the optional disk-retention surface. It performs the same
	// planner and policy writes as the periodic job.
	Retention RetentionService
	// Cloudflare is the bounded provider-credential setup surface. It is not a
	// platform action and never enters River.
	Cloudflare CloudflareSetupService
}

// CloudflareCredentialPreview is the revision-bound, secret-free setup plan.
type CloudflareCredentialPreview struct {
	Provider        string   `json:"provider"`
	ZoneID          string   `json:"zone_id"`
	ZoneName        string   `json:"zone_name"`
	CurrentRevision uint64   `json:"current_revision"`
	NextRevision    uint64   `json:"next_revision"`
	ReadValidated   bool     `json:"read_validated"`
	Validation      string   `json:"validation"`
	Warnings        []string `json:"warnings,omitempty"`
}

// CloudflareSetupService is the narrow AppService dependency. Implementations
// validate the token with a read-only provider request before activation and
// must leave the previous credential active on any failure.
type CloudflareSetupService interface {
	PreviewCloudflareCredential(context.Context, contract.Actor, string, string, []byte) (CloudflareCredentialPreview, error)
	ConfigureCloudflareCredential(context.Context, contract.Actor, string, string, uint64, []byte) (vault.ProviderMetadata, error)
	ListCloudflareCredentials(context.Context, contract.Actor, string) ([]vault.ProviderMetadata, error)
}

// RetentionService is the node-side retention surface. It is separate from
// operations because show and plan are read-only and server policy is not an
// environment revision.
type RetentionService interface {
	RetentionPlan(ctx context.Context, node, app, environment string) (retention.PlanResult, error)
	RetentionPlanWithInventory(ctx context.Context, node, app, environment string, inventory retention.Inventory) (retention.PlanResult, error)
	RetentionShow(ctx context.Context, app, environment string) (retention.PolicyView, error)
	SetServerRetentionPolicy(ctx context.Context, policy retention.Policy) error
}

// NewAppService builds the shared boundary.
func NewAppService(operationsService operations.Service, vaultService VaultService, ready func(context.Context) error) AppService {
	return AppService{Operations: operationsService, Vault: vaultService, ReadyCheck: ready}
}

// Ready reports control-plane database/schema readiness.
func (s AppService) Ready(ctx context.Context) error {
	if s.ReadyCheck == nil {
		return contract.NewFault(contract.Unavailable, "readiness is not configured", "Start the node's control plane service.")
	}
	return s.ReadyCheck(ctx)
}

// PreviewCloudflareCredential is the dedicated bounded setup preview. The
// token travels as transient request data, never as operation input.
func (s AppService) PreviewCloudflareCredential(ctx context.Context, actor contract.Actor, zoneID, zoneName string, token []byte) (CloudflareCredentialPreview, error) {
	if s.Cloudflare == nil {
		return CloudflareCredentialPreview{}, contract.NewFault(contract.Unavailable, "Cloudflare setup is not configured on this node", "Set EREGION_VAULT_KEY_FILE and start the node control plane.")
	}
	return s.Cloudflare.PreviewCloudflareCredential(ctx, actor, zoneID, zoneName, token)
}

// ConfigureCloudflareCredential commits one revision-bound provider credential.
func (s AppService) ConfigureCloudflareCredential(ctx context.Context, actor contract.Actor, zoneID, zoneName string, expectedRevision uint64, token []byte) (vault.ProviderMetadata, error) {
	if s.Cloudflare == nil {
		return vault.ProviderMetadata{}, contract.NewFault(contract.Unavailable, "Cloudflare setup is not configured on this node", "Set EREGION_VAULT_KEY_FILE and start the node control plane.")
	}
	return s.Cloudflare.ConfigureCloudflareCredential(ctx, actor, zoneID, zoneName, expectedRevision, token)
}

// ListCloudflareCredentials reads secret-free provider metadata for status.
func (s AppService) ListCloudflareCredentials(ctx context.Context, actor contract.Actor, zoneRef string) ([]vault.ProviderMetadata, error) {
	if s.Cloudflare == nil {
		return nil, contract.NewFault(contract.Unavailable, "Cloudflare setup is not configured on this node", "Set EREGION_VAULT_KEY_FILE and start the node control plane.")
	}
	return s.Cloudflare.ListCloudflareCredentials(ctx, actor, zoneRef)
}

// NormalizeAction validates one platform action before authorization or
// mutation. HTTP uses it to authorize the canonical kind/target; node RPC uses
// the same result through PreviewOrSubmit.
func NormalizeAction(kind, target string, input json.RawMessage) (platform.Normalized, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return platform.Default().Normalize(kind, target, input)
}

// PreviewOrSubmit normalizes a registered action and previews or submits it.
func (s AppService) PreviewOrSubmit(ctx context.Context, actor contract.Actor, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool) (any, error) {
	return s.PreviewOrSubmitWithFactor(ctx, actor, kind, target, input, idempotencyKey, dryRun, "")
}

// PreviewOrSubmitWithFactor is PreviewOrSubmit with a transient human-factor
// code for the same request. The code never enters operation input, events or
// River arguments.
func (s AppService) PreviewOrSubmitWithFactor(ctx context.Context, actor contract.Actor, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool, humanFactor string) (any, error) {
	normalized, err := NormalizeAction(kind, target, input)
	if err != nil {
		return nil, err
	}
	return s.SubmitNormalizedWithFactor(ctx, actor, normalized, idempotencyKey, dryRun, humanFactor)
}

// SubmitNormalized previews or submits an already-normalized platform action.
func (s AppService) SubmitNormalized(ctx context.Context, actor contract.Actor, normalized platform.Normalized, idempotencyKey string, dryRun bool) (any, error) {
	return s.SubmitNormalizedWithFactor(ctx, actor, normalized, idempotencyKey, dryRun, "")
}

// SubmitNormalizedWithFactor is SubmitNormalized with a transient human-factor
// code. The control plane verifies it inside the submission transaction when
// the locked target state requires it.
func (s AppService) SubmitNormalizedWithFactor(ctx context.Context, actor contract.Actor, normalized platform.Normalized, idempotencyKey string, dryRun bool, humanFactor string) (any, error) {
	result, err := s.SubmitNormalizedWithReceipt(ctx, actor, normalized, idempotencyKey, dryRun, humanFactor, nil)
	if err != nil {
		return nil, err
	}
	if receipt, ok := result.(operations.SubmissionResult); ok {
		return receipt.Operation, nil
	}
	return result, nil
}

// PreviewOrSubmitWithReceipt is the node transport entry point. It returns a
// preview for dry runs and operations.SubmissionResult for accepted work, so
// the caller can distinguish a fresh admission from an authoritative replay
// before installing or printing a webhook secret.
func (s AppService) PreviewOrSubmitWithReceipt(ctx context.Context, actor contract.Actor, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool, humanFactor string, webhookSecret []byte) (any, error) {
	normalized, err := NormalizeAction(kind, target, input)
	if err != nil {
		return nil, err
	}
	return s.SubmitNormalizedWithReceipt(ctx, actor, normalized, idempotencyKey, dryRun, humanFactor, webhookSecret)
}

// SubmitNormalizedWithReceipt previews or atomically submits an already
// normalized action and returns the admission receipt when the service
// supports it.
func (s AppService) SubmitNormalizedWithReceipt(ctx context.Context, actor contract.Actor, normalized platform.Normalized, idempotencyKey string, dryRun bool, humanFactor string, webhookSecret []byte) (any, error) {
	if dryRun {
		if s.Operations != nil {
			if previewer, ok := s.Operations.(platformPreviewService); ok {
				return previewer.Preview(ctx, actor, normalized, idempotencyKey)
			}
		}
		return platform.Preview{
			Kind: normalized.Kind, Target: normalized.Target.String(), DryRun: true,
			Input: normalized.Input.Preview(), Summary: normalized.Summary,
			Warnings: []string{"A detailed deployment plan requires a control-plane service."},
		}, nil
	}
	if s.Operations == nil {
		return nil, contract.NewFault(contract.Unavailable, "the operation service is not configured", "Start the node's control plane service.")
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, contract.NewFault(contract.InputRequired, "an idempotency key is required", "Reuse one stable key for retries of this submission.")
	}
	submission := operations.Submission{
		Kind: normalized.Kind, Target: normalized.Target.String(), Actor: actor,
		IdempotencyKey: idempotencyKey, Input: normalized.Canonical, HumanFactor: humanFactor,
		WebhookSecret: webhookSecret,
	}
	if submitter, ok := s.Operations.(operations.AtomicSubmitter); ok {
		return submitter.SubmitWithReceipt(ctx, submission)
	}
	if len(webhookSecret) > 0 || normalizedRequestsWebhook(normalized) {
		return nil, contract.NewFault(contract.Unavailable,
			"the operation service cannot report webhook admission atomically",
			"Start a node control plane with transactional webhook admission; do not use a preview-only or legacy service.")
	}
	return s.Operations.Submit(ctx, submission)
}

func normalizedRequestsWebhook(normalized platform.Normalized) bool {
	input, ok := normalized.Input.(*platform.EnvironmentUpdateInput)
	if !ok {
		return false
	}
	return input.RotateWebhookSecret || (input.AutoDeploy != nil && *input.AutoDeploy)
}

// SubmitDiagnostic previews or submits the diagnostic test action.
func (s AppService) SubmitDiagnostic(ctx context.Context, actor contract.Actor, input json.RawMessage, idempotencyKey string, dryRun bool) (any, error) {
	if s.Operations == nil {
		return nil, contract.NewFault(contract.Unavailable, "the operation service is not configured", "Start the node's control plane service.")
	}
	normalized, err := operations.NormalizeTestInput(input)
	if err != nil {
		return nil, err
	}
	if dryRun {
		return operations.TestPreview{Kind: operations.TestKind, Target: operations.TestTarget, Input: normalized, DryRun: true}, nil
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, contract.NewFault(contract.InputRequired, "an idempotency key is required", "Reuse one stable key for retries of this submission.")
	}
	canonical, _ := json.Marshal(normalized)
	return s.Operations.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: actor,
		IdempotencyKey: idempotencyKey, Input: canonical,
	})
}

// GetOperation reads one operation under the caller's authority.
func (s AppService) GetOperation(ctx context.Context, authority operations.Authority, id contract.OperationID) (contract.Operation, error) {
	if s.Operations == nil {
		return contract.Operation{}, contract.NewFault(contract.Unavailable, "the operation service is not configured", "Start the node's control plane service.")
	}
	return s.Operations.Get(ctx, authority, id)
}

// OperationEvents reads one bounded event page under the caller's authority.
func (s AppService) OperationEvents(ctx context.Context, authority operations.Authority, cursor contract.Cursor, limit int) (operations.EventPage, error) {
	if s.Operations == nil {
		return operations.EventPage{}, contract.NewFault(contract.Unavailable, "the operation service is not configured", "Start the node's control plane service.")
	}
	return s.Operations.Events(ctx, authority, cursor, limit)
}

// CancelOperation requests cancellation under the caller's authority.
func (s AppService) CancelOperation(ctx context.Context, authority operations.Authority, id contract.OperationID) (contract.Operation, error) {
	if s.Operations == nil {
		return contract.Operation{}, contract.NewFault(contract.Unavailable, "the operation service is not configured", "Start the node's control plane service.")
	}
	return s.Operations.RequestCancel(ctx, authority, id)
}

// ParseInventoryQueryValues validates an inventory request supplied as values
// rather than a raw URL query.
func ParseInventoryQueryValues(filters map[string][]string) (platform.InventoryQuery, error) {
	return parseInventoryQuery(url.Values(filters).Encode())
}

// operationListService is implemented by a control-plane service that can read
// bounded operation history. It is optional so existing test doubles keep
// compiling and a missing implementation fails closed.
type operationListService interface {
	ListOperations(context.Context, operations.Authority, operations.ListQuery) (operations.ListPage, error)
}

// ListOperations reads operation history under the caller's authority.
func (s AppService) ListOperations(ctx context.Context, authority operations.Authority, query operations.ListQuery) (operations.ListPage, error) {
	service, ok := s.Operations.(operationListService)
	if !ok {
		return operations.ListPage{}, contract.NewFault(contract.Unavailable, "operation history is not configured", "Use a control-plane service with durable operation state.")
	}
	return service.ListOperations(ctx, authority, query)
}

// Inventory reads one bounded inventory collection.
func (s AppService) Inventory(ctx context.Context, actor contract.Actor, query platform.InventoryQuery) (platform.Inventory, error) {
	service, ok := s.Operations.(platformInventoryService)
	if !ok {
		return platform.Inventory{}, contract.NewFault(contract.Unavailable, "platform inventory is not configured", "Use a control-plane service with durable platform state.")
	}
	return service.Inventory(ctx, actor, query)
}

// SetSecretValue encrypts one revision. Values never enter an operation input.
func SetSecretValue(ctx context.Context, service VaultService, actor contract.Actor, namespace vault.Namespace, value []byte, expectedRevision uint64) (vault.Metadata, error) {
	if service == nil {
		return vault.Metadata{}, contract.NewFault(contract.Unavailable, "the secret service is not configured", "Set EREGION_VAULT_KEY_FILE on the node.")
	}
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, err
	}
	if err := vault.ValidateValue(value); err != nil {
		return vault.Metadata{}, err
	}
	return service.Set(ctx, namespace, value, expectedRevision, actor)
}

// ArchiveSecretValue archives one namespace under a CAS revision guard.
func ArchiveSecretValue(ctx context.Context, service VaultService, actor contract.Actor, namespace vault.Namespace, expectedRevision uint64) (vault.Metadata, error) {
	if service == nil {
		return vault.Metadata{}, contract.NewFault(contract.Unavailable, "the secret service is not configured", "Set EREGION_VAULT_KEY_FILE on the node.")
	}
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, err
	}
	return service.Archive(ctx, namespace, expectedRevision, actor)
}

// GetSecretMetadata reads secret-free metadata.
func GetSecretMetadata(ctx context.Context, service VaultService, namespace vault.Namespace) (vault.Metadata, error) {
	if service == nil {
		return vault.Metadata{}, contract.NewFault(contract.Unavailable, "the secret service is not configured", "Set EREGION_VAULT_KEY_FILE on the node.")
	}
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, err
	}
	return service.Get(ctx, namespace)
}

// ListSecretMetadata reads one environment's metadata page.
func ListSecretMetadata(ctx context.Context, service VaultService, app, environment string, limit int) ([]vault.Metadata, error) {
	if service == nil {
		return nil, contract.NewFault(contract.Unavailable, "the secret service is not configured", "Set EREGION_VAULT_KEY_FILE on the node.")
	}
	if err := vault.ValidateApp(app); err != nil {
		return nil, err
	}
	if err := vault.ValidateEnvironment(environment); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > vault.MaxListLimit {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid secret list limit", "Use a limit from 1 to 200.")
	}
	return service.List(ctx, app, environment, limit)
}

// ExportSecretValue returns plaintext for an explicit protected destination.
// Callers must never print the value and clear the slice after use.
func ExportSecretValue(ctx context.Context, service VaultService, namespace vault.Namespace) (vault.Metadata, []byte, error) {
	if service == nil {
		return vault.Metadata{}, nil, contract.NewFault(contract.Unavailable, "the secret service is not configured", "Set EREGION_VAULT_KEY_FILE on the node.")
	}
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, nil, err
	}
	return service.Export(ctx, namespace)
}
