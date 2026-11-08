package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// A deployment lifecycle owner is the one operation allowed to change an
// environment's route intent. It is acquired and released under the environment
// advisory lock. Route writers read only this row (falling back to
// serving_deployment_id when no row exists), so deployment creation timestamps
// and running step rows never elect a candidate.
type deploymentOwnerPhase string

const (
	deploymentOwnerPrepared  deploymentOwnerPhase = "prepared"
	deploymentOwnerSwitching deploymentOwnerPhase = "switching"
	deploymentOwnerVerifying deploymentOwnerPhase = "verifying"
	deploymentOwnerServing   deploymentOwnerPhase = "serving"
	deploymentOwnerRestoring deploymentOwnerPhase = "restoring"
	deploymentOwnerRestored  deploymentOwnerPhase = "restored"
	deploymentOwnerAmbiguous deploymentOwnerPhase = "ambiguous"
)

const deploymentOwnerDefer = 2 * time.Second

type deploymentOwner struct {
	EnvironmentID       platform.EnvironmentID
	OperationID         contract.OperationID
	DeploymentID        platform.DeploymentID
	Attempt             uint32
	Phase               deploymentOwnerPhase
	RouteTarget         platform.DeploymentID
	RestoreReleaseID    platform.ReleaseID
	RestoreDeploymentID platform.DeploymentID
}

type deploymentOwnerRow struct {
	deploymentOwner
	App         string
	Environment string
}

const deploymentOwnerColumns = `coalesce(o.operation_id,''),coalesce(o.deployment_id,''),
    coalesce(o.owner_attempt,0),o.phase,coalesce(o.route_target_deployment_id,''),
    coalesce(o.restore_release_id,''),coalesce(o.restore_deployment_id,'')`

func scanDeploymentOwner(row pgx.Row, environmentID platform.EnvironmentID) (deploymentOwner, error) {
	owner := deploymentOwner{EnvironmentID: environmentID}
	err := row.Scan(&owner.OperationID, &owner.DeploymentID, &owner.Attempt, &owner.Phase,
		&owner.RouteTarget, &owner.RestoreReleaseID, &owner.RestoreDeploymentID)
	return owner, err
}

func (s *Service) deploymentOwnerByEnvironment(ctx context.Context, q rowQuerier, environmentID platform.EnvironmentID) (deploymentOwner, bool, error) {
	owner, err := scanDeploymentOwner(q.QueryRow(ctx, `SELECT `+deploymentOwnerColumns+`
        FROM eregion_platform_deployment_owners o WHERE o.environment_id=$1`, environmentID), environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return owner, false, nil
	}
	if err != nil {
		return owner, false, unavailable()
	}
	return owner, true, nil
}

// deploymentLifecycleKind is the operation set that can change traffic or the
// desired formation applied by a traffic-changing restart.
func deploymentLifecycleKind(kind string) bool {
	switch kind {
	case platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote:
		return true
	default:
		return false
	}
}

