package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// PostgresWebhookDeliveries is the shared durable admission and replay guard
// for GitHub deliveries. It lives in eregion_webhook_deliveries, separate from
// operation retention, and stores only SHA-256 hashes plus sanitized intent,
// non-secret identity and the accepted operation/job IDs.
type PostgresWebhookDeliveries struct {
	store *entities.Store
}

var _ api.WebhookAdmissionStore = (*PostgresWebhookDeliveries)(nil)

func NewWebhookDeliveryStore(store *entities.Store) *PostgresWebhookDeliveries {
	return &PostgresWebhookDeliveries{store: store}
}

var errWebhookSpeculativeDuplicate = errors.New("webhook speculative admission duplicate")

func webhookReceiptInsert(incoming api.WebhookAdmission, jobID int64) entities.WebhookReceiptInsert {
	return entities.WebhookReceiptInsert{
		DeliveryIDHash: incoming.DeliveryIDHash, BodySHA256: incoming.BodySHA256,
		Actor: incoming.Actor, Kind: incoming.Kind, Target: incoming.Target,
		EnvironmentID: incoming.EnvironmentID, RevisionID: incoming.RevisionID,
		SecretRevision: incoming.SecretRevision, SecretRevisionHash: incoming.SecretRevisionHash,
		Repository: incoming.Repository, Branch: incoming.Branch, Commit: incoming.Commit,
		JobID: jobID,
	}
}

func webhookAdmissionFromReceipt(receipt entities.WebhookReceipt) api.WebhookAdmission {
	return api.WebhookAdmission{
		DeliveryIDHash: receipt.DeliveryIDHash, BodySHA256: receipt.BodySHA256,
		Actor: receipt.Actor, Kind: receipt.Kind, Target: receipt.Target,
		State: api.WebhookDeliveryState(receipt.State), EnvironmentID: receipt.EnvironmentID,
		RevisionID: receipt.RevisionID, SecretRevision: receipt.SecretRevision,
		SecretRevisionHash: receipt.SecretRevisionHash, Repository: receipt.Repository,
		Branch: receipt.Branch, Commit: receipt.Commit,
		OperationID: receipt.OperationID, JobID: receipt.JobID,
		AdmissionAttempts: int(receipt.AdmissionAttempts), ErrorCode: string(receipt.ErrorCode),
		FirstSeenAt: receipt.FirstSeenAt,
	}
}

// Admit inserts one new admission and its River processing job in one
// transaction, or returns the existing record. The primary key arbitrates
// concurrent replays across control planes. A known delivery bound to a
// different body hash or actor/kind/target is refused as a conflict.
//
// A new row is inserted first and its observed environment/revision/secret
// binding is checked in the same transaction. A changed binding rolls the
// insert back and returns ErrWebhookBindingChanged, so the handler can
// re-resolve and re-verify the signature instead of admitting stale intent.
func (s *PostgresWebhookDeliveries) Admit(ctx context.Context, incoming api.WebhookAdmission) (api.WebhookAdmission, bool, error) {
	if err := validateWebhookAdmissionRecord(incoming); err != nil {
		return api.WebhookAdmission{}, false, err
	}
	var result api.WebhookAdmission
	var created bool
	err := s.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var jobID int64
		var firstSeen time.Time
		var admitted bool
		scopeErr := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
			var err error
			jobID, err = nested.Jobs().EnqueueWebhook(ctx, incoming.EnvironmentID, incoming.DeliveryIDHash)
			if err != nil {
				return err
			}
			firstSeen, admitted, err = nested.Webhooks().Admit(ctx, webhookReceiptInsert(incoming, jobID))
			if err != nil {
				return err
			}
			if !admitted {
				return errWebhookSpeculativeDuplicate
			}
			return nil
		})
		if scopeErr == errWebhookSpeculativeDuplicate {
			existing, found, err := tx.Webhooks().Get(ctx, incoming.EnvironmentID, incoming.DeliveryIDHash)
			if err != nil {
				return err
			}
			if !found {
				return contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation disappeared", "Do not resubmit; the delivery record is no longer available.")
			}
			if err := compareWebhookAdmission(webhookAdmissionFromReceipt(existing), incoming); err != nil {
				return err
			}
			result, created = webhookAdmissionFromReceipt(existing), false
			return nil
		}
		if scopeErr != nil {
			return scopeErr
		}
		current, err := webhookBindingCurrentTx(ctx, tx, incoming)
		if err != nil {
			return err
		}
		if !current {
			return api.ErrWebhookBindingChanged
		}
		result = incoming
		result.State = api.WebhookDeliveryAdmitted
		result.JobID = jobID
		result.AdmissionAttempts = 1
		result.FirstSeenAt = firstSeen.UTC()
		created = true
		return nil
	})
	if err != nil {
		if errors.Is(err, api.ErrWebhookBindingChanged) {
			return api.WebhookAdmission{}, false, api.ErrWebhookBindingChanged
		}
		var fault *contract.Fault
		if errors.As(err, &fault) {
			return api.WebhookAdmission{}, false, err
		}
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	return result, created, nil
}

