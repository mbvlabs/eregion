package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

// platformStepTotal is the progress denominator. It is deliberately larger than
// any current pipeline and is informational only; completion depends on the
// actual step records, never this number.
const platformStepTotal = 32

// Deployment step identities are shared by the worker, reconciliation, and the
// viewer. A restored switch is proven by the completed step record, never by a
// message.
const (
	deploymentPreflightStep    = "platform-preflight-02-v1"
	deploymentCommandStep      = "platform-command-01-v1"
	deploymentPackStep         = "platform-pack-03-v1"
	deploymentImageStep        = "platform-image-02-v1"
	deploymentReadyImageStep   = "platform-ready-image-02-v1"
	deploymentPullStep         = "platform-pull-01-v1"
	deploymentNetworkStep      = "platform-network-01-v1"
	deploymentRouteStep        = "platform-routes-01-v1"
	deploymentPublicStep       = "platform-public-01-v1"
	deploymentRestoreRouteStep = "platform-route-restore-01-v1"
	deploymentBuildStopStep    = "platform-build-stop-01-v1"
	deploymentBuildStageLabel  = "build"
)

// Dynamic step prefixes: one container/health/cleanup effect per process
// replica, so the exact names are derived from the deployment plan.
const (
	deploymentContainerStepPrefix = "platform-container-"
	deploymentHealthStepPrefix    = "platform-health-"
	deploymentCleanupStepPrefix   = "platform-cleanup-"
)

type platformWorker struct {
	river.WorkerDefaults[jobargs.PlatformAction]
	service *Service
}

// typedOperationInput narrows a normalized operation input. A mismatch is a
// binary/registration defect, never retryable operator input.
func typedOperationInput[T any](input any) (T, error) {
	value, ok := input.(T)
	if !ok {
		var zero T
		return zero, contract.NewFault(contract.Internal, "operation input has an unexpected type", "This is a binary/registration mismatch; do not resubmit accepted work.")
	}
	return value, nil
}

// Timeout covers a deployment waiting on detached build and health effects. It
// is deliberately longer than River's rescue threshold, so a worker started on
// this host first recovers its predecessor's running platform jobs instead of
// waiting for this deadline (see Service.rescueHostPlatformJobs).
func (*platformWorker) Timeout(*river.Job[jobargs.PlatformAction]) time.Duration {
	return 30 * time.Minute
}

func (*platformWorker) NextRetry(job *river.Job[jobargs.PlatformAction]) time.Time {
	delay := time.Duration(job.Attempt) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay)
}

func (w *platformWorker) Work(ctx context.Context, job *river.Job[jobargs.PlatformAction]) error {
	attempt := uint32(job.Attempt) //nolint:gosec // G115: River attempts are non-negative.
	op, raw, claimed, err := w.service.claimPlatform(ctx, job.Args.OperationID, attempt)
	if err != nil {
		return err
	}
	if !claimed {
		return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
	}
	if op.Kind == platform.KindDefinitionApply {
		return w.runDefinitionApply(ctx, op, attempt)
	}
	if handled, err := w.handleCancellingPlatformWork(ctx, op, raw, attempt); handled {
		return err
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return w.settle(ctx, op.ID, attempt, err)
	}
	releaseLock, err := w.acquirePlatformRunLock(ctx, op, normalized)
	if err != nil {
		return err
	}
	defer releaseLock()
	if err := w.guardDomainRemoveRecovery(ctx, op, attempt); err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, op.ID); err != nil {
		return w.settlePlatformCancellation(ctx, op, attempt, err)
	}
	if executor := normalized.Executor(); executor != nil {
		return w.runExtension(ctx, op.ID, attempt, normalized, executor)
	}
	return w.dispatchPlatformAction(ctx, op, attempt, normalized)
}

func (w *platformWorker) handleCancellingPlatformWork(ctx context.Context, op contract.Operation, raw json.RawMessage, attempt uint32) (bool, error) {
	if op.State != contract.Cancelling {
		return false, nil
	}
	if op.Kind == platform.KindBuildStop {
		return true, w.service.cancelRecoveredBuildStop(ctx, op.ID, attempt)
	}
	if op.Kind == platform.KindBuildRetry {
		return true, w.service.reconcileBuildRetry(ctx, op)
	}
	if op.Kind == platform.KindDomainAdd || op.Kind == platform.KindDomainRemove {
		normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
		if err != nil {
			return true, err
		}
		release, err := w.service.lockPlatformTargets(ctx, normalized)
		if err != nil {
			return true, err
		}
		defer release()
		if handled, err := w.stopDefinitionDNSChild(ctx, op, attempt); handled {
			return true, err
		}
	}
	if deploymentLifecycleKind(op.Kind) {
		release, acquired, lockErr := w.service.tryLockTargets(ctx, []string{op.Target})
		if lockErr != nil {
			return true, lockErr
		}
		if !acquired {
			return true, river.JobSnooze(deploymentOwnerDefer)
		}
		defer release()
		return true, w.cancelOwnedDeployment(ctx, op.ID, attempt)
	}
	return true, w.service.cancelPlatform(
		ctx,
		op.ID,
		attempt,
		"Cancellation requested before execution started.",
	)
}

func (w *platformWorker) acquirePlatformRunLock(ctx context.Context, op contract.Operation, normalized platform.Normalized) (func(), error) {
	if deploymentLifecycleKind(op.Kind) {
		release, acquired, err := w.service.tryLockTargets(ctx, []string{normalized.Target.String()})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if !acquired {
			return nil, river.JobSnooze(deploymentOwnerDefer)
		}
		return release, nil
	}
	release, err := w.service.lockPlatformTargets(ctx, normalized)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return release, nil
}

// guardDomainRemoveRecovery latches read-only definition DNS recovery once any
// cleanup attempt failed; a DELETE is never redispatched.
func (w *platformWorker) guardDomainRemoveRecovery(ctx context.Context, op contract.Operation, attempt uint32) error {
	if op.Kind != platform.KindDomainRemove {
		return nil
	}
	step, err := w.service.definitionDNSChild(ctx, op.ID)
	if err != nil {
		return err
	}
	if step == nil || step.Cleanup == nil {
		return nil
	}
	ledger, err := w.service.loadDomainDNSLedger(ctx, step.Input.App, step.Input.Environment, step.Input.Domain)
	if err != nil {
		return err
	}
	if ledger == nil {
		return contract.NewFault(
			contract.Unavailable,
			"definition DNS cleanup ledger is unavailable",
			"Retain the operation and reservation.",
		)
	}
	if ledger.State != domainDNSStateCleanupFailed && ledger.CleanupError != definitionCleanupUnknown {
		return nil
	}
	if err := w.service.latchDefinitionDNSRecovery(ctx, op.ID); err != nil {
		return err
	}
	if handled, err := w.stopDefinitionDNSChild(ctx, op, attempt); handled {
		return err
	}
	return contract.NewFault(
		contract.Unavailable,
		"definition DNS recovery is unresolved",
		"Retain the operation and reservation.",
	)
}

func (w *platformWorker) settlePlatformCancellation(ctx context.Context, op contract.Operation, attempt uint32, err error) error {
	if errors.Is(err, errPlatformCancelled) && op.Kind == platform.KindBuildStop {
		return w.service.cancelRecoveredBuildStop(ctx, op.ID, attempt)
	}
	if errors.Is(err, errPlatformCancelled) &&
		(op.Kind == platform.KindDomainAdd || op.Kind == platform.KindDomainRemove) {
		if handled, recoveryErr := w.stopDefinitionDNSChild(ctx, op, attempt); handled {
			return recoveryErr
		}
	}
	if errors.Is(err, errPlatformCancelled) && deploymentLifecycleKind(op.Kind) {
		return w.cancelOwnedDeployment(ctx, op.ID, attempt)
	}
	return w.settle(ctx, op.ID, attempt, err)
}

func (w *platformWorker) dispatchPlatformAction(ctx context.Context, op contract.Operation, attempt uint32, normalized platform.Normalized) error {
	switch normalized.Kind {
	case platform.KindAppCreate, platform.KindNodeRegister, platform.KindEnvironmentCreate,
		platform.KindEnvironmentUpdate, platform.KindEnvironmentProtect, platform.KindProcessScale, platform.KindConfigSet,
		platform.KindConfigUnset, platform.KindEnvironmentSecretAttach,
		platform.KindEnvironmentResourceAttach, platform.KindEnvironmentResourceDetach:
		return w.runRecordAction(ctx, op.ID, attempt, normalized)
	case platform.KindNodeCheck:
		return w.runNodeCheck(ctx, op.ID, attempt, normalized)
	case platform.KindNodeEnroll:
		return w.runNodeEnroll(ctx, op.ID, attempt, normalized)
	case platform.KindNodeReconcile:
		return w.runNodeReconcile(ctx, op.ID, attempt, normalized)
	case platform.KindReleaseCreate:
		return contract.NewFault(
			contract.ValidationFailed,
			"the platform.release.create action is removed",
			"Use an explicit deploy selector (--image-tag, --image-digest, or --image) or a Git deploy; there is no arbitrary release destination or pull toggle.",
		)
	case platform.KindBuildStop:
		return w.runBuildStop(ctx, op.ID, attempt, normalized)
	case platform.KindBuildRetry:
		return w.runBuildRetry(ctx, op.ID, attempt, normalized)
	case platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote:
		return w.runDeployment(ctx, op.ID, attempt, normalized)
	case platform.KindDomainAdd, platform.KindDomainRemove:
		return w.runDomain(ctx, op.ID, attempt, normalized)
	case platform.KindResourceApply:
		input, err := typedOperationInput[*platform.ResourceApplyInput](normalized.Input)
		if err != nil {
			return w.settle(ctx, op.ID, attempt, err)
		}
		return w.runResourceApply(ctx, op.ID, attempt, input)
	case platform.KindResourceDestroy:
		input, err := typedOperationInput[*platform.ResourceDestroyInput](normalized.Input)
		if err != nil {
			return w.settle(ctx, op.ID, attempt, err)
		}
		return w.runResourceDestroy(ctx, op.ID, attempt, input)
	case platform.KindAppDelete, platform.KindEnvironmentDelete:
		return w.runDelete(ctx, op.ID, attempt, normalized)
	default:
		return w.settle(
			ctx,
			op.ID,
			attempt,
			contract.NewFault(
				contract.Internal,
				"platform action has no worker path",
				"This is a binary/registration mismatch; do not resubmit accepted work.",
			),
		)
	}
}

func (s *Service) claimPlatform(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
) (contract.Operation, json.RawMessage, bool, error) {
	var raw json.RawMessage
	op, claimed, err := s.mutateOwnedOrClaim(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			routine := (op.Kind == platform.KindDefinitionApply && op.State != contract.Queued) ||
				((op.Kind == platform.KindDomainAdd || op.Kind == platform.KindDomainRemove) && op.State == contract.Cancelling)
			if op.State == contract.Queued {
				if err := transition(
					ctx,
					tx,
					op,
					contract.Running,
					"",
					"Platform worker started durable execution.",
				); err != nil {
					return err
				}
			}
			// The claim persists the attempt: every later fenced write compares
			// against it, so a claim that left it at 0 could never finish.
			claimedInput, found, err := tx.Operations().ClaimAttempt(ctx, id, op.Attempt)
			if err != nil {
				return err
			}
			if !found {
				return contract.NewFault(contract.NotFound, "operation disappeared while it was claimed", "Inspect operation history; do not resubmit accepted work.")
			}
			raw = claimedInput
			if _, err := tx.Platform().Deployments().MarkRunningByOperation(ctx, id); err != nil {
				return err
			}
			if routine {
				return nil
			}
			return appendEvent(
				ctx,
				tx,
				op,
				contract.Event{
					Kind:    "step",
					Stage:   "platform",
					Message: "Platform execution attempt started under the target lock.",
				},
			)
		},
	)
	return op, raw, claimed, err
}

var errPlatformCancelled = errors.New("platform operation cancelled at a safe boundary")

// errDeploymentCancelledAtFinalize is returned from a success callback when the
// operation row is already cancelling. The callback transaction rolls back, so
// success is never committed before cancellation restoration.
var errDeploymentCancelledAtFinalize = errors.New(
	"deployment cancellation was accepted before finalization",
)

// errNodeStepPermanentFailure marks an effect the node itself reported as
// permanently failed. Transport, status and acknowledgement errors do not carry
// it: their outcome is unknown and must be recovered through node status.
var errNodeStepPermanentFailure = errors.New("node step permanently failed")

// workerInterrupted distinguishes the worker's own canceled context from a
// durable cancellation request. A job shutdown must not remove candidates or
// terminalize the operation: River retries, and recovery resumes from the
// completed container steps. A durable cancelling state still takes the normal
// cancellation/cleanup path.
func (s *Service) workerInterrupted(
	ctx context.Context,
	id contract.OperationID,
	cause error,
) bool {
	if cause == nil || !errors.Is(cause, context.Canceled) {
		return false
	}
	state, _, err := s.operationState(context.WithoutCancel(ctx), id)
	if err != nil {
		return true
	}
	return state != contract.Cancelling
}

