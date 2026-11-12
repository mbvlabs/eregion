package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

const platformMaxAttempts = 8

// platformStepTotal is the progress denominator. It is deliberately larger than
// any current pipeline and is informational only; completion depends on the
// actual step records, never this number.
const platformStepTotal = 32

// Deployment step identities are shared by the worker, reconciliation, and the
// viewer. A restored switch is proven by the completed step record, never by a
// message.
const (
	deploymentPreflightStep    = "platform-preflight-01-v1"
	deploymentCommandStep      = "platform-command-01-v1"
	deploymentPackStep         = "platform-pack-02-v1"
	deploymentImageStep        = "platform-image-01-v1"
	deploymentReadyImageStep   = "platform-ready-image-01-v1"
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
	river.WorkerDefaults[platformArgs]
	service *Service
}

// Timeout covers a deployment waiting on detached build and health effects. It
// is deliberately longer than River's rescue threshold, so a worker started on
// this host first recovers its predecessor's running platform jobs instead of
// waiting for this deadline (see Service.rescueHostPlatformJobs).
func (*platformWorker) Timeout(*river.Job[platformArgs]) time.Duration { return 30 * time.Minute }

func (*platformWorker) NextRetry(job *river.Job[platformArgs]) time.Time {
	delay := time.Duration(job.Attempt) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay)
}

func (w *platformWorker) Work(ctx context.Context, job *river.Job[platformArgs]) error {
	attempt := uint32(job.Attempt)
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
	// Diagnostic execution has its own cleanup-first lifecycle. It must bypass
	// generic cancellation, settlement and target normalization entirely.
	if op.Kind == platform.KindDiagnosticExecution {
		return w.runDiagnosticExecution(ctx, op, raw, attempt)
	}
	if op.State == contract.Cancelling {
		if deploymentLifecycleKind(op.Kind) {
			release, acquired, lockErr := w.service.tryLockTargets(ctx, []string{op.Target})
			if lockErr != nil {
				return lockErr
			}
			if !acquired {
				return river.JobSnooze(deploymentOwnerDefer)
			}
			defer release()
			return w.cancelOwnedDeployment(ctx, op.ID, attempt)
		}
		return w.service.cancelPlatform(ctx, op.ID, attempt, "Cancellation requested before execution started.")
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return w.settle(ctx, op.ID, attempt, err)
	}

	var releaseLock func()
	if deploymentLifecycleKind(op.Kind) {
		var acquired bool
		releaseLock, acquired, err = w.service.tryLockTargets(ctx, []string{normalized.Target.String()})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if !acquired {
			return river.JobSnooze(deploymentOwnerDefer)
		}
	} else {
		releaseLock, err = w.service.lockPlatformTargets(ctx, normalized)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
	defer releaseLock()

	if err := w.service.checkCancelled(ctx, op.ID); err != nil {
		if errors.Is(err, errPlatformCancelled) && deploymentLifecycleKind(op.Kind) {
			return w.cancelOwnedDeployment(ctx, op.ID, attempt)
		}
		return w.settle(ctx, op.ID, attempt, err)
	}
	if executor := normalized.Executor(); executor != nil {
		return w.runExtension(ctx, op.ID, attempt, normalized, executor)
	}
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
		return contract.NewFault(contract.ValidationFailed, "the platform.release.create action is removed", "Use an explicit deploy selector (--image-tag, --image-digest, or --image) or a Git deploy; there is no arbitrary release destination or pull toggle.")
	case platform.KindBuildStop:
		return w.runBuildStop(ctx, op.ID, attempt, normalized)
	case platform.KindBuildRetry:
		return w.runBuildRetry(ctx, op.ID, attempt, normalized)
	case platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote:
		return w.runDeployment(ctx, op.ID, attempt, normalized)
	case platform.KindDomainAdd, platform.KindDomainRemove:
		return w.runDomain(ctx, op.ID, attempt, normalized)
	case platform.KindResourceApply:
		return w.runResourceApply(ctx, op.ID, attempt, normalized.Input.(*platform.ResourceApplyInput))
	case platform.KindResourceDestroy:
		return w.runResourceDestroy(ctx, op.ID, attempt, normalized.Input.(*platform.ResourceDestroyInput))
	case platform.KindAppDelete, platform.KindEnvironmentDelete:
		return w.runDelete(ctx, op.ID, attempt, normalized)
	default:
		return w.settle(ctx, op.ID, attempt, contract.NewFault(contract.Internal, "platform action has no worker path", "This is a binary/registration mismatch; do not resubmit accepted work."))
	}
}

func (s *Service) claimPlatform(ctx context.Context, id contract.OperationID, attempt uint32) (contract.Operation, json.RawMessage, bool, error) {
	var raw json.RawMessage
	op, claimed, err := s.mutateOwnedOrClaim(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "", "Platform worker started durable execution."); err != nil {
				return err
			}
		}
		// The claim persists the attempt: every later fenced write compares
		// against it, so a claim that left it at 0 could never finish.
		if err := tx.QueryRow(ctx, `UPDATE eregion_operations SET attempt=$2 WHERE id=$1 RETURNING input`, id, op.Attempt).Scan(&raw); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='running', updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id)
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: "platform", Message: "Platform execution attempt started under the target lock."})
	})
	return op, raw, claimed, err
}

var errPlatformCancelled = errors.New("platform operation cancelled at a safe boundary")

// errDeploymentCancelledAtFinalize is returned from a success callback when the
// operation row is already cancelling. The callback transaction rolls back, so
// success is never committed before cancellation restoration.
var errDeploymentCancelledAtFinalize = errors.New("deployment cancellation was accepted before finalization")

// errNodeStepPermanentFailure marks an effect the node itself reported as
// permanently failed. Transport, status and acknowledgement errors do not carry
// it: their outcome is unknown and must be recovered through node status.
var errNodeStepPermanentFailure = errors.New("node step permanently failed")

// workerInterrupted distinguishes the worker's own canceled context from a
// durable cancellation request. A job shutdown must not remove candidates or
// terminalize the operation: River retries, and recovery resumes from the
// completed container steps. A durable cancelling state still takes the normal
// cancellation/cleanup path.
func (s *Service) workerInterrupted(ctx context.Context, id contract.OperationID, cause error) bool {
	if cause == nil || !errors.Is(cause, context.Canceled) {
		return false
	}
	state, _, err := s.operationState(context.WithoutCancel(ctx), id)
	if err != nil {
		return true
	}
	return state != contract.Cancelling
}

func (w *platformWorker) settle(ctx context.Context, id contract.OperationID, attempt uint32, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errOperationTerminal) {
		return nil
	}
	if errors.Is(err, errNotOwned) {
		return w.service.waitForNewerAttempt(ctx, id, attempt)
	}
	if errors.Is(err, errPlatformCancelled) {
		return w.service.cancelPlatform(ctx, id, attempt, "Cancellation acknowledged at a step boundary; completed effects were not rolled back.")
	}
	if errors.Is(err, engine.ErrPermanent) {
		// A retry cannot fix a permanent step failure: fail the operation now
		// and keep the reason, instead of letting River retry it silently.
		message := err.Error()
		if w.service.redaction != nil {
			message = w.service.redaction.Redact(message)
		}
		return w.service.failPlatformOperation(ctx, id, attempt, contract.PreconditionFailed, message)
	}
	fault := contract.Classify(err)
	if reason, ok := IsPlacementViolation(err); ok {
		// A refusal is also an event, so an agent reading the operation sees the
		// machine-readable reason without parsing the message.
		w.service.appendWorkerEvent(ctx, id, attempt, "placement_violation", fault.Message+" [reason="+reason+"]")
	}
	switch fault.Code {
	case contract.Unavailable:
		return err
	case contract.Cancelled:
		return w.service.cancelPlatform(ctx, id, attempt, fault.Message)
	case contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound, contract.Forbidden, contract.Conflict:
		if failErr := w.service.failPlatformOperation(ctx, id, attempt, fault.Code, fault.Message); failErr != nil {
			return failErr
		}
		return nil
	default:
		return err
	}
}

// lockTarget serializes every operation for one canonical target across control
// plane processes. It includes extension actions and node targets, not just
// environment built-ins. A session lock is released automatically if the worker
// process dies, so it cannot wedge a target permanently.
func (s *Service) lockTarget(ctx context.Context, target string) (func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, unavailable()
	}
	sum := sha256.Sum256([]byte(target))
	key := int64(binary.BigEndian.Uint64(sum[:8]))
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable()
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}, nil
}