// ReconcileLegacy links an old pending row to the operation its former
// synchronous handler may already have created. It never resubmits: the old
// row has no commit to reconstruct, so found=false is reported as ambiguous.
func (s *PostgresWebhookDeliveries) ReconcileLegacy(ctx context.Context, incoming api.WebhookAdmission, legacyKey string, canonical []byte) (api.WebhookAdmission, bool, error) {
	if err := validateWebhookAdmissionRecord(incoming); err != nil {
		return api.WebhookAdmission{}, false, err
	}
	if err := contract.ValidateIdempotencyKey(legacyKey); err != nil {
		return api.WebhookAdmission{}, false, err
	}
	if len(canonical) == 0 {
		return api.WebhookAdmission{}, false, contract.NewFault(contract.ValidationFailed, "legacy webhook reconciliation has no canonical input", "Recompute the deploy input before reconciling an old delivery.")
	}
	var result api.WebhookAdmission
	var linked bool
	err := s.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		record, found, err := tx.Webhooks().Lock(ctx, incoming.EnvironmentID, incoming.DeliveryIDHash)
		if err != nil {
			return err
		}
		if !found {
			return contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation disappeared", "Do not resubmit; the delivery record is no longer available.")
		}
		existing := webhookAdmissionFromReceipt(record)
		if err := compareWebhookAdmission(existing, incoming); err != nil {
			return err
		}
		if existing.State == api.WebhookDeliverySubmitted || existing.State != api.WebhookDeliveryPending {
			result, linked = existing, true
			return nil
		}
		identity, found, err := tx.Operations().IdentityForShare(ctx, entities.OperationKey{
			Kind: incoming.Kind, Target: incoming.Target, Actor: incoming.Actor, IdempotencyKey: legacyKey,
		})
		if err != nil {
			return err
		}
		if !found {
			result, linked = existing, false
			return nil
		}
		if identity.InputHash != sha256Hex(canonical) {
			return idempotencyInputConflict()
		}
		submitted, err := tx.Webhooks().Submit(ctx, incoming.EnvironmentID, incoming.DeliveryIDHash, identity.ID, string(api.WebhookDeliveryPending))
		if err != nil {
			return err
		}
		if !submitted {
			return contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation changed", "Retry the redelivery; do not submit a second operation.")
		}
		existing.State = api.WebhookDeliverySubmitted
		existing.OperationID = identity.ID
		result, linked = existing, true
		return nil
	})
	if err != nil {
		var fault *contract.Fault
		if errors.As(err, &fault) {
			return api.WebhookAdmission{}, false, err
		}
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	return result, linked, nil
}

// Lookup reads one record. It is exposed for tests and status reporting.
func (s *PostgresWebhookDeliveries) Lookup(ctx context.Context, environmentID, deliveryIDHash string) (api.WebhookAdmission, bool, error) {
	record, found, err := s.store.Webhooks().Get(ctx, environmentID, deliveryIDHash)
	if err != nil {
		return api.WebhookAdmission{}, false, err
	}
	if !found {
		return api.WebhookAdmission{}, false, nil
	}
	return webhookAdmissionFromReceipt(record), true, nil
}

