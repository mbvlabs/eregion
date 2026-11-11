package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// PostgresWebhookDeliveries is the shared durable admission and replay guard
// for GitHub deliveries. It lives in eregion_webhook_deliveries, separate from
// operation retention, and stores only SHA-256 hashes plus sanitized intent,
// non-secret identity and the accepted operation/job IDs.
type PostgresWebhookDeliveries struct {
	pool    *pgxpool.Pool
	enqueue func(context.Context, pgx.Tx, string, string) (int64, error)
}

var _ api.WebhookAdmissionStore = (*PostgresWebhookDeliveries)(nil)

func NewWebhookDeliveryStore(pool *pgxpool.Pool) *PostgresWebhookDeliveries {
	return &PostgresWebhookDeliveries{pool: pool}
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	defer rollback(tx)
	if s.enqueue == nil {
		return api.WebhookAdmission{}, false, unavailable()
	}
	// The admitted lifecycle check requires job_id immediately. Insert the
	// River job and the receipt that references it inside a savepoint first; a
	// duplicate delivery rolls that savepoint back, which removes the
	// speculative job and its notification before the existing record is read.
	insertTx, err := tx.Begin(ctx)
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	jobID, err := s.enqueue(ctx, insertTx, incoming.EnvironmentID, incoming.DeliveryIDHash)
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	var firstSeen time.Time
	err = insertTx.QueryRow(ctx, `INSERT INTO eregion_webhook_deliveries
        (delivery_id_hash, body_sha256, actor_kind, actor_subject, kind, target, state,
         environment_id, revision_id, secret_revision, secret_revision_hash,
         repository, branch, commit_sha, job_id, admitted_at, admission_attempts)
        VALUES ($1,$2,$3,$4,$5,$6,'admitted',$7,$8,$9,$10,$11,$12,$13,$14,clock_timestamp(),1)
        ON CONFLICT (environment_id, delivery_id_hash) DO NOTHING
        RETURNING first_seen_at`,
		incoming.DeliveryIDHash, incoming.BodySHA256, incoming.Actor.Kind, incoming.Actor.Subject,
		incoming.Kind, incoming.Target, incoming.EnvironmentID, incoming.RevisionID,
		incoming.SecretRevision, incoming.SecretRevisionHash, incoming.Repository, incoming.Branch,
		incoming.Commit, jobID).Scan(&firstSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := insertTx.Rollback(ctx); err != nil {
			return api.WebhookAdmission{}, false, unavailable(err)
		}
		existing, err := loadWebhookAdmissionTx(ctx, tx, incoming.EnvironmentID, incoming.DeliveryIDHash, false)
		if err != nil {
			return api.WebhookAdmission{}, false, unavailable(err)
		}
		if err := compareWebhookAdmission(existing, incoming); err != nil {
			return api.WebhookAdmission{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return api.WebhookAdmission{}, false, unavailable(err)
		}
		return existing, false, nil
	}
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	if err := insertTx.Commit(ctx); err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	current, err := s.bindingCurrentTx(ctx, tx, incoming)
	if err != nil {
		return api.WebhookAdmission{}, false, err
	}
	if !current {
		return api.WebhookAdmission{}, false, api.ErrWebhookBindingChanged
	}
	if err := tx.Commit(ctx); err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	incoming.State = api.WebhookDeliveryAdmitted
	incoming.JobID = jobID
	incoming.AdmissionAttempts = 1
	incoming.FirstSeenAt = firstSeen.UTC()
	return incoming, true, nil
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	defer rollback(tx)
	existing, err := loadWebhookAdmissionTx(ctx, tx, incoming.EnvironmentID, incoming.DeliveryIDHash, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.WebhookAdmission{}, false, contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation disappeared", "Do not resubmit; the delivery record is no longer available.")
	}
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	if err := compareWebhookAdmission(existing, incoming); err != nil {
		return api.WebhookAdmission{}, false, err
	}
	if existing.State == api.WebhookDeliverySubmitted {
		if err := tx.Commit(ctx); err != nil {
			return api.WebhookAdmission{}, false, unavailable(err)
		}
		return existing, true, nil
	}
	if existing.State != api.WebhookDeliveryPending {
		if err := tx.Commit(ctx); err != nil {
			return api.WebhookAdmission{}, false, unavailable(err)
		}
		return existing, true, nil
	}
	var operationID string
	var inputHash string
	err = tx.QueryRow(ctx, `SELECT id,input_hash FROM eregion_operations
        WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5
        FOR SHARE`,
		incoming.Actor.Kind, incoming.Actor.Subject, incoming.Kind, incoming.Target, legacyKey).
		Scan(&operationID, &inputHash)
	if errors.Is(err, pgx.ErrNoRows) {
		// The old operation was purged. Do not invent a new one: the original
		// acceptance cannot be proven, and resubmitting could deploy twice.
		if err := tx.Commit(ctx); err != nil {
			return api.WebhookAdmission{}, false, unavailable(err)
		}
		return existing, false, nil
	}
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	if inputHash != sha256Hex(canonical) {
		return api.WebhookAdmission{}, false, idempotencyInputConflict()
	}
	tag, err := tx.Exec(ctx, `UPDATE eregion_webhook_deliveries
        SET state='submitted', operation_id=$3, submitted_at=clock_timestamp()
        WHERE environment_id=$1 AND delivery_id_hash=$2 AND state='pending'`, incoming.EnvironmentID, incoming.DeliveryIDHash, operationID)
	if err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	if tag.RowsAffected() != 1 {
		return api.WebhookAdmission{}, false, contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation changed", "Retry the redelivery; do not submit a second operation.")
	}
	if err := tx.Commit(ctx); err != nil {
		return api.WebhookAdmission{}, false, unavailable(err)
	}
	existing.State = api.WebhookDeliverySubmitted
	existing.OperationID = contract.OperationID(operationID)
	return existing, true, nil
}

// Lookup reads one record. It is exposed for tests and status reporting.
func (s *PostgresWebhookDeliveries) Lookup(ctx context.Context, environmentID, deliveryIDHash string) (api.WebhookAdmission, bool, error) {
	record, err := loadWebhookAdmissionTx(ctx, s.pool, environmentID, deliveryIDHash, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.WebhookAdmission{}, false, nil
	}
	if err != nil {
		return api.WebhookAdmission{}, false, err
	}
	return record, true, nil
}

// bindingCurrentTx rechecks the exact non-secret identities the resolver
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
func (s *PostgresWebhookDeliveries) bindingCurrentTx(ctx context.Context, tx pgx.Tx, record api.WebhookAdmission) (bool, error) {
	var appName, envName, revisionID, secretName string
	var autoDeploy bool
	err := tx.QueryRow(ctx, `SELECT a.name, e.name, coalesce(e.current_revision_id,''),
        coalesce(r.auto_deploy,false), coalesce(r.webhook_secret_name,'')
        FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
        WHERE e.id=$1
        FOR SHARE OF e`, record.EnvironmentID).
		Scan(&appName, &envName, &revisionID, &autoDeploy, &secretName)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, unavailable(err)
	}
	if revisionID != record.RevisionID || !autoDeploy || secretName == "" {
		return false, nil
	}
	// Environment first, then secret: this is the same order as environment
	// deletion, which updates the environment row and then deletes its secrets.
	var locked int
	err = tx.QueryRow(ctx, `SELECT 1 FROM eregion_secrets
        WHERE app=$1 AND environment=$2 AND name=$3
        FOR SHARE`, appName, envName, secretName).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, unavailable(err)
	}
	// Both locks are held now. Re-read the whole non-secret snapshot and
	// compare it with the resolver's observation; neither row can change
	// before this transaction commits.
	var currentRevisionID, currentSecretName, secretState, revisionHash string
	var currentAutoDeploy bool
	var latestRevision uint64
	err = tx.QueryRow(ctx, `SELECT coalesce(e.current_revision_id,''),
        coalesce(r.auto_deploy,false), coalesce(r.webhook_secret_name,''),
        coalesce(s.state,''), coalesce(s.latest_revision,0),
        coalesce((SELECT sr.revision_hash FROM eregion_secret_revisions sr
                  WHERE sr.app=s.app AND sr.environment=s.environment
                    AND sr.name=s.name AND sr.revision=s.latest_revision), '')
        FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
        LEFT JOIN eregion_secrets s
          ON s.app=a.name AND s.environment=e.name AND s.name=r.webhook_secret_name
        WHERE e.id=$1`, record.EnvironmentID).
		Scan(&currentRevisionID, &currentAutoDeploy, &currentSecretName, &secretState, &latestRevision, &revisionHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, unavailable(err)
	}
	return currentRevisionID == record.RevisionID &&
		currentAutoDeploy && currentSecretName == secretName &&
		secretState == "active" &&
		latestRevision == record.SecretRevision &&
		revisionHash == record.SecretRevisionHash, nil
}

