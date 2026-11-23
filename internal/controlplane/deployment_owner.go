package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
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

func deploymentOwnerFromEntities(owner entities.Owner) deploymentOwner {
	return deploymentOwner{
		EnvironmentID: owner.EnvironmentID, OperationID: owner.OperationID,
		DeploymentID: owner.DeploymentID, Attempt: owner.Attempt, Phase: deploymentOwnerPhase(owner.Phase),
		RouteTarget: owner.RouteTarget, RestoreReleaseID: owner.RestoreRelease,
		RestoreDeploymentID: owner.RestoreDeployment,
	}
}

func (s *Service) deploymentOwnerByEnvironment(ctx context.Context, owners *entities.DeploymentOwnerStore, environmentID platform.EnvironmentID) (deploymentOwner, bool, error) {
	owner, found, err := owners.Get(ctx, environmentID)
	if err != nil {
		return deploymentOwner{}, false, unavailable(err)
	}
	if !found {
		return deploymentOwner{}, false, nil
	}
	return deploymentOwnerFromEntities(owner), true, nil
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
	owner, has, err := s.deploymentOwnerByEnvironment(ctx, s.entities.Platform().Owners(), env.ID)
	if err != nil {
		return owner, false, err
	}
	if has {
		return s.renewDeploymentOwner(ctx, owner, dep, attempt)
	}
	phase, target, restoreRelease, restoreDeployment, err := s.deploymentOwnerStart(ctx, dep, env)
	if err != nil {
		return deploymentOwner{}, false, err
	}
	owner = deploymentOwner{
		EnvironmentID: env.ID, OperationID: dep.OperationID, DeploymentID: dep.ID,
		Attempt: attempt, Phase: phase, RouteTarget: target,
		RestoreReleaseID: restoreRelease, RestoreDeploymentID: restoreDeployment,
	}
	raced, err := s.insertDeploymentOwner(ctx, dep, owner)
	if err != nil {
		return deploymentOwner{}, false, err
	}
	if !raced {
		return owner, true, nil
	}
	owner, has, err = s.deploymentOwnerByEnvironment(ctx, s.entities.Platform().Owners(), env.ID)
	if err != nil {
		return owner, false, err
	}
	if !has {
		return deploymentOwner{}, false, unavailable()
	}
	return s.renewDeploymentOwner(ctx, owner, dep, attempt)
}

// deploymentOwnerStart derives route intent from exact step evidence. A route
// that started or completed without a completed verification is intent, not
// proof, so it restores the frozen predecessor before any switch.
func (s *Service) deploymentOwnerStart(ctx context.Context, dep platform.Deployment, env platform.Environment) (deploymentOwnerPhase, platform.DeploymentID, platform.ReleaseID, platform.DeploymentID, error) {
	route, routeHas, err := s.stepRecord(ctx, dep.OperationID, deploymentRouteStep)
	if err != nil {
		return "", "", "", "", err
	}
	public, publicHas, err := s.stepRecord(ctx, dep.OperationID, deploymentPublicStep)
	if err != nil {
		return "", "", "", "", err
	}
	_, restoreHas, err := s.stepRecord(ctx, dep.OperationID, deploymentRestoreRouteStep)
	if err != nil {
		return "", "", "", "", err
	}
	routeCompleted := routeHas && route.State == "completed"
	publicCompleted := publicHas && public.State == "completed"
	restoreRelease, restoreDeployment := frozenPredecessor(dep, env, routeCompleted)
	phase := deploymentOwnerPrepared
	target := restoreDeployment
	if routeCompleted && publicCompleted && !restoreHas {
		return deploymentOwnerServing, dep.ID, restoreRelease, restoreDeployment, nil
	}
	if routeHas || publicHas || restoreHas {
		phase = deploymentOwnerRestoring
	}
	return phase, target, restoreRelease, restoreDeployment, nil
}

