package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

// webhookWorker drains one admitted delivery into a submitted platform deploy.
type webhookWorker struct {
	river.WorkerDefaults[jobargs.WebhookAdmission]
	service *Service
}

func (*webhookWorker) NextRetry(job *river.Job[jobargs.WebhookAdmission]) time.Time {
	delay := time.Duration(job.Attempt) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay)
}

func (w *webhookWorker) Work(ctx context.Context, job *river.Job[jobargs.WebhookAdmission]) error {
	return w.service.submitWebhookAdmission(ctx, job.Args.EnvironmentID, job.Args.DeliveryIDHash)
}

var errWebhookTerminalFailure = errors.New("webhook admission failed terminally")

// submitWebhookAdmission submits the ordinary platform.deploy operation for one
// admitted push. The receipt row is locked, the operation/domain rows/River
// platform job are recorded by the same transaction-owned helper as an HTTP
// submission, and the delivery link is committed with them. A crash before
// commit rolls everything back; a retry after commit sees the submitted link
// and does nothing.
func (s *Service) submitWebhookAdmission(ctx context.Context, environmentID, deliveryIDHash string) error {
	if environmentID == "" || !validHash(deliveryIDHash) {
		return contract.NewFault(contract.ValidationFailed, "webhook admission job has a malformed delivery hash", "Inspect the River job; do not retry it with a different identity.")
	}
	var terminalCode contract.Code
	var terminalDetail string
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		terminalCode, terminalDetail, err = s.submitWebhookAdmissionTx(ctx, tx, environmentID, deliveryIDHash)
		return err
	})
	if err != nil {
		if errors.Is(err, errWebhookTerminalFailure) {
			return s.markWebhookAdmissionFailed(ctx, environmentID, deliveryIDHash, terminalCode, terminalDetail)
		}
		var fault *contract.FaultError
		if errors.As(err, &fault) {
			return err
		}
		return unavailable(err)
	}
	return nil
}

func (s *Service) submitWebhookAdmissionTx(ctx context.Context, tx *entities.StoreTx, environmentID, deliveryIDHash string) (contract.Code, string, error) {
	record, found, err := tx.Webhooks().Lock(ctx, environmentID, deliveryIDHash)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", nil
	}
	admission := webhookAdmissionFromReceipt(record)
	switch admission.State {
	case api.WebhookDeliverySubmitted, api.WebhookDeliveryFailed, api.WebhookDeliveryPending:
		return "", "", nil
	case api.WebhookDeliveryAdmitted:
	default:
		// Legacy pending rows have no recorded commit and are reconciled only
		// by an authenticated redelivery, never by this worker.
		return "", "", nil
	}
	return s.admitWebhookDeploymentTx(ctx, tx, admission)
}