func (w *platformWorker) runRecordAction(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	return w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		switch input := normalized.Input.(type) {
		case *platform.AppCreateInput:
			if err := insertPlatformChange(ctx, tx, op, "app.created", input.Name, "",
				"Created application "+input.Name, map[string]any{"app": input.Name}, nil); err != nil {
				return err
			}
			for index := range input.Environments {
				if err := recordRevisionChange(ctx, tx, op, "environment.created", input.Name, input.Environments[index].Name,
					"Created environment "+input.Environments[index].Name+" and its first desired-state revision."); err != nil {
					return err
				}
			}
			return nil
		case *platform.EnvironmentProtectInput:
			kind := "environment.protected"
			if !input.Protected {
				kind = "environment.unprotected"
			}
			return insertPlatformChange(ctx, tx, op, kind, input.App, input.Environment,
				"Changed environment protection.", map[string]any{"protected": input.Protected}, nil)
		case *platform.EnvironmentCreateInput:
			return recordRevisionChange(ctx, tx, op, "environment.created", input.App, input.Name,
				"Created environment "+input.Name+" on node "+input.Node+" with its first desired-state revision.")
		case *platform.EnvironmentUpdateInput:
			hasRevision, err := revisionCreatedByOperation(ctx, tx, op.ID)
			if err != nil {
				return err
			}
			if !hasRevision {
				if !input.RotateWebhookSecret {
					return contract.NewFault(contract.Internal, "environment update has neither a revision nor a webhook rotation", "Report the command and binary version.")
				}
				return insertPlatformChange(ctx, tx, op, "environment.webhook.rotated", input.App, input.Environment,
					"Rotated the push-to-deploy webhook secret; desired state is unchanged.",
					map[string]any{"app": input.App, "environment": input.Environment}, nil)
			}
			if input.RotateWebhookSecret {
				if err := insertPlatformChange(ctx, tx, op, "environment.webhook.rotated", input.App, input.Environment,
					"Rotated the push-to-deploy webhook secret with the desired-state update.",
					map[string]any{"app": input.App, "environment": input.Environment}, nil); err != nil {
					return err
				}
			}
			return recordRevisionChange(ctx, tx, op, "environment.updated", input.App, input.Environment,
				"Updated environment "+input.Environment+" desired state as a new revision.")
		case *platform.ProcessScaleInput:
			return recordRevisionChange(ctx, tx, op, "process.scaled", input.App, input.Environment,
				"Scaled process "+input.Process+" in a new revision.")
		case *platform.ConfigSetInput:
			return recordRevisionChange(ctx, tx, op, "config.set", input.App, input.Environment,
				"Set plain configuration in a new revision; no deploy was queued.")
		case *platform.ConfigUnsetInput:
			return recordRevisionChange(ctx, tx, op, "config.unset", input.App, input.Environment,
				"Removed plain configuration in a new revision; no deploy was queued.")
		case *platform.EnvironmentSecretAttachInput:
			return recordRevisionChange(ctx, tx, op, "environment.secret.attached", input.App, input.Environment,
				"Attached vault secret name "+input.Name+" to the environment revision.")
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
	})
}

func recordRevisionChange(ctx context.Context, tx pgx.Tx, op *contract.Operation, kind, app, environment, summary string) error {
	var revisionID string
	if err := tx.QueryRow(ctx, `SELECT coalesce(e.current_revision_id,'')
        FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name=$1 AND e.name=$2`, app, environment).Scan(&revisionID); err != nil {
		return err
	}
	var number int
	if revisionID != "" {
		_ = tx.QueryRow(ctx, `SELECT number FROM eregion_platform_environment_revisions WHERE id=$1`, revisionID).Scan(&number)
	}
	desired := map[string]any{"revision_id": revisionID}
	if number > 0 {
		desired["revision_number"] = number
	}
	return insertPlatformChange(ctx, tx, op, kind, app, environment, summary, desired, nil)
}

// revisionCreatedByOperation distinguishes a combined desired-state update
// from an admitted rotation-only operation, whose semantics do not fabricate an
// unchanged environment revision.
func revisionCreatedByOperation(ctx context.Context, tx pgx.Tx, id contract.OperationID) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func (w *platformWorker) runNodeCheck(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.NodeCheckInput)
	rec, err := loadNodeConn(ctx, w.service.pool, input.Node)
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
		w.service.recordNodeCheck(ctx, input.Node, checkStateFor(err), contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, "platform-node-01-v1", platform.ArgvPayload{
		Step: "platform-node-01-v1", Argv: []string{"docker", "version"}, TimeoutSeconds: 30,
	}, nil); err != nil {
		w.service.recordNodeCheck(ctx, input.Node, checkStateFor(err), contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, "platform-node-02-v1", platform.ArgvPayload{
		Step: "platform-node-02-v1", Argv: []string{"pack", "version"}, AllowFailure: true, TimeoutSeconds: 30,
	}, nil); err != nil {
		w.service.recordNodeCheck(ctx, input.Node, checkStateFor(err), contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes SET status='ready',last_seen_at=clock_timestamp(),last_check_at=clock_timestamp(),
            last_check_state='ok',last_check_detail='docker reachable (declared facts, not enrolled)',updated_at=clock_timestamp() WHERE name=$1`, input.Node)
		return err
	}); err != nil {
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
func (w *platformWorker) observeHealth(ctx context.Context, id contract.OperationID, attempt uint32, step, target string, config platform.HealthConfig, result json.RawMessage, probeErr error) {
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
	message := fmt.Sprintf("Health observation for %s: passed=%t checks=%d deadline=%ds", target, observation.Passed, observation.Checks, config.TimeoutSeconds)
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

func (s *Service) releaseForOperation(ctx context.Context, operationID contract.OperationID) (platform.Release, error) {
	var releaseID platform.ReleaseID
	err := s.pool.QueryRow(ctx, `SELECT id FROM eregion_platform_releases WHERE operation_id=$1`, operationID).Scan(&releaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Release{}, contract.NewFault(contract.NotFound, "release record is missing", "Do not retry an operation that has no durable release row.")
	}
	if err != nil {
		return platform.Release{}, unavailable()
	}
	return s.releaseByID(ctx, releaseID)
}

func (w *platformWorker) runExtension(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized, executor platform.Executor) error {
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return err
	}
	result, err := executor.Execute(ctx, platform.Execution{
		OperationID: id, Attempt: attempt, Kind: normalized.Kind, Target: normalized.Target.String(),
		Actor: op.Actor, Input: normalized.Canonical,
		Report: func(event platform.Event) {
			message := strings.TrimSpace(event.Message)
			if message == "" {
				return
			}
			if len(message) > 4096 {
				message = message[:4096]
			}
			kind := "log"
			if event.Level == "warning" {
				kind = "warning"
			}
			_, _, _ = w.service.mutateOwned(context.WithoutCancel(ctx), id, attempt, func(tx pgx.Tx, current *contract.Operation) error {
				return appendEvent(ctx, tx, current, contract.Event{Kind: kind, Stage: event.Stage, Message: message})
			})
		},
	})
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = normalized.Summary + " completed."
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		return nil
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	// finishPlatformOperation appends a generic success event; record the
	// executor's redacted summary as a separate observation.
	_, _, _ = w.service.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		return appendEvent(ctx, tx, current, contract.Event{Kind: "log", Message: message})
	})
	return nil
}

func (s *Service) operationSnapshot(ctx context.Context, id contract.OperationID) (contract.Operation, json.RawMessage, error) {
	op, err := scanOperation(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return op, nil, unavailable()
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, id).Scan(&raw); err != nil {
		return op, nil, unavailable()
	}
	return op, raw, nil
}

// checkNodeIdentity requires the authenticated transport identity to match the
// node record. Registration alone is never proof of identity.
func (s *Service) checkNodeIdentity(n platform.Node) error {
	identity, known := s.transportIdentity()
	if !known {
		if s.transport != nil {
			return contract.NewFault(contract.PreconditionFailed, "node transport identity is not pinned", "Configure the transport's expected node name before checking a node.")
		}
		return nil
	}
	if identity != n.Name {
		return contract.NewFault(contract.PreconditionFailed, "node identity does not match the authenticated transport", fmt.Sprintf("The transport identifies %q, not %q. Hostnames are not authorization.", identity, n.Name))
	}
	return nil
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

func (s *Service) finishPlatformOperation(ctx context.Context, id contract.OperationID, attempt uint32, apply func(pgx.Tx, *contract.Operation) error) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if err := apply(tx, op); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Succeeded, "", "Platform action succeeded; domain state and history are durable.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) failPlatformOperation(ctx context.Context, id contract.OperationID, attempt uint32, code contract.Code, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
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
func (s *Service) failPlatformOperationTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, code contract.Code, message string) error {
	if len(message) > 4096 {
		message = message[:4096]
	}
	if op.State.Terminal() {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_builds SET state='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
        WHERE operation_id=$1 AND state IN ('pending','running')`, op.ID, string(code), message); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET status='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
        WHERE operation_id=$1 AND status='pending'`, op.ID, string(code), message); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
        SET state='failed',error_code=$2,message=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp()
        WHERE operation_id=$1 AND state IN ('pending','running')`, op.ID, string(code), message); err != nil {
		return err
	}
	var action, app, environment, releaseID, previousID, servingID string
	if err := tx.QueryRow(ctx, `SELECT d.action,a.name,e.name,d.release_id,coalesce(d.previous_release_id,''),coalesce(e.serving_release_id,'')
        FROM eregion_platform_deployments d
        JOIN eregion_platform_apps a ON a.id=d.app_id
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        WHERE d.operation_id=$1`, op.ID).Scan(&action, &app, &environment, &releaseID, &previousID, &servingID); err == nil {
		if err := appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: releaseID, ServingReleaseID: servingID,
			Message: deploymentWord(op.Kind) + " failed; failed release " + releaseID + " is not recorded as serving" + servingSuffix(servingID) + ".",
		}); err != nil {
			return err
		}
		unchanged := servingID == "" || servingID == previousID
		if changeErr := insertPlatformChange(ctx, tx, op, deploymentChangeKind(op.Kind, platform.DeploymentAction(action), "failed"), app, environment,
			deploymentWord(op.Kind)+" failed: "+message,
			map[string]any{"release_id": releaseID},
			map[string]any{"error_code": code, "serving_release_id": servingID, "serving_unchanged": unchanged}); changeErr != nil {
			return changeErr
		}
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
	return transition(ctx, tx, op, contract.Failed, code, message)
}