// acquireDeploymentOwner freezes the predecessor and creates the owner row, or
// renews an owner already belonging to this exact operation/deployment. It must
// run while holding env/<app>/<env>. A different live owner returns false; the
// caller must snooze without consuming a River attempt.
func (s *Service) acquireDeploymentOwner(ctx context.Context, dep platform.Deployment, env platform.Environment, attempt uint32) (deploymentOwner, bool, error) {
	owner, has, err := s.deploymentOwnerByEnvironment(ctx, s.pool, env.ID)
	if err != nil {
		return owner, false, err
	}
	if has {
		return s.renewDeploymentOwner(ctx, owner, dep, attempt)
	}

	route, routeHas, err := s.stepRecord(ctx, dep.OperationID, deploymentRouteStep)
	if err != nil {
		return deploymentOwner{}, false, err
	}
	public, publicHas, err := s.stepRecord(ctx, dep.OperationID, deploymentPublicStep)
	if err != nil {
		return deploymentOwner{}, false, err
	}
	_, restoreHas, err := s.stepRecord(ctx, dep.OperationID, deploymentRestoreRouteStep)
	if err != nil {
		return deploymentOwner{}, false, err
	}
	routeCompleted := routeHas && route.State == "completed"
	publicCompleted := publicHas && public.State == "completed"
	restoreRelease, restoreDeployment := frozenPredecessor(dep, env, routeCompleted)

	phase := deploymentOwnerPrepared
	target := restoreDeployment
	evidence := routeHas || publicHas || restoreHas
	if routeCompleted && publicCompleted && !restoreHas {
		phase = deploymentOwnerServing
		target = dep.ID
	} else if evidence {
		// A route that started or completed without a completed verification is
		// intent, not proof. Generic route writers point at the frozen
		// predecessor until recovery writes and acknowledges the restore.
		phase = deploymentOwnerRestoring
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return deploymentOwner{}, false, unavailable()
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `INSERT INTO eregion_platform_deployment_owners
        (environment_id,operation_id,deployment_id,owner_attempt,phase,route_target_deployment_id,restore_release_id,restore_deployment_id)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8)
        ON CONFLICT (environment_id) DO NOTHING`,
		env.ID, dep.OperationID, dep.ID, attempt, string(phase),
		nullableDeploymentID(target), nullableReleaseID(restoreRelease), nullableDeploymentID(restoreDeployment))
	if err != nil {
		return deploymentOwner{}, false, unavailable()
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		owner, has, err = s.deploymentOwnerByEnvironment(ctx, s.pool, env.ID)
		if err != nil {
			return owner, false, err
		}
		if !has {
			return deploymentOwner{}, false, unavailable()
		}
		return s.renewDeploymentOwner(ctx, owner, dep, attempt)
	}
	owner = deploymentOwner{
		EnvironmentID: env.ID, OperationID: dep.OperationID, DeploymentID: dep.ID,
		Attempt: attempt, Phase: phase, RouteTarget: target,
		RestoreReleaseID: restoreRelease, RestoreDeploymentID: restoreDeployment,
	}
	if err := bindDeploymentPredecessor(ctx, tx, dep, owner); err != nil {
		return deploymentOwner{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return deploymentOwner{}, false, unavailable()
	}
	return owner, true, nil
}

func bindDeploymentPredecessor(ctx context.Context, tx pgx.Tx, dep platform.Deployment, owner deploymentOwner) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
        SET previous_release_id=$2,previous_deployment_id=$3,updated_at=updated_at
        WHERE id=$1`, dep.ID, nullableReleaseID(owner.RestoreReleaseID), nullableDeploymentID(owner.RestoreDeploymentID)); err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) renewDeploymentOwner(ctx context.Context, owner deploymentOwner, dep platform.Deployment, attempt uint32) (deploymentOwner, bool, error) {
	if owner.Phase == deploymentOwnerAmbiguous || owner.OperationID != dep.OperationID || owner.DeploymentID != dep.ID {
		return owner, false, nil
	}
	if owner.Attempt > attempt {
		return owner, false, errNotOwned
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return owner, false, unavailable()
	}
	defer rollback(tx)
	if owner.Attempt < attempt {
		tag, err := tx.Exec(ctx, `UPDATE eregion_platform_deployment_owners
            SET owner_attempt=$3,updated_at=clock_timestamp()
            WHERE environment_id=$1 AND operation_id=$2 AND owner_attempt <= $3`,
			owner.EnvironmentID, owner.OperationID, attempt)
		if err != nil {
			return owner, false, unavailable()
		}
		if tag.RowsAffected() == 0 {
			return owner, false, errNotOwned
		}
		owner.Attempt = attempt
	}
	// Repair or reassert the deployment's frozen predecessor from the owner
	// before any effects. A crash between owner creation and the old separate
	// binding left stale previous pointers; renewal closes that gap atomically.
	if err := bindDeploymentPredecessor(ctx, tx, dep, owner); err != nil {
		return owner, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return owner, false, unavailable()
	}
	return owner, true, nil
}

func frozenPredecessor(dep platform.Deployment, env platform.Environment, routeCompleted bool) (platform.ReleaseID, platform.DeploymentID) {
	if routeCompleted {
		if dep.PreviousDeploymentID != "" || dep.PreviousReleaseID != "" {
			return dep.PreviousReleaseID, dep.PreviousDeploymentID
		}
		if env.ServingDeploymentID != "" && env.ServingDeploymentID != dep.ID {
			return env.ServingRelease, env.ServingDeploymentID
		}
		return "", ""
	}
	if env.ServingDeploymentID != "" && env.ServingDeploymentID != dep.ID {
		return env.ServingRelease, env.ServingDeploymentID
	}
	return dep.PreviousReleaseID, dep.PreviousDeploymentID
}

// setDeploymentOwnerRoute changes the exclusive route intent. A stale attempt
// gets errNotOwned and must not touch the route or candidates.
func (s *Service) setDeploymentOwnerRoute(ctx context.Context, owner deploymentOwner, target platform.DeploymentID, phase deploymentOwnerPhase, attempt uint32) (deploymentOwner, error) {
	updated, err := scanDeploymentOwner(s.pool.QueryRow(ctx, `UPDATE eregion_platform_deployment_owners o
        SET route_target_deployment_id=$3,phase=$4,owner_attempt=$5,updated_at=clock_timestamp()
        WHERE o.environment_id=$1 AND o.operation_id=$2 AND o.deployment_id=$6 AND o.owner_attempt <= $5
        RETURNING `+deploymentOwnerColumns,
		owner.EnvironmentID, owner.OperationID, nullableDeploymentID(target), string(phase), attempt, owner.DeploymentID),
		owner.EnvironmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return owner, errNotOwned
	}
	if err != nil {
		return owner, unavailable()
	}
	return updated, nil
}

// releaseRestoredDeploymentOwnerTx removes an owner after the exact frozen
// restore target is durable. It must never release a serving-phase owner: only
// the success finalizer may do that.
func releaseRestoredDeploymentOwnerTx(ctx context.Context, tx pgx.Tx, operationID contract.OperationID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_deployment_owners o
        USING eregion_platform_environments e
        WHERE o.operation_id=$1 AND o.environment_id=e.id
          AND o.phase IN ('prepared','restored')
          AND o.route_target_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id
          AND e.serving_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id`, operationID); err != nil {
		return unavailable()
	}
	return nil
}