func (w *platformWorker) settle(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	err error,
) error {
	if err == nil {
		return nil
	}
	var snooze *river.JobSnoozeError
	if errors.As(err, &snooze) {
		return err
	}
	if errors.Is(err, errOperationTerminal) {
		return nil
	}
	if errors.Is(err, errNotOwned) {
		return w.service.waitForNewerAttempt(ctx, id, attempt)
	}
	if errors.Is(err, errPlatformCancelled) {
		return w.service.cancelPlatform(
			ctx,
			id,
			attempt,
			"Cancellation acknowledged at a step boundary; completed effects were not rolled back.",
		)
	}
	if errors.Is(err, engine.ErrPermanent) {
		// A retry cannot fix a permanent step failure: fail the operation now
		// and keep the reason, instead of letting River retry it silently.
		message := err.Error()
		if w.service.redaction != nil {
			message = w.service.redaction.Redact(message)
		}
		return w.service.failPlatformOperation(
			ctx,
			id,
			attempt,
			contract.PreconditionFailed,
			message,
		)
	}
	fault := contract.Classify(err)
	if reason, ok := IsPlacementViolation(err); ok {
		// A refusal is also an event, so an agent reading the operation sees the
		// machine-readable reason without parsing the message.
		w.service.appendWorkerEvent(
			ctx,
			id,
			attempt,
			"placement_violation",
			fault.Message+" [reason="+reason+"]",
		)
	}
	switch fault.Code {
	case contract.Unavailable:
		if recordErr := w.service.recordRetryResult(
			ctx,
			id,
			attempt,
			fault.Code,
			fault.Message,
		); recordErr != nil {
			return recordErr
		}
		return err
	case contract.Cancelled:
		return w.service.cancelPlatform(ctx, id, attempt, fault.Message)
	case contract.ValidationFailed,
		contract.PreconditionFailed,
		contract.NotFound,
		contract.Forbidden,
		contract.Conflict:
		if failErr := w.service.failPlatformOperation(
			ctx,
			id,
			attempt,
			fault.Code,
			fault.Message,
		); failErr != nil {
			return failErr
		}
		return nil
	case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.MissingTool,
		contract.Unauthenticated, contract.DoctorFailed, contract.ConfirmationRequired,
		contract.NodeVersionMismatch, contract.RemoteUnreachable, contract.Internal:
		fallthrough
	default:
		if recordErr := w.service.recordRetryResult(
			ctx,
			id,
			attempt,
			fault.Code,
			fault.Message,
		); recordErr != nil {
			return recordErr
		}
		return err
	}
}

// lockTarget serializes every operation for one canonical target across control
// plane processes. It includes extension actions and node targets, not just
// environment built-ins. A session lock is released automatically if the worker
// process dies, so it cannot wedge a target permanently.
func (s *Service) lockTarget(ctx context.Context, target string) (func(), error) {
	sum := sha256.Sum256([]byte(target))
	// Preserve the exact 64-bit key bits; advisory locks only compare identity.
	key := int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // G115: intentional two's-complement preservation of a 64-bit advisory key.
	lease, err := s.entities.Platform().Locks().AcquireTargets(ctx, []int64{key})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable(err)
	}
	return func() { _ = lease.Release() }, nil
}

func (w *platformWorker) runRecordAction(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	normalized platform.Normalized,
) error {
	return w.service.finishPlatformOperation(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			return recordPlatformActionChange(ctx, tx, op, normalized)
		},
	)
}

func recordPlatformActionChange(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, normalized platform.Normalized) error {
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		return recordAppCreateChange(ctx, tx, op, input)
	case *platform.EnvironmentProtectInput:
		return recordEnvironmentProtectChange(ctx, tx, op, input)
	case *platform.EnvironmentCreateInput:
		return recordRevisionChange(
			ctx,
			tx,
			op,
			"environment.created",
			input.App,
			input.Name,
			"Created environment "+input.Name+" on node "+input.Node+" with its first desired-state revision.",
		)
	case *platform.EnvironmentUpdateInput:
		return recordEnvironmentUpdateChange(ctx, tx, op, input)
	case *platform.ProcessScaleInput:
		return recordRevisionChange(
			ctx,
			tx,
			op,
			"process.scaled",
			input.App,
			input.Environment,
			"Scaled process "+input.Process+" in a new revision.",
		)
	case *platform.ConfigSetInput:
		return recordRevisionChange(ctx, tx, op, "config.set", input.App, input.Environment,
			"Set plain configuration in a new revision; no deploy was queued.")
	case *platform.ConfigUnsetInput:
		return recordRevisionChange(
			ctx,
			tx,
			op,
			"config.unset",
			input.App,
			input.Environment,
			"Removed plain configuration in a new revision; no deploy was queued.",
		)
	case *platform.EnvironmentSecretAttachInput:
		return recordRevisionChange(
			ctx,
			tx,
			op,
			"environment.secret.attached",
			input.App,
			input.Environment,
			"Attached vault secret name "+input.Name+" to the environment revision.",
		)
	case *platform.EnvironmentResourceInput:
		kind := "environment.resource.attached"
		if op.Kind == platform.KindEnvironmentResourceDetach {
			kind = "environment.resource.detached"
		}
		return recordRevisionChange(ctx, tx, op, kind, input.App, input.Environment,
			"Changed managed resource "+input.Resource+" in the environment revision.")
	}
	// Node registration records declared facts only. It is not enrollment or
	// verification; node.check performs an authenticated transport check.
	return nil
}

func recordAppCreateChange(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, input *platform.AppCreateInput) error {
	if err := insertPlatformChange(
		ctx,
		tx,
		op,
		"app.created",
		input.Name,
		"",
		"Created application "+input.Name,
		map[string]any{"app": input.Name},
		nil,
	); err != nil {
		return err
	}
	for index := range input.Environments {
		if err := recordRevisionChange(
			ctx,
			tx,
			op,
			"environment.created",
			input.Name,
			input.Environments[index].Name,
			"Created environment "+input.Environments[index].Name+" and its first desired-state revision.",
		); err != nil {
			return err
		}
	}
	return nil
}

func recordEnvironmentProtectChange(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, input *platform.EnvironmentProtectInput) error {
	kind := "environment.protected"
	if !input.Protected {
		kind = "environment.unprotected"
	}
	return insertPlatformChange(
		ctx,
		tx,
		op,
		kind,
		input.App,
		input.Environment,
		"Changed environment protection.",
		map[string]any{"protected": input.Protected},
		nil,
	)
}

func recordEnvironmentUpdateChange(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, input *platform.EnvironmentUpdateInput) error {
	hasRevision, err := revisionCreatedByOperation(ctx, tx, op.ID)
	if err != nil {
		return err
	}
	if !hasRevision {
		if !input.RotateWebhookSecret {
			return contract.NewFault(
				contract.Internal,
				"environment update has neither a revision nor a webhook rotation",
				"Report the command and binary version.",
			)
		}
		return insertPlatformChange(
			ctx,
			tx,
			op,
			"environment.webhook.rotated",
			input.App,
			input.Environment,
			"Rotated the push-to-deploy webhook secret; desired state is unchanged.",
			map[string]any{"app": input.App, "environment": input.Environment},
			nil,
		)
	}
	if input.RotateWebhookSecret {
		if err := insertPlatformChange(
			ctx,
			tx,
			op,
			"environment.webhook.rotated",
			input.App,
			input.Environment,
			"Rotated the push-to-deploy webhook secret with the desired-state update.",
			map[string]any{
				"app":         input.App,
				"environment": input.Environment,
			},
			nil,
		); err != nil {
			return err
		}
	}
	return recordRevisionChange(
		ctx,
		tx,
		op,
		"environment.updated",
		input.App,
		input.Environment,
		"Updated environment "+input.Environment+" desired state as a new revision.",
	)
}

func recordRevisionChange(
	ctx context.Context,
	tx *entities.StoreTx,
	op *contract.Operation,
	kind, app, environment, summary string,
) error {
	revisionID, found, err := tx.Platform().Environments().CurrentRevisionID(ctx, app, environment)
	if err != nil {
		return err
	}
	var number int
	if found && revisionID != "" {
		number, _, _ = tx.Platform().Revisions().Number(ctx, revisionID)
	}
	desired := map[string]any{"revision_id": string(revisionID)}
	if number > 0 {
		desired["revision_number"] = number
	}
	return insertPlatformChange(ctx, tx, op, kind, app, environment, summary, desired, nil)
}

// revisionCreatedByOperation distinguishes a combined desired-state update
// from an admitted rotation-only operation, whose semantics do not fabricate an
// unchanged environment revision.
func revisionCreatedByOperation(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
) (bool, error) {
	exists, err := tx.Platform().Revisions().ExistsForOperation(ctx, id)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (w *platformWorker) runNodeCheck(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	normalized platform.Normalized,
) error {
	input, err := typedOperationInput[*platform.NodeCheckInput](normalized.Input)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	rec, err := loadNodeConn(ctx, w.service.entities.Nodes(), input.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if rec.Enrollment == platform.EnrollmentEnrolled && rec.Transport == "ssh" {
		return w.runEnrolledCheck(ctx, id, attempt, rec)
	}
	// A registered node is observed through the worker's own transport, and
	// only when that transport authenticates as exactly this node.
	tr, err := w.service.resolver.TransportFor(ctx, input.Node)
	if err != nil {
		w.service.recordNodeCheck(
			ctx,
			input.Node,
			checkStateFor(err),
			contract.Classify(err).Message,
		)
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, "platform-node-01-v1", platform.ArgvPayload{
		Step: "platform-node-01-v1", Argv: []string{"docker", "version"}, TimeoutSeconds: 30,
	}, nil); err != nil {
		w.service.recordNodeCheck(
			ctx,
			input.Node,
			checkStateFor(err),
			contract.Classify(err).Message,
		)
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, "platform-node-02-v1", platform.ArgvPayload{
		Step:           "platform-node-02-v1",
		Argv:           []string{"pack", "version"},
		AllowFailure:   true,
		TimeoutSeconds: 30,
	}, nil); err != nil {
		w.service.recordNodeCheck(
			ctx,
			input.Node,
			checkStateFor(err),
			contract.Classify(err).Message,
		)
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			_, err := tx.Nodes().RecordCheckReady(ctx, input.Node)
			return err
		},
	); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func stepStdout(raw json.RawMessage) string {
	var value struct {
		Stdout string `json:"stdout"`
	}
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value.Stdout)
}

// observeHealth emits a structured health observation beside the node's log
// line, so clients never parse the message text.
func (w *platformWorker) observeHealth(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	step, target string,
	config platform.HealthConfig,
	result json.RawMessage,
	probeErr error,
) {
	var decoded struct {
		Healthy    bool `json:"healthy"`
		Checks     int  `json:"checks"`
		LastStatus int  `json:"last_status"`
	}
	_ = json.Unmarshal(result, &decoded)
	// A nil probe error is proof the step passed. The node may complete the step
	// from its read-only Check without running Apply, in which case no probe
	// count was reported (zero) and the viewer says so instead of inventing one.
	observation := &contract.HealthObservation{
		Target: target, Type: config.Type, Passed: probeErr == nil,
		Checks: decoded.Checks, TimeoutSeconds: config.TimeoutSeconds,
	}
	message := fmt.Sprintf(
		"Health observation for %s: passed=%t checks=%d deadline=%ds",
		target,
		observation.Passed,
		observation.Checks,
		config.TimeoutSeconds,
	)
	if probeErr != nil {
		observation.Detail = probeErr.Error()
		message += " detail=" + probeErr.Error()
	} else {
		observation.Detail = "probe succeeded"
		message += " detail=probe succeeded"
	}
	w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
		Kind: "log", Stage: step, Step: step, Health: observation, Message: message,
	})
}

func sortedPlatformKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// Local deterministic sort avoids importing another package's helper.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func (s *Service) releaseForOperation(
	ctx context.Context,
	operationID contract.OperationID,
) (platform.Release, error) {
	releaseID, found, err := s.entities.Platform().Releases().IDForOperation(ctx, operationID)
	if err != nil {
		return platform.Release{}, unavailable(err)
	}
	if !found {
		return platform.Release{}, contract.NewFault(
			contract.NotFound,
			"release record is missing",
			"Do not retry an operation that has no durable release row.",
		)
	}
	return s.releaseByID(ctx, releaseID)
}

func (w *platformWorker) runExtension(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	normalized platform.Normalized,
	executor platform.Executor,
) error {
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return err
	}
	result, err := executor.Execute(ctx, platform.Execution{
		OperationID: id,
		Attempt:     attempt,
		Kind:        normalized.Kind,
		Target:      normalized.Target.String(),
		Actor:       op.Actor,
		Input:       normalized.Canonical,
		Report: func(event platform.Event) {
			w.reportExtensionEvent(ctx, id, attempt, event)
		},
	})
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	message := w.service.resultText(strings.TrimSpace(result.Message))
	if message == "" {
		message = normalized.Summary + " completed."
	}
	if err := w.service.finishPlatformOperation(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			op.PendingResult = &contract.ActionResult{Message: w.service.resultText(message)}
			w.applyExtensionResultData(op, normalized, executor, result.Data)
			return nil
		},
	); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) reportExtensionEvent(ctx context.Context, id contract.OperationID, attempt uint32, event platform.Event) {
	message := w.service.resultText(strings.TrimSpace(event.Message))
	if message == "" {
		return
	}
	kind := "log"
	if event.Level == "warning" {
		kind = "warning"
	}
	_, _, _ = w.service.mutateOwned(
		context.WithoutCancel(ctx),
		id,
		attempt,
		func(tx *entities.StoreTx, current *contract.Operation) error {
			return appendEvent(
				ctx,
				tx,
				current,
				contract.Event{Kind: kind, Stage: event.Stage, Message: message},
			)
		},
	)
}

func (w *platformWorker) applyExtensionResultData(op *contract.Operation, normalized platform.Normalized, executor platform.Executor, data any) {
	if data == nil {
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		op.PendingResult.DataStatus = "omitted_unserializable"
		return
	}
	// The existing worker event text cap is 4096 bytes. Extension payloads use
	// the same bound and report omission explicitly.
	if len(raw) > 4096 {
		op.PendingResult.DataStatus = "omitted_too_large"
		return
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		op.PendingResult.DataStatus = "omitted_unserializable"
		return
	}
	preserve := trustedIntegrationFields(*op, normalized, executor, decoded)
	clean, omitted := w.service.sanitizeResultData(decoded, preserve)
	if omitted > 0 {
		if object, ok := clean.(map[string]any); ok {
			if _, exists := object["redaction_omissions"]; exists {
				// Reserve the marker rather than trusting a plugin-supplied count.
				omitted++
			}
			object["redaction_omissions"] = omitted
		}
	}
	redacted, err := json.Marshal(clean)
	if err != nil || len(redacted) > 4096 {
		op.PendingResult.DataStatus = "omitted_too_large"
		return
	}
	op.PendingResult.Data = redacted
	op.PendingResult.DataStatus = "available"
	if omitted > 0 {
		op.PendingResult.DataStatus = "partial"
	}
}

func (s *Service) operationSnapshot(
	ctx context.Context,
	id contract.OperationID,
) (contract.Operation, json.RawMessage, error) {
	op, found, err := s.entities.Operations().Get(ctx, id)
	if err != nil || !found {
		return op, nil, unavailable(err)
	}
	raw, found, err := s.entities.Operations().Input(ctx, id)
	if err != nil || !found {
		return op, nil, unavailable(err)
	}
	return op, raw, nil
}