func servingSuffix(releaseID string) string {
	if releaseID == "" {
		return ""
	}
	return "; release " + releaseID + " still serves"
}

func (s *Service) cancelPlatform(ctx context.Context, id contract.OperationID, attempt uint32, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_builds
            SET state='cancelled',error_code=$2,error=$3,completed_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, string(contract.Cancelled), message); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases
            SET status='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
            WHERE operation_id=$1 AND status='pending'`, id, string(contract.Cancelled), message); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='cancelled',message=$2,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, message); err != nil {
			return err
		}
		if err := settleResourceForTerminalOperationTx(ctx, tx, id, op.Kind, contract.Cancelled); err != nil {
			return err
		}
		var releaseID, servingID string
		if err := tx.QueryRow(ctx, `SELECT coalesce(d.release_id,''),coalesce(e.serving_release_id,'')
            FROM eregion_platform_deployments d
            JOIN eregion_platform_environments e ON e.id=d.environment_id
            WHERE d.operation_id=$1`, id).Scan(&releaseID, &servingID); err == nil {
			if err := appendEvent(ctx, tx, op, contract.Event{
				Kind: "log", Stage: "deploy", ReleaseID: releaseID, ServingReleaseID: servingID,
				Message: "Deployment cancelled before a verified switch" + servingSuffix(servingID) + ".",
			}); err != nil {
				return err
			}
		}
		if err := markIncompleteSteps(ctx, tx, id, contract.Cancelled, message); err != nil {
			return err
		}
		if err := releaseRestoredDeploymentOwnerTx(ctx, tx, id); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Canceled, "", message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
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

func (s *Service) newNodeEventStream(ctx context.Context, id contract.OperationID, attempt uint32) *nodeEventStream {
	stream := &nodeEventStream{service: s, ctx: ctx, id: id, attempt: attempt, done: make(chan struct{})}
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
	if err := stream.service.appendNodeEvents(stream.ctx, stream.id, stream.attempt, events); err != nil {
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
		return 0, nodeEffectIdentityFault("the failed node task identity does not match its operation, attempt and step")
	}
	return response.Attempt, nil
}

func nodeEffectIdentityFault(message string) error {
	return contract.NewFault(contract.Unavailable,
		"node failure identity is missing or ambiguous: "+message,
		"Inspect the node journal; do not dispatch another effect under an ambiguous execution identity.")
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
	return contract.NewFault(contract.Unavailable,
		"the node effect wait ended before a safe boundary",
		"Retry the operation; a detached node task may still be running and status will reconcile it before dispatch.")
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
// earlier task may still be live. Status is always observed before Run. A live
// running task is polled, a pre-Check/Apply deferral is backed off, and a
// durable cancellation stops new dispatch. The caller owns the database step
// row, so beginStep happens once outside this loop.
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
		switch response.Status {
		case engine.StatusCompleted:
			l.observe(response)
			if err := l.Completed(ctx, response, true); err != nil {
				return response, err
			}
			if err := l.postCompletionCancellation(ctx); err != nil {
				return response, err
			}
			return response, nil
		case engine.StatusFailed:
			l.observe(response)
			if response.Retryable {
				// A retryable failure may only be redispatched when it belongs
				// to an older execution attempt. A failure from the current
				// River attempt returns to River; the next attempt owns the
				// retry budget. Missing or mismatched identities fail closed.
				if cancelErr != nil {
					return response, cancelErr
				}
				observedAttempt, identityErr := l.observedFailureAttempt(response)
				if identityErr != nil {
					return response, identityErr
				}
				switch {
				case observedAttempt == l.Current.Attempt:
					return response, l.currentAttemptFailure(ctx, response)
				case observedAttempt > l.Current.Attempt:
					return response, nodeEffectIdentityFault("the failed node task belongs to a future attempt")
				case retriedPriorFailure:
					return response, contract.NewFault(contract.Unavailable,
						"a prior node attempt's retryable failure was already retried in this attempt",
						"Return the current attempt to River instead of dispatching another effect.")
				default:
					retriedPriorFailure = true
					if err := l.wait(ctx, policy.RetryBackoff); err != nil {
						return response, nodeEffectWaitInterrupted()
					}
					done, out, err := l.dispatch(ctx)
					if done {
						return out, err
					}
					continue
				}
			}
			failErr := l.Failed(ctx, response)
			if failErr != nil {
				return response, failErr
			}
			if err := l.postCompletionCancellation(ctx); err != nil {
				return response, err
			}
			return response, nil
		case engine.StatusRunning:
			l.observe(response)
			if err := l.wait(ctx, policy.StatusPoll); err != nil {
				return response, nodeEffectWaitInterrupted()
			}
		case engine.StatusDeferred:
			l.observe(response)
			if cancelErr != nil {
				// No task holds the effect lock and no Check/Apply ran.
				return response, cancelErr
			}
			if err := l.wait(ctx, policy.DeferredBackoff); err != nil {
				return response, nodeEffectWaitInterrupted()
			}
			done, out, err := l.dispatch(ctx)
			if done {
				return out, err
			}
		case engine.StatusAbsent, engine.StatusStale:
			l.observe(response)
			if cancelErr != nil {
				return response, cancelErr
			}
			if err := l.wait(ctx, policy.RetryBackoff); err != nil {
				return response, nodeEffectWaitInterrupted()
			}
			done, out, err := l.dispatch(ctx)
			if done {
				return out, err
			}
		default:
			return response, fmt.Errorf("node returned unexpected status %q", response.Status)
		}
	}
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
	switch response.Status {
	case engine.StatusCompleted:
		if err := l.Completed(ctx, response, false); err != nil {
			return true, response, err
		}
		if cancelErr != nil {
			return true, response, cancelErr
		}
		return true, response, nil
	case engine.StatusFailed:
		if response.Retryable {
			observedAttempt, identityErr := l.observedFailureAttempt(response)
			if identityErr != nil {
				return true, response, identityErr
			}
			if observedAttempt != l.Current.Attempt {
				return true, response, nodeEffectIdentityFault("the dispatched failure does not belong to the current attempt")
			}
		}
		failErr := l.Failed(ctx, response)
		if failErr != nil {
			return true, response, failErr
		}
		if cancelErr != nil {
			return true, response, cancelErr
		}
		return true, response, nil
	case engine.StatusRunning:
		if err := l.wait(ctx, l.Policy.withDefaults().StatusPoll); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
		return false, response, nil
	case engine.StatusDeferred:
		if cancelErr != nil {
			return true, response, cancelErr
		}
		return false, response, nil
	case engine.StatusAbsent, engine.StatusStale:
		if cancelErr != nil {
			return true, response, cancelErr
		}
		if err := l.wait(ctx, l.Policy.withDefaults().RetryBackoff); err != nil {
			return true, response, nodeEffectWaitInterrupted()
		}
		return false, response, nil
	default:
		return true, response, fmt.Errorf("node returned unexpected status %q", response.Status)
	}
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
	return stepID == platform.DiagnosticCleanupStep ||
		strings.HasPrefix(stepID, deploymentCleanupStepPrefix)
}

// runStep mirrors the diagnostic worker's durable effect protocol for a generic
// platform payload. Database acknowledgement is attempt-fenced; node status is
// always consulted before a retry dispatches another effect.
func (w *platformWorker) runStepOn(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, stepID string, payload any, secrets map[string]string) (json.RawMessage, error) {
	if tr == nil {
		return nil, contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	started := time.Now()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	exec := engine.Execution{OperationID: id, Attempt: attempt, Step: stepID, RequestID: newRequestID()}
	step, err := node.DefaultRegistry().Build(exec, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid platform step: %v", engine.ErrPermanent, err)
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
			return nil, contract.NewFault(contract.PreconditionFailed, "step inputs changed for an existing effect", "Never reuse an operation ID with different inputs; inspect the node journal.")
		}
		if record.State == "completed" {
			return record.Result, nil
		}
	}
	rowExists := hasRecord
	begunThisAttempt := hasRecord && record.Attempt == attempt && record.ExecutionID == exec.ExecutionID()
	ensureRow := func(ctx context.Context, executionID string) error {
		if rowExists {
			return nil
		}
		already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, executionID)
		if err != nil {
			return err
		}
		rowExists = true
		if already {
			return errNodeEffectAlreadyCompleted
		}
		return nil
	}
	ensureDispatchBegun := func(ctx context.Context) error {
		if begunThisAttempt {
			return nil
		}
		already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, exec.ExecutionID())
		if err != nil {
			return err
		}
		rowExists = true
		begunThisAttempt = true
		if already {
			return errNodeEffectAlreadyCompleted
		}
		return nil
	}

	status := func(ctx context.Context) (node.Response, error) {
		response, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
		if err != nil {
			if node.IsCallCancelled(err) {
				return node.Response{}, nodeEffectWaitInterrupted()
			}
			if errors.Is(err, engine.ErrJournalCorrupt) {
				return node.Response{}, contract.NewFault(contract.PreconditionFailed, "node journal is corrupt", "Inspect the node journal before retrying.")
			}
			if node.IsPermanentTransport(err) {
				return node.Response{}, contract.NewFault(contract.PreconditionFailed, "the node transport refused the request", "Check the pinned node identity and host configuration.")
			}
			return node.Response{}, fmt.Errorf("%w: node status %s: %w", errNodeEffectOutcomeUnknown, stepID, err)
		}
		if response.InputsHash != "" && response.InputsHash != inputsHash {
			return node.Response{}, contract.NewFault(contract.PreconditionFailed, "node effect has different inputs", "Do not acknowledge an effect recorded under other inputs.")
		}
		if stepID == deploymentCommandStep && response.InputsHash == "" && (response.Status == engine.StatusCompleted || response.Status == engine.StatusFailed) {
			return node.Response{}, contract.NewFault(contract.Unavailable, "release command status has no effect evidence", "Recover the original node effect before rerunning the release command.")
		}
		return response, nil
	}
	dispatch := func(ctx context.Context) (node.Response, error) {
		w.service.registerSecretValues(secrets)
		events := w.service.newNodeEventStream(ctx, id, attempt)
		response, err := tr.Call(ctx, node.Request{
			Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec, Payload: raw, Secrets: secrets,
		}, events.add)
		if appendErr := events.close(); appendErr != nil && err == nil {
			err = appendErr
		}
		if err != nil {
			if node.IsCallCancelled(err) {
				return node.Response{}, nodeEffectWaitInterrupted()
			}
			if node.IsPermanentTransport(err) {
				return node.Response{}, contract.NewFault(contract.PreconditionFailed, "the node transport refused the request", "Check the pinned node identity and host configuration.")
			}
			// Unknown outcome: the next observation asks status first and
			// never starts a second effect under the same key.
			return node.Response{}, fmt.Errorf("%w: node run %s: %w", errNodeEffectOutcomeUnknown, stepID, err)
		}
		if response.InputsHash != "" && response.InputsHash != inputsHash {
			return node.Response{}, contract.NewFault(contract.PreconditionFailed, "node effect has different inputs", "Do not acknowledge an effect recorded under other inputs.")
		}
		if stepID == deploymentCommandStep && response.InputsHash == "" {
			return node.Response{}, contract.NewFault(contract.Unavailable, "release command response has no effect evidence", "Recover the original node effect before rerunning the release command.")
		}
		return response, nil
	}
	completed := func(ctx context.Context, response node.Response, recovered bool) error {
		if err := ensureRow(ctx, response.ExecutionID); err != nil {
			return err
		}
		if recovered {
			if err := w.service.recordRecoveredStep(ctx, id, attempt, stepID, inputsHash, response); err != nil {
				return err
			}
		}
		if _, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, response.ExecutionID, response.Result, platformStepTotal); err != nil {
			return err
		}
		if !recovered {
			duration := time.Since(started).Milliseconds()
			w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
				Kind: "log", Stage: stepID, Step: stepID, DurationMS: &duration,
				Message: fmt.Sprintf("Node step %s completed in %dms.", stepID, duration),
			})
		}
		return nil
	}
	failed := func(ctx context.Context, response node.Response) error {
		code := contract.Code(response.ErrorCode)
		if code == "" {
			code = contract.Internal
		}
		detail := response.Detail
		if detail == "" {
			detail = "node step failed"
		}
		if failErr := w.service.failStep(ctx, id, attempt, stepID, code, detail); failErr != nil {
			return failErr
		}
		if code == contract.Conflict {
			// Preserve the machine-readable refusal so a caller that can
			// classify it (retention) records a per-item skip instead of a
			// generic permanent failure.
			return contract.NewFault(contract.Conflict, "node step "+stepID+" conflicted: "+detail, "Inspect the node state; a deterministic conflict is not fixed by retrying.")
		}
		if response.Retryable {
			return fmt.Errorf("node step %s failed: %s", stepID, detail)
		}
		return errors.Join(errNodeStepPermanentFailure,
			contract.NewFault(contract.PreconditionFailed, "node step failed: "+detail, "Inspect operation events and node state before resubmitting."))
	}

	policy := nodeEffectPolicy{}.withDefaults()
	lastObservation := time.Time{}
	lastObservationKey := ""
	observe := func(response node.Response) {
		key := string(response.Status) + "/" + response.ExecutionID + "/" + string(response.Phase)
		observedAt := time.Now()
		if key == lastObservationKey && observedAt.Sub(lastObservation) < policy.ObservationEvery {
			return
		}
		lastObservation, lastObservationKey = observedAt, key
		message := fmt.Sprintf("Node step %s is %s", stepID, response.Status)
		if response.Phase != "" {
			message += " (phase=" + string(response.Phase)
			if response.WaitMS > 0 {
				message += fmt.Sprintf(", waited=%dms", response.WaitMS)
			}
			message += ")"
		} else if response.WaitMS > 0 {
			message += fmt.Sprintf(" (waited=%dms)", response.WaitMS)
		}
		w.service.appendWorkerEvent(ctx, id, attempt, stepID, message+".")
	}

	loop := nodeEffectLoop{
		Status:    status,
		Dispatch:  dispatch,
		Begin:     ensureDispatchBegun,
		Completed: completed,
		Failed:    failed,
		Observe:   observe,
		Policy:    policy,
		Current:   exec,
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
	if op.Kind == platform.KindDiagnosticExecution {
		return s.requeueDiagnosticCleanup(ctx, op.ID)
	}
	if op.Kind == platform.KindDeploy || op.Kind == platform.KindRollback || op.Kind == platform.KindRestart || op.Kind == platform.KindPromote {
		return s.reconcileDeployment(ctx, op)
	}
	if op.State == contract.Cancelling {
		return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled after the River job ended.")
	}
	switch op.Kind {
	case platform.KindBuildRetry:
		release, err := s.releaseForOperation(ctx, op.ID)
		if err != nil {
			if fault := contract.Classify(err); fault.Code == contract.NotFound {
				return s.failReconciledPlatform(ctx, op, "release execution ended before a durable release was ready", contract.Unavailable)
			}
			return err
		}
		if release.Status == platform.ReleaseReady {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		return s.failReconciledPlatform(ctx, op, "release execution ended before the image was verified or built", contract.Unavailable)
	case platform.KindBuildStop:
		var raw []byte
		if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", op.ID).Scan(&raw); err != nil {
			return unavailable()
		}
		normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
		if err != nil {
			return err
		}
		input := normalized.Input.(*platform.BuildStopInput)
		build, err := s.buildByID(ctx, input.BuildID)
		if err != nil {
			return err
		}
		if build.State == platform.BuildCancelled {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		return s.failReconciledPlatform(ctx, op, "build stop ended before the build was recorded as cancelled", contract.Unavailable)
	case platform.KindNodeCheck, platform.KindNodeReconcile:
		return s.failReconciledPlatform(ctx, op, "the node action ended before it recorded a result; it is idempotent, so submit it again", contract.Unavailable)
	case platform.KindNodeEnroll:
		// Enrollment is durable only once the worker recorded it. A job that ended
		// first leaves the node pending (or failed) for an enrollment retry; the
		// operation never claims verification it did not see.
		var state string
		var enrollOp *string
		_ = s.pool.QueryRow(ctx, `SELECT enrollment_state,enroll_operation_id FROM eregion_platform_nodes WHERE name=(SELECT split_part($1,'/',2))`, op.Target).Scan(&state, &enrollOp)
		if state == string(platform.EnrollmentEnrolled) && enrollOp != nil && *enrollOp == string(op.ID) {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		return s.failReconciledPlatform(ctx, op, "enrollment ended before the node was verified; retry node enrollment", contract.Unavailable)
	case platform.KindDomainAdd, platform.KindDomainRemove:
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
		if err := s.applyDomainDNSEffect(ctx, op, op.Attempt, input, op.Kind == platform.KindDomainRemove); err != nil {
			if errors.Is(err, errPlatformCancelled) {
				return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation acknowledged after provider effects; binding and cleanup ledger were retained.")
			}
			fault := contract.Classify(err)
			return s.failReconciledPlatform(ctx, op, err.Error(), fault.Code)
		}
		if err := s.checkDomainDNSBoundary(ctx, op.ID, op.Attempt); err != nil {
			if errors.Is(err, errPlatformCancelled) {
				return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation acknowledged before binding deletion; cleanup state was retained.")
			}
			if errors.Is(err, errNotOwned) {
				return s.waitForNewerAttempt(ctx, op.ID, op.Attempt)
			}
			return err
		}
		if op.Kind == platform.KindDomainRemove {
			if err := s.deleteDomainBindingRowOwned(ctx, op.ID, op.Attempt, input.App, input.Environment, input.Domain); err != nil {
				if errors.Is(err, errPlatformCancelled) {
					return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation acknowledged before binding deletion; cleanup state was retained.")
				}
				if errors.Is(err, errNotOwned) {
					return s.waitForNewerAttempt(ctx, op.ID, op.Attempt)
				}
				return s.failReconciledPlatform(ctx, op, err.Error(), contract.Unavailable)
			}
		}
		if err := s.syncRoutes(ctx); err != nil {
			return s.failReconciledPlatform(ctx, op, err.Error(), contract.Unavailable)
		}
		if err := s.finishDomainOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil }); err != nil {
			if errors.Is(err, errPlatformCancelled) {
				return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation acknowledged at domain finalization; completed effects were not rolled back.")
			}
			return err
		}
		return nil
	case platform.KindAppDelete:
		var raw []byte
		if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", op.ID).Scan(&raw); err != nil {
			return unavailable()
		}
		normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
		if err != nil {
			return err
		}
		input := normalized.Input.(*platform.AppDeleteInput)
		if _, _, err := s.applicationEnvironments(ctx, input.App); err != nil {
			if contract.Classify(err).Code == contract.NotFound {
				return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
			}
			return err
		}
		return s.failReconciledPlatform(ctx, op, "application deletion ended before the platform rows were removed", contract.Unavailable)
	case platform.KindEnvironmentDelete:
		var raw []byte
		if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", op.ID).Scan(&raw); err != nil {
			return unavailable()
		}
		normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
		if err != nil {
			return err
		}
		input := normalized.Input.(*platform.EnvironmentDeleteInput)
		if _, err := s.environment(ctx, input.App, input.Environment); err != nil {
			if contract.Classify(err).Code == contract.NotFound {
				return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
			}
			return err
		}
		return s.failReconciledPlatform(ctx, op, "environment deletion ended before the platform rows were removed", contract.Unavailable)
	case platform.KindResourceApply, platform.KindResourceDestroy:
		var raw []byte
		if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", op.ID).Scan(&raw); err != nil {
			return unavailable()
		}
		_, err := platform.Default().Normalize(op.Kind, op.Target, raw)
		if err != nil {
			return err
		}
		target, err := platform.ParseTarget(op.Target)
		if err != nil {
			return err
		}
		resource, err := s.resourceByName(ctx, target.App, target.Environment, target.Resource)
		if err != nil {
			if contract.Classify(err).Code == contract.NotFound {
				return s.failReconciledPlatform(ctx, op, "resource execution ended before a durable resource record existed", contract.Unavailable)
			}
			return err
		}
		if resource.OperationID != op.ID {
			return s.failReconciledPlatform(ctx, op, "resource operation was superseded by another operation", contract.Conflict)
		}
		if op.Kind == platform.KindResourceApply && resource.State == platform.ResourceReady {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		if op.Kind == platform.KindResourceDestroy && resource.State == platform.ResourceDestroyed {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		cause := contract.NewFault(contract.Unavailable, "resource execution ended before its durable state was recorded", "Inspect resource status; the container may still exist. Re-apply or re-destroy explicitly after checking the node.")
		s.recordResourceFailure(ctx, op.ID, resource.App, resource.Environment, resource.Name, cause)
		return s.failReconciledPlatform(ctx, op, cause.Message, contract.Unavailable)
	case platform.KindAppCreate, platform.KindEnvironmentCreate, platform.KindEnvironmentUpdate,
		platform.KindProcessScale, platform.KindConfigSet, platform.KindConfigUnset,
		platform.KindEnvironmentSecretAttach, platform.KindEnvironmentResourceAttach,
		platform.KindEnvironmentResourceDetach, platform.KindNodeRegister:
		exists, err := s.recordActionExists(ctx, op)
		if err != nil {
			return err
		}
		if exists {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		return s.failReconciledPlatform(ctx, op, "execution ended before the record was durable", contract.Unavailable)
	default:
		return s.failReconciledPlatform(ctx, op, "extension execution ended without a durable operation result; extension side effects are not assumed successful", contract.Unavailable)
	}
}

func (s *Service) deploymentByID(ctx context.Context, id platform.DeploymentID) (platform.Deployment, error) {
	dep, err := scanDeploymentRow(s.pool.QueryRow(ctx, deploymentSelect+" WHERE d.id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return dep, contract.NewFault(contract.NotFound, "serving deployment is missing", "Inspect the environment serving deployment before retrying.")
	}
	if err != nil {
		return dep, unavailable(err)
	}
	return dep, nil
}

func (s *Service) deploymentByOperation(ctx context.Context, id contract.OperationID) (platform.Deployment, bool, error) {
	dep, err := scanDeploymentRow(s.pool.QueryRow(ctx, deploymentSelect+" WHERE d.operation_id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return dep, false, nil
	}
	if err != nil {
		return dep, false, unavailable()
	}
	return dep, true, nil
}

func (s *Service) recordActionExists(ctx context.Context, op contract.Operation) (bool, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", op.ID).Scan(&raw); err != nil {
		return false, unavailable()
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return false, err
	}
	var exists bool
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_apps WHERE name=$1)`, input.Name).Scan(&exists)
	case *platform.EnvironmentCreateInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2)`, input.App, input.Name).Scan(&exists)
	case *platform.EnvironmentUpdateInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
		if err == nil && !exists && input.RotateWebhookSecret {
			// Rotation-only admissions intentionally publish no new revision;
			// the accepted operation and sealed secret revision are the state.
			exists = true
		}
	case *platform.ProcessScaleInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.ConfigSetInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.ConfigUnsetInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.EnvironmentSecretAttachInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.EnvironmentResourceInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.NodeRegisterInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_nodes WHERE name=$1)`, input.Name).Scan(&exists)
	default:
		return false, nil
	}
	if err != nil {
		return false, unavailable()
	}
	return exists, nil
}

