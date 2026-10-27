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
const platformStepTotal = 16

// Deployment step identities are shared by the worker, reconciliation, and the
// viewer. A restored switch is proven by the completed step record, never by a
// message.
const (
	deploymentPreflightStep    = "platform-preflight-01-v1"
	deploymentCommandStep      = "platform-command-01-v1"
	deploymentPackStep         = "platform-pack-01-v1"
	deploymentImageStep        = "platform-image-01-v1"
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
	if op.State == contract.Cancelling {
		return w.service.cancelPlatform(ctx, op.ID, attempt, "Cancellation requested before execution started.")
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return w.settle(ctx, op.ID, attempt, err)
	}

	releaseLock, err := w.service.lockPlatformTargets(ctx, normalized)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer releaseLock()

	if err := w.service.checkCancelled(ctx, op.ID); err != nil {
		return w.settle(ctx, op.ID, attempt, err)
	}
	if executor := normalized.Executor(); executor != nil {
		return w.runExtension(ctx, op.ID, attempt, normalized, executor)
	}
	switch normalized.Kind {
	case platform.KindAppCreate, platform.KindNodeRegister, platform.KindEnvironmentCreate,
		platform.KindEnvironmentUpdate, platform.KindProcessScale, platform.KindConfigSet,
		platform.KindConfigUnset, platform.KindEnvironmentSecretAttach:
		return w.runRecordAction(ctx, op.ID, attempt, normalized)
	case platform.KindNodeCheck:
		return w.runNodeCheck(ctx, op.ID, attempt, normalized)
	case platform.KindNodeEnroll:
		return w.runNodeEnroll(ctx, op.ID, attempt, normalized)
	case platform.KindNodeReconcile:
		return w.runNodeReconcile(ctx, op.ID, attempt, normalized)
	case platform.KindReleaseCreate:
		return w.runRelease(ctx, op.ID, attempt, normalized)
	case platform.KindBuildStop:
		return w.runBuildStop(ctx, op.ID, attempt, normalized)
	case platform.KindBuildRetry:
		return w.runBuildRetry(ctx, op.ID, attempt, normalized)
	case platform.KindDeploy, platform.KindRollback, platform.KindRestart:
		return w.runDeployment(ctx, op.ID, attempt, normalized)
	case platform.KindDomainAdd, platform.KindDomainRemove:
		return w.runDomain(ctx, op.ID, attempt, normalized)
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
			if input.Environment == nil {
				return nil
			}
			return recordRevisionChange(ctx, tx, op, "environment.created", input.Name, input.Environment.Name,
				"Created environment "+input.Environment.Name+" and its first desired-state revision.")
		case *platform.EnvironmentCreateInput:
			return recordRevisionChange(ctx, tx, op, "environment.created", input.App, input.Name,
				"Created environment "+input.Name+" on node "+input.Node+" with its first desired-state revision.")
		case *platform.EnvironmentUpdateInput:
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
	if len(message) > 4096 {
		message = message[:4096]
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_builds SET state='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, string(code), message); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET status='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
            WHERE operation_id=$1 AND status='pending'`, id, string(code), message); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='failed',error_code=$2,message=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, string(code), message); err != nil {
			return err
		}
		var action, app, environment, releaseID, previousID, servingID string
		if err := tx.QueryRow(ctx, `SELECT d.action,a.name,e.name,d.release_id,coalesce(d.previous_release_id,''),coalesce(e.serving_release_id,'')
            FROM eregion_platform_deployments d
            JOIN eregion_platform_apps a ON a.id=d.app_id
            JOIN eregion_platform_environments e ON e.id=d.environment_id
            WHERE d.operation_id=$1`, id).Scan(&action, &app, &environment, &releaseID, &previousID, &servingID); err == nil {
			if err := appendEvent(ctx, tx, op, contract.Event{
				Kind: "log", Stage: "deploy", ReleaseID: releaseID, ServingReleaseID: servingID,
				Message: "Deployment failed; failed release " + releaseID + " is not recorded as serving" + servingSuffix(servingID) + ".",
			}); err != nil {
				return err
			}
			unchanged := servingID == "" || servingID == previousID
			if changeErr := insertPlatformChange(ctx, tx, op, action+".failed", app, environment,
				"Deployment failed: "+message,
				map[string]any{"release_id": releaseID},
				map[string]any{"error_code": code, "serving_release_id": servingID, "serving_unchanged": unchanged}); changeErr != nil {
				return changeErr
			}
		}
		if err := markIncompleteSteps(ctx, tx, id, code, message); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Failed, code, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
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
	status, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil {
		if errors.Is(err, engine.ErrJournalCorrupt) {
			return nil, contract.NewFault(contract.PreconditionFailed, "node journal is corrupt", "Inspect the node journal before retrying.")
		}
		if node.IsPermanentTransport(err) {
			return nil, contract.NewFault(contract.PreconditionFailed, "the node transport refused the request", "Check the pinned node identity and host configuration.")
		}
		return nil, fmt.Errorf("node status %s: %w", stepID, err)
	}
	if status.InputsHash != "" && status.InputsHash != inputsHash {
		return nil, contract.NewFault(contract.PreconditionFailed, "node effect has different inputs", "Do not acknowledge an effect recorded under other inputs.")
	}
	if status.Status == engine.StatusCompleted {
		if !hasRecord {
			if _, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID); err != nil {
				return nil, err
			}
		}
		if err := w.service.recordRecoveredStep(ctx, id, attempt, stepID, inputsHash, status); err != nil {
			return nil, err
		}
		if _, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID, status.Result, platformStepTotal); err != nil {
			return nil, err
		}
		return status.Result, nil
	}
	already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, exec.ExecutionID())
	if err != nil {
		return nil, err
	}
	if already {
		return record.Result, nil
	}
	w.service.registerSecretValues(secrets)
	events := w.service.newNodeEventStream(ctx, id, attempt)
	response, err := tr.Call(ctx, node.Request{
		Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec, Payload: raw, Secrets: secrets,
	}, events.add)
	if appendErr := events.close(); appendErr != nil && err == nil {
		err = appendErr
	}
	if err != nil {
		if node.IsPermanentTransport(err) {
			return nil, contract.NewFault(contract.PreconditionFailed, "the node transport refused the request", "Check the pinned node identity and host configuration.")
		}
		// Unknown outcome: the next attempt asks status first and never starts a
		// second effect under the same key.
		return nil, fmt.Errorf("node run %s: %w", stepID, err)
	}
	if response.InputsHash != "" && response.InputsHash != inputsHash {
		return nil, contract.NewFault(contract.PreconditionFailed, "node effect has different inputs", "Do not acknowledge an effect recorded under other inputs.")
	}
	switch response.Status {
	case engine.StatusCompleted:
		if _, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, response.ExecutionID, response.Result, platformStepTotal); err != nil {
			return nil, err
		}
		duration := time.Since(started).Milliseconds()
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind: "log", Stage: stepID, Step: stepID, DurationMS: &duration,
			Message: fmt.Sprintf("Node step %s completed in %dms.", stepID, duration),
		})
		return response.Result, nil
	case engine.StatusRunning:
		return nil, contract.NewFault(contract.Unavailable, "node task is still running", "The operation remains running; retrying will attach to the existing effect, not start another.")
	case engine.StatusFailed:
		code := contract.Code(response.ErrorCode)
		if code == "" {
			code = contract.Internal
		}
		detail := response.Detail
		if detail == "" {
			detail = "node step failed"
		}
		if failErr := w.service.failStep(ctx, id, attempt, stepID, code, detail); failErr != nil {
			return nil, failErr
		}
		if response.Retryable {
			return nil, fmt.Errorf("node step %s failed: %s", stepID, detail)
		}
		return nil, contract.NewFault(contract.PreconditionFailed, "node step failed: "+detail, "Inspect operation events and node state before resubmitting.")
	default:
		return nil, fmt.Errorf("node returned unexpected status %q for %s", response.Status, stepID)
	}
}

// reconcilePlatformOperation closes an operation whose River job disappeared.
// It never invents success: it accepts only durable domain state that proves the
// action committed, and otherwise records failure with the serving release that
// the environment actually had (or a switched release with unknown health).
func (s *Service) reconcilePlatformOperation(ctx context.Context, op contract.Operation) error {
	if op.Kind == platform.KindDeploy || op.Kind == platform.KindRollback || op.Kind == platform.KindRestart {
		return s.reconcileDeployment(ctx, op)
	}
	if op.State == contract.Cancelling {
		return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled after the River job ended.")
	}
	switch op.Kind {
	case platform.KindReleaseCreate, platform.KindBuildRetry:
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
		// The table already holds the change; the routes file is derived from it.
		if err := s.syncRoutes(ctx); err != nil {
			return s.failReconciledPlatform(ctx, op, err.Error(), contract.Unavailable)
		}
		return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
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
	case platform.KindAppCreate, platform.KindEnvironmentCreate, platform.KindEnvironmentUpdate,
		platform.KindProcessScale, platform.KindConfigSet, platform.KindConfigUnset,
		platform.KindEnvironmentSecretAttach, platform.KindNodeRegister:
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
	case *platform.ProcessScaleInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.ConfigSetInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.ConfigUnsetInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=$1)`, op.ID).Scan(&exists)
	case *platform.EnvironmentSecretAttachInput:
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

// reconcileDeployment never trusts the deployment row's intended release. It
// inspects completed step records and records the release that the node effects
// prove is serving, including a completed restore of the previous release.
func (s *Service) reconcileDeployment(ctx context.Context, op contract.Operation) error {
	dep, has, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if !has {
		return s.failReconciledPlatform(ctx, op, "deployment record is missing", contract.Unavailable)
	}
	env, envErr := s.environment(ctx, dep.App, dep.Environment)
	if dep.State == "succeeded" {
		return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
	}
	restored := s.completedStep(ctx, op.ID, deploymentRestoreRouteStep)
	switched := s.completedStep(ctx, op.ID, deploymentRouteStep)
	verified := s.completedStep(ctx, op.ID, deploymentPublicStep)
	cancelling := op.State == contract.Cancelling

	if restored {
		// The new release failed verification and the previous upstreams were
		// put back. Recording the failed new release as serving would be a lie.
		if envErr != nil {
			return envErr
		}
		s.reconcileDeploymentCleanup(ctx, op, env, dep)
		return s.failAfterRestore(ctx, op, dep, env, false)
	}
	if switched {
		if verified {
			// The switch and public verification both committed before the
			// process died; the worker would have finished successfully.
			return s.finishDeploymentAfterSwitch(ctx, op, dep, env)
		}
		// The switch committed but the public check did not. Restore the
		// previous upstreams from the still-old serving deployment and remove
		// the unverified candidate formation.
		restoreErr := s.syncRoutes(context.WithoutCancel(ctx))
		s.reconcileDeploymentCleanup(ctx, op, env, dep)
		if cancelling {
			return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled after the switch; the previous serving release was restored.")
		}
		if restoreErr != nil {
			return s.failReconciledPlatform(ctx, op, "execution ended after the switch and restoring the previous upstreams failed: "+restoreErr.Error(), contract.Unavailable)
		}
		return s.failReconciledPlatform(ctx, op, "execution ended after the switch before public verification; the previous serving release was restored", contract.Unavailable)
	}
	if envErr == nil {
		s.reconcileDeploymentCleanup(ctx, op, env, dep)
	}
	if cancelling {
		return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled before the traffic switch; the previous serving release is unchanged.")
	}
	return s.failReconciledPlatform(ctx, op, "execution ended before the serving release was verified; the previous serving release is unchanged", contract.Unavailable)
}

func (s *Service) completedStep(ctx context.Context, id contract.OperationID, step string) bool {
	record, ok, err := s.stepRecord(ctx, id, step)
	return err == nil && ok && record.State == "completed"
}

// reconcileDeploymentCleanup removes a candidate left by a crashed attempt. The
// candidate step row proves a container may exist. Cleanup is best effort here:
// an unreachable node must not keep the operation running forever, but the
// failure is visible.
func (s *Service) reconcileDeploymentCleanup(ctx context.Context, op contract.Operation, env platform.Environment, dep platform.Deployment) {
	instances := dep.Instances
	revision := env.Revision
	if dep.RevisionID != "" && (revision == nil || revision.ID != dep.RevisionID) {
		pinned, err := s.revisionByID(ctx, dep.RevisionID)
		if err == nil {
			revision = &pinned
		}
	}
	if len(instances) == 0 && revision != nil && s.hasStepPrefix(ctx, op.ID, deploymentContainerStepPrefix) {
		plan, err := platform.PlanDeployment(env.App, env.Name, revision.RevisionState, dep.ReleaseID, dep.ID)
		if err == nil {
			for _, planned := range plan {
				instances = append(instances, platform.DeploymentInstance{Name: planned.Name})
			}
		}
	}
	if len(instances) == 0 {
		return
	}
	if s.resolver == nil {
		s.appendWorkerEvent(ctx, op.ID, op.Attempt, "cleanup", "Candidate cleanup was not attempted: the worker has no node resolver.")
		return
	}
	// The candidate's pinned revision records the node it ran on; the
	// environment may already point at another node.
	nodeName := env.Node
	if revision != nil && revision.Runtime.Node != "" {
		nodeName = revision.Runtime.Node
	}
	tr, err := s.resolver.TransportFor(ctx, nodeName)
	if err != nil {
		s.appendWorkerEvent(ctx, op.ID, op.Attempt, "cleanup", "Candidate cleanup could not reach the application node: "+contract.Classify(err).Message)
		return
	}
	if err := s.removeDeploymentContainers(ctx, tr, op.ID, op.Attempt, instances); err != nil {
		s.appendWorkerEvent(ctx, op.ID, op.Attempt, "cleanup", "Candidate cleanup did not complete: "+contract.Classify(err).Message)
	}
}

func (s *Service) hasStepPrefix(ctx context.Context, id contract.OperationID, prefix string) bool {
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return false
	}
	for _, record := range records {
		if strings.HasPrefix(record.Step, prefix) {
			return true
		}
	}
	return false
}

// failAfterRestore records the previous release as serving after a completed
// restore step. This is the honest outcome of a failed verification.
func (s *Service) failAfterRestore(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, healthy bool) error {
	health, detail, verification := platform.HealthUnknown, "previous release restored after a failed verification; restore health was not observed", "unknown"
	if healthy {
		health, detail, verification = platform.HealthHealthy, "previous release restored after a failed verification and passed its health check", "healthy"
	}
	serving := dep.PreviousReleaseID
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='failed',error_code=$2,message=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`,
			op.ID, string(contract.Unavailable), "The new release failed verification; the previous release "+string(serving)+" was restored."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,serving_deployment_id=$3,applied_release_id=$2,health_state=$4,health_detail=$5,health_checked_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE id=$1`, env.ID, serving, dep.PreviousDeploymentID, string(health), detail); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, current, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(dep.ReleaseID), ServingReleaseID: string(serving),
			Message: "Restore completed: release " + string(serving) + " serves again after release " + string(dep.ReleaseID) + " failed verification; restore health is " + verification + ".",
		}); err != nil {
			return err
		}
		if err := markIncompleteSteps(ctx, tx, op.ID, contract.Unavailable, "operation reconciled after restoring the previous release"); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, contract.Unavailable,
			"Verification failed and the previous release "+string(serving)+" was restored; recorded restore health is "+verification+".")
	})
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return nil
}

func (s *Service) finishDeploymentAfterSwitch(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment) error {
	return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
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
			Message: "Deployment completed after the switch and verification; serving release " + string(dep.ReleaseID) + ".",
		}); err != nil {
			return err
		}
		return insertPlatformChange(ctx, tx, current, string(dep.Action)+".succeeded", dep.App, dep.Environment,
			"Serving release "+string(dep.ReleaseID)+" for "+dep.App+"/"+dep.Environment+".",
			map[string]any{"release_id": dep.ReleaseID},
			map[string]any{"serving_release_id": dep.ReleaseID, "health": "healthy"})
	})
}

func depEnvironmentID(ctx context.Context, tx pgx.Tx, dep platform.Deployment) string {
	var id string
	_ = tx.QueryRow(ctx, `SELECT e.id FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2`, dep.App, dep.Environment).Scan(&id)
	return id
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