func (s *Service) checkCancelled(ctx context.Context, id contract.OperationID) error {
	state, _, err := s.operationState(ctx, id)
	if err != nil {
		return err
	}
	if state == contract.Cancelling {
		return errPlatformCancelled
	}
	if state.Terminal() {
		return errOperationTerminal
	}
	return nil
}

func (s *Service) finishPlatformOperation(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	apply func(*entities.StoreTx, *contract.Operation) error,
) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if err := apply(tx, op); err != nil {
			return err
		}
		if err := s.appendActionResultTx(
			ctx,
			tx,
			op,
			contract.Succeeded,
			"",
			"Platform action succeeded.",
			true,
		); err != nil {
			return err
		}
		return transition(
			ctx,
			tx,
			op,
			contract.Succeeded,
			"",
			"Platform action succeeded; domain state and history are durable.",
		)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) failPlatformOperation(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	code contract.Code,
	message string,
) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		return s.failPlatformOperationTx(ctx, tx, op, code, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// failPlatformOperationTx records the domain half of a platform failure on an
// already-locked operation. Callers that must commit another change with the
// failure history, such as worker recovery, run it inside their own transaction.
func (s *Service) failPlatformOperationTx(
	ctx context.Context,
	tx *entities.StoreTx,
	op *contract.Operation,
	code contract.Code,
	message string,
) error {
	message = s.resultText(message)
	if op.State.Terminal() {
		return nil
	}
	if err := guardDefinitionDNSTerminalTx(ctx, tx, op); err != nil {
		return err
	}
	if err := lockOperationParentsTx(
		ctx,
		tx,
		op.ID,
		operationRequiresPlacement(op.Kind),
	); err != nil {
		return err
	}
	if _, err := tx.Platform().Builds().MarkFailedByOperation(ctx, op.ID, code, message); err != nil {
		return err
	}
	if _, err := tx.Platform().Releases().MarkFailedByOperation(ctx, op.ID, code, message); err != nil {
		return err
	}
	if _, err := tx.Platform().Deployments().MarkFailedByOperation(ctx, op.ID, code, message); err != nil {
		return err
	}
	facts, found, err := tx.Platform().Deployments().FailureFacts(ctx, op.ID)
	if err == nil && found {
		if err := appendEvent(ctx, tx, op, contract.Event{
			Kind:             "log",
			Stage:            "deploy",
			ReleaseID:        string(facts.ReleaseID),
			ServingReleaseID: string(facts.ServingRelease),
			Message: deploymentWord(
				op.Kind,
			) + " failed; failed release " + string(facts.ReleaseID) + " is not recorded as serving" + servingSuffix(
				string(facts.ServingRelease),
			) + ".",
		}); err != nil {
			return err
		}
		unchanged := facts.ServingRelease == "" || facts.ServingRelease == facts.PreviousRelease
		if changeErr := insertPlatformChange(
			ctx,
			tx,
			op,
			deploymentChangeKind(op.Kind, facts.Action, "failed"),
			facts.App,
			facts.Environment,
			deploymentWord(op.Kind)+" failed: "+message,
			map[string]any{"release_id": string(facts.ReleaseID)},
			map[string]any{
				"error_code":         code,
				"serving_release_id": string(facts.ServingRelease),
				"serving_unchanged":  unchanged,
			},
		); changeErr != nil {
			return changeErr
		}
	} else if err != nil {
		return unavailable(err)
	}
	if err := settleResourceForTerminalOperationTx(ctx, tx, op.ID, op.Kind, code); err != nil {
		return err
	}
	if err := markIncompleteSteps(ctx, tx, op.ID, code, message); err != nil {
		return err
	}
	if err := releaseRestoredDeploymentOwnerTx(ctx, tx, op.ID); err != nil {
		return err
	}
	if err := s.appendActionResultTx(
		ctx,
		tx,
		op,
		contract.Failed,
		code,
		message,
		true,
	); err != nil {
		return err
	}
	return transition(ctx, tx, op, contract.Failed, code, message)
}

func servingSuffix(releaseID string) string {
	if releaseID == "" {
		return ""
	}
	return "; release " + releaseID + " still serves"
}

func (s *Service) cancelPlatform(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	message string,
) error {
	message = s.resultText(message)
	var retry *contract.Operation
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if op.Kind == platform.KindBuildRetry {
			snapshot := *op
			retry = &snapshot
			return nil
		}
		return s.cancelPlatformTx(ctx, tx, op, id, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	if retry != nil {
		if retry.State != contract.Cancelling {
			return unknownRetryPack()
		}
		return s.reconcileBuildRetry(ctx, *retry)
	}
	return nil
}

func (s *Service) cancelPlatformTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, id contract.OperationID, message string) error {
	if err := guardDefinitionDNSTerminalTx(ctx, tx, op); err != nil {
		return err
	}
	if err := lockOperationParentsTx(
		ctx,
		tx,
		id,
		operationRequiresPlacement(op.Kind),
	); err != nil {
		return err
	}
	if _, err := tx.Platform().Builds().MarkCancelledByOperation(ctx, id, contract.Cancelled, message); err != nil {
		return err
	}
	if _, err := tx.Platform().Releases().MarkFailedByOperation(ctx, id, contract.Cancelled, message); err != nil {
		return err
	}
	if _, err := tx.Platform().Deployments().MarkCancelledByOperation(ctx, id, message); err != nil {
		return err
	}
	if err := settleResourceForTerminalOperationTx(
		ctx,
		tx,
		id,
		op.Kind,
		contract.Cancelled,
	); err != nil {
		return err
	}
	if err := appendDeploymentCancellationEvent(ctx, tx, op, id); err != nil {
		return err
	}
	if err := markIncompleteSteps(ctx, tx, id, contract.Cancelled, message); err != nil {
		return err
	}
	if err := releaseRestoredDeploymentOwnerTx(ctx, tx, id); err != nil {
		return err
	}
	if err := s.appendActionResultTx(
		ctx,
		tx,
		op,
		contract.Canceled,
		contract.Cancelled,
		message,
		true,
	); err != nil {
		return err
	}
	return transition(ctx, tx, op, contract.Canceled, "", message)
}

func appendDeploymentCancellationEvent(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, id contract.OperationID) error {
	facts, found, err := tx.Platform().Deployments().FailureFacts(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return nil
	}
	return appendEvent(ctx, tx, op, contract.Event{
		Kind:             "log",
		Stage:            "deploy",
		ReleaseID:        string(facts.ReleaseID),
		ServingReleaseID: string(facts.ServingRelease),
		Message: "Deployment cancelled before a verified switch" + servingSuffix(
			string(facts.ServingRelease),
		) + ".",
	})
}

// Streamed node events are persisted while the step runs, in bounded batches,
// so build logs and the deploy viewer show output live. The attempt fence in
// appendNodeEvents still decides ownership of every batch.
const (
	nodeEventBatch = 16
	nodeEventFlush = 750 * time.Millisecond
)

type nodeEventStream struct {
	mu      sync.Mutex
	flushMu sync.Mutex
	service *Service
	ctx     context.Context
	id      contract.OperationID
	attempt uint32
	pending []node.Event
	err     error
	stopped bool
	done    chan struct{}
}

func (s *Service) newNodeEventStream(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
) *nodeEventStream {
	stream := &nodeEventStream{
		service: s,
		ctx:     ctx,
		id:      id,
		attempt: attempt,
		done:    make(chan struct{}),
	}
	go func() {
		ticker := time.NewTicker(nodeEventFlush)
		defer ticker.Stop()
		for {
			select {
			case <-stream.done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				stream.flush()
			}
		}
	}()
	return stream
}

func (stream *nodeEventStream) add(event node.Event) {
	stream.mu.Lock()
	if stream.stopped {
		stream.mu.Unlock()
		return
	}
	stream.pending = append(stream.pending, event)
	full := len(stream.pending) >= nodeEventBatch
	stream.mu.Unlock()
	if full {
		stream.flush()
	}
}

func (stream *nodeEventStream) flush() {
	stream.flushMu.Lock()
	defer stream.flushMu.Unlock()
	stream.mu.Lock()
	events := stream.pending
	stream.pending = nil
	stream.mu.Unlock()
	if len(events) == 0 {
		return
	}
	if err := stream.service.appendNodeEvents(
		stream.ctx,
		stream.id,
		stream.attempt,
		events,
	); err != nil {
		stream.mu.Lock()
		if stream.err == nil {
			stream.err = err
		}
		stream.mu.Unlock()
	}
}

// close stops the flush ticker and persists the remainder. It returns the
// first persistence error so the caller keeps the old contract: a successful
// step whose events could not be recorded is not silently history-free.
func (stream *nodeEventStream) close() error {
	stream.mu.Lock()
	if !stream.stopped {
		stream.stopped = true
		close(stream.done)
	}
	stream.mu.Unlock()
	stream.flush()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.err
}

// Node effect scheduling. A node step may be waiting for the node host lock
// behind a long build. The wait is benign: the task has not run Check or Apply
// and its execution attempt is still live. The worker observes that wait in
// place instead of unwinding the deployment pipeline and consuming a River
// retry.
const (
	defaultNodeEffectStatusPoll       = time.Second
	defaultNodeEffectRetryBackoff     = time.Second
	defaultNodeEffectDeferredBackoff  = 2 * time.Second
	defaultNodeEffectObservationEvery = 15 * time.Second
)

type nodeEffectPolicy struct {
	StatusPoll       time.Duration
	RetryBackoff     time.Duration
	DeferredBackoff  time.Duration
	ObservationEvery time.Duration
}

func (p nodeEffectPolicy) withDefaults() nodeEffectPolicy {
	if p.StatusPoll <= 0 {
		p.StatusPoll = defaultNodeEffectStatusPoll
	}
	if p.RetryBackoff <= 0 {
		p.RetryBackoff = defaultNodeEffectRetryBackoff
	}
	if p.DeferredBackoff <= 0 {
		p.DeferredBackoff = defaultNodeEffectDeferredBackoff
	}
	if p.ObservationEvery <= 0 {
		p.ObservationEvery = defaultNodeEffectObservationEvery
	}
	return p
}

// observedFailureAttempt validates the observed failed execution identity. A
// retryable failure may only be redispatched when it belongs to a strictly
// older attempt. A current-attempt failure returns to River; a missing,
// mismatched, or future identity fails closed without another dispatch.
func (l nodeEffectLoop) observedFailureAttempt(response node.Response) (uint32, error) {
	if response.Attempt == 0 {
		return 0, nodeEffectIdentityFault("the failed node task has no attempt")
	}
	if response.ExecutionID == "" {
		return 0, nodeEffectIdentityFault("the failed node task has no execution identity")
	}
	if l.Current.Step == "" {
		return 0, nodeEffectIdentityFault("the current node effect has no step identity")
	}
	expected := engine.Execution{
		OperationID: l.Current.OperationID,
		Attempt:     response.Attempt,
		Step:        l.Current.Step,
	}.ExecutionID()
	if response.ExecutionID != expected {
		return 0, nodeEffectIdentityFault(
			"the failed node task identity does not match its operation, attempt and step",
		)
	}
	return response.Attempt, nil
}

func nodeEffectIdentityFault(message string) error {
	return contract.NewFault(
		contract.Unavailable,
		"node failure identity is missing or ambiguous: "+message,
		"Inspect the node journal; do not dispatch another effect under an ambiguous execution identity.",
	)
}

// currentAttemptFailure records a retryable failure owned by this attempt and
// returns it to River. It never redispatches: the next River attempt owns the
// retry budget.
func (l nodeEffectLoop) currentAttemptFailure(ctx context.Context, response node.Response) error {
	if err := l.Failed(ctx, response); err != nil {
		return err
	}
	return contract.NewFault(contract.Unavailable,
		"the current node attempt failed",
		"Retry the operation; this attempt's retry budget is consumed before another dispatch.")
}

// nodeEffectWaitInterrupted reports a worker deadline or shutdown that ended
// the wait before a safe boundary. It deliberately does not unwrap the context
// error: a shutdown is retryable and must not be mistaken for a durable
// cancellation that authorizes cleanup while a detached task may be live.
func nodeEffectWaitInterrupted() error {
	return contract.NewFault(
		contract.Unavailable,
		"the node effect wait ended before a safe boundary",
		"Retry the operation; a detached node task may still be running and status will reconcile it before dispatch.",
	)
}

// errNodeEffectOutcomeUnknown means the transport did not establish whether a
// node effect ran. Callers must not record the build or step as failed and must
// not terminalize cancellation while such an effect may still be live.
var errNodeEffectOutcomeUnknown = errors.New("node effect outcome is unknown")

// errNodeEffectAlreadyCompleted means another worker acknowledged the step row
// between this worker's status/record read and its beginStep. The durable
// result wins; the loop never dispatches after it.
var errNodeEffectAlreadyCompleted = errors.New("node effect was acknowledged by another writer")

// nodeEffectLoop drives one node effect without starting a second Run while an
// earlier task may still be live. Status is always observed before Run. A first
// absent observation dispatches immediately; later absence and staleness are
// reconciled with a backoff. A live running task is polled, a pre-Check/Apply
// deferral is backed off, and a durable cancellation stops new dispatch. The
// caller owns the database step row, so beginStep happens once outside this
// loop.
type nodeEffectLoop struct {
	Status    func(context.Context) (node.Response, error)
	Dispatch  func(context.Context) (node.Response, error)
	Begin     func(context.Context) error
	Completed func(context.Context, node.Response, bool) error
	Failed    func(context.Context, node.Response) error
	Cancelled func(context.Context) error
	Observe   func(node.Response)
	Wait      func(context.Context, time.Duration) error
	Policy    nodeEffectPolicy
	Current   engine.Execution
}

func (l nodeEffectLoop) run(ctx context.Context) (node.Response, error) {
	policy := l.Policy.withDefaults()
	retriedPriorFailure := false
	firstObservation := true
	for {
		if err := ctx.Err(); err != nil {
			return node.Response{}, nodeEffectWaitInterrupted()
		}
		cancelErr, err := l.cancellation(ctx)
		if err != nil {
			return node.Response{}, err
		}
		response, err := l.Status(ctx)
		if err != nil {
			// An unknown outcome is never reinterpreted as cancellation.
			return node.Response{}, err
		}
		firstStatus := firstObservation
		firstObservation = false
		done, out, err := l.handleStatus(ctx, response, cancelErr, firstStatus, policy, &retriedPriorFailure)
		if done {
			return out, err
		}
	}
}

func (l nodeEffectLoop) handleStatus(ctx context.Context, response node.Response, cancelErr error, firstStatus bool, policy nodeEffectPolicy, retriedPriorFailure *bool) (bool, node.Response, error) {
	switch response.Status {
	case engine.StatusCompleted:
		return true, response, l.finishCompleted(ctx, response)
	case engine.StatusFailed:
		return l.handleFailedStatus(ctx, response, cancelErr, policy, retriedPriorFailure)
	case engine.StatusRunning:
		l.observe(response)
		if err := l.wait(ctx, policy.StatusPoll); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
		return false, response, nil
	case engine.StatusDeferred:
		l.observe(response)
		if cancelErr != nil {
			// No task holds the effect lock and no Check/Apply ran.
			return true, response, cancelErr
		}
		if err := l.wait(ctx, policy.DeferredBackoff); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
		done, out, err := l.dispatch(ctx)
		return done, out, err
	case engine.StatusAbsent, engine.StatusStale:
		return l.handleAbsentStatus(ctx, response, cancelErr, firstStatus, policy)
	default:
		return true, response, fmt.Errorf("node returned unexpected status %q", response.Status)
	}
}

func (l nodeEffectLoop) finishCompleted(ctx context.Context, response node.Response) error {
	l.observe(response)
	if err := l.Completed(ctx, response, true); err != nil {
		return err
	}
	return l.postCompletionCancellation(ctx)
}

func (l nodeEffectLoop) handleFailedStatus(ctx context.Context, response node.Response, cancelErr error, policy nodeEffectPolicy, retriedPriorFailure *bool) (bool, node.Response, error) {
	l.observe(response)
	if response.Retryable {
		return l.retryFailedStatus(ctx, response, cancelErr, policy, retriedPriorFailure)
	}
	if err := l.Failed(ctx, response); err != nil {
		return true, response, err
	}
	if err := l.postCompletionCancellation(ctx); err != nil {
		return true, response, err
	}
	return true, response, nil
}

func (l nodeEffectLoop) retryFailedStatus(ctx context.Context, response node.Response, cancelErr error, policy nodeEffectPolicy, retriedPriorFailure *bool) (bool, node.Response, error) {
	// A retryable failure may only be redispatched when it belongs to an older
	// execution attempt. A failure from the current River attempt returns to
	// River; the next attempt owns the retry budget. Missing or mismatched
	// identities fail closed.
	if cancelErr != nil {
		return true, response, cancelErr
	}
	observedAttempt, err := l.observedFailureAttempt(response)
	if err != nil {
		return true, response, err
	}
	switch {
	case observedAttempt == l.Current.Attempt:
		return true, response, l.currentAttemptFailure(ctx, response)
	case observedAttempt > l.Current.Attempt:
		return true, response, nodeEffectIdentityFault("the failed node task belongs to a future attempt")
	case *retriedPriorFailure:
		return true, response, contract.NewFault(
			contract.Unavailable,
			"a prior node attempt's retryable failure was already retried in this attempt",
			"Return the current attempt to River instead of dispatching another effect.",
		)
	default:
		*retriedPriorFailure = true
		if err := l.wait(ctx, policy.RetryBackoff); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
		return l.dispatch(ctx)
	}
}

func (l nodeEffectLoop) handleAbsentStatus(ctx context.Context, response node.Response, cancelErr error, firstStatus bool, policy nodeEffectPolicy) (bool, node.Response, error) {
	l.observe(response)
	if cancelErr != nil {
		return true, response, cancelErr
	}
	// The first status observation in this loop is not a reconciliation poll:
	// no live status was observed before it, so dispatch immediately. The
	// engine effect lock and durable step state are the actual safety fence,
	// not the pre-dispatch sleep. After the first observation,
	// absence/staleness keeps its reconciliation backoff; stale always keeps it.
	if !firstStatus || response.Status != engine.StatusAbsent {
		if err := l.wait(ctx, policy.RetryBackoff); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
	}
	return l.dispatch(ctx)
}

// dispatch runs one effect after the caller observed a non-live status. It
// rechecks durable cancellation after the backoff and after the dispatch
// returns, so a cancellation accepted while a task is starting is not mistaken
// for a safe boundary.
func (l nodeEffectLoop) dispatch(ctx context.Context) (bool, node.Response, error) {
	cancelErr, err := l.cancellation(ctx)
	if err != nil {
		return true, node.Response{}, err
	}
	if cancelErr != nil {
		return true, node.Response{}, cancelErr
	}
	if l.Begin != nil {
		if err := l.Begin(ctx); err != nil {
			return true, node.Response{}, err
		}
	}
	response, err := l.Dispatch(ctx)
	if err != nil {
		return true, node.Response{}, err
	}
	l.observe(response)
	cancelErr, err = l.cancellation(ctx)
	if err != nil {
		return true, response, err
	}
	return l.dispatchResponse(ctx, response, cancelErr)
}

func (l nodeEffectLoop) dispatchResponse(ctx context.Context, response node.Response, cancelErr error) (bool, node.Response, error) {
	switch response.Status {
	case engine.StatusCompleted:
		return l.dispatchCompleted(ctx, response, cancelErr)
	case engine.StatusFailed:
		return l.dispatchFailed(ctx, response, cancelErr)
	case engine.StatusRunning, engine.StatusDeferred, engine.StatusAbsent, engine.StatusStale:
		return l.dispatchNonTerminal(ctx, response, cancelErr)
	default:
		return true, response, fmt.Errorf("node returned unexpected status %q", response.Status)
	}
}

func (l nodeEffectLoop) dispatchCompleted(ctx context.Context, response node.Response, cancelErr error) (bool, node.Response, error) {
	if err := l.Completed(ctx, response, false); err != nil {
		return true, response, err
	}
	if cancelErr != nil {
		return true, response, cancelErr
	}
	return true, response, nil
}

func (l nodeEffectLoop) dispatchFailed(ctx context.Context, response node.Response, cancelErr error) (bool, node.Response, error) {
	if response.Retryable {
		observedAttempt, err := l.observedFailureAttempt(response)
		if err != nil {
			return true, response, err
		}
		if observedAttempt != l.Current.Attempt {
			return true, response, nodeEffectIdentityFault(
				"the dispatched failure does not belong to the current attempt",
			)
		}
	}
	if err := l.Failed(ctx, response); err != nil {
		return true, response, err
	}
	if cancelErr != nil {
		return true, response, cancelErr
	}
	return true, response, nil
}

func (l nodeEffectLoop) dispatchNonTerminal(ctx context.Context, response node.Response, cancelErr error) (bool, node.Response, error) {
	if response.Status == engine.StatusRunning {
		if err := l.wait(ctx, l.Policy.withDefaults().StatusPoll); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
		return false, response, nil
	}
	if cancelErr != nil {
		return true, response, cancelErr
	}
	if response.Status == engine.StatusAbsent || response.Status == engine.StatusStale {
		if err := l.wait(ctx, l.Policy.withDefaults().RetryBackoff); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
	}
	return false, response, nil
}

// cancellation returns errPlatformCancelled when cancellation was durably
// accepted. A different error stops the loop; a live or unknown task must
// retain nonterminal ownership instead of entering cleanup.
func (l nodeEffectLoop) cancellation(ctx context.Context) (error, error) {
	if l.Cancelled == nil {
		return nil, nil
	}
	err := l.Cancelled(ctx)
	if err == nil {
		return nil, nil
	}
	if errors.Is(err, errPlatformCancelled) {
		return err, nil
	}
	return nil, err
}

func (l nodeEffectLoop) postCompletionCancellation(ctx context.Context) error {
	cancelErr, err := l.cancellation(ctx)
	if err != nil {
		return err
	}
	if cancelErr != nil {
		return cancelErr
	}
	return nil
}

func (l nodeEffectLoop) observe(response node.Response) {
	if l.Observe == nil {
		return
	}
	if response.Status != engine.StatusRunning && response.Status != engine.StatusDeferred {
		return
	}
	l.Observe(response)
}

func (l nodeEffectLoop) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	if l.Wait != nil {
		return l.Wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// nodeEffectCancellationExempt reports post-cancellation cleanup effects that
// must still reach the node. Cancellation stops forward work, never the
// removal step that makes a failed or cancelled deployment safe.
func nodeEffectCancellationExempt(stepID string) bool {
	return strings.HasPrefix(stepID, deploymentCleanupStepPrefix)
}

// runStep mirrors the diagnostic worker's durable effect protocol for a generic
// platform payload. Database acknowledgement is attempt-fenced; node status is
// always consulted before a retry dispatches another effect.
type stepRowFence struct {
	rowExists        bool
	begunThisAttempt bool
}

func (f *stepRowFence) ensureRow(ctx context.Context, w *platformWorker, id contract.OperationID, attempt uint32, stepID, inputsHash, executionID string) error {
	if f.rowExists {
		return nil
	}
	already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, executionID)
	if err != nil {
		return err
	}
	f.rowExists = true
	if already {
		return errNodeEffectAlreadyCompleted
	}
	return nil
}

func (f *stepRowFence) ensureDispatchBegun(ctx context.Context, w *platformWorker, id contract.OperationID, attempt uint32, stepID, inputsHash, executionID string) error {
	if f.begunThisAttempt {
		return nil
	}
	already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, executionID)
	if err != nil {
		return err
	}
	f.rowExists = true
	f.begunThisAttempt = true
	if already {
		return errNodeEffectAlreadyCompleted
	}
	return nil
}

