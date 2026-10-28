package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// releaseCommandRun is the durable state of one release command digest.
type releaseCommandRun struct {
	State       string
	OperationID contract.OperationID
	// Live reports whether the row's operation is still queued, running or
	// cancelling. Only a live operation on another deployment blocks a re-run.
	Live bool
}

type releaseCommandDecision int

const (
	releaseCommandRunNow releaseCommandDecision = iota
	releaseCommandSkip
	releaseCommandRefuse
	// releaseCommandRecover means another operation left a running row behind.
	// Its original node effect must be recovered before ownership can move.
	releaseCommandRecover
)

// releaseCommandAction is the pure D21 decision. A deploy runs a command that
// has not succeeded and skips a succeeded one. A running row owned by another
// operation is never taken over blindly: a live owner refuses, and a terminal
// owner must have its original node effect recovered first. Restart, scale and
// rollback never run it: they skip a succeeded command and refuse otherwise.
func releaseCommandAction(row releaseCommandRun, action platform.DeploymentAction, current contract.OperationID) releaseCommandDecision {
	if row.State == "succeeded" {
		return releaseCommandSkip
	}
	if action != platform.ActionDeploy {
		return releaseCommandRefuse
	}
	if row.State == "running" {
		if row.OperationID == current {
			return releaseCommandRunNow
		}
		if row.Live {
			return releaseCommandRefuse
		}
		return releaseCommandRecover
	}
	return releaseCommandRunNow
}

// releaseCommandRecovery is the confirmed outcome of an old operation's
// command effect, recovered from durable step state and node status.
type releaseCommandRecovery int

const (
	releaseCommandRecoveryDeferred releaseCommandRecovery = iota
	releaseCommandRecoverySucceeded
	releaseCommandRecoveryFailed
)

// releaseCommandRecoveryOutcome is the pure recovery classifier. A completed
// step record proves success without another node read. Every other step state
// is resolved by node status, because operation failure, cancellation and retry
// exhaustion mark unfinished steps failed without proving what the node did. A
// terminal node status proves an outcome only with a matching, nonempty inputs
// hash. Missing or mismatched evidence, running, absent, stale and not-yet-queried
// effects stay deferred; none proves that the command may run again.
func releaseCommandRecoveryOutcome(record StepRecord, hasRecord bool, status engine.Status, statusInputsHash string) releaseCommandRecovery {
	if !hasRecord || record.ExecutionID == "" || record.InputsHash == "" {
		return releaseCommandRecoveryDeferred
	}
	if record.State == "completed" {
		return releaseCommandRecoverySucceeded
	}
	if statusInputsHash == "" || statusInputsHash != record.InputsHash {
		return releaseCommandRecoveryDeferred
	}
	switch status {
	case engine.StatusCompleted:
		return releaseCommandRecoverySucceeded
	case engine.StatusFailed:
		return releaseCommandRecoveryFailed
	default:
		return releaseCommandRecoveryDeferred
	}
}

// deploymentReleaseCommand is the command the deployment would run: the
// pinned revision's runtime command, falling back to the release's own command.
func deploymentReleaseCommand(env platform.Environment, release platform.Release) *platform.CommandSpec {
	if env.Revision != nil && env.Revision.Runtime.ReleaseCommand != nil {
		return env.Revision.Runtime.ReleaseCommand
	}
	return release.Command
}

// releaseCommandErrorIsTerminal reports a confirmed command failure. Transport,
// status and acknowledgement errors leave the run row running for recovery; an
// unknown outcome must never let another deploy run the command a second time.
func releaseCommandErrorIsTerminal(err error) bool {
	return err != nil && (errors.Is(err, errNodeStepPermanentFailure) || errors.Is(err, engine.ErrPermanent))
}

func (s *Service) releaseCommandRun(ctx context.Context, releaseID platform.ReleaseID, digest string) (releaseCommandRun, error) {
	var row releaseCommandRun
	err := s.pool.QueryRow(ctx, `SELECT r.state,coalesce(r.operation_id,''),
        EXISTS(SELECT 1 FROM eregion_operations o
               WHERE o.id=r.operation_id AND o.state IN ('queued','running','cancelling'))
        FROM eregion_platform_release_command_runs r
        WHERE r.release_id=$1 AND r.command_digest=$2`, releaseID, digest).
		Scan(&row.State, &row.OperationID, &row.Live)
	if errors.Is(err, pgx.ErrNoRows) {
		return releaseCommandRun{}, nil
	}
	if err != nil {
		return releaseCommandRun{}, unavailable()
	}
	return row, nil
}