func (s *Service) failReconciledPlatform(ctx context.Context, op contract.Operation, message string, code contract.Code) error {
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
func (s *Service) deploymentNodeEffectOutstanding(ctx context.Context, dep platform.Deployment, env platform.Environment, id contract.OperationID, attempt uint32) (bool, error) {
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return true, err
	}
	var outstanding []StepRecord
	for _, record := range records {
		if record.State == "completed" || !deploymentNodeEffectStep(record.Step) {
			continue
		}
		outstanding = append(outstanding, record)
	}
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
		recordAttempt := record.Attempt
		if recordAttempt == 0 {
			recordAttempt = attempt
		}
		exec := engine.Execution{OperationID: id, Attempt: recordAttempt, Step: record.Step, RequestID: newRequestID()}
		response, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
		if err != nil {
			return true, fmt.Errorf("observe node step %s before cancellation cleanup: %w", record.Step, err)
		}
		if response.InputsHash != "" && response.InputsHash != record.InputsHash {
			return true, contract.NewFault(contract.PreconditionFailed,
				"node effect has different inputs during cancellation recovery",
				"Do not discard an effect recorded under other inputs; inspect the node journal.")
		}
		switch response.Status {
		case engine.StatusCompleted:
			if _, err := s.ackStep(ctx, id, attempt, record.Step, record.InputsHash, response.ExecutionID, response.Result, platformStepTotal); err != nil {
				return true, err
			}
		case engine.StatusRunning:
			// A live effect is not a cleanup boundary. A pre-Check/Apply
			// deferral is different: that process exited and no side effect was
			// attempted, so cancellation may proceed from that safe boundary.
			return true, nil
		case engine.StatusDeferred:
			// Exited before Check/Apply. Safe to cancel and clean up.
		case engine.StatusAbsent, engine.StatusStale, engine.StatusFailed:
			// The task is gone. Restore/cleanup recovery remains authoritative
			// for what it may have changed.
		default:
			return true, nil
		}
	}
	return false, nil
}