// releaseOutcomeGuard rejects any absence evidence for a non-idempotent release
// command; its container may already have been removed after execution.
func releaseOutcomeGuard(stepID string, rowExists bool, attempt uint32, response node.Response) error {
	if stepID != deploymentCommandStep {
		return nil
	}
	if response.Status == engine.StatusStale ||
		(response.Status == engine.StatusAbsent && rowExists) ||
		(response.Status == engine.StatusFailed && response.Retryable &&
			(response.Attempt == 0 || response.Attempt < attempt)) {
		return fmt.Errorf(
			"%w: release command execution cannot be proven absent; inspect the original node journal before any further execution",
			errNodeEffectOutcomeUnknown,
		)
	}
	return nil
}

func (w *platformWorker) nodeEffectStatus(ctx context.Context, tr node.Transport, exec engine.Execution, stepID, inputsHash string, rowExists bool, attempt uint32) (node.Response, error) {
	response, err := tr.Call(
		ctx,
		node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec},
		nil,
	)
	if err != nil {
		if node.IsCallCancelled(err) {
			return node.Response{}, nodeEffectWaitInterrupted()
		}
		if errors.Is(err, engine.ErrJournalCorrupt) {
			return node.Response{}, contract.NewFault(
				contract.PreconditionFailed,
				"node journal is corrupt",
				"Inspect the node journal before retrying.",
			)
		}
		if node.IsPermanentTransport(err) {
			return node.Response{}, contract.NewFault(
				contract.PreconditionFailed,
				"the node transport refused the request",
				"Check the pinned node identity and host configuration.",
			)
		}
		return node.Response{}, fmt.Errorf(
			"%w: node status %s: %w",
			errNodeEffectOutcomeUnknown,
			stepID,
			err,
		)
	}
	if response.InputsHash != "" && response.InputsHash != inputsHash {
		return node.Response{}, contract.NewFault(
			contract.PreconditionFailed,
			"node effect has different inputs",
			"Do not acknowledge an effect recorded under other inputs.",
		)
	}
	if stepID == deploymentCommandStep && response.InputsHash == "" &&
		(response.Status == engine.StatusCompleted || response.Status == engine.StatusFailed) {
		return node.Response{}, contract.NewFault(
			contract.Unavailable,
			"release command status has no effect evidence",
			"Recover the original node effect before rerunning the release command.",
		)
	}
	if err := w.service.retainNodeResponse(ctx, exec, response); err != nil {
		return node.Response{}, err
	}
	if err := releaseOutcomeGuard(stepID, rowExists, attempt, response); err != nil {
		return node.Response{}, err
	}
	return response, nil
}

func (w *platformWorker) nodeEffectDispatch(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, exec engine.Execution, stepID, inputsHash string, raw json.RawMessage, secrets map[string]string, rowExists bool) (node.Response, error) {
	w.service.registerSecretValues(secrets)
	events := w.service.newNodeEventStream(ctx, id, attempt)
	response, err := tr.Call(ctx, node.Request{
		Method:    node.MethodRun,
		RequestID: exec.RequestID,
		Execution: exec,
		Payload:   raw,
		Secrets:   secrets,
	}, events.add)
	if appendErr := events.close(); appendErr != nil && err == nil {
		err = appendErr
	}
	if err != nil {
		if node.IsCallCancelled(err) {
			return node.Response{}, nodeEffectWaitInterrupted()
		}
		if node.IsPermanentTransport(err) {
			return node.Response{}, contract.NewFault(
				contract.PreconditionFailed,
				"the node transport refused the request",
				"Check the pinned node identity and host configuration.",
			)
		}
		// Unknown outcome: the next observation asks status first and never
		// starts a second effect under the same key.
		return node.Response{}, fmt.Errorf(
			"%w: node run %s: %w",
			errNodeEffectOutcomeUnknown,
			stepID,
			err,
		)
	}
	if response.InputsHash != "" && response.InputsHash != inputsHash {
		return node.Response{}, contract.NewFault(
			contract.PreconditionFailed,
			"node effect has different inputs",
			"Do not acknowledge an effect recorded under other inputs.",
		)
	}
	if stepID == deploymentCommandStep && response.InputsHash == "" {
		return node.Response{}, contract.NewFault(
			contract.Unavailable,
			"release command response has no effect evidence",
			"Recover the original node effect before rerunning the release command.",
		)
	}
	if err := w.service.retainNodeResponse(ctx, exec, response); err != nil {
		return node.Response{}, err
	}
	if err := releaseOutcomeGuard(stepID, rowExists, attempt, response); err != nil {
		return node.Response{}, err
	}
	return response, nil
}