// startReleaseCommandRun records the one running command for this release and
// digest. The conditional update is the ownership guard: it claims only the
// caller's own running row or a confirmed failed row. A running row owned by
// another operation and a succeeded row are never overwritten, even if the
// caller's pre-read raced another writer.
func (s *Service) startReleaseCommandRun(ctx context.Context, releaseID platform.ReleaseID, digest string, operationID contract.OperationID) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO eregion_platform_release_command_runs
        (release_id,command_digest,operation_id,state,started_at)
        VALUES($1,$2,$3,'running',clock_timestamp())
        ON CONFLICT (release_id,command_digest) DO UPDATE
        SET operation_id=$3,state='running',error_code='',error='',
            started_at=clock_timestamp(),completed_at=NULL
        WHERE eregion_platform_release_command_runs.operation_id=EXCLUDED.operation_id
           OR eregion_platform_release_command_runs.state='failed'`,
		releaseID, digest, operationID)
	if err != nil {
		return unavailable()
	}
	if tag.RowsAffected() == 0 {
		return contract.NewFault(contract.Unavailable,
			"the release command run is owned by another operation",
			"Retry the deployment; an unresolved release command is never overwritten.")
	}
	return nil
}

// finishRecoveredReleaseCommandRun records a recovery result only while the
// row is still running under the original owner. A racing recovery or claim
// leaves the row untouched.
func (s *Service) finishRecoveredReleaseCommandRun(ctx context.Context, releaseID platform.ReleaseID, digest string, owner contract.OperationID, state string, code contract.Code, message string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE eregion_platform_release_command_runs
        SET state=$4,error_code=$5,error=$6,completed_at=clock_timestamp()
        WHERE release_id=$1 AND command_digest=$2 AND state='running' AND operation_id=$3`,
		releaseID, digest, owner, state, string(code), boundedRouteError(message))
	if err != nil {
		return false, unavailable()
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Service) finishReleaseCommandRun(ctx context.Context, releaseID platform.ReleaseID, digest string, operationID contract.OperationID, cause error) error {
	state, code, message := "succeeded", "", ""
	if cause != nil {
		classified := contract.Classify(cause)
		state, code, message = "failed", string(classified.Code), classified.Message
	}
	_, err := s.pool.Exec(ctx, `UPDATE eregion_platform_release_command_runs
        SET state=$3,error_code=$4,error=$5,completed_at=clock_timestamp()
        WHERE release_id=$1 AND command_digest=$2 AND operation_id=$6 AND state='running'`,
		releaseID, digest, state, code, message, operationID)
	if err != nil {
		return unavailable()
	}
	return nil
}

// deploymentReleaseCommandTx resolves the same command the deployment worker
// will use, from the release and the revision the deployment pins.
func deploymentReleaseCommandTx(ctx context.Context, tx pgx.Tx, releaseID platform.ReleaseID, revisionID platform.RevisionID) (*platform.CommandSpec, error) {
	var releaseCommand []byte
	if err := tx.QueryRow(ctx, `SELECT command FROM eregion_platform_releases WHERE id=$1`, releaseID).Scan(&releaseCommand); err != nil {
		return nil, unavailable()
	}
	var command *platform.CommandSpec
	if len(releaseCommand) > 0 {
		var value platform.CommandSpec
		if err := json.Unmarshal(releaseCommand, &value); err != nil {
			return nil, contract.NewFault(contract.PreconditionFailed, "the release command does not parse", "Create the release again with a valid release command.")
		}
		command = &value
	}
	if revisionID == "" {
		return command, nil
	}
	var runtimeRaw []byte
	if err := tx.QueryRow(ctx, `SELECT runtime FROM eregion_platform_environment_revisions WHERE id=$1`, revisionID).Scan(&runtimeRaw); err != nil {
		return nil, unavailable()
	}
	var runtime platform.RuntimeSpec
	if len(runtimeRaw) > 0 {
		if err := json.Unmarshal(runtimeRaw, &runtime); err != nil {
			return nil, contract.NewFault(contract.PreconditionFailed, "the environment runtime does not parse", "Save the environment revision again.")
		}
	}
	if runtime.ReleaseCommand != nil {
		return runtime.ReleaseCommand, nil
	}
	return command, nil
}

// checkReleaseCommandForDeploymentTx refuses a restart, scale restart or
// rollback whose release command has not succeeded for the revision applied.
func checkReleaseCommandForDeploymentTx(ctx context.Context, tx pgx.Tx, releaseID platform.ReleaseID, revisionID platform.RevisionID, action platform.DeploymentAction) error {
	if action == platform.ActionDeploy {
		return nil
	}
	command, err := deploymentReleaseCommandTx(ctx, tx, releaseID, revisionID)
	if err != nil || command == nil {
		return err
	}
	digest := platform.ReleaseCommandDigest(*command)
	var row releaseCommandRun
	err = tx.QueryRow(ctx, `SELECT state,coalesce(operation_id,'') FROM eregion_platform_release_command_runs
        WHERE release_id=$1 AND command_digest=$2`, releaseID, digest).
		Scan(&row.State, &row.OperationID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return unavailable()
	}
	if releaseCommandAction(row, action, "") == releaseCommandSkip {
		return nil
	}
	return contract.NewFault(contract.PreconditionFailed,
		"release command has not succeeded for this release",
		"Deploy the release first; restart, scale and rollback never run the release command.")
}