// ensureNoOutstandingDeploymentEffect blocks cancellation, cleanup and
// terminalization while a detached effect for the deployment may still be live.
// It uses the deployment's pinned placement, so an environment moved to another
// node cannot hide an effect on the old node.
func (w *platformWorker) ensureNoOutstandingDeploymentEffect(ctx context.Context, id contract.OperationID, attempt uint32, dep platform.Deployment, env platform.Environment, stage string) error {
	outstanding, err := w.service.deploymentNodeEffectOutstanding(ctx, dep, env, id, attempt)
	if err != nil {
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind: "warning", Stage: stage,
			Message: "Deployment effects cannot be observed on the pinned node yet: " + contract.Classify(err).Message,
		})
		return contract.NewFault(contract.Unavailable,
			"deployment node effects cannot yet be proven safe",
			"Keep the deployment owner and retry after the pinned node transport is available.")
	}
	if outstanding {
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind: "warning", Stage: stage,
			Message: "A detached node effect is still live or its outcome is unknown; route restoration and candidate cleanup were not started.",
		})
		return contract.NewFault(contract.Unavailable,
			"a detached node effect is still live or its outcome is unknown",
			"Keep the deployment owner and retry after the effect completes or exits before Check/Apply.")
	}
	return nil
}

// cancelOwnedDeployment acknowledges cancellation only through the durable
// owner. The caller holds the environment lock. A cancelled operation that never
// owned the route can end immediately; an owner must restore its exact target
// before the cancellation becomes terminal.
func (w *platformWorker) cancelOwnedDeployment(ctx context.Context, id contract.OperationID, attempt uint32) error {
	dep, has, err := w.service.deploymentByOperation(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if !has {
		return w.service.cancelPlatform(ctx, id, attempt, "Cancellation acknowledged; no deployment record exists.")
	}
	env, err := w.service.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		if isNotFound(err) {
			return w.service.cancelPlatform(ctx, id, attempt, "Cancellation acknowledged; the environment no longer exists.")
		}
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.ensureNoOutstandingDeploymentEffect(ctx, id, attempt, dep, env, "cancellation"); err != nil {
		return err
	}
	owner, acquired, err := w.service.acquireDeploymentOwner(ctx, dep, env, attempt)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if !acquired {
		settled, recoverErr := w.service.recoverStaleDeploymentOwner(ctx, owner, env)
		if recoverErr != nil {
			return w.settle(ctx, id, attempt, recoverErr)
		}
		if settled {
			if state, _, stateErr := w.service.operationState(ctx, id); stateErr == nil && state.Terminal() {
				return nil
			}
			// The settled owner may have restored a different serving
			// deployment. Re-read before freezing this owner's predecessor.
			refreshedEnv, envErr := w.service.environment(ctx, dep.App, dep.Environment)
			if envErr != nil {
				return w.finishDeploymentFailure(ctx, nil, id, attempt, env, dep, nil, envErr)
			}
			env = refreshedEnv
			owner, acquired, err = w.service.acquireDeploymentOwner(ctx, dep, env, attempt)
			if err != nil {
				return w.settle(ctx, id, attempt, err)
			}
		}
	}
	if !acquired {
		return w.service.cancelPlatform(ctx, id, attempt,
			"Cancellation acknowledged; another deployment owner controls the route and this operation made no route change.")
	}
	refreshed, refreshedHas, err := w.service.deploymentByOperation(ctx, id)
	if err != nil {
		return w.finishDeploymentFailure(ctx, nil, id, attempt, env, dep, nil, err)
	}
	if !refreshedHas {
		return w.service.cancelPlatform(ctx, id, attempt, "Cancellation acknowledged; no deployment record exists.")
	}
	dep = refreshed
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	_, err = w.service.recoverOwnedDeployment(ctx, op, dep, env, owner, false)
	return err
}

// recoverOwnedDeployment inspects durable proof and the owner's frozen restore
// target. A running route step is never proof: only a completed route step plus
// completed public verification is enough to finish success. Everything else
// that may have touched the route is restored to the exact frozen predecessor
// before candidates are cleaned or the operation is failed/cancelled.
func (s *Service) recoverOwnedDeployment(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, owner deploymentOwner, allowResume bool) (bool, error) {
	if op.State.Terminal() {
		return true, nil
	}
	if op.State == contract.Cancelling || !allowResume {
		stage := "recovery"
		if op.State == contract.Cancelling {
			stage = "cancellation"
		}
		if err := (&platformWorker{service: s}).ensureNoOutstandingDeploymentEffect(ctx, op.ID, op.Attempt, dep, env, stage); err != nil {
			return false, err
		}
	}
	routeRecord, routeHas, err := s.stepRecord(ctx, op.ID, deploymentRouteStep)
	if err != nil {
		return false, err
	}
	publicRecord, publicHas, err := s.stepRecord(ctx, op.ID, deploymentPublicStep)
	if err != nil {
		return false, err
	}
	_, restoreHas, err := s.stepRecord(ctx, op.ID, deploymentRestoreRouteStep)
	if err != nil {
		return false, err
	}
	routeCompleted := routeHas && routeRecord.State == "completed"
	publicCompleted := publicHas && publicRecord.State == "completed"
	evidence := routeHas || publicHas || restoreHas
	cancelling := op.State == contract.Cancelling

	if !cancelling && dep.State == "succeeded" && ownerAllowsSuccess(owner, dep.ID) {
		owner, err = s.setDeploymentOwnerRoute(ctx, owner, dep.ID, deploymentOwnerServing, op.Attempt)
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
			s.appendCleanupWarning(ctx, op.ID, op.Attempt, "Previous release cleanup could not be pre-recorded; ownership is retained: "+contract.Classify(err).Message)
			return false, contract.NewFault(contract.Unavailable,
				"previous release cleanup could not be recorded durably",
				"Keep the deployment owner and retry after the control database and previous deployment record are readable.")
		}
		if s.beforeDeploymentFinalize != nil {
			s.beforeDeploymentFinalize()
		}
		err := s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(_ pgx.Tx, current *contract.Operation) error {
			if current.State == contract.Cancelling {
				return errDeploymentCancelledAtFinalize
			}
			return nil
		})
		if errors.Is(err, errDeploymentCancelledAtFinalize) {
			return s.restoreAndCancelOwnedDeployment(ctx, op, dep, env, owner)
		}
		if err != nil {
			return false, err
		}
		_ = s.releaseServedDeploymentOwner(ctx, owner)
		s.removePreviousDeploymentCleanupNow(ctx, env, dep.PreviousDeploymentID)
		return true, nil
	}

	if !cancelling && ownerAllowsSuccess(owner, dep.ID) && routeCompleted && publicCompleted && !restoreHas {
		// The switch and public verification both committed. Reassert the
		// owner's candidate intent and finish the operation without an
		// election based on creation time.
		owner, err = s.setDeploymentOwnerRoute(ctx, owner, dep.ID, deploymentOwnerServing, op.Attempt)
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
			s.appendCleanupWarning(ctx, op.ID, op.Attempt, "Previous release cleanup could not be pre-recorded; ownership is retained: "+contract.Classify(err).Message)
			return false, contract.NewFault(contract.Unavailable,
				"previous release cleanup could not be recorded durably",
				"Keep the deployment owner and retry after the control database and previous deployment record are readable.")
		}
		if s.beforeDeploymentFinalize != nil {
			s.beforeDeploymentFinalize()
		}
		err := s.finishDeploymentAfterSwitch(ctx, op, dep, env)
		if errors.Is(err, errDeploymentCancelledAtFinalize) {
			return s.restoreAndCancelOwnedDeployment(ctx, op, dep, env, owner)
		}
		if err != nil {
			return false, err
		}
		_ = s.releaseServedDeploymentOwner(ctx, owner)
		s.removePreviousDeploymentCleanupNow(ctx, env, dep.PreviousDeploymentID)
		return true, nil
	}

	if !evidence && owner.Phase == deploymentOwnerPrepared {
		if op.State == contract.Cancelling {
			if err := s.persistDeploymentCleanupIntent(ctx, env, dep, "candidate"); err != nil {
				return false, deploymentCleanupIntentFault(err)
			}
			if err := s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation acknowledged before the traffic switch; the frozen serving route was unchanged and candidates were cleaned where safe."); err != nil {
				return false, err
			}
			s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
			return true, nil
		}
		if !allowResume {
			if err := s.persistDeploymentCleanupIntent(ctx, env, dep, "candidate"); err != nil {
				return false, deploymentCleanupIntentFault(err)
			}
			if err := s.failPlatformOperation(ctx, op.ID, op.Attempt, contract.Unavailable,
				"Execution ended before the traffic switch and cannot resume without a worker lease; the frozen serving route was unchanged and candidates were cleaned where safe."); err != nil {
				return false, err
			}
			s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
			return true, nil
		}
		return false, nil
	}

	if cancelling {
		return s.restoreAndCancelOwnedDeployment(ctx, op, dep, env, owner)
	}
	if owner.RestoreDeploymentID != "" && owner.RestoreDeploymentID == dep.ID {
		return false, contract.NewFault(contract.Unavailable,
			"deployment owner's restore target is the candidate deployment",
			"Ownership is retained; inspect and repair the owner before retrying, because restoring the candidate would race the live deployment.")
	}
	owner, err = s.restoreDeploymentRoute(context.WithoutCancel(ctx), op.ID, op.Attempt, env, owner)
	if err != nil {
		s.appendWorkerEventWith(ctx, op.ID, op.Attempt, contract.Event{
			Kind: "warning", Stage: "reconcile",
			Message: "The recorded serving route could not be restored; ownership and candidate containers were retained for retry: " + contract.Classify(err).Message,
		})
		return false, contract.NewFault(contract.Unavailable,
			"deployment route state is not safely restored and the operation remains owner",
			"Keep the candidate containers; retry after the route writer is available.")
	}
	if err := s.persistDeploymentCleanupIntent(ctx, env, dep, "candidate"); err != nil {
		return false, deploymentCleanupIntentFault(err)
	}
	if err := s.failPlatformOperation(ctx, op.ID, op.Attempt, contract.Unavailable,
		"Execution ended without a completed route proof; the owner restored the exact recorded serving route and cleaned candidates where safe."); err != nil {
		return false, err
	}
	s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
	return true, nil
}