func (w *platformWorker) nodeEffectCompleted(ctx context.Context, fence *stepRowFence, id contract.OperationID, attempt uint32, stepID, inputsHash string, started time.Time, response node.Response, recovered bool) error {
	if err := fence.ensureRow(ctx, w, id, attempt, stepID, inputsHash, response.ExecutionID); err != nil {
		return err
	}
	if recovered {
		if err := w.service.recordRecoveredStep(ctx, id, attempt, stepID, response); err != nil {
			return err
		}
	}
	if err := w.service.ackStep(
		ctx,
		id,
		attempt,
		stepID,
		inputsHash,
		response.ExecutionID,
		response.Result,
		platformStepTotal,
	); err != nil {
		return err
	}
	if recovered {
		return nil
	}
	duration := time.Since(started).Milliseconds()
	w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
		Kind: "log", Stage: stepID, Step: stepID, DurationMS: &duration,
		Message: fmt.Sprintf("Node step %s completed in %dms.", stepID, duration),
	})
	return nil
}

func (w *platformWorker) nodeEffectFailed(ctx context.Context, id contract.OperationID, attempt uint32, stepID string, response node.Response) error {
	code := contract.Code(response.ErrorCode)
	if code == "" {
		code = contract.Internal
	}
	detail := response.Detail
	if detail == "" {
		detail = "node step failed"
	}
	if err := w.service.failStep(ctx, id, attempt, stepID, code, detail); err != nil {
		return err
	}
	if code == contract.Conflict {
		// Preserve the machine-readable refusal so a caller that can classify
		// it (retention) records a per-item skip instead of a generic permanent
		// failure.
		return contract.NewFault(
			contract.Conflict,
			"node step "+stepID+" conflicted: "+detail,
			"Inspect the node state; a deterministic conflict is not fixed by retrying.",
		)
	}
	if response.Retryable {
		return fmt.Errorf("node step %s failed: %s", stepID, detail)
	}
	return errors.Join(
		errNodeStepPermanentFailure,
		contract.NewFault(
			contract.PreconditionFailed,
			"node step failed: "+detail,
			"Inspect operation events and node state before resubmitting.",
		),
	)
}

type nodeEffectObserver struct {
	w       *platformWorker
	ctx     context.Context
	id      contract.OperationID
	attempt uint32
	stepID  string
	policy  nodeEffectPolicy
	last    time.Time
	lastKey string
}

func (o *nodeEffectObserver) observe(response node.Response) {
	key := string(response.Status) + "/" + response.ExecutionID + "/" + string(response.Phase)
	observedAt := time.Now()
	if key == o.lastKey && observedAt.Sub(o.last) < o.policy.ObservationEvery {
		return
	}
	o.last, o.lastKey = observedAt, key
	message := fmt.Sprintf("Node step %s is %s", o.stepID, response.Status)
	if response.Phase != "" {
		message += " (phase=" + string(response.Phase)
		if response.WaitMS > 0 {
			message += fmt.Sprintf(", waited=%dms", response.WaitMS)
		}
		message += ")"
	} else if response.WaitMS > 0 {
		message += fmt.Sprintf(" (waited=%dms)", response.WaitMS)
	}
	o.w.service.appendWorkerEvent(o.ctx, o.id, o.attempt, o.stepID, message+".")
}

func (w *platformWorker) runStepOn(
	ctx context.Context,
	tr node.Transport,
	id contract.OperationID,
	attempt uint32,
	stepID string,
	payload any,
	secrets map[string]string,
) (json.RawMessage, error) {
	return w.runStepOnWithBuild(ctx, tr, id, attempt, stepID, payload, secrets, "", "", "", "", "", nil)
}

func (w *platformWorker) runStepOnBuild(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, payload any, secrets map[string]string, nodeName, identity, source string, buildID platform.BuildID, releaseID platform.ReleaseID) (json.RawMessage, error) {
	return w.runStepOnWithBuild(ctx, tr, id, attempt, deploymentPackStep, payload, secrets, nodeName, identity, source, buildID, releaseID, nil)
}

func (w *platformWorker) runStepOnWithBuild( //nolint:gocognit,gocyclo // The effect protocol keeps status, dispatch and acknowledgement together.
	ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32,
	stepID string, payload any, secrets map[string]string, buildNode, buildIdentity, buildSource string,
	buildID platform.BuildID, releaseID platform.ReleaseID,
	stopDispatch *buildStopDispatch,
) (json.RawMessage, error) {
	if platform.IsPackStep(stepID) && (buildNode == "" || buildIdentity == "" || buildID == "" || releaseID == "" || stepID != deploymentPackStep) {
		return nil, contract.NewFault(contract.PreconditionFailed, "build dispatch has no pinned capacity admission", "Use the build execution path with its original builder.")
	}
	if tr == nil {
		return nil, contract.NewFault(
			contract.Unavailable,
			"node transport is not configured",
			"Run this work in a worker configured with a node transport.",
		)
	}
	started := time.Now()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	exec := engine.Execution{
		OperationID: id,
		Attempt:     attempt,
		Step:        stepID,
		RequestID:   newRequestID(),
	}
	step, err := node.DefaultRegistry().Build(exec, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid platform step: %w", engine.ErrPermanent, err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		return nil, err
	}
	inputsHash := engine.InputsHash(step.ID(), inputs)
	record, hasRecord, err := w.service.stepRecord(ctx, id, stepID)
	if err != nil {
		return nil, err
	}
	if hasRecord {
		if record.InputsHash != inputsHash {
			return nil, contract.NewFault(
				contract.PreconditionFailed,
				"step inputs changed for an existing effect",
				"Never reuse an operation ID with different inputs; inspect the node journal.",
			)
		}
		if record.State == "completed" {
			return record.Result, nil
		}
	}
	fence := &stepRowFence{
		rowExists:        hasRecord,
		begunThisAttempt: hasRecord && record.Attempt == attempt && record.ExecutionID == exec.ExecutionID(),
	}
	policy := nodeEffectPolicy{}.withDefaults()
	observer := &nodeEffectObserver{w: w, ctx: ctx, id: id, attempt: attempt, stepID: stepID, policy: policy}
	loop := nodeEffectLoop{
		Status: func(ctx context.Context) (node.Response, error) {
			response, err := w.nodeEffectStatus(ctx, tr, exec, stepID, inputsHash, fence.rowExists, attempt)
			if err == nil && buildNode != "" && !fence.rowExists && response.Status != engine.StatusAbsent {
				return node.Response{}, unknownBuildPlacement()
			}
			return validateBuildStopResponse(response, err, stopDispatch, exec, inputsHash, fence.rowExists)
		},
		Dispatch: func(ctx context.Context) (node.Response, error) {
			response, err := w.nodeEffectDispatch(ctx, tr, id, attempt, exec, stepID, inputsHash, raw, secrets, fence.rowExists)
			return validateBuildStopResponse(response, err, stopDispatch, exec, inputsHash, true)
		},
		Begin: func(ctx context.Context) error {
			if buildNode != "" {
				already, err := w.service.beginBuildDispatch(ctx, id, attempt, buildNode, buildIdentity, inputsHash, exec.ExecutionID(), buildID, releaseID)
				if err != nil {
					var snooze *river.JobSnoozeError
					if errors.As(err, &snooze) {
						w.service.appendWorkerEvent(ctx, id, attempt, deploymentBuildStageLabel,
							fmt.Sprintf("build_queued on node %s; build capacity is busy", buildNode))
					}
					return err
				}
				fence.rowExists, fence.begunThisAttempt = true, true
				if already {
					return errNodeEffectAlreadyCompleted
				}
				w.service.appendWorkerEvent(ctx, id, attempt, deploymentBuildStageLabel,
					fmt.Sprintf("build_running on node %s; %s", buildNode, buildSource))
				return nil
			}
			if stopDispatch != nil {
				already, err := w.service.beginBuildStopDispatch(ctx, exec, inputsHash, stopDispatch)
				if err != nil {
					return err
				}
				fence.rowExists, fence.begunThisAttempt = true, true
				if already {
					return errNodeEffectAlreadyCompleted
				}
				return nil
			}
			return fence.ensureDispatchBegun(ctx, w, id, attempt, stepID, inputsHash, exec.ExecutionID())
		},
		Completed: func(ctx context.Context, response node.Response, recovered bool) error {
			return w.nodeEffectCompleted(ctx, fence, id, attempt, stepID, inputsHash, started, response, recovered)
		},
		Failed: func(ctx context.Context, response node.Response) error {
			return w.nodeEffectFailed(ctx, id, attempt, stepID, response)
		},
		Observe: observer.observe,
		Policy:  policy,
		Current: exec,
	}
	if !nodeEffectCancellationExempt(stepID) {
		loop.Cancelled = func(ctx context.Context) error { return w.service.checkCancelled(ctx, id) }
	}
	response, err := loop.run(ctx)
	if err != nil {
		if errors.Is(err, errNodeEffectAlreadyCompleted) {
			if refreshed, found, recordErr := w.service.stepRecord(ctx, id, stepID); recordErr != nil {
				return nil, recordErr
			} else if found {
				return refreshed.Result, nil
			}
			return nil, nil
		}
		return nil, err
	}
	return response.Result, nil
}

// reconcilePlatformOperation closes an operation whose River job disappeared.
// It never invents success: it accepts only durable domain state that proves the
// action committed, and otherwise records failure with the serving release that
// the environment actually had (or a switched release with unknown health).
func (s *Service) reconcilePlatformOperation(ctx context.Context, op contract.Operation) error {
	if op.Kind == platform.KindDefinitionApply {
		return s.requeueDefinitionDrain(ctx, op.ID)
	}
	if op.Kind == platform.KindDomainAdd || op.Kind == platform.KindDomainRemove {
		if handled, err := s.requeueDefinitionDNSRecovery(ctx, op.ID); handled || err != nil {
			return err
		}
	}
	if op.Kind == platform.KindDeploy || op.Kind == platform.KindRollback ||
		op.Kind == platform.KindRestart ||
		op.Kind == platform.KindPromote {
		return s.reconcileDeployment(ctx, op)
	}
	if op.Kind == platform.KindBuildStop {
		if err := s.recoverBuildStopEffect(ctx, op); err != nil {
			return err
		}
		if op.State == contract.Cancelling {
			return s.settleReconciledBuildStop(ctx, op, true, "")
		}
	}
	if op.Kind == platform.KindBuildRetry {
		return s.reconcileBuildRetry(ctx, op)
	}
	if op.State == contract.Cancelling {
		return s.cancelPlatform(
			ctx,
			op.ID,
			op.Attempt,
			"Cancellation reconciled after the River job ended.",
		)
	}
	switch op.Kind {
	case platform.KindBuildStop:
		return s.reconcileBuildStop(ctx, op)
	case platform.KindNodeCheck, platform.KindNodeReconcile:
		return s.reconcileNodeAction(ctx, op)
	case platform.KindNodeEnroll:
		return s.reconcileNodeEnroll(ctx, op)
	case platform.KindDomainAdd, platform.KindDomainRemove:
		return s.reconcileDomainOperation(ctx, op)
	case platform.KindAppDelete:
		return s.reconcileAppDelete(ctx, op)
	case platform.KindEnvironmentDelete:
		return s.reconcileEnvironmentDelete(ctx, op)
	case platform.KindResourceApply, platform.KindResourceDestroy:
		return s.reconcileResourceOperation(ctx, op)
	case platform.KindAppCreate, platform.KindEnvironmentCreate, platform.KindEnvironmentUpdate,
		platform.KindProcessScale, platform.KindConfigSet, platform.KindConfigUnset,
		platform.KindEnvironmentSecretAttach, platform.KindEnvironmentResourceAttach,
		platform.KindEnvironmentResourceDetach, platform.KindNodeRegister:
		return s.reconcileRecordAction(ctx, op)
	default:
		return s.failReconciledPlatform(
			ctx,
			op,
			"extension execution ended without a durable operation result; extension side effects are not assumed successful",
			contract.Unavailable,
		)
	}
}

func (s *Service) reconcileBuildRetry(ctx context.Context, op contract.Operation) error {
	return s.recoverBuildRetry(ctx, op)
}

func (s *Service) reconcileBuildStop(ctx context.Context, op contract.Operation) error {
	normalized, err := s.operationInput(ctx, op)
	if err != nil {
		return err
	}
	input, err := typedOperationInput[*platform.BuildStopInput](normalized.Input)
	if err != nil {
		return err
	}
	build, err := s.buildByID(ctx, input.BuildID)
	if err != nil {
		return err
	}
	if build.State == platform.BuildCancelled {
		// The build is marked cancelled before the stop marker is dispatched.
		// That state alone does not prove the stop request reached the node.
		record, found, err := s.stepRecord(ctx, op.ID, deploymentBuildStopStep)
		if err != nil {
			return err
		}
		if !found {
			return s.settleReconciledBuildStop(ctx, op, false, "build stop ended before a cancellation marker was dispatched")
		}
		if record.State != "completed" {
			return contract.NewFault(contract.Unavailable, "build stop has no acknowledged cancellation marker",
				"Recover the original node effect before reconciling the operation.")
		}
		return s.finishPlatformOperation(
			ctx,
			op.ID,
			op.Attempt,
			func(*entities.StoreTx, *contract.Operation) error { return nil },
		)
	}
	return s.settleReconciledBuildStop(ctx, op, false, "build stop ended before the build was recorded as cancelled")
}

func (s *Service) reconcileNodeAction(ctx context.Context, op contract.Operation) error {
	return s.failReconciledPlatform(
		ctx,
		op,
		"the node action ended before it recorded a result; it is idempotent, so submit it again",
		contract.Unavailable,
	)
}