// recoverReleaseCommandRun resolves another operation's running command without
// dispatching it again. It reads only durable step state and node status.
func (w *platformWorker) recoverReleaseCommandRun(ctx context.Context, releaseID platform.ReleaseID, digest string, run releaseCommandRun, current contract.OperationID) (releaseCommandDecision, error) {
	if run.OperationID == "" || run.OperationID == current {
		return 0, contract.NewFault(contract.PreconditionFailed,
			"the unresolved release command has no separate owner to recover",
			"Inspect the release command row and deployment history before retrying.")
	}
	deployment, has, err := w.service.deploymentByOperation(ctx, run.OperationID)
	if err != nil {
		return 0, err
	}
	if !has || deployment.ReleaseID != releaseID {
		return 0, contract.NewFault(contract.PreconditionFailed,
			"the original release command deployment is not recorded for this release",
			"Inspect the original operation and node journal; do not rerun the release command blindly.")
	}
	nodeName := w.service.deploymentNode(ctx, deployment)
	if nodeName == "" {
		return 0, contract.NewFault(contract.PreconditionFailed,
			"the original release command node is not recorded",
			"Inspect the original deployment revision; the release command stays owned by the original operation.")
	}
	record, has, err := w.service.stepRecord(ctx, run.OperationID, deploymentCommandStep)
	if err != nil {
		return 0, err
	}
	outcome := releaseCommandRecoveryOutcome(record, has, "", "")
	if outcome == releaseCommandRecoveryDeferred {
		if !has || record.ExecutionID == "" || record.InputsHash == "" {
			return 0, contract.NewFault(contract.PreconditionFailed,
				"the original release command has no recoverable execution identity",
				"Inspect the original deployment and node journal; do not rerun the release command blindly.")
		}
		if w.service.resolver == nil {
			return 0, contract.NewFault(contract.Unavailable,
				"the worker cannot reach the original release command node",
				"Run recovery in a worker configured with node transports; the release command stays owned by the original operation.")
		}
		tr, err := w.service.resolver.TransportFor(ctx, nodeName)
		if err != nil {
			return 0, releaseCommandRecoveryDeferredFault(nodeName, err)
		}
		execution := engine.Execution{
			OperationID: run.OperationID, Attempt: record.Attempt,
			Step: deploymentCommandStep, RequestID: newRequestID(),
		}
		response, err := tr.Call(ctx, node.Request{
			Method: node.MethodStatus, RequestID: execution.RequestID, Execution: execution,
		}, nil)
		if err != nil {
			return 0, releaseCommandRecoveryDeferredFault(nodeName, err)
		}
		outcome = releaseCommandRecoveryOutcome(record, has, response.Status, response.InputsHash)
	}
	switch outcome {
	case releaseCommandRecoverySucceeded:
		updated, err := w.service.finishRecoveredReleaseCommandRun(ctx, releaseID, digest, run.OperationID, "succeeded", "", "")
		if err != nil {
			return 0, err
		}
		if !updated {
			return 0, releaseCommandRecoveryChangedFault()
		}
		return releaseCommandSkip, nil
	case releaseCommandRecoveryFailed:
		// The node's status detail may include a task-log tail that was not
		// redacted with this command's transient secrets. Persist a generic
		// message; the original operation events already hold the redacted
		// failure reason.
		updated, err := w.service.finishRecoveredReleaseCommandRun(ctx, releaseID, digest, run.OperationID, "failed", contract.PreconditionFailed,
			"the original release command failed on its node")
		if err != nil {
			return 0, err
		}
		if !updated {
			return 0, releaseCommandRecoveryChangedFault()
		}
		return releaseCommandRunNow, nil
	default:
		return 0, contract.NewFault(contract.Unavailable,
			"the original release command outcome is still unknown",
			"Keep the release command owned by the original operation and retry after the node is reachable and the command is no longer running.")
	}
}

func releaseCommandRecoveryDeferredFault(nodeName string, cause error) error {
	if node.IsPermanentTransport(cause) {
		return contract.NewFault(contract.PreconditionFailed,
			"the original release command node cannot be reached",
			"Restore node "+nodeName+" and the pinned transport; the release command stays owned by the original operation.")
	}
	return contract.NewFault(contract.Unavailable,
		"the original release command outcome is unknown",
		"Retry after the original node is reachable; do not rerun the release command until its effect is recovered.")
}

func releaseCommandRecoveryChangedFault() error {
	return contract.NewFault(contract.Unavailable,
		"the release command state changed during recovery",
		"Retry the deployment; an unresolved release command is never overwritten.")
}
