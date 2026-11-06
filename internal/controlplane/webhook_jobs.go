package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

const webhookQueue = "webhook"

// webhookArgs deliberately carries only the delivery hash. The body, signature,
// secret, sender and raw delivery ID never enter River arguments or logs.
type webhookArgs struct {
	DeliveryIDHash string `json:"delivery_id_hash"`
}

func (webhookArgs) Kind() string { return "webhook_admission" }

type webhookWorker struct {
	river.WorkerDefaults[webhookArgs]
	service *Service
}

func (*webhookWorker) NextRetry(job *river.Job[webhookArgs]) time.Time {
	delay := time.Duration(job.Attempt) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay)
}

func (w *webhookWorker) Work(ctx context.Context, job *river.Job[webhookArgs]) error {
	return w.service.submitWebhookAdmission(ctx, job.Args.DeliveryIDHash)
}

// submitWebhookAdmission submits the ordinary platform.deploy operation for one
// admitted push. The receipt row is locked, the operation/domain rows/River
// platform job are recorded by the same transaction-owned helper as an HTTP
// submission, and the delivery link is committed with them. A crash before
// commit rolls everything back; a retry after commit sees the submitted link
// and does nothing.
func (s *Service) submitWebhookAdmission(ctx context.Context, deliveryIDHash string) error {
	if !validHash(deliveryIDHash) {
		return contract.NewFault(contract.ValidationFailed, "webhook admission job has a malformed delivery hash", "Inspect the River job; do not retry it with a different identity.")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	record, err := loadWebhookAdmissionTx(ctx, tx, deliveryIDHash, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable(err)
	}
	switch record.State {
	case api.WebhookDeliverySubmitted, api.WebhookDeliveryFailed:
		return nil
	case api.WebhookDeliveryAdmitted:
	default:
		// Legacy pending rows have no recorded commit and are reconciled only
		// by an authenticated redelivery, never by this worker.
		return nil
	}
	target, err := platform.ParseTarget(record.Target)
	if err != nil || target.Kind != platform.TargetEnvironment {
		return s.failWebhookAdmission(ctx, tx, record, contract.ValidationFailed, "the admitted target is not an environment deploy")
	}
	env, err := environmentByIDTx(ctx, tx, platform.EnvironmentID(record.EnvironmentID))
	if err != nil {
		if contract.Classify(err).Code == contract.NotFound {
			return s.failWebhookAdmission(ctx, tx, record, contract.PreconditionFailed, "the environment was deleted after the push was admitted")
		}
		return err
	}
	if env.App != target.App || env.Name != target.Environment {
		return s.failWebhookAdmission(ctx, tx, record, contract.PreconditionFailed, "the environment was recreated after the push was admitted")
	}
	if err := webhookSourceBindingCurrent(env, record); err != nil {
		fault := contract.Classify(err)
		return s.failWebhookAdmission(ctx, tx, record, fault.Code, fault.Message)
	}
	raw, err := json.Marshal(platform.DeployInput{App: target.App, Environment: target.Environment, Ref: record.Commit})
	if err != nil {
		return s.failWebhookAdmission(ctx, tx, record, contract.Internal, "the admitted deploy input could not be encoded")
	}
	normalized, err := platform.Default().Normalize(platform.KindDeploy, record.Target, raw)
	if err != nil {
		code, detail, retry := webhookAdmissionFailure(err)
		if retry {
			return err
		}
		return s.failWebhookAdmission(ctx, tx, record, code, detail)
	}
	// The webhook carries an exact commit, so no remote read is needed. The
	// transaction helper still checks this URL and ref against the revision it
	// reads, exactly as an ordinary deploy does.
	resolved := &resolvedGitRef{
		Kind: "git", URL: env.Revision.Source.URL,
		Ref: record.Commit, SHA: record.Commit,
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return err
	}
	op, _, err := s.recordPlatformSubmissionTx(ctx, tx, id, normalized, record.Actor, "ghdelivery-"+record.DeliveryIDHash, resolved, "")
	if err != nil {
		code, detail, retry := webhookAdmissionFailure(err)
		if retry {
			return err
		}
		// The helper may have inserted the operation before rejecting the
		// submission. Roll the whole attempt back before recording a terminal
		// receipt, so a failed admission can never leave a partial operation
		// or platform job behind.
		rollback(tx)
		return s.markWebhookAdmissionFailed(ctx, record.DeliveryIDHash, code, detail)
	}
	tag, err := tx.Exec(ctx, `UPDATE eregion_webhook_deliveries
        SET state='submitted', operation_id=$2, submitted_at=clock_timestamp()
        WHERE delivery_id_hash=$1 AND state='admitted'`, record.DeliveryIDHash, op.ID)
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() != 1 {
		return contract.NewFault(contract.Unavailable, "webhook admission changed while its operation was submitted", "Retry the delivery; do not submit a second operation.")
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

// webhookSourceBindingCurrent accepts the current revision when it still binds
// the repository and branch the signed push matched. The revision may have
// advanced to a newer runtime, process or config state; only deletion or a
// repository/branch rebind makes the accepted intent terminal.
func webhookSourceBindingCurrent(env platform.Environment, record api.WebhookAdmission) error {
	if env.Revision == nil || env.Revision.Source.Kind != "git" {
		return contract.NewFault(contract.PreconditionFailed, "the environment source is no longer a git repository", "The push was accepted against a git source; create a new push after changing the source.")
	}
	repository, ok := platform.GitHubRepository(env.Revision.Source.URL)
	if !ok {
		return contract.NewFault(contract.PreconditionFailed, "the environment source is no longer a GitHub repository", "The push was accepted against a GitHub repository; create a new push after changing the source.")
	}
	branch := strings.TrimPrefix(strings.TrimSpace(env.Revision.Source.Ref), "refs/heads/")
	if repository != record.Repository || branch != record.Branch {
		return contract.NewFault(contract.PreconditionFailed, "the repository or branch was rebound after the push was admitted", "Create a new push after rebinding the environment source.")
	}
	return nil
}

// markWebhookAdmissionFailed records a terminal failure in its own transaction.
// The worker uses it after rolling back a failed submission attempt so no
// partial operation can commit with the receipt.
func (s *Service) markWebhookAdmissionFailed(ctx context.Context, deliveryIDHash string, code contract.Code, detail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	record, err := loadWebhookAdmissionTx(ctx, tx, deliveryIDHash, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable(err)
	}
	if record.State != api.WebhookDeliveryAdmitted {
		return nil
	}
	return s.failWebhookAdmission(ctx, tx, record, code, detail)
}

// failWebhookAdmission commits one sanitized terminal admission failure. The
// operation and platform job were never committed, so the receipt becomes
// terminal instead of staying pending forever.
func (s *Service) failWebhookAdmission(ctx context.Context, tx pgx.Tx, record api.WebhookAdmission, code contract.Code, detail string) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_webhook_deliveries
        SET state='failed', failed_at=clock_timestamp(), error_code=$2, error_detail=$3
        WHERE delivery_id_hash=$1 AND state='admitted'`,
		record.DeliveryIDHash, string(code), sanitizeWebhookFailure(detail)); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

// webhookAdmissionFailure decides whether a worker error is transient database
// trouble (River should retry) or a terminal sanitized refusal to persist. Only
// the safe fault message is stored; the cause may contain connection details.
func webhookAdmissionFailure(err error) (contract.Code, string, bool) {
	fault := contract.Classify(err)
	switch fault.Code {
	case contract.Unavailable, contract.Cancelled:
		return fault.Code, "", true
	default:
		return fault.Code, sanitizeWebhookFailure(fault.Message), false
	}
}

// sanitizeWebhookFailure keeps one bounded, printable line. It never receives a
// payload, signature, secret or raw delivery ID; this is defence in depth for
// a validation message that might echo an input.
func sanitizeWebhookFailure(message string) string {
	message = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

type webhookAdmissionDisposition int

const (
	webhookAdmissionLeave webhookAdmissionDisposition = iota
	webhookAdmissionRequeue
	webhookAdmissionFail
)

// webhookAdmissionDispositionFor classifies the River state of an admitted
// receipt's job. Missing or completed jobs are re-enqueued because the worker's
// operation/link transaction is idempotent; discarded or cancelled jobs have
// exhausted their attempts and become a terminal receipt.
func webhookAdmissionDispositionFor(jobState string) webhookAdmissionDisposition {
	switch jobState {
	case "", "completed":
		return webhookAdmissionRequeue
	case "discarded", "cancelled":
		return webhookAdmissionFail
	default:
		return webhookAdmissionLeave
	}
}

// reconcileWebhookAdmissions reuses the existing operation maintenance pass.
// It never starts a second polling queue: the periodic reconcile job already
// runs on every worker and now also repairs admitted receipts whose River job
// vanished or reached a terminal state.
//
// The SQL filters to actionable River states before LIMIT, so a large healthy
// backlog cannot hide later broken receipts. Each candidate is re-read under
// its receipt row lock; a concurrent maintenance pass may already have replaced
// the job, so the current job identity and state decide the action.
func (s *Service) reconcileWebhookAdmissions(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT d.delivery_id_hash
        FROM eregion_webhook_deliveries d
        LEFT JOIN river_job j ON j.id=d.job_id
        WHERE d.state='admitted'
          AND (d.job_id IS NULL OR j.id IS NULL OR j.state IN ('completed','discarded','cancelled'))
        ORDER BY d.first_seen_at
        LIMIT 100`)
	if err != nil {
		return unavailable(err)
	}
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, hash := range hashes {
		if err := s.reconcileWebhookAdmission(ctx, hash); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// reconcileWebhookAdmission revalidates one receipt under its row lock. The
// current job_id and River state are read inside that lock, so a concurrent
// maintenance requeue cannot make this pass fail a valid replacement job or
// enqueue the same receipt twice.
func (s *Service) reconcileWebhookAdmission(ctx context.Context, deliveryIDHash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	record, err := loadWebhookAdmissionTx(ctx, tx, deliveryIDHash, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable(err)
	}
	if record.State != api.WebhookDeliveryAdmitted {
		return nil
	}
	jobState, err := webhookJobStateTx(ctx, tx, record.JobID)
	if err != nil {
		return unavailable(err)
	}
	switch webhookAdmissionDispositionFor(jobState) {
	case webhookAdmissionFail:
		return s.failWebhookAdmission(ctx, tx, record, contract.Unavailable, "the durable admission job was "+jobState+" before submitting the deploy")
	case webhookAdmissionRequeue:
		if record.AdmissionAttempts >= platformMaxAttempts {
			return s.failWebhookAdmission(ctx, tx, record, contract.Unavailable, "the admission job disappeared after its final attempt")
		}
		if s.enqueueWebhook == nil {
			return unavailable()
		}
		jobID, err := s.enqueueWebhook(ctx, tx, deliveryIDHash)
		if err != nil {
			return unavailable(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_webhook_deliveries
            SET job_id=$2, admission_attempts=admission_attempts+1
            WHERE delivery_id_hash=$1 AND state='admitted'`, deliveryIDHash, jobID); err != nil {
			return unavailable(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
	}
	return nil
}

func webhookJobStateTx(ctx context.Context, q rowQueryer, jobID int64) (string, error) {
	if jobID == 0 {
		return "", nil
	}
	var state string
	err := q.QueryRow(ctx, `SELECT state FROM river_job WHERE id=$1`, jobID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return state, nil
}