func deploymentCleanupIntentFault(err error) *contract.Fault {
	return contract.NewFault(contract.Unavailable,
		"candidate cleanup intent could not be recorded durably: "+contract.Classify(err).Message,
		"Keep the deployment owner and candidate containers; retry after the control database and cleanup table are available.")
}

// handlePostSwitchRouteFailure is shared by the live attempt and recovery.
// Retryable route failures retain the serving-phase owner and operation. A
// nonretryable failure restores the frozen predecessor before terminalizing;
// if restoration or cleanup-intent persistence fails, ownership stays
// retryable and nothing is terminalized.
func (s *Service) handlePostSwitchRouteFailure(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, owner deploymentOwner, cause error) (bool, error) {
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
		return false, contract.NewFault(contract.Unavailable,
			"deployment owner's restore target is the candidate deployment",
			"Ownership is retained; inspect and repair the owner before retrying, because restoring the candidate would race the live deployment.")
	}
	if _, err := s.restoreDeploymentRoute(context.WithoutCancel(ctx), op.ID, op.Attempt, env, owner); err != nil {
		s.appendWorkerEventWith(ctx, op.ID, op.Attempt, contract.Event{
			Kind: "warning", Stage: "reconcile",
			Message: "The post-switch route failure could not restore the frozen predecessor; ownership and candidates were retained: " + contract.Classify(err).Message,
		})
		return false, contract.NewFault(contract.Unavailable,
			"post-switch route failure could not restore the previous serving route",
			"Keep the candidate containers and owner; retry after the route writer is available.")
	}
	if err := s.persistDeploymentCleanupIntent(ctx, env, dep, "candidate"); err != nil {
		return false, deploymentCleanupIntentFault(err)
	}
	if err := s.checkCancelled(ctx, op.ID); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			if err := s.cancelPlatform(ctx, op.ID, op.Attempt,
				"Cancellation acknowledged after the post-switch failure restored the frozen predecessor; candidates were cleaned where safe."); err != nil {
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
		if err := s.cancelPlatform(ctx, op.ID, op.Attempt,
			"Post-switch route failure cancelled after restoring the frozen predecessor."); err != nil {
			return false, err
		}
		s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
		return true, nil
	}
	if err := s.failPlatformOperation(ctx, op.ID, op.Attempt, code,
		"Post-switch route failure; the frozen predecessor was restored before terminal failure: "+fault.Message); err != nil {
		return false, err
	}
	s.removeDeploymentCleanupRowsNow(context.WithoutCancel(ctx), env, dep.ID)
	return true, nil
}