// releaseServedDeploymentOwnerTx is called only by the successful deployment
// finalizer after the serving pointer and history commit.
func releaseServedDeploymentOwnerTx(ctx context.Context, tx pgx.Tx, operationID contract.OperationID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_deployment_owners o
        USING eregion_platform_environments e
        WHERE o.operation_id=$1 AND o.environment_id=e.id
          AND o.phase='serving'
          AND o.route_target_deployment_id=o.deployment_id
          AND e.serving_deployment_id=o.deployment_id`, operationID); err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) releaseRestoredDeploymentOwner(ctx context.Context, owner deploymentOwner) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM eregion_platform_deployment_owners o
        USING eregion_platform_environments e
        WHERE o.environment_id=$1 AND o.operation_id=$2 AND o.owner_attempt=$3
          AND o.environment_id=e.id
          AND o.phase IN ('prepared','restored')
          AND o.route_target_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id
          AND e.serving_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id`,
		owner.EnvironmentID, owner.OperationID, owner.Attempt)
	if err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) releaseServedDeploymentOwner(ctx context.Context, owner deploymentOwner) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM eregion_platform_deployment_owners o
        USING eregion_platform_environments e
        WHERE o.environment_id=$1 AND o.operation_id=$2 AND o.owner_attempt=$3
          AND o.environment_id=e.id
          AND o.phase='serving'
          AND o.route_target_deployment_id=o.deployment_id
          AND e.serving_deployment_id=o.deployment_id`,
		owner.EnvironmentID, owner.OperationID, owner.Attempt)
	if err != nil {
		return unavailable()
	}
	return nil
}

// deploymentOwnerRowsToSettle returns owner rows that cannot progress on their
// own: ambiguous legacy rows and rows whose operation is already terminal.
func (s *Service) deploymentOwnerRowsToSettle(ctx context.Context) ([]deploymentOwnerRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.environment_id,`+deploymentOwnerColumns+`,a.name,e.name
        FROM eregion_platform_deployment_owners o
        JOIN eregion_platform_environments e ON e.id=o.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_operations op ON op.id=o.operation_id
        WHERE o.phase='ambiguous' OR op.id IS NULL OR op.state NOT IN ('queued','running','cancelling')
        ORDER BY o.environment_id
        LIMIT 100`)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	var out []deploymentOwnerRow
	for rows.Next() {
		var row deploymentOwnerRow
		if err := rows.Scan(&row.EnvironmentID, &row.OperationID, &row.DeploymentID, &row.Attempt,
			&row.Phase, &row.RouteTarget, &row.RestoreReleaseID, &row.RestoreDeploymentID,
			&row.App, &row.Environment); err != nil {
			return nil, unavailable()
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// recoverStaleDeploymentOwner is called while the caller already holds the
// environment lock. It settles ambiguous legacy rows and terminal owners before
// another job can acquire. A live owner returns false.
func (s *Service) recoverStaleDeploymentOwner(ctx context.Context, owner deploymentOwner, env platform.Environment) (bool, error) {
	if owner.Phase == deploymentOwnerAmbiguous || owner.OperationID == "" {
		return true, s.resolveAmbiguousDeploymentOwner(ctx, owner, env)
	}
	op, err := scanOperation(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", owner.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return true, s.settleTerminalDeploymentOwner(ctx, owner, env, nil)
	}
	if err != nil {
		return false, unavailable()
	}
	if !op.State.Terminal() {
		return false, nil
	}
	return true, s.settleTerminalDeploymentOwner(ctx, owner, env, &op)
}

func (s *Service) settleTerminalDeploymentOwner(ctx context.Context, owner deploymentOwner, env platform.Environment, op *contract.Operation) error {
	dep, depErr := s.deploymentByID(ctx, owner.DeploymentID)
	hasDep := depErr == nil
	if depErr != nil && !isNotFound(depErr) {
		return depErr
	}
	if owner.RestoreDeploymentID != "" &&
		(owner.DeploymentID == owner.RestoreDeploymentID || (hasDep && dep.ID == owner.RestoreDeploymentID)) {
		return contract.NewFault(contract.Unavailable,
			"deployment owner's restore target is its candidate deployment",
			"Ownership is retained; inspect and repair the inconsistent owner before retrying terminal recovery.")
	}

	if op != nil && op.State == contract.Succeeded && hasDep && env.ServingDeploymentID == owner.DeploymentID {
		owner, err := s.setDeploymentOwnerRoute(ctx, owner, owner.DeploymentID, deploymentOwnerServing, owner.Attempt)
		if err != nil {
			return err
		}
		if err := s.syncRoutes(ctx); err != nil {
			return err
		}
		return s.releaseServedDeploymentOwner(ctx, owner)
	}

	routeAlreadySafe := (owner.Phase == deploymentOwnerPrepared || owner.Phase == deploymentOwnerRestored) &&
		owner.RouteTarget == owner.RestoreDeploymentID &&
		env.ServingDeploymentID == owner.RestoreDeploymentID
	if !routeAlreadySafe {
		owner, err := s.setDeploymentOwnerRoute(ctx, owner, owner.RestoreDeploymentID, deploymentOwnerRestoring, owner.Attempt)
		if err != nil {
			return err
		}
		if err := s.writeRoutesForDeployment(ctx, env, owner.RestoreDeploymentID); err != nil {
			return err
		}
		if err := s.recordDeploymentRestoreServing(ctx, env, owner.RestoreReleaseID, owner.RestoreDeploymentID); err != nil {
			return err
		}
		owner, err = s.setDeploymentOwnerRoute(ctx, owner, owner.RestoreDeploymentID, deploymentOwnerRestored, owner.Attempt)
		if err != nil {
			return err
		}
	}
	if hasDep && dep.ID != owner.RestoreDeploymentID {
		// The terminal operation cannot run its normal cleanup steps. Record
		// the candidate containers durably before ownership is released. Use
		// the post-restore target, not the environment snapshot captured before
		// recordDeploymentRestoreServing changed the live serving pointer.
		if err := s.recordDeploymentCleanupCandidates(ctx, env, dep); err != nil {
			return err
		}
	}
	return s.releaseRestoredDeploymentOwner(ctx, owner)
}

// ownerAllowsSuccess treats the owner phase and route target as authoritative.
// Completed step rows are necessary evidence, but a restoring/restored owner or
// a target that no longer names the candidate can never finalize success.
func ownerAllowsSuccess(owner deploymentOwner, deploymentID platform.DeploymentID) bool {
	if owner.DeploymentID != deploymentID || owner.RouteTarget != deploymentID {
		return false
	}
	return owner.Phase != deploymentOwnerRestoring && owner.Phase != deploymentOwnerRestored
}

func isNotFound(err error) bool {
	fault := contract.Classify(err)
	return fault.Code == contract.NotFound
}

// recordDeploymentCleanupCandidates records (but does not remove) a terminal
// owner's instances. Maintenance removes them only after ownership is released
// and the guards prove they are neither serving nor the current route target.
func (s *Service) recordDeploymentRestoreServing(ctx context.Context, env platform.Environment, release platform.ReleaseID, deployment platform.DeploymentID) error {
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_environments
        SET serving_release_id=$2,serving_deployment_id=$3,applied_release_id=$2,
            health_state='unknown',health_detail='the recorded serving route was restored; restore health was not observed',
            health_checked_at=clock_timestamp(),updated_at=clock_timestamp()
        WHERE id=$1`, env.ID, nullableReleaseID(release), nullableDeploymentID(deployment)); err != nil {
		return unavailable(fmt.Errorf("record restored serving state: %w", err))
	}
	return nil
}

// deploymentCleanupProtected refuses to remove any container set that the
// database currently records as serving or that the owner currently routes to.
// It reads the authoritative rows under the caller's environment lock instead
// of trusting an environment snapshot captured before a switch or restore.
func (s *Service) deploymentCleanupProtected(ctx context.Context, env platform.Environment, deploymentID platform.DeploymentID) (bool, error) {
	var serving platform.DeploymentID
	var phase deploymentOwnerPhase
	var ownerDeployment, routeTarget, restoreTarget platform.DeploymentID
	var hasOwner bool
	err := s.pool.QueryRow(ctx, `SELECT coalesce(e.serving_deployment_id,''),
            coalesce(o.phase,''),coalesce(o.deployment_id,''),coalesce(o.route_target_deployment_id,''),
            coalesce(o.restore_deployment_id,''),o.environment_id IS NOT NULL
        FROM eregion_platform_environments e
        LEFT JOIN eregion_platform_deployment_owners o ON o.environment_id=e.id
        WHERE e.id=$1`, env.ID).Scan(&serving, &phase, &ownerDeployment, &routeTarget, &restoreTarget, &hasOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, contract.NewFault(contract.NotFound, "environment is missing", "Inspect the environment before retrying cleanup.")
	}
	if err != nil {
		return false, unavailable(err)
	}
	if deploymentID != "" && deploymentID == serving {
		return true, nil
	}
	if !hasOwner {
		return false, nil
	}
	if phase == deploymentOwnerAmbiguous {
		return true, nil
	}
	// The owner's own candidate must survive maintenance cleanup for the whole
	// owner lifetime. A prepared or switching owner may still resume and skip
	// already-completed container steps, so partial cleanup intent recorded
	// before settlement cannot be allowed to delete the containers first.
	if deploymentID != "" && deploymentID == ownerDeployment {
		return true, nil
	}
	// The frozen restore target must remain intact for the entire owner
	// lifetime. A cancellation may need it after candidate intent and even
	// after recordServing changed the live serving pointer.
	if deploymentID != "" && deploymentID == restoreTarget {
		return true, nil
	}
	if deploymentID == "" {
		return true, nil
	}
	return routeTarget == deploymentID, nil
}

// cleanupInstancesForDeployment returns the recorded instance names, or
// reconstructs the deterministic plan when containers were started before
// saveDeploymentInstances completed. A pinned revision lookup failure is an
// error rather than an empty cleanup.
func (s *Service) cleanupInstancesForDeployment(ctx context.Context, env platform.Environment, dep platform.Deployment) ([]platform.DeploymentInstance, error) {
	if len(dep.Instances) > 0 {
		return dep.Instances, nil
	}
	hasContainerSteps, err := s.hasStepPrefix(ctx, dep.OperationID, deploymentContainerStepPrefix)
	if err != nil {
		return nil, fmt.Errorf("read container step evidence for deployment %s: %w", dep.ID, err)
	}
	if !hasContainerSteps {
		return nil, nil
	}
	if dep.RevisionID == "" {
		return nil, fmt.Errorf("deployment %s has container steps but no pinned revision", dep.ID)
	}
	revision, err := s.revisionByID(ctx, dep.RevisionID)
	if err != nil {
		return nil, fmt.Errorf("read pinned revision %s for deployment %s: %w", dep.RevisionID, dep.ID, err)
	}
	plan, err := platform.PlanDeployment(env.App, env.Name, revision.RevisionState, dep.ReleaseID, dep.ID)
	if err != nil {
		return nil, fmt.Errorf("plan cleanup for deployment %s: %w", dep.ID, err)
	}
	out := make([]platform.DeploymentInstance, 0, len(plan))
	for _, planned := range plan {
		out = append(out, platform.DeploymentInstance{Name: planned.Name})
	}
	return out, nil
}

func (s *Service) persistDeploymentCleanupInstances(ctx context.Context, env platform.Environment, dep platform.Deployment, instances []platform.DeploymentInstance, reason string) error {
	if len(instances) == 0 {
		var err error
		instances, err = s.cleanupInstancesForDeployment(ctx, env, dep)
		if err != nil {
			return err
		}
	}
	if len(instances) == 0 {
		return nil
	}
	nodeName, err := deploymentCleanupNode(ctx, s, dep, env.Node)
	if err != nil {
		return err
	}
	if nodeName == "" {
		return fmt.Errorf("deployment %s has no cleanup node", dep.ID)
	}
	var failures []error
	for _, instance := range instances {
		if instance.Name == "" {
			continue
		}
		if err := s.ensureContainerCleanup(ctx, instance.Name, env.ID, nodeName, dep.ID, reason,
			contract.NewFault(contract.Unavailable, "deployment cleanup intent", "The maintenance cleanup retries after the serving state is terminal.")); err != nil {
			failures = append(failures, fmt.Errorf("record cleanup for %s: %w", instance.Name, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) persistDeploymentCleanupIntent(ctx context.Context, env platform.Environment, dep platform.Deployment, reason string) error {
	return s.persistDeploymentCleanupInstances(ctx, env, dep, dep.Instances, reason)
}

func (s *Service) recordDeploymentCleanupCandidates(ctx context.Context, env platform.Environment, dep platform.Deployment) error {
	return s.persistDeploymentCleanupIntent(ctx, env, dep, "candidate")
}

func (s *Service) resolveAmbiguousDeploymentOwner(ctx context.Context, owner deploymentOwner, env platform.Environment) error {
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_deployment_owners
        SET route_target_deployment_id=$2,updated_at=clock_timestamp()
        WHERE environment_id=$1 AND operation_id IS NULL`,
		env.ID, nullableDeploymentID(owner.RestoreDeploymentID)); err != nil {
		return unavailable(fmt.Errorf("set ambiguous owner restore target: %w", err))
	}
	if err := s.writeRoutesForDeployment(ctx, env, owner.RestoreDeploymentID); err != nil {
		return err
	}
	if err := s.recordDeploymentRestoreServing(ctx, env, owner.RestoreReleaseID, owner.RestoreDeploymentID); err != nil {
		return err
	}

	rows, err := s.pool.Query(ctx, `SELECT o.id,o.attempt,o.state,d.id
        FROM eregion_operations o
        JOIN eregion_platform_deployments d ON d.operation_id=o.id
        WHERE d.environment_id=$1
          AND o.state IN ('queued','running','cancelling')
          AND o.kind IN ('platform.deploy','platform.rollback','platform.restart','platform.promote')
        ORDER BY o.id`, env.ID)
	if err != nil {
		return unavailable(fmt.Errorf("list ambiguous active deployments: %w", err))
	}
	type activeDeployment struct {
		id         contract.OperationID
		attempt    uint32
		state      contract.State
		deployment platform.DeploymentID
	}
	var active []activeDeployment
	for rows.Next() {
		var item activeDeployment
		if err := rows.Scan(&item.id, &item.attempt, &item.state, &item.deployment); err != nil {
			rows.Close()
			return unavailable(fmt.Errorf("scan ambiguous active deployment: %w", err))
		}
		active = append(active, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var recordFailures, lookupFailures []error
	for _, item := range active {
		dep, depErr := s.deploymentByID(ctx, item.deployment)
		if depErr != nil {
			lookupFailures = append(lookupFailures, fmt.Errorf("read ambiguous deployment %s: %w", item.deployment, depErr))
			continue
		}
		if dep.ID != owner.RestoreDeploymentID {
			if err := s.recordDeploymentCleanupCandidates(ctx, env, dep); err != nil {
				recordFailures = append(recordFailures, fmt.Errorf("record cleanup for %s: %w", item.id, err))
			}
		}
	}
	if len(lookupFailures) > 0 {
		return errors.Join(lookupFailures...)
	}
	if len(recordFailures) > 0 {
		return errors.Join(recordFailures...)
	}
	for _, item := range active {
		if item.state == contract.Cancelling {
			err = s.cancelPlatform(ctx, item.id, item.attempt,
				"Legacy ambiguous deployment ownership was recovered conservatively; the recorded serving route was restored and candidate containers were retained for guarded cleanup.")
		} else {
			err = s.failPlatformOperation(ctx, item.id, item.attempt, contract.Conflict,
				"Legacy ambiguous deployment ownership was recovered conservatively; the recorded serving route was restored and candidate containers were retained for guarded cleanup.")
		}
		if err != nil && !errors.Is(err, errNotOwned) && !errors.Is(err, errOperationTerminal) {
			return fmt.Errorf("settle ambiguous operation %s: %w", item.id, err)
		}
	}
	if len(active) > 0 {
		var remaining int
		if err := s.pool.QueryRow(ctx, `SELECT count(*)
            FROM eregion_operations o JOIN eregion_platform_deployments d ON d.operation_id=o.id
            WHERE d.environment_id=$1 AND o.state IN ('queued','running','cancelling')
              AND o.kind IN ('platform.deploy','platform.rollback','platform.restart','platform.promote')`,
			env.ID).Scan(&remaining); err != nil {
			return unavailable(fmt.Errorf("count remaining ambiguous deployments: %w", err))
		}
		if remaining > 0 {
			return nil
		}
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM eregion_platform_deployment_owners
        WHERE environment_id=$1 AND operation_id IS NULL`, env.ID); err != nil {
		return unavailable(fmt.Errorf("delete ambiguous owner: %w", err))
	}
	return nil
}

func (s *Service) reconcileDeploymentOwners(ctx context.Context) error {
	rows, err := s.deploymentOwnerRowsToSettle(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		release, err := s.lockTargets(ctx, []string{"env/" + row.App + "/" + row.Environment})
		if err != nil {
			failures = append(failures, fmt.Errorf("lock deployment owner for %s/%s: %w", row.App, row.Environment, err))
			continue
		}
		env, envErr := s.environment(ctx, row.App, row.Environment)
		if envErr == nil {
			owner, has, ownerErr := s.deploymentOwnerByEnvironment(ctx, s.pool, env.ID)
			if ownerErr != nil {
				envErr = ownerErr
			} else if has {
				_, envErr = s.recoverStaleDeploymentOwner(ctx, owner, env)
			}
		}
		release()
		if envErr != nil {
			failures = append(failures, fmt.Errorf("recover deployment owner for %s/%s: %w", row.App, row.Environment, envErr))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}