func (s *Service) reconcileNodeEnroll(ctx context.Context, op contract.Operation) error {
	// Enrollment is durable only once the worker recorded it. A job that ended
	// first leaves the node pending (or failed) for an enrollment retry; the
	// operation never claims verification it did not see.
	state, enrollOp, found := platform.EnrollmentRegistered, contract.OperationID(""), false
	observed, enrollID, ok, err := s.entities.Nodes().EnrollmentForTarget(ctx, op.Target)
	if err == nil && ok {
		state, enrollOp, found = observed, enrollID, true
	}
	if found && state == platform.EnrollmentEnrolled && enrollOp != "" && enrollOp == op.ID {
		return s.finishPlatformOperation(
			ctx,
			op.ID,
			op.Attempt,
			func(*entities.StoreTx, *contract.Operation) error { return nil },
		)
	}
	return s.failReconciledPlatform(
		ctx,
		op,
		"enrollment ended before the node was verified; retry node enrollment",
		contract.Unavailable,
	)
}

func (s *Service) reconcileDomainOperation(ctx context.Context, op contract.Operation) error {
	op, input, normalized, err := s.domainOperationInput(ctx, op.ID)
	if err != nil {
		return err
	}
	releaseLock, err := s.lockPlatformTargets(ctx, normalized)
	if err != nil {
		return err
	}
	defer releaseLock()
	if err := s.revalidateDomainOperation(ctx, op, input); err != nil {
		fault := contract.Classify(err)
		return s.failReconciledPlatform(ctx, op, err.Error(), fault.Code)
	}
	if err := s.applyDomainDNSEffect(
		ctx,
		op,
		op.Attempt,
		input,
		op.Kind == platform.KindDomainRemove,
	); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return s.cancelPlatform(
				ctx,
				op.ID,
				op.Attempt,
				"Cancellation acknowledged after provider effects; binding and cleanup ledger were retained.",
			)
		}
		fault := contract.Classify(err)
		return s.failReconciledPlatform(ctx, op, err.Error(), fault.Code)
	}
	if err := s.reconcileDomainBoundary(ctx, op); err != nil {
		return err
	}
	if op.Kind == platform.KindDomainRemove {
		if err := s.deleteReconciledDomainBinding(ctx, op, input); err != nil {
			return err
		}
	}
	if err := s.syncRoutes(ctx); err != nil {
		return s.failReconciledPlatform(ctx, op, err.Error(), contract.Unavailable)
	}
	if err := s.finishDomainOperation(
		ctx,
		op.ID,
		op.Attempt,
		func(*entities.StoreTx, *contract.Operation) error { return nil },
	); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return s.cancelPlatform(
				ctx,
				op.ID,
				op.Attempt,
				"Cancellation acknowledged at domain finalization; completed effects were not rolled back.",
			)
		}
		return err
	}
	return nil
}

func (s *Service) deleteReconciledDomainBinding(ctx context.Context, op contract.Operation, input *platform.DomainInput) error {
	if err := s.deleteDomainBindingRowOwned(
		ctx,
		op.ID,
		op.Attempt,
		input.App,
		input.Environment,
		input.Domain,
	); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return s.cancelPlatform(
				ctx,
				op.ID,
				op.Attempt,
				"Cancellation acknowledged before binding deletion; cleanup state was retained.",
			)
		}
		if errors.Is(err, errNotOwned) {
			return s.waitForNewerAttempt(ctx, op.ID, op.Attempt)
		}
		return s.failReconciledPlatform(ctx, op, err.Error(), contract.Unavailable)
	}
	return nil
}

func (s *Service) reconcileDomainBoundary(ctx context.Context, op contract.Operation) error {
	if err := s.checkDomainDNSBoundary(ctx, op.ID, op.Attempt); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return s.cancelPlatform(
				ctx,
				op.ID,
				op.Attempt,
				"Cancellation acknowledged before binding deletion; cleanup state was retained.",
			)
		}
		if errors.Is(err, errNotOwned) {
			return s.waitForNewerAttempt(ctx, op.ID, op.Attempt)
		}
		return err
	}
	return nil
}

func (s *Service) reconcileAppDelete(ctx context.Context, op contract.Operation) error {
	normalized, err := s.operationInput(ctx, op)
	if err != nil {
		return err
	}
	input, err := typedOperationInput[*platform.AppDeleteInput](normalized.Input)
	if err != nil {
		return err
	}
	if _, _, err := s.applicationEnvironments(ctx, input.App); err != nil {
		if contract.Classify(err).Code == contract.NotFound {
			return s.finishPlatformOperation(
				ctx,
				op.ID,
				op.Attempt,
				func(*entities.StoreTx, *contract.Operation) error { return nil },
			)
		}
		return err
	}
	return s.failReconciledPlatform(
		ctx,
		op,
		"application deletion ended before the platform rows were removed",
		contract.Unavailable,
	)
}

func (s *Service) reconcileEnvironmentDelete(ctx context.Context, op contract.Operation) error {
	normalized, err := s.operationInput(ctx, op)
	if err != nil {
		return err
	}
	input, err := typedOperationInput[*platform.EnvironmentDeleteInput](normalized.Input)
	if err != nil {
		return err
	}
	if _, err := s.environment(ctx, input.App, input.Environment); err != nil {
		if contract.Classify(err).Code == contract.NotFound {
			return s.finishPlatformOperation(
				ctx,
				op.ID,
				op.Attempt,
				func(*entities.StoreTx, *contract.Operation) error { return nil },
			)
		}
		return err
	}
	return s.failReconciledPlatform(
		ctx,
		op,
		"environment deletion ended before the platform rows were removed",
		contract.Unavailable,
	)
}

func (s *Service) reconcileResourceOperation(ctx context.Context, op contract.Operation) error {
	normalized, err := s.operationInput(ctx, op)
	if err != nil {
		return err
	}
	_ = normalized
	target, err := platform.ParseTarget(op.Target)
	if err != nil {
		return err
	}
	resource, err := s.resourceByName(ctx, target.App, target.Environment, target.Resource)
	if err != nil {
		if contract.Classify(err).Code == contract.NotFound {
			return s.failReconciledPlatform(
				ctx,
				op,
				"resource execution ended before a durable resource record existed",
				contract.Unavailable,
			)
		}
		return err
	}
	if resource.OperationID != op.ID {
		return s.failReconciledPlatform(
			ctx,
			op,
			"resource operation was superseded by another operation",
			contract.Conflict,
		)
	}
	if op.Kind == platform.KindResourceApply && resource.State == platform.ResourceReady {
		return s.finishPlatformOperation(
			ctx,
			op.ID,
			op.Attempt,
			func(*entities.StoreTx, *contract.Operation) error { return nil },
		)
	}
	if op.Kind == platform.KindResourceDestroy && resource.State == platform.ResourceDestroyed {
		return s.finishPlatformOperation(
			ctx,
			op.ID,
			op.Attempt,
			func(*entities.StoreTx, *contract.Operation) error { return nil },
		)
	}
	cause := contract.NewFault(
		contract.Unavailable,
		"resource execution ended before its durable state was recorded",
		"Inspect resource status; the container may still exist. Re-apply or re-destroy explicitly after checking the node.",
	)
	s.recordOwnedResourceFailure(
		ctx,
		op.ID,
		op.Attempt,
		resource.App,
		resource.Environment,
		resource.Name,
		cause,
	)
	return s.failReconciledPlatform(ctx, op, cause.Message, contract.Unavailable)
}

func (s *Service) reconcileRecordAction(ctx context.Context, op contract.Operation) error {
	exists, err := s.recordActionExists(ctx, op)
	if err != nil {
		return err
	}
	if exists {
		return s.finishPlatformOperation(
			ctx,
			op.ID,
			op.Attempt,
			func(*entities.StoreTx, *contract.Operation) error { return nil },
		)
	}
	return s.failReconciledPlatform(
		ctx,
		op,
		"execution ended before the record was durable",
		contract.Unavailable,
	)
}

func (s *Service) deploymentByID(
	ctx context.Context,
	id platform.DeploymentID,
) (platform.Deployment, error) {
	dep, found, err := s.entities.Platform().Deployments().GetByID(ctx, id)
	if err != nil {
		return dep, unavailable(err)
	}
	if !found {
		return dep, contract.NewFault(
			contract.NotFound,
			"serving deployment is missing",
			"Inspect the environment serving deployment before retrying.",
		)
	}
	return dep, nil
}

func (s *Service) deploymentByOperation(
	ctx context.Context,
	id contract.OperationID,
) (platform.Deployment, bool, error) {
	dep, found, err := s.entities.Platform().Deployments().GetByOperation(ctx, id)
	if err != nil {
		return dep, false, unavailable(err)
	}
	return dep, found, nil
}

func (s *Service) operationInput(ctx context.Context, op contract.Operation) (platform.Normalized, error) {
	raw, found, err := s.entities.Operations().Input(ctx, op.ID)
	if err != nil || !found {
		return platform.Normalized{}, unavailable(err)
	}
	return platform.Default().Normalize(op.Kind, op.Target, raw)
}

func (s *Service) recordActionExists(ctx context.Context, op contract.Operation) (bool, error) {
	raw, found, err := s.entities.Operations().Input(ctx, op.ID)
	if err != nil || !found {
		return false, unavailable(err)
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return false, err
	}
	var exists bool
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		exists, err = s.entities.Platform().Apps().Exists(ctx, input.Name)
	case *platform.EnvironmentCreateInput:
		exists, err = s.entities.Platform().Environments().Exists(ctx, input.App, input.Name)
	case *platform.EnvironmentUpdateInput:
		exists, err = s.entities.Platform().Revisions().ExistsForOperation(ctx, op.ID)
		if err == nil && !exists && input.RotateWebhookSecret {
			// Rotation-only admissions intentionally publish no new revision;
			// the accepted operation and sealed secret revision are the state.
			exists = true
		}
	case *platform.ProcessScaleInput:
		exists, err = s.entities.Platform().Revisions().ExistsForOperation(ctx, op.ID)
	case *platform.ConfigSetInput:
		exists, err = s.entities.Platform().Revisions().ExistsForOperation(ctx, op.ID)
	case *platform.ConfigUnsetInput:
		exists, err = s.entities.Platform().Revisions().ExistsForOperation(ctx, op.ID)
	case *platform.EnvironmentSecretAttachInput:
		exists, err = s.entities.Platform().Revisions().ExistsForOperation(ctx, op.ID)
	case *platform.EnvironmentResourceInput:
		exists, err = s.entities.Platform().Revisions().ExistsForOperation(ctx, op.ID)
	case *platform.NodeRegisterInput:
		exists, err = s.entities.Nodes().Exists(ctx, input.Name)
	default:
		return false, nil
	}
	if err != nil {
		return false, unavailable()
	}
	return exists, nil
}

func (s *Service) failReconciledPlatform(
	ctx context.Context,
	op contract.Operation,
	message string,
	code contract.Code,
) error {
	if err := s.failPlatformOperation(ctx, op.ID, op.Attempt, code, message); err != nil {
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	return nil
}

// deploymentNodeEffectStep reports whether a deployment step is executed by
// the application node. Route, public, and restore steps are control-plane
// effects and must never be mistaken for an outstanding node task.
func deploymentNodeEffectStep(step string) bool {
	switch step {
	case deploymentPreflightStep, deploymentCommandStep, deploymentPackStep,
		deploymentImageStep, deploymentReadyImageStep, deploymentPullStep,
		deploymentNetworkStep, deploymentBuildStopStep:
		return true
	}
	return strings.HasPrefix(step, deploymentContainerStepPrefix) ||
		strings.HasPrefix(step, deploymentHealthStepPrefix) ||
		strings.HasPrefix(step, "platform-port-") ||
		strings.HasPrefix(step, deploymentCleanupStepPrefix)
}

// deploymentNodeEffectOutstanding reports whether a deployment step may still
// be live on the deployment's pinned node, or whether its outcome cannot be
// observed. It asks the placement-correct node transport and never starts a
// step. A completed effect is acknowledged so cancellation cleanup cannot
// discard it.
func (s *Service) deploymentNodeEffectOutstanding(
	ctx context.Context,
	dep platform.Deployment,
	env platform.Environment,
	id contract.OperationID,
	attempt uint32,
) (bool, error) {
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return true, err
	}
	outstanding := outstandingDeploymentRecords(records)
	if len(outstanding) == 0 {
		return false, nil
	}
	// Resolve the deployment's pinned revision node, not the environment's
	// current node. The environment may have moved A -> B after this
	// deployment was pinned to A; observing B would miss a live A effect.
	nodeName, err := deploymentCleanupNode(ctx, s, dep, env.Node)
	if err != nil {
		return true, err
	}
	if nodeName == "" {
		for _, record := range outstanding {
			if record.Step == deploymentCommandStep {
				return true, nil
			}
		}
		return false, nil
	}
	if s.resolver == nil {
		// An incomplete node step exists and no transport can observe it. Its
		// outcome is unknown; retain ownership instead of terminalizing.
		return true, nil
	}
	tr, err := s.resolver.TransportFor(ctx, nodeName)
	if err != nil {
		return true, err
	}
	for _, record := range outstanding {
		live, err := s.outstandingNodeStep(ctx, tr, id, attempt, record)
		if err != nil || live {
			return live, err
		}
	}
	return false, nil
}

func outstandingDeploymentRecords(records []StepRecord) []StepRecord {
	outstanding := make([]StepRecord, 0, len(records))
	for _, record := range records {
		if record.State == "completed" || !deploymentNodeEffectStep(record.Step) {
			continue
		}
		outstanding = append(outstanding, record)
	}
	return outstanding
}