// restoreAndCancelOwnedDeployment is the single cancellation tail for a
// verifying or verified owner. It restores the exact frozen target before the
// cancellation becomes terminal.
func (s *Service) restoreAndCancelOwnedDeployment(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, owner deploymentOwner) (bool, error) {
	if owner.RestoreDeploymentID != "" && owner.RestoreDeploymentID == dep.ID {
		return false, contract.NewFault(contract.Unavailable,
			"deployment owner's restore target is the candidate deployment",
			"Ownership is retained; inspect and repair the owner before retrying, because restoring the candidate would race the live deployment.")
	}
	owner, err := s.restoreDeploymentRoute(context.WithoutCancel(ctx), op.ID, op.Attempt, env, owner)
	if err != nil {
		s.appendWorkerEventWith(ctx, op.ID, op.Attempt, contract.Event{
			Kind: "warning", Stage: "reconcile",
			Message: "Cancellation is waiting because the exact serving route could not be restored; ownership and candidate containers were retained: " + contract.Classify(err).Message,
		})
		return false, contract.NewFault(contract.Unavailable,
			"deployment route state is not safely restored and the operation remains owner",
			"Keep the candidate containers; retry after the route writer is available.")
	}
	if err := s.persistDeploymentCleanupIntent(ctx, env, dep, "candidate"); err != nil {
		return false, deploymentCleanupIntentFault(err)
	}
	if err := s.cancelPlatform(ctx, op.ID, op.Attempt,
		"Cancellation acknowledged after the owner restored the exact recorded serving route; candidates were cleaned where safe."); err != nil {
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
		return s.failReconciledPlatform(ctx, op, "deployment record is missing", contract.Unavailable)
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
	owner, acquired, err := s.acquireDeploymentOwner(ctx, dep, env, op.Attempt)
	if err != nil {
		return err
	}
	if !acquired {
		settled, err := s.recoverStaleDeploymentOwner(ctx, owner, env)
		if err != nil {
			return err
		}
		if settled {
			if state, _, stateErr := s.operationState(ctx, op.ID); stateErr == nil && state.Terminal() {
				return nil
			}
			// The settled owner may have restored a different serving
			// deployment. Re-read before freezing this owner's predecessor.
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
	}
	refreshed, refreshedHas, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if !refreshedHas {
		return s.failReconciledPlatform(ctx, op, "deployment record is missing", contract.Unavailable)
	}
	dep = refreshed
	_, err = s.recoverOwnedDeployment(ctx, op, dep, env, owner, false)
	return err
}

func (s *Service) completedStep(ctx context.Context, id contract.OperationID, step string) bool {
	record, ok, err := s.stepRecord(ctx, id, step)
	return err == nil && ok && record.State == "completed"
}

func (s *Service) hasStepPrefix(ctx context.Context, id contract.OperationID, prefix string) (bool, error) {
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

func (s *Service) finishDeploymentAfterSwitch(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment) error {
	return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State == contract.Cancelling {
			return errDeploymentCancelledAtFinalize
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='succeeded',completed_at=$2,updated_at=$2,message=$3,error_code=''
            WHERE operation_id=$1 AND state IN ('pending','running')`, op.ID, now,
			"Serving release "+string(dep.ReleaseID)+" passed health verification."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,serving_deployment_id=$3,applied_release_id=$2,health_state='healthy',health_detail='serving release passed health verification',health_checked_at=$4,updated_at=$4
            WHERE id=$1`, env.ID, dep.ReleaseID, dep.ID, now); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, current, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(dep.ReleaseID), ServingReleaseID: string(dep.ReleaseID),
			Message: deploymentWord(current.Kind) + " completed after the switch and verification; serving release " + string(dep.ReleaseID) + ".",
		}); err != nil {
			return err
		}
		if err := insertPlatformChange(ctx, tx, current, deploymentChangeKind(current.Kind, dep.Action, "succeeded"), dep.App, dep.Environment,
			"Serving release "+string(dep.ReleaseID)+" for "+dep.App+"/"+dep.Environment+".",
			map[string]any{"release_id": dep.ReleaseID},
			map[string]any{"serving_release_id": dep.ReleaseID, "health": "healthy"}); err != nil {
			return err
		}
		return releaseServedDeploymentOwnerTx(ctx, tx, op.ID)
	})
}