func (s *Service) admitWebhookDeploymentTx(ctx context.Context, tx *entities.StoreTx, admission api.WebhookAdmission) (contract.Code, string, error) {
	target, err := platform.ParseTarget(admission.Target)
	if err != nil || target.Kind != platform.TargetEnvironment {
		code, detail, retry := webhookAdmissionFailure(contract.NewFault(contract.ValidationFailed, "the admitted target is not an environment deploy", "Admit only an environment deploy target."))
		if retry {
			return "", "", contract.NewFault(contract.Unavailable, "webhook admission is not retryable", "Inspect the delivery record.")
		}
		return "", "", s.failWebhookAdmissionInTx(ctx, tx, admission, code, detail)
	}
	env, handled, err := s.webhookAdmissionEnvironmentFacts(ctx, tx, admission, target)
	if err != nil || handled {
		return "", "", err
	}
	raw, err := json.Marshal(platform.DeployInput{App: target.App, Environment: target.Environment, Ref: admission.Commit})
	if err != nil {
		return "", "", s.failWebhookAdmissionInTx(ctx, tx, admission, contract.Internal, "the admitted deploy input could not be encoded")
	}
	normalized, err := platform.Default().Normalize(platform.KindDeploy, admission.Target, raw)
	if err != nil {
		code, detail, retry := webhookAdmissionFailure(err)
		if retry {
			return "", "", err
		}
		return "", "", s.failWebhookAdmissionInTx(ctx, tx, admission, code, detail)
	}
	// The webhook carries an exact commit, so no remote read is needed. The
	// transaction helper still checks this URL and ref against the revision it
	// reads, exactly as an ordinary deploy does.
	resolved := &resolvedGitRef{
		URL: env.Revision.Source.URL,
		Ref: admission.Commit, SHA: admission.Commit,
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return "", "", err
	}
	result, err := s.recordPlatformSubmissionTx(ctx, tx, id, normalized, admission.Actor, "ghdelivery-"+admission.EnvironmentID+"-"+admission.DeliveryIDHash, resolved, nil, "", nil)
	if err != nil {
		code, detail, retry := webhookAdmissionFailure(err)
		if retry {
			return "", "", err
		}
		return code, detail, errWebhookTerminalFailure
	}
	submitted, err := tx.Webhooks().Submit(ctx, admission.EnvironmentID, admission.DeliveryIDHash, result.Operation.ID, string(api.WebhookDeliveryAdmitted))
	if err != nil {
		return "", "", err
	}
	if !submitted {
		return "", "", contract.NewFault(contract.Unavailable, "webhook admission changed while its operation was submitted", "Retry the delivery; do not submit a second operation.")
	}
	return "", "", nil
}

func (s *Service) webhookAdmissionEnvironmentFacts(ctx context.Context, tx *entities.StoreTx, admission api.WebhookAdmission, target platform.Target) (platform.Environment, bool, error) {
	// Shared platform admission takes application before environment locks.
	// The delivery row remains locked so receipt/idempotency behavior is
	// unchanged.
	if _, found, err := tx.Platform().Apps().GetByNameForUpdate(ctx, target.App); err != nil {
		return platform.Environment{}, false, err
	} else if !found {
		code, detail := contract.PreconditionFailed, "the application was deleted after the push was admitted"
		return platform.Environment{}, true, s.failWebhookAdmissionInTx(ctx, tx, admission, code, detail)
	}
	env, found, err := tx.Platform().Environments().GetByID(ctx, platform.EnvironmentID(admission.EnvironmentID))
	if err != nil {
		return platform.Environment{}, false, err
	}
	if !found {
		code, detail := contract.PreconditionFailed, "the environment was deleted after the push was admitted"
		return platform.Environment{}, true, s.failWebhookAdmissionInTx(ctx, tx, admission, code, detail)
	}
	if env.Environment.App != target.App || env.Environment.Name != target.Environment {
		code, detail := contract.PreconditionFailed, "the environment was recreated after the push was admitted"
		return platform.Environment{}, true, s.failWebhookAdmissionInTx(ctx, tx, admission, code, detail)
	}
	if env.Environment.Revision == nil {
		return platform.Environment{}, true, s.failWebhookAdmissionInTx(ctx, tx, admission, contract.PreconditionFailed, "the environment has no current revision")
	}
	if err := webhookSourceBindingCurrent(env.Environment, admission); err != nil {
		fault := contract.Classify(err)
		return platform.Environment{}, true, s.failWebhookAdmissionInTx(ctx, tx, admission, fault.Code, fault.Message)
	}
	return env.Environment, false, nil
}

// webhookSourceBindingCurrent accepts the current revision when it still binds
// the repository and branch the signed push matched. The revision may have
// advanced to a newer runtime, process or config state; only deletion or a
// repository/branch rebind makes the accepted intent terminal.
func webhookSourceBindingCurrent(env platform.Environment, record api.WebhookAdmission) error {
	if env.Revision == nil || env.Revision.Source.URL == "" {
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
func (s *Service) markWebhookAdmissionFailed(ctx context.Context, environmentID, deliveryIDHash string, code contract.Code, detail string) error {
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		record, found, err := tx.Webhooks().Lock(ctx, environmentID, deliveryIDHash)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		admission := webhookAdmissionFromReceipt(record)
		if admission.State != api.WebhookDeliveryAdmitted {
			return nil
		}
		return s.failWebhookAdmissionInTx(ctx, tx, admission, code, detail)
	})
	if err != nil {
		var fault *contract.FaultError
		if errors.As(err, &fault) {
			return err
		}
		return unavailable(err)
	}
	return nil
}