func loadWebhookAdmissionTx(ctx context.Context, q rowQueryer, environmentID, deliveryIDHash string, forUpdate bool) (api.WebhookAdmission, error) {
	statement := `SELECT delivery_id_hash, body_sha256, actor_kind, actor_subject, kind, target, state,
        coalesce(operation_id,''), coalesce(job_id,0), admission_attempts, error_code, first_seen_at,
        environment_id, revision_id, secret_revision, secret_revision_hash,
        repository, branch, commit_sha
        FROM eregion_webhook_deliveries WHERE environment_id=$1 AND delivery_id_hash=$2`
	if forUpdate {
		statement += " FOR UPDATE"
	}
	var record api.WebhookAdmission
	var state string
	var operationID string
	var secretRevision int64
	err := q.QueryRow(ctx, statement, environmentID, deliveryIDHash).Scan(
		&record.DeliveryIDHash, &record.BodySHA256, &record.Actor.Kind, &record.Actor.Subject,
		&record.Kind, &record.Target, &state, &operationID, &record.JobID, &record.AdmissionAttempts, &record.ErrorCode,
		&record.FirstSeenAt, &record.EnvironmentID, &record.RevisionID, &secretRevision,
		&record.SecretRevisionHash, &record.Repository, &record.Branch, &record.Commit)
	if err != nil {
		return api.WebhookAdmission{}, err
	}
	if secretRevision < 0 {
		return api.WebhookAdmission{}, contract.NewFault(contract.PreconditionFailed, "webhook delivery has a negative secret revision", "Inspect the delivery row before retrying.")
	}
	record.SecretRevision = uint64(secretRevision)
	record.OperationID = contract.OperationID(operationID)
	record.State = api.WebhookDeliveryState(state)
	record.FirstSeenAt = record.FirstSeenAt.UTC()
	return record, nil
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