// webhookBindingCurrentTx rechecks the exact non-secret identities the resolver
// observed and holds the locks until the admission transaction commits.
// Lock order is environment then secret, matching deleteEnvironmentRowsTx:
// the environment row is share-locked first, then the secret namespace row.
// Existing paths are consistent: vault Store.Set/Archive lock only the secret
// row, environment updates lock only the environment row, and platform delete
// locks environment then secret. A rotation/archival therefore either commits
// before validation (making the old proof stale) or waits until admission
// commits, without a reverse-order deadlock against deletion.
//
// After both locks are held, the complete snapshot is read again and compared
// with the resolver's observation. The locks prevent either identity from
// changing before the receipt commits.
func webhookBindingCurrentTx(ctx context.Context, tx *entities.StoreTx, record api.WebhookAdmission) (bool, error) {
	environmentID := platform.EnvironmentID(record.EnvironmentID)
	binding, found, err := tx.Platform().Outcomes().WebhookBinding(ctx, environmentID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if binding.CurrentRevisionID != platform.RevisionID(record.RevisionID) || !binding.AutoDeploy || binding.WebhookSecretName == "" {
		return false, nil
	}
	// Environment first, then secret: this is the same order as environment
	// deletion, which updates the environment row and then deletes its secrets.
	locked, err := tx.Secrets().LockBinding(ctx, binding.App, binding.Environment, binding.WebhookSecretName)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	// Both locks are held now. Re-read the whole non-secret snapshot and
	// compare it with the resolver's observation; neither row can change
	// before this transaction commits.
	snapshot, found, err := tx.Platform().Outcomes().SecretBinding(ctx, environmentID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return snapshot.CurrentRevisionID == platform.RevisionID(record.RevisionID) &&
		snapshot.AutoDeploy && snapshot.WebhookSecretName == binding.WebhookSecretName &&
		snapshot.SecretState == string(vault.StateActive) &&
		snapshot.LatestRevision == int64(record.SecretRevision) &&
		snapshot.RevisionHash == record.SecretRevisionHash, nil
}

// compareWebhookAdmission binds one delivery ID to exactly one payload and
// operation identity. Comparing the body hash alone would let the same signed
// delivery ID be rebound to a different actor or target after a path change.
func compareWebhookAdmission(existing, incoming api.WebhookAdmission) error {
	if existing.EnvironmentID != incoming.EnvironmentID || existing.BodySHA256 != incoming.BodySHA256 ||
		existing.Actor.Kind != incoming.Actor.Kind ||
		existing.Actor.Subject != incoming.Actor.Subject ||
		existing.Kind != incoming.Kind ||
		existing.Target != incoming.Target {
		return contract.NewFault(contract.Conflict, "webhook delivery already bound a different identity", "Treat this as a replay conflict; never submit a second operation for one GitHub delivery ID.")
	}
	return nil
}

func validateWebhookAdmissionRecord(record api.WebhookAdmission) error {
	if !validHash(record.DeliveryIDHash) || !validHash(record.BodySHA256) {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery hashes are malformed", "Store only lowercase SHA-256 hex digests of the delivery ID and body.")
	}
	if record.Actor.Kind != "webhook" || record.Actor.Subject == "" || len(record.Actor.Subject) > 256 {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery actor is invalid", "Use the webhook actor resolved from the signed payload.")
	}
	if record.Kind != platform.KindDeploy {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery action is invalid", "Admit only platform.deploy pushes.")
	}
	target, err := platform.ParseTarget(record.Target)
	if err != nil {
		return err
	}
	if target.Kind != platform.TargetEnvironment {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery target is invalid", "Admit only an environment deploy target.")
	}
	if record.EnvironmentID == "" || record.RevisionID == "" {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery has no environment snapshot", "Bind the delivery to the observed environment and revision.")
	}
	if record.SecretRevision == 0 || !validHash(record.SecretRevisionHash) {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery has no secret revision identity", "Bind the delivery to the vault revision that verified its signature.")
	}
	if record.Repository == "" || record.Branch == "" || !platform.IsGitCommit(record.Commit) {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery has no validated source binding", "Record the signed repository, bound branch and exact commit.")
	}
	return nil
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