func depEnvironmentID(ctx context.Context, tx pgx.Tx, dep platform.Deployment) string {
	var id string
	_ = tx.QueryRow(ctx, `SELECT e.id FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2`, dep.App, dep.Environment).Scan(&id)
	return id
}

// deploymentChangeKind names the change history row for a deployment-family
// operation. Promotion reuses the deploy runtime path but keeps its own
// history kind, so an operator can tell a rebuilt deploy from an exact
// cross-environment artifact move.
func deploymentChangeKind(operationKind string, action platform.DeploymentAction, outcome string) string {
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
func insertPlatformChange(ctx context.Context, tx pgx.Tx, op *contract.Operation, kind, app, environment, summary string, desired, applied any) error {
	var appID string
	if err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name=$1`, app).Scan(&appID); err != nil {
		return err
	}
	var envID any
	if environment != "" {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_environments WHERE app_id=$1 AND name=$2`, appID, environment).Scan(&id); err != nil {
			return err
		}
		envID = id
	}
	var sequence int64
	if envID == nil {
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0)+1 FROM eregion_platform_changes WHERE app_id=$1 AND environment_id IS NULL`, appID).Scan(&sequence); err != nil {
			return err
		}
	} else {
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0)+1 FROM eregion_platform_changes WHERE environment_id=$1`, envID).Scan(&sequence); err != nil {
			return err
		}
	}
	changeID, err := platform.NewChangeID()
	if err != nil {
		return err
	}
	desiredJSON, _ := json.Marshal(desired)
	appliedJSON, _ := json.Marshal(applied)
	_, err = tx.Exec(ctx, `INSERT INTO eregion_platform_changes
        (id,app_id,environment_id,sequence,kind,operation_id,actor_kind,actor_subject,summary,desired,applied)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
        ON CONFLICT (operation_id,kind) DO NOTHING`,
		changeID, appID, envID, sequence, kind, op.ID, op.Actor.Kind, op.Actor.Subject, summary,
		nullableJSON(desiredJSON), nullableJSON(appliedJSON))
	return err
}

// appendWorkerEvent records a best-effort observation under the attempt fence.
func (s *Service) appendWorkerEvent(ctx context.Context, id contract.OperationID, attempt uint32, stage, message string) {
	s.appendWorkerEventWith(ctx, id, attempt, contract.Event{Kind: "log", Stage: stage, Message: message})
}

// appendCleanupWarning records that container cleanup did not complete. It is
// a warning, so a viewer cannot report the cleanup as completed because a
// later removal step succeeded. The message can carry the node's removal
// reason, so the worker's own secrets are redacted before it is stored.
func (s *Service) appendCleanupWarning(ctx context.Context, id contract.OperationID, attempt uint32, message string) {
	s.appendWorkerEventWith(ctx, id, attempt, contract.Event{Kind: "warning", Stage: "cleanup", Message: s.redact(message)})
}

// appendWorkerEventWith records one structured observation. It is best effort:
// a terminal operation, or one owned by a newer attempt, is never rewritten.
func (s *Service) appendWorkerEventWith(ctx context.Context, id contract.OperationID, attempt uint32, event contract.Event) {
	if event.Kind == "" {
		event.Kind = "log"
	}
	if len(event.Message) > 4096 {
		event.Message = event.Message[:4096]
	}
	if event.Health != nil && len(event.Health.Detail) > 4096 {
		event.Health.Detail = event.Health.Detail[:4096]
	}
	_, _, _ = s.mutateOwned(context.WithoutCancel(ctx), id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		return appendEvent(ctx, tx, op, event)
	})
}

// stepExists reports whether a durable step row exists, including a running or
// failed one. Reconciliation uses it to decide whether a container may exist.
func (s *Service) stepExists(ctx context.Context, id contract.OperationID, step string) bool {
	_, ok, err := s.stepRecord(ctx, id, step)
	return err == nil && ok
}

// crossNodeBuildUnsupported refuses a distinct build node honestly until the
// built image can be moved to the application node. The placement and
// capability checks still run for every build node; this gate exists because a
// local Docker image on one node is not visible on another without a registry.
func crossNodeBuildUnsupported(env platform.Environment, sourceType string) error {
	if sourceType == "image" || env.BuildNodeName() == env.Node {
		return nil
	}
	return contract.NewFault(contract.PreconditionFailed,
		"build_node is distinct from the application node; moving the built image requires a shared registry",
		"Use build_node equal to the environment node on a co-located builder, use an image source, or configure registry transfer before building across nodes.")
}

// buildLimit is the node's concurrent-build allowance.
func buildLimit(n platform.Node) int {
	if n.BuildConcurrency < 1 {
		return 1
	}
	return n.BuildConcurrency
}