func (s *Service) insertDeploymentOwner(ctx context.Context, dep platform.Deployment, owner deploymentOwner) (bool, error) {
	var raced bool
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Platform().Owners().Insert(ctx, entities.OwnerInsert{
			EnvironmentID: owner.EnvironmentID, OperationID: owner.OperationID, DeploymentID: owner.DeploymentID,
			Attempt: owner.Attempt, Phase: string(owner.Phase), RouteTarget: owner.RouteTarget,
			RestoreRelease: owner.RestoreReleaseID, RestoreDeployment: owner.RestoreDeploymentID,
		})
		if err != nil {
			return unavailable(err)
		}
		if !inserted {
			raced = true
			return nil
		}
		return bindDeploymentPredecessor(ctx, tx, dep, owner)
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return false, unavailable(err)
		}
		return false, err
	}
	return raced, nil
}

func bindDeploymentPredecessor(ctx context.Context, tx *entities.StoreTx, dep platform.Deployment, owner deploymentOwner) error {
	if _, err := tx.Platform().Deployments().BindPredecessor(ctx, dep.ID, owner.RestoreReleaseID, owner.RestoreDeploymentID); err != nil {
		return unavailable(err)
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
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if owner.Attempt < attempt {
			renewed, err := tx.Platform().Owners().Renew(ctx, owner.EnvironmentID, owner.OperationID, attempt)
			if err != nil {
				return unavailable(err)
			}
			if !renewed {
				return errNotOwned
			}
			owner.Attempt = attempt
		}
		// Repair or reassert the deployment's frozen predecessor from the owner
		// before any effects. A crash between owner creation and the old separate
		// binding left stale previous pointers; renewal closes that gap atomically.
		return bindDeploymentPredecessor(ctx, tx, dep, owner)
	})
	if err != nil {
		if errors.Is(err, errNotOwned) {
			return owner, false, errNotOwned
		}
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return owner, false, unavailable(err)
		}
		return owner, false, err
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
	var updated entities.Owner
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := guardDeploymentRouteIntent(ctx, tx, owner.OperationID, attempt); err != nil {
			return err
		}
		result, found, err := tx.Platform().Owners().SetRoute(ctx,
			owner.EnvironmentID, owner.OperationID, owner.DeploymentID, target, string(phase), attempt)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return errNotOwned
		}
		updated = result
		return nil
	})
	if err != nil {
		return owner, err
	}
	return deploymentOwnerFromEntities(updated), nil
}