func (s *Service) outstandingNodeStep(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, record StepRecord) (bool, error) {
	recordAttempt := record.Attempt
	if recordAttempt == 0 {
		recordAttempt = attempt
	}
	exec := engine.Execution{
		OperationID: id,
		Attempt:     recordAttempt,
		Step:        record.Step,
		RequestID:   newRequestID(),
	}
	response, err := tr.Call(
		ctx,
		node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec},
		nil,
	)
	if err != nil {
		return true, fmt.Errorf(
			"observe node step %s before cancellation cleanup: %w",
			record.Step,
			err,
		)
	}
	if response.InputsHash != "" && response.InputsHash != record.InputsHash {
		return true, contract.NewFault(contract.PreconditionFailed,
			"node effect has different inputs during cancellation recovery",
			"Do not discard an effect recorded under other inputs; inspect the node journal.")
	}
	if response.Status == engine.StatusFailed {
		// Evidence collection is best effort and cannot veto safe cleanup.
		// The helper validates the original execution and current write fence.
		current := engine.Execution{OperationID: id, Attempt: attempt, Step: record.Step}
		_ = s.retainNodeResponse(ctx, current, response)
	}
	if outstanding, decided := releaseCommandStillOutstanding(id, record, response); decided {
		return outstanding, nil
	}
	switch response.Status {
	case engine.StatusCompleted:
		if err := s.ackStep(
			ctx,
			id,
			attempt,
			record.Step,
			record.InputsHash,
			response.ExecutionID,
			response.Result,
			platformStepTotal,
		); err != nil {
			return true, err
		}
	case engine.StatusRunning:
		// A live effect is not a cleanup boundary. A pre-Check/Apply deferral is
		// different: that process exited and no side effect was attempted, so
		// cancellation may proceed from that safe boundary.
		return true, nil
	case engine.StatusDeferred:
		// Exited before Check/Apply. Safe to cancel and clean up.
	case engine.StatusAbsent, engine.StatusStale, engine.StatusFailed:
		// The task is gone. Restore/cleanup recovery remains authoritative for
		// what it may have changed.
	default:
		return true, nil
	}
	return false, nil
}

func releaseCommandStillOutstanding(id contract.OperationID, record StepRecord, response node.Response) (bool, bool) {
	if record.Step != deploymentCommandStep {
		return false, false
	}
	switch response.Status {
	case engine.StatusAbsent, engine.StatusStale:
		// Intent is durable but the command's completion isn't. Its container
		// may have run and disappeared before acknowledgement.
		return true, true
	case engine.StatusFailed, engine.StatusCompleted:
		// A terminal task is usable only when it names this exact execution and
		// inputs. A retryable failure is still unknown.
		if response.InputsHash == "" ||
			!releaseCommandObservedIdentity(id, record, response) ||
			(response.Status == engine.StatusFailed && response.Retryable) {
			return true, true
		}
		// A valid terminal response still needs the general handling below:
		// completed commands must be acknowledged before cleanup proceeds.
		return false, false
	case engine.StatusRunning, engine.StatusDeferred:
		// Not a terminal command boundary; the general status handling decides.
		return false, false
	default:
		return false, false
	}
}

// ensureNoOutstandingDeploymentEffect blocks cancellation, cleanup and
// terminalization while a detached effect for the deployment may still be live.
// It uses the deployment's pinned placement, so an environment moved to another
// node cannot hide an effect on the old node.
func (w *platformWorker) ensureNoOutstandingDeploymentEffect(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	dep platform.Deployment,
	env platform.Environment,
	stage string,
) error {
	outstanding, err := w.service.deploymentNodeEffectOutstanding(ctx, dep, env, id, attempt)
	if err != nil {
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind:  "warning",
			Stage: stage,
			Message: "Deployment effects cannot be observed on the pinned node yet: " + contract.Classify(
				err,
			).Message,
		})
		return contract.NewFault(contract.Unavailable,
			"deployment node effects cannot yet be proven safe",
			"Keep the deployment owner and retry after the pinned node transport is available.")
	}
	if outstanding {
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind:    "warning",
			Stage:   stage,
			Message: "A detached node effect is still live or its outcome is unknown; route restoration and candidate cleanup were not started.",
		})
		return contract.NewFault(
			contract.Unavailable,
			"a detached node effect is still live or its outcome is unknown",
			"Keep the deployment owner and retry after the effect completes or exits before Check/Apply.",
		)
	}
	return nil
}

// cancelOwnedDeployment acknowledges cancellation only through the durable
// owner. The caller holds the environment lock. A cancelled operation that never
// owned the route can end immediately; an owner must restore its exact target
// before the cancellation becomes terminal.
func (w *platformWorker) cancelOwnedDeployment(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
) error {
	dep, has, err := w.service.deploymentByOperation(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if !has {
		return w.service.cancelPlatform(
			ctx,
			id,
			attempt,
			"Cancellation acknowledged; no deployment record exists.",
		)
	}
	env, err := w.service.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		if isNotFound(err) {
			return w.service.cancelPlatform(
				ctx,
				id,
				attempt,
				"Cancellation acknowledged; the environment no longer exists.",
			)
		}
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.ensureNoOutstandingDeploymentEffect(
		ctx,
		id,
		attempt,
		dep,
		env,
		"cancellation",
	); err != nil {
		return err
	}
	owner, acquired, terminal, staleSettled, err := w.service.acquireDeploymentOwnerForRecovery(ctx, id, dep, env, attempt)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if terminal {
		return nil
	}
	if staleSettled {
		// The settled owner may have restored a different serving deployment.
		// Re-read before freezing this owner's predecessor.
		refreshedEnv, envErr := w.service.environment(ctx, dep.App, dep.Environment)
		if envErr != nil {
			return w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil, envErr)
		}
		env = refreshedEnv
		owner, acquired, err = w.service.acquireDeploymentOwner(ctx, dep, env, attempt)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
	}
	if !acquired {
		return w.service.cancelPlatform(
			ctx,
			id,
			attempt,
			"Cancellation acknowledged; another deployment owner controls the route and this operation made no route change.",
		)
	}
	refreshed, refreshedHas, err := w.service.deploymentByOperation(ctx, id)
	if err != nil {
		return w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil, err)
	}
	if !refreshedHas {
		return w.service.cancelPlatform(
			ctx,
			id,
			attempt,
			"Cancellation acknowledged; no deployment record exists.",
		)
	}
	dep = refreshed
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	_, err = w.service.recoverOwnedDeployment(ctx, op, dep, env, owner, false)
	return err
}

// acquireDeploymentOwnerForRecovery acquires the owner, settling a stale owner
// first. terminal reports that the stale owner already ended this operation;
// staleSettled reports that the caller must re-read the environment and retry
// acquisition before freezing its predecessor.
func (s *Service) acquireDeploymentOwnerForRecovery(ctx context.Context, id contract.OperationID, dep platform.Deployment, env platform.Environment, attempt uint32) (deploymentOwner, bool, bool, bool, error) {
	owner, acquired, err := s.acquireDeploymentOwner(ctx, dep, env, attempt)
	if err != nil {
		return owner, false, false, false, err
	}
	if acquired {
		return owner, true, false, false, nil
	}
	settled, err := s.recoverStaleDeploymentOwner(ctx, owner, env)
	if err != nil {
		return owner, false, false, false, err
	}
	if !settled {
		return owner, false, false, false, nil
	}
	if state, _, stateErr := s.operationState(ctx, id); stateErr == nil && state.Terminal() {
		return owner, false, true, false, nil
	}
	return owner, false, false, true, nil
}

// recoverOwnedDeployment inspects durable proof and the owner's frozen restore
// target. A running route step is never proof: only a completed route step plus
// completed public verification is enough to finish success. Everything else
// that may have touched the route is restored to the exact frozen predecessor
// before candidates are cleaned or the operation is failed/cancelled.
func (s *Service) recoverOwnedDeployment(
	ctx context.Context,
	op contract.Operation,
	dep platform.Deployment,
	env platform.Environment,
	owner deploymentOwner,
	allowResume bool,
) (bool, error) {
	if op.State.Terminal() {
		return true, nil
	}
	if err := ensureRecoveryEffectBoundary(ctx, s, op, dep, env, allowResume); err != nil {
		return false, err
	}
	routeCompleted, publicCompleted, restoreHas, evidence, err := s.deploymentRecoveryEvidence(ctx, op)
	if err != nil {
		return false, err
	}
	cancelling := op.State == contract.Cancelling

	if !cancelling && dep.State == "succeeded" && ownerAllowsSuccess(owner, dep.ID) {
		return s.finishRecoveredOwnedDeployment(ctx, op, dep, env, owner, func() error {
			return s.finishPlatformOperation(
				ctx,
				op.ID,
				op.Attempt,
				func(_ *entities.StoreTx, current *contract.Operation) error {
					if current.State == contract.Cancelling {
						return errDeploymentCancelledAtFinalize
					}
					return nil
				},
			)
		})
	}

	if !cancelling && ownerAllowsSuccess(owner, dep.ID) && routeCompleted && publicCompleted && !restoreHas {
		// The switch and public verification both committed. Reassert the
		// owner's candidate intent and finish the operation without an election
		// based on creation time.
		return s.finishRecoveredOwnedDeployment(ctx, op, dep, env, owner, func() error {
			return s.finishDeploymentAfterSwitch(ctx, op, dep, env)
		})
	}

	if !evidence && owner.Phase == deploymentOwnerPrepared {
		return s.settlePreparedRecovery(ctx, op, dep, env, allowResume)
	}

	if cancelling {
		return s.restoreAndCancelOwnedDeployment(ctx, op, dep, env, owner)
	}
	if owner.RestoreDeploymentID != "" && owner.RestoreDeploymentID == dep.ID {
		return false, contract.NewFault(
			contract.Unavailable,
			"deployment owner's restore target is the candidate deployment",
			"Ownership is retained; inspect and repair the owner before retrying, because restoring the candidate would race the live deployment.",
		)
	}
	return s.failRecoveredOwnedDeployment(ctx, op, dep, env, owner)
}

func ensureRecoveryEffectBoundary(ctx context.Context, s *Service, op contract.Operation, dep platform.Deployment, env platform.Environment, allowResume bool) error {
	if op.State != contract.Cancelling && allowResume {
		return nil
	}
	stage := "recovery"
	if op.State == contract.Cancelling {
		stage = "cancellation"
	}
	return (&platformWorker{service: s}).ensureNoOutstandingDeploymentEffect(
		ctx,
		op.ID,
		op.Attempt,
		dep,
		env,
		stage,
	)
}

func (s *Service) deploymentRecoveryEvidence(ctx context.Context, op contract.Operation) (routeCompleted, publicCompleted, restoreHas, evidence bool, err error) {
	routeRecord, routeHas, err := s.stepRecord(ctx, op.ID, deploymentRouteStep)
	if err != nil {
		return false, false, false, false, err
	}
	publicRecord, publicHas, err := s.stepRecord(ctx, op.ID, deploymentPublicStep)
	if err != nil {
		return false, false, false, false, err
	}
	_, restoreHas, err = s.stepRecord(ctx, op.ID, deploymentRestoreRouteStep)
	if err != nil {
		return false, false, false, false, err
	}
	routeCompleted = routeHas && routeRecord.State == "completed"
	publicCompleted = publicHas && publicRecord.State == "completed"
	evidence = routeHas || publicHas || restoreHas
	return routeCompleted, publicCompleted, restoreHas, evidence, nil
}

// finishRecoveredOwnedDeployment reasserts serving intent, syncs routes and
// finalizes an owner whose candidate is proven serving. Lock ownership and the
// cleanup pre-record stay ordered exactly as the live deployment path.
func (s *Service) finishRecoveredOwnedDeployment(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, owner deploymentOwner, finalize func() error) (bool, error) {
	owner, err := s.setDeploymentOwnerRoute(
		ctx,
		owner,
		dep.ID,
		deploymentOwnerServing,
		op.Attempt,
	)
	if err != nil {
		return false, err
	}
	if err := s.checkCancelled(ctx, op.ID); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return s.restoreAndCancelOwnedDeployment(ctx, op, dep, env, owner)
		}
		return false, err
	}
	if err := s.syncRoutes(ctx); err != nil {
		return s.handlePostSwitchRouteFailure(ctx, op, dep, env, owner, err)
	}
	if err := s.ensurePreviousDeploymentCleanup(ctx, env, dep); err != nil {
		s.appendCleanupWarning(
			ctx,
			op.ID,
			op.Attempt,
			"Previous release cleanup could not be pre-recorded; ownership is retained: "+contract.Classify(
				err,
			).Message,
		)
		return false, contract.NewFault(
			contract.Unavailable,
			"previous release cleanup could not be recorded durably",
			"Keep the deployment owner and retry after the control database and previous deployment record are readable.",
		)
	}
	if s.beforeDeploymentFinalize != nil {
		s.beforeDeploymentFinalize()
	}
	if err := finalize(); errors.Is(err, errDeploymentCancelledAtFinalize) {
		return s.restoreAndCancelOwnedDeployment(ctx, op, dep, env, owner)
	} else if err != nil {
		return false, err
	}
	_ = s.releaseServedDeploymentOwner(ctx, owner)
	s.removePreviousDeploymentCleanupNow(ctx, env, dep.PreviousDeploymentID)
	return true, nil
}

func (s *Service) settlePreparedRecovery(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, allowResume bool) (bool, error) {
	if op.State == contract.Cancelling {
		if err := s.persistDeploymentCleanupIntent(ctx, env, dep, op.Attempt); err != nil {
			return false, deploymentCleanupIntentFault(err)
		}
		if err := s.cancelPlatform(
			ctx,
			op.ID,
			op.Attempt,
			"Cancellation acknowledged before the traffic switch; the frozen serving route was unchanged and candidates were cleaned where safe.",
		); err != nil {
			return false, err
		}
		s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
		return true, nil
	}
	if allowResume {
		return false, nil
	}
	if err := s.persistDeploymentCleanupIntent(ctx, env, dep, op.Attempt); err != nil {
		return false, deploymentCleanupIntentFault(err)
	}
	if err := s.failPlatformOperation(
		ctx,
		op.ID,
		op.Attempt,
		contract.Unavailable,
		"Execution ended before the traffic switch and cannot resume without a worker lease; the frozen serving route was unchanged and candidates were cleaned where safe.",
	); err != nil {
		return false, err
	}
	s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
	return true, nil
}

func (s *Service) failRecoveredOwnedDeployment(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, owner deploymentOwner) (bool, error) {
	if err := s.restoreDeploymentRoute(context.WithoutCancel(ctx), op.ID, op.Attempt, env, owner); err != nil {
		s.appendWorkerEventWith(ctx, op.ID, op.Attempt, contract.Event{
			Kind:  "warning",
			Stage: "reconcile",
			Message: "The recorded serving route could not be restored; ownership and candidate containers were retained for retry: " + contract.Classify(
				err,
			).Message,
		})
		return false, contract.NewFault(contract.Unavailable,
			"deployment route state is not safely restored and the operation remains owner",
			"Keep the candidate containers; retry after the route writer is available.")
	}
	if err := s.persistDeploymentCleanupIntent(ctx, env, dep, op.Attempt); err != nil {
		return false, deploymentCleanupIntentFault(err)
	}
	if err := s.failPlatformOperation(
		ctx,
		op.ID,
		op.Attempt,
		contract.Unavailable,
		"Execution ended without a completed route proof; the owner restored the exact recorded serving route and cleaned candidates where safe.",
	); err != nil {
		return false, err
	}
	s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
	return true, nil
}