// failWebhookAdmissionInTx records one sanitized terminal admission failure in
// the caller's transaction. The operation and platform job were never
// committed, so the receipt becomes terminal instead of staying pending.
func (s *Service) failWebhookAdmissionInTx(ctx context.Context, tx *entities.StoreTx, record api.WebhookAdmission, code contract.Code, detail string) error {
	failed, err := tx.Webhooks().Fail(ctx, record.EnvironmentID, record.DeliveryIDHash, code, sanitizeWebhookFailure(detail))
	if err != nil {
		return err
	}
	if !failed {
		return contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation changed", "Retry the delivery; do not submit a second operation.")
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
	case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.MissingTool, contract.NotFound, contract.Unauthenticated, contract.Forbidden,
		contract.PreconditionFailed, contract.DoctorFailed, contract.ConfirmationRequired,
		contract.Conflict, contract.NodeVersionMismatch, contract.RemoteUnreachable, contract.Internal:
		fallthrough
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
// The typed query filters to actionable River states before the limit, so a
// large healthy backlog cannot hide later broken receipts. Each candidate is
// re-read under its receipt row lock; a concurrent maintenance pass may already
// have replaced the job, so the current job identity and state decide the action.
func (s *Service) reconcileWebhookAdmissions(ctx context.Context) error {
	identities, err := s.entities.Webhooks().ListNeedingRiver(ctx, 100)
	if err != nil {
		return unavailable(err)
	}
	var failures []error
	for _, identity := range identities {
		if err := s.reconcileWebhookAdmission(ctx, identity.EnvironmentID, identity.DeliveryIDHash); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// reconcileWebhookAdmission revalidates one receipt under its row lock. The
// current job_id and River state are read inside that lock, so a concurrent
// maintenance requeue cannot make this pass fail a valid replacement job or
// enqueue the same receipt twice.
func (s *Service) reconcileWebhookAdmission(ctx context.Context, environmentID, deliveryIDHash string) error {
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		return s.reconcileWebhookAdmissionTx(ctx, tx, environmentID, deliveryIDHash)
	})
	if err != nil {
		var fault *contract.FaultError
		if errors.As(err, &fault) {
			return err
		}
		return unavailable(err)
	}
	return nil
}

func (s *Service) reconcileWebhookAdmissionTx(ctx context.Context, tx *entities.StoreTx, environmentID, deliveryIDHash string) error {
	record, found, err := tx.Webhooks().Lock(ctx, environmentID, deliveryIDHash)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	admission := webhookAdmissionFromReceipt(record)
	if admission.State != api.WebhookDeliveryAdmitted {
		return nil
	}
	jobState, found, err := tx.RiverJobs().GetState(ctx, admission.JobID)
	if err != nil {
		return err
	}
	if !found {
		jobState = ""
	}
	switch webhookAdmissionDispositionFor(jobState) {
	case webhookAdmissionFail:
		return s.failWebhookAdmissionInTx(ctx, tx, admission, contract.Unavailable, "the durable admission job was "+jobState+" before submitting the deploy")
	case webhookAdmissionRequeue:
		if admission.AdmissionAttempts >= jobargs.PlatformMaxAttempts {
			return s.failWebhookAdmissionInTx(ctx, tx, admission, contract.Unavailable, "the admission job disappeared after its final attempt")
		}
		jobID, err := tx.Jobs().EnqueueWebhook(ctx, environmentID, deliveryIDHash)
		if err != nil {
			return err
		}
		reassigned, err := tx.Webhooks().ReassignJob(ctx, environmentID, deliveryIDHash, jobID)
		if err != nil {
			return err
		}
		if !reassigned {
			return contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation changed", "Retry the delivery; do not submit a second operation.")
		}
	case webhookAdmissionLeave:
		// A live admission job keeps ownership; the worker will finish it.
	}
	return nil
}