func guardDeploymentRouteIntent(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, attempt uint32) error {
	if id == "" {
		return nil
	}
	op, found, err := tx.Operations().Lock(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	if executor, ok := ctx.Value(buildTargetContextKey{}).(buildTargetExecutor); ok && executor.claim.OperationID == id {
		if !found || op.Attempt != attempt || op.State.Terminal() {
			return errNotOwned
		}
		return guardBuildTargetExecutor(ctx, tx, op)
	}
	_, held, err := tx.UnreleasedTargetClaimByOperation(ctx, id)
	if err != nil {
		return err
	}
	if held || (found && op.Attempt != attempt) {
		return errNotOwned
	}
	return nil
}

// releaseRestoredDeploymentOwnerTx removes an owner after the exact frozen
// restore target is durable. It must never release a serving-phase owner: only
// the success finalizer may do that.
func releaseRestoredDeploymentOwnerTx(ctx context.Context, tx *entities.StoreTx, operationID contract.OperationID) error {
	if _, err := tx.Platform().Owners().DeleteRestoredByOperation(ctx, operationID); err != nil {
		return unavailable(err)
	}
	return nil
}

// releaseServedDeploymentOwnerTx is called only by the successful deployment
// finalizer after the serving pointer and history commit.
func releaseServedDeploymentOwnerTx(ctx context.Context, tx *entities.StoreTx, operationID contract.OperationID) error {
	if _, err := tx.Platform().Owners().DeleteServedByOperation(ctx, operationID); err != nil {
		return unavailable(err)
	}
	return nil
}

func ownerEntity(owner deploymentOwner) entities.Owner {
	return entities.Owner{
		EnvironmentID: owner.EnvironmentID, OperationID: owner.OperationID,
		DeploymentID: owner.DeploymentID, Attempt: owner.Attempt, Phase: string(owner.Phase),
		RouteTarget: owner.RouteTarget, RestoreRelease: owner.RestoreReleaseID,
		RestoreDeployment: owner.RestoreDeploymentID,
	}
}

func (s *Service) releaseRestoredDeploymentOwner(ctx context.Context, owner deploymentOwner) error {
	if _, err := s.entities.Platform().Owners().DeleteRestored(ctx, ownerEntity(owner)); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) releaseServedDeploymentOwner(ctx context.Context, owner deploymentOwner) error {
	if _, err := s.entities.Platform().Owners().DeleteServed(ctx, ownerEntity(owner)); err != nil {
		return unavailable(err)
	}
	return nil
}

// deploymentOwnerRowsToSettle returns owner rows that cannot progress on their
// own: ambiguous legacy rows and rows whose operation is already terminal.
func (s *Service) deploymentOwnerRowsToSettle(ctx context.Context) ([]deploymentOwnerRow, error) {
	rows, err := s.entities.Platform().Owners().ListSettleable(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	out := make([]deploymentOwnerRow, 0, len(rows))
	for _, row := range rows {
		owner := deploymentOwnerFromEntities(row)
		out = append(out, deploymentOwnerRow{deploymentOwner: owner, App: row.App, Environment: row.Environment})
	}
	return out, nil
}

// recoverStaleDeploymentOwner is called while the caller already holds the
// environment lock. It settles ambiguous legacy rows and terminal owners before
// another job can acquire. A live owner returns false.
func (s *Service) recoverStaleDeploymentOwner(ctx context.Context, owner deploymentOwner, env platform.Environment) (bool, error) {
	if owner.Phase == deploymentOwnerAmbiguous || owner.OperationID == "" {
		return true, s.resolveAmbiguousDeploymentOwner(ctx, owner, env)
	}
	op, found, err := s.entities.Operations().Get(ctx, owner.OperationID)
	if err != nil {
		return false, unavailable(err)
	}
	if !found {
		return true, s.settleTerminalDeploymentOwner(ctx, owner, env, nil)
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
		return s.settleServingDeploymentOwner(ctx, owner)
	}
	routeAlreadySafe := (owner.Phase == deploymentOwnerPrepared || owner.Phase == deploymentOwnerRestored) &&
		owner.RouteTarget == owner.RestoreDeploymentID &&
		env.ServingDeploymentID == owner.RestoreDeploymentID
	if !routeAlreadySafe {
		restored, err := s.restoreTerminalOwnerRoute(ctx, owner, env)
		if err != nil {
			return err
		}
		owner = restored
	}
	if hasDep && dep.ID != owner.RestoreDeploymentID {
		// The terminal operation cannot run its normal cleanup steps. Record
		// the candidate containers durably before ownership is released. Use
		// the post-restore target, not the environment snapshot captured before
		// recordDeploymentRestoreServing changed the live serving pointer.
		if err := s.recordDeploymentCleanupCandidates(ctx, env, dep, owner.Attempt); err != nil {
			return err
		}
	}
	return s.releaseRestoredDeploymentOwner(ctx, owner)
}

func (s *Service) settleServingDeploymentOwner(ctx context.Context, owner deploymentOwner) error {
	owner, err := s.setDeploymentOwnerRoute(ctx, owner, owner.DeploymentID, deploymentOwnerServing, owner.Attempt)
	if err != nil {
		return err
	}
	if err := s.syncRoutes(ctx); err != nil {
		return err
	}
	return s.releaseServedDeploymentOwner(ctx, owner)
}

func (s *Service) restoreTerminalOwnerRoute(ctx context.Context, owner deploymentOwner, env platform.Environment) (deploymentOwner, error) {
	owner, err := s.setDeploymentOwnerRoute(ctx, owner, owner.RestoreDeploymentID, deploymentOwnerRestoring, owner.Attempt)
	if err != nil {
		return owner, err
	}
	if err := s.writeRoutesForDeployment(ctx, env, owner.RestoreDeploymentID); err != nil {
		return owner, err
	}
	if err := s.recordDeploymentRestoreServing(ctx, env, owner.RestoreReleaseID, owner.RestoreDeploymentID); err != nil {
		return owner, err
	}
	return s.setDeploymentOwnerRoute(ctx, owner, owner.RestoreDeploymentID, deploymentOwnerRestored, owner.Attempt)
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
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := lockServingPointersTx(ctx, tx, env.ID, release, deployment); err != nil {
			return err
		}
		_, err := tx.Platform().Environments().SetServing(ctx, entities.EnvironmentServing{
			ID: env.ID, ReleaseID: release, DeploymentID: deployment,
			HealthState:  platform.HealthUnknown,
			HealthDetail: "the recorded serving route was restored; restore health was not observed",
			CheckedAt:    time.Now().UTC(),
		})
		if err != nil {
			return unavailable(fmt.Errorf("record restored serving state: %w", err))
		}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	return nil
}

// deploymentCleanupProtected refuses to remove any container set that the
// database currently records as serving or that the owner currently routes to.
// It reads the authoritative rows under the caller's environment lock instead
// of trusting an environment snapshot captured before a switch or restore.
func (s *Service) deploymentCleanupProtected(ctx context.Context, env platform.Environment, deploymentID platform.DeploymentID) (bool, error) {
	facts, found, err := s.entities.Platform().Owners().CleanupProtected(ctx, env.ID)
	if err != nil {
		return false, unavailable(err)
	}
	if !found {
		return false, contract.NewFault(contract.NotFound, "environment is missing", "Inspect the environment before retrying cleanup.")
	}
	serving := facts.ServingDeployment
	phase := deploymentOwnerPhase(facts.OwnerPhase)
	ownerDeployment := facts.OwnerDeployment
	routeTarget := facts.RouteTarget
	restoreTarget := facts.RestoreDeployment
	hasOwner := facts.HasOwner
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

func (s *Service) persistDeploymentCleanupInstances(ctx context.Context, env platform.Environment, dep platform.Deployment, instances []platform.DeploymentInstance, attempt uint32) error {
	// Route recovery records cleanup intent only. Diagnostic capture dispatches
	// node work and has a different uncertainty boundary.
	if ctx.Value(deploymentRouteRecoveryKey{}) == nil {
		s.captureCandidateDiagnostics(ctx, env, dep, attempt)
	}
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
		if err := s.ensureContainerCleanup(ctx, instance.Name, env.ID, nodeName, dep.ID, "candidate",
			contract.NewFault(contract.Unavailable, "deployment cleanup intent", "The maintenance cleanup retries after the serving state is terminal.")); err != nil {
			failures = append(failures, fmt.Errorf("record cleanup for %s: %w", instance.Name, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) persistDeploymentCleanupIntent(ctx context.Context, env platform.Environment, dep platform.Deployment, attempt uint32) error {
	return s.persistDeploymentCleanupInstances(ctx, env, dep, dep.Instances, attempt)
}

func (s *Service) recordDeploymentCleanupCandidates(ctx context.Context, env platform.Environment, dep platform.Deployment, attempt uint32) error {
	return s.persistDeploymentCleanupIntent(ctx, env, dep, attempt)
}

func (s *Service) resolveAmbiguousDeploymentOwner(ctx context.Context, owner deploymentOwner, env platform.Environment) error {
	if _, err := s.entities.Platform().Owners().ResolveAmbiguous(ctx, env.ID, owner.RestoreDeploymentID); err != nil {
		return unavailable(fmt.Errorf("set ambiguous owner restore target: %w", err))
	}
	if err := s.writeRoutesForDeployment(ctx, env, owner.RestoreDeploymentID); err != nil {
		return err
	}
	if err := s.recordDeploymentRestoreServing(ctx, env, owner.RestoreReleaseID, owner.RestoreDeploymentID); err != nil {
		return err
	}
	active, err := s.ambiguousActiveDeployments(ctx, env.ID)
	if err != nil {
		return err
	}
	if err := s.recordAmbiguousCleanup(ctx, env, owner, active); err != nil {
		return err
	}
	if err := s.settleAmbiguousDeployments(ctx, active); err != nil {
		return err
	}
	if len(active) > 0 {
		remaining, err := s.entities.Operations().CountActiveDeploymentsForEnvironment(ctx, env.ID)
		if err != nil {
			return unavailable(fmt.Errorf("count remaining ambiguous deployments: %w", err))
		}
		if remaining > 0 {
			return nil
		}
	}
	if _, err := s.entities.Platform().Owners().ClearAmbiguous(ctx, env.ID); err != nil {
		return unavailable(fmt.Errorf("delete ambiguous owner: %w", err))
	}
	return nil
}

type ambiguousActiveDeployment struct {
	id         contract.OperationID
	attempt    uint32
	state      contract.State
	deployment platform.DeploymentID
}

func (s *Service) ambiguousActiveDeployments(ctx context.Context, environmentID platform.EnvironmentID) ([]ambiguousActiveDeployment, error) {
	rows, err := s.entities.Operations().ListActiveDeploymentsForEnvironment(ctx, environmentID)
	if err != nil {
		return nil, unavailable(fmt.Errorf("list ambiguous active deployments: %w", err))
	}
	active := make([]ambiguousActiveDeployment, 0, len(rows))
	for _, item := range rows {
		active = append(active, ambiguousActiveDeployment{
			id: item.ID, attempt: item.Attempt, state: item.State, deployment: item.DeploymentID,
		})
	}
	return active, nil
}

func (s *Service) recordAmbiguousCleanup(ctx context.Context, env platform.Environment, owner deploymentOwner, active []ambiguousActiveDeployment) error {
	var recordFailures, lookupFailures []error
	for _, item := range active {
		dep, err := s.deploymentByID(ctx, item.deployment)
		if err != nil {
			lookupFailures = append(lookupFailures, fmt.Errorf("read ambiguous deployment %s: %w", item.deployment, err))
			continue
		}
		if dep.ID == owner.RestoreDeploymentID {
			continue
		}
		if err := s.recordDeploymentCleanupCandidates(ctx, env, dep, item.attempt); err != nil {
			recordFailures = append(recordFailures, fmt.Errorf("record cleanup for %s: %w", item.id, err))
		}
	}
	if len(lookupFailures) > 0 {
		return errors.Join(lookupFailures...)
	}
	return errors.Join(recordFailures...)
}

func (s *Service) settleAmbiguousDeployments(ctx context.Context, active []ambiguousActiveDeployment) error {
	for _, item := range active {
		var err error
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
	return nil
}

func (s *Service) reconcileDeploymentOwners(ctx context.Context) error {
	rows, err := s.deploymentOwnerRowsToSettle(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if err := s.reconcileDeploymentOwnerTarget(ctx, row); err != nil {
			failures = append(failures, fmt.Errorf("recover deployment owner for %s/%s: %w", row.App, row.Environment, err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func (s *Service) reconcileDeploymentOwnerTarget(ctx context.Context, row deploymentOwnerRow) error {
	workCtx, claim, acquired, err := s.acquireDeploymentRecoveryTarget(ctx, row.App, row.Environment)
	if err != nil || !acquired {
		return err
	}
	recoveryErr := s.recoverCurrentOwner(workCtx, row)
	return errors.Join(recoveryErr, s.settleDeploymentRecoveryTarget(ctx, claim, recoveryErr == nil))
}

func (s *Service) recoverCurrentOwner(ctx context.Context, row deploymentOwnerRow) error {
	env, err := s.environment(ctx, row.App, row.Environment)
	if err != nil {
		return err
	}
	owner, found, err := s.deploymentOwnerByEnvironment(ctx, s.entities.Platform().Owners(), env.ID)
	if err != nil || !found || owner != row.deploymentOwner {
		return err
	}
	_, err = s.recoverStaleDeploymentOwner(ctx, owner, env)
	return err
}