func deploymentCleanupIntentFault(err error) *contract.FaultError {
	return contract.NewFault(
		contract.Unavailable,
		"candidate cleanup intent could not be recorded durably: "+contract.Classify(err).Message,
		"Keep the deployment owner and candidate containers; retry after the control database and cleanup table are available.",
	)
}

// handlePostSwitchRouteFailure is shared by the live attempt and recovery.
// Retryable route failures retain the serving-phase owner and operation. A
// nonretryable failure restores the frozen predecessor before terminalizing;
// if restoration or cleanup-intent persistence fails, ownership stays
// retryable and nothing is terminalized.
func (s *Service) handlePostSwitchRouteFailure(
	ctx context.Context,
	op contract.Operation,
	dep platform.Deployment,
	env platform.Environment,
	owner deploymentOwner,
	cause error,
) (bool, error) {
	if err := s.checkCancelled(ctx, op.ID); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return s.restoreAndCancelOwnedDeployment(ctx, op, dep, env, owner)
		}
		if errors.Is(err, errOperationTerminal) {
			return true, nil
		}
		return false, err
	}
	if s.workerInterrupted(ctx, op.ID, cause) {
		return false, cause
	}
	fault := contract.Classify(cause)
	terminal := errors.Is(cause, engine.ErrPermanent) ||
		fault.Code == contract.ValidationFailed || fault.Code == contract.PreconditionFailed ||
		fault.Code == contract.NotFound || fault.Code == contract.Forbidden ||
		fault.Code == contract.Conflict || fault.Code == contract.Cancelled
	if !terminal {
		return false, cause
	}
	if owner.RestoreDeploymentID != "" && owner.RestoreDeploymentID == dep.ID {
		return false, contract.NewFault(
			contract.Unavailable,
			"deployment owner's restore target is the candidate deployment",
			"Ownership is retained; inspect and repair the owner before retrying, because restoring the candidate would race the live deployment.",
		)
	}
	if err := s.restorePostSwitchRoute(ctx, op, env, owner); err != nil {
		return false, err
	}
	return s.settlePostSwitchFailure(ctx, op, dep, env, fault, cause)
}

func (s *Service) restorePostSwitchRoute(ctx context.Context, op contract.Operation, env platform.Environment, owner deploymentOwner) error {
	if err := s.restoreDeploymentRoute(
		context.WithoutCancel(ctx),
		op.ID,
		op.Attempt,
		env,
		owner,
	); err != nil {
		s.appendWorkerEventWith(ctx, op.ID, op.Attempt, contract.Event{
			Kind:  "warning",
			Stage: "reconcile",
			Message: "The post-switch route failure could not restore the frozen predecessor; ownership and candidates were retained: " + contract.Classify(
				err,
			).Message,
		})
		return contract.NewFault(contract.Unavailable,
			"post-switch route failure could not restore the previous serving route",
			"Keep the candidate containers and owner; retry after the route writer is available.")
	}
	return nil
}

func (s *Service) settlePostSwitchFailure(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, fault *contract.FaultError, cause error) (bool, error) {
	if err := s.persistDeploymentCleanupIntent(ctx, env, dep, op.Attempt); err != nil {
		return false, deploymentCleanupIntentFault(err)
	}
	if err := s.checkCancelled(ctx, op.ID); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			if err := s.cancelPlatform(
				ctx,
				op.ID,
				op.Attempt,
				"Cancellation acknowledged after the post-switch failure restored the frozen predecessor; candidates were cleaned where safe.",
			); err != nil {
				return false, err
			}
			s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
			return true, nil
		}
		if errors.Is(err, errOperationTerminal) {
			return true, nil
		}
		return false, err
	}
	code := fault.Code
	if errors.Is(cause, engine.ErrPermanent) {
		code = contract.PreconditionFailed
	}
	if code == contract.Cancelled {
		if err := s.cancelPlatform(
			ctx,
			op.ID,
			op.Attempt,
			"Post-switch route failure cancelled after restoring the frozen predecessor.",
		); err != nil {
			return false, err
		}
		s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
		return true, nil
	}
	if err := s.failPlatformOperation(
		ctx,
		op.ID,
		op.Attempt,
		code,
		"Post-switch route failure; the frozen predecessor was restored before terminal failure: "+fault.Message,
	); err != nil {
		return false, err
	}
	s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
	return true, nil
}

// restoreAndCancelOwnedDeployment is the single cancellation tail for a
// verifying or verified owner. It restores the exact frozen target before the
// cancellation becomes terminal.
func (s *Service) restoreAndCancelOwnedDeployment(
	ctx context.Context,
	op contract.Operation,
	dep platform.Deployment,
	env platform.Environment,
	owner deploymentOwner,
) (bool, error) {
	if owner.RestoreDeploymentID != "" && owner.RestoreDeploymentID == dep.ID {
		return false, contract.NewFault(
			contract.Unavailable,
			"deployment owner's restore target is the candidate deployment",
			"Ownership is retained; inspect and repair the owner before retrying, because restoring the candidate would race the live deployment.",
		)
	}
	if err := s.restoreDeploymentRoute(
		context.WithoutCancel(ctx),
		op.ID,
		op.Attempt,
		env,
		owner,
	); err != nil {
		s.appendWorkerEventWith(ctx, op.ID, op.Attempt, contract.Event{
			Kind:  "warning",
			Stage: "reconcile",
			Message: "Cancellation is waiting because the exact serving route could not be restored; ownership and candidate containers were retained: " + contract.Classify(
				err,
			).Message,
		})
		return false, contract.NewFault(contract.Unavailable,
			"deployment route state is not safely restored and the operation remains owner",
			"Keep the candidate containers; retry after the route writer is available.")
	}
	if err := s.persistDeploymentCleanupIntent(ctx, env, dep, op.Attempt); err != nil {
		return false, deploymentCleanupIntentFault(err)
	}
	if err := s.cancelPlatform(
		ctx,
		op.ID,
		op.Attempt,
		"Cancellation acknowledged after the owner restored the exact recorded serving route; candidates were cleaned where safe.",
	); err != nil {
		return false, err
	}
	s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
	return true, nil
}

// reconcileDeployment never trusts a deployment row's intended release. It
// acquires the same durable owner as the live path and recovers only the route
// intent and proof that owner recorded.
func (s *Service) reconcileDeployment(ctx context.Context, op contract.Operation) error {
	dep, has, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if !has {
		return s.failReconciledPlatform(
			ctx,
			op,
			"deployment record is missing",
			contract.Unavailable,
		)
	}
	releaseLock, err := s.lockTargets(ctx, []string{"env/" + dep.App + "/" + dep.Environment})
	if err != nil {
		return err
	}
	defer releaseLock()
	env, err := s.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		return err
	}
	owner, acquired, terminal, staleSettled, err := s.acquireDeploymentOwnerForRecovery(ctx, op.ID, dep, env, op.Attempt)
	if err != nil {
		return err
	}
	if terminal {
		return nil
	}
	if staleSettled {
		// The settled owner may have restored a different serving deployment.
		// Re-read before freezing this owner's predecessor.
		refreshedEnv, envErr := s.environment(ctx, dep.App, dep.Environment)
		if envErr != nil {
			return envErr
		}
		env = refreshedEnv
		owner, acquired, err = s.acquireDeploymentOwner(ctx, dep, env, op.Attempt)
		if err != nil {
			return err
		}
	}
	if !acquired {
		return nil
	}
	refreshed, refreshedHas, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if !refreshedHas {
		return s.failReconciledPlatform(
			ctx,
			op,
			"deployment record is missing",
			contract.Unavailable,
		)
	}
	dep = refreshed
	_, err = s.recoverOwnedDeployment(ctx, op, dep, env, owner, false)
	return err
}

func (s *Service) hasStepPrefix(
	ctx context.Context,
	id contract.OperationID,
	prefix string,
) (bool, error) {
	if s.deploymentCleanupEvidence != nil {
		return s.deploymentCleanupEvidence(ctx, id, prefix)
	}
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if strings.HasPrefix(record.Step, prefix) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) finishDeploymentAfterSwitch(
	ctx context.Context,
	op contract.Operation,
	dep platform.Deployment,
	env platform.Environment,
) error {
	return s.finishPlatformOperation(
		ctx,
		op.ID,
		op.Attempt,
		func(tx *entities.StoreTx, current *contract.Operation) error {
			if current.State == contract.Cancelling {
				return errDeploymentCancelledAtFinalize
			}
			if err := lockServingPointersTx(ctx, tx, env.ID, dep.ReleaseID, dep.ID); err != nil {
				return err
			}
			now := time.Now().UTC()
			if _, err := tx.Platform().Deployments().MarkSucceededByOperation(ctx, op.ID, now,
				"Serving release "+string(dep.ReleaseID)+" passed health verification.",
			); err != nil {
				return err
			}
			if _, err := tx.Platform().Environments().SetServing(ctx, entities.EnvironmentServing{
				ID: env.ID, ReleaseID: dep.ReleaseID, DeploymentID: dep.ID,
				HealthState:  platform.HealthHealthy,
				HealthDetail: "serving release passed health verification",
				CheckedAt:    now,
			}); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, current, contract.Event{
				Kind:             "log",
				Stage:            "deploy",
				ReleaseID:        string(dep.ReleaseID),
				ServingReleaseID: string(dep.ReleaseID),
				Message: deploymentWord(
					current.Kind,
				) + " completed after the switch and verification; serving release " + string(
					dep.ReleaseID,
				) + ".",
			}); err != nil {
				return err
			}
			if err := insertPlatformChange(
				ctx,
				tx,
				current,
				deploymentChangeKind(current.Kind, dep.Action, "succeeded"),
				dep.App,
				dep.Environment,
				"Serving release "+string(dep.ReleaseID)+" for "+dep.App+"/"+dep.Environment+".",
				map[string]any{"release_id": dep.ReleaseID},
				map[string]any{"serving_release_id": dep.ReleaseID, "health": "healthy"},
			); err != nil {
				return err
			}
			return releaseServedDeploymentOwnerTx(ctx, tx, op.ID)
		},
	)
}

// deploymentChangeKind names the change history row for a deployment-family
// operation. Promotion reuses the deploy runtime path but keeps its own
// history kind, so an operator can tell a rebuilt deploy from an exact
// cross-environment artifact move.
func deploymentChangeKind(
	operationKind string,
	action platform.DeploymentAction,
	outcome string,
) string {
	if operationKind == platform.KindPromote {
		return "promotion." + outcome
	}
	return string(action) + "." + outcome
}

func deploymentWord(operationKind string) string {
	if operationKind == platform.KindPromote {
		return "Promotion"
	}
	return "Deployment"
}

// insertPlatformChange records desired/applied history without secret values.
func insertPlatformChange(
	ctx context.Context,
	tx *entities.StoreTx,
	op *contract.Operation,
	kind, app, environment, summary string,
	desired, applied any,
) error {
	appID, found, err := tx.Platform().Apps().IDByName(ctx, app)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return contract.NewFault(contract.NotFound, "application not found", "Inspect the application inventory before recording history.")
	}
	if err := lockAppKeyShareTx(ctx, tx, appID); err != nil {
		return err
	}
	environmentID := platform.EnvironmentID("")
	if environment != "" {
		id, found, err := tx.Platform().Environments().LockIDForAppName(ctx, appID, environment)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return pointerIntegrityFault()
		}
		environmentID = id
	}
	var sequence uint64
	if environmentID == "" {
		if sequence, err = tx.Platform().Changes().NextSequenceForApp(ctx, appID); err != nil {
			return unavailable(err)
		}
	} else {
		if sequence, err = tx.Platform().Changes().NextSequenceForEnvironment(ctx, environmentID); err != nil {
			return unavailable(err)
		}
	}
	changeID, err := platform.NewChangeID()
	if err != nil {
		return err
	}
	desiredJSON, _ := json.Marshal(desired)
	appliedJSON, _ := json.Marshal(applied)
	if _, err := tx.Platform().Changes().Insert(ctx, entities.ChangeInsert{
		ID: changeID, AppID: appID, EnvironmentID: environmentID, Sequence: sequence,
		Kind: kind, OperationID: op.ID, Actor: op.Actor, Summary: summary,
		Desired: desiredJSON, Applied: appliedJSON,
	}); err != nil {
		return err
	}
	return nil
}

// appendWorkerEvent records a best-effort observation under the attempt fence.
func (s *Service) appendWorkerEvent(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	stage, message string,
) {
	s.appendWorkerEventWith(
		ctx,
		id,
		attempt,
		contract.Event{Kind: "log", Stage: stage, Message: message},
	)
}

// appendCleanupWarning records that container cleanup did not complete. It is
// a warning, so a viewer cannot report the cleanup as completed because a
// later removal step succeeded. The message can carry the node's removal
// reason, so the worker's own secrets are redacted before it is stored.
func (s *Service) appendCleanupWarning(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	message string,
) {
	s.appendWorkerEventWith(
		ctx,
		id,
		attempt,
		contract.Event{Kind: "warning", Stage: "cleanup", Message: s.redact(message)},
	)
}

// appendWorkerEventWith records one structured observation. It is best effort:
// a terminal operation, or one owned by a newer attempt, is never rewritten.
func (s *Service) appendWorkerEventWith(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	event contract.Event,
) {
	if event.Kind == "" {
		event.Kind = "log"
	}
	if len(event.Message) > 4096 {
		event.Message = event.Message[:4096]
	}
	if event.Health != nil && len(event.Health.Detail) > 4096 {
		event.Health.Detail = event.Health.Detail[:4096]
	}
	_, _, _ = s.mutateOwned(
		context.WithoutCancel(ctx),
		id,
		attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			if op.State.Terminal() {
				return nil
			}
			return appendEvent(ctx, tx, op, event)
		},
	)
}
