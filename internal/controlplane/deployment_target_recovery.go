package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

type deploymentRouteRecoveryKey struct{}
type deploymentHealthDispatchKey struct{}

func unknownDeploymentHealth() error {
	return contract.NewFault(contract.Unavailable, "deployment recovery is waiting for health effect proof", "Keep the target reserved until the original node effect can be verified.")
}

func pinDeploymentHealthDispatchTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, attempt uint32, step, hash, executionID string) error {
	placement, ok := ctx.Value(deploymentHealthDispatchKey{}).(entities.PackPlacement)
	if !ok {
		return nil
	}
	d := entities.DeploymentHealthDispatch{Placement: placement, Attempt: attempt, ExecutionID: executionID, InputsHash: hash}
	old, found, err := tx.DeploymentHealthDispatch(ctx, id, step)
	if err != nil {
		return err
	}
	if found {
		if old != d {
			return unknownDeploymentHealth()
		}
		return nil
	}
	// Never attach a placement retrospectively to an existing intent.
	if _, exists, err := tx.Steps().GetForUpdate(ctx, id, step); err != nil {
		return err
	} else if exists {
		return unknownDeploymentHealth()
	}
	return tx.InsertDeploymentHealthDispatch(ctx, id, step, d)
}

// The caller seals the stale claim before making any status requests.
func (s *Service) recoverDeploymentHealth(ctx context.Context, op contract.Operation, claim entities.TargetClaimID) error {
	if err := s.entities.WithinReadTx(ctx, func(tx *entities.StoreTx) error {
		held, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if !found || held.ID != claim || !held.Sealed {
			return unknownDeploymentHealth()
		}
		return nil
	}); err != nil {
		return err
	}
	records, err := s.stepRecords(ctx, op.ID)
	if err != nil {
		return err
	}
	for _, record := range records {
		if !strings.HasPrefix(record.Step, "platform-health-") || record.State == "completed" {
			continue
		}
		if err := s.recoverOneDeploymentHealth(ctx, op, claim, record); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverOneDeploymentHealth(ctx context.Context, op contract.Operation, claim entities.TargetClaimID, record StepRecord) error {
	var d entities.DeploymentHealthDispatch
	err := s.entities.WithinReadTx(ctx, func(tx *entities.StoreTx) error {
		var found bool
		var err error
		d, found, err = tx.DeploymentHealthDispatch(ctx, op.ID, record.Step)
		if err == nil && !found {
			return unknownDeploymentHealth()
		}
		return err
	})
	if err != nil {
		return err
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: record.Attempt, Step: record.Step, RequestID: newRequestID()}
	if d.Attempt != 1 || d.Attempt != record.Attempt || d.ExecutionID != record.ExecutionID || d.ExecutionID != exec.ExecutionID() || d.InputsHash == "" || d.InputsHash != record.InputsHash || d.Placement.Identity == "" || strings.HasSuffix(d.Placement.Identity, ":unverified") {
		return unknownDeploymentHealth()
	}
	tr, identity, err := s.buildTransportFor(ctx, d.Placement.Node)
	if err != nil || identity != d.Placement.Identity {
		return unknownDeploymentHealth()
	}
	response, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, Execution: exec, RequestID: exec.RequestID}, nil)
	// A successful Health.Check can complete without Apply, so the node
	// reports unknown applied attempt (zero). ExecutionID still identifies
	// the exact attempt that recorded the durable effect.
	if err != nil || response.Status != engine.StatusCompleted || response.ExecutionID != d.ExecutionID || (response.Attempt != 0 && response.Attempt != d.Attempt) || response.InputsHash != d.InputsHash {
		return unknownDeploymentHealth()
	}
	guard := func(tx *entities.StoreTx) error {
		current, found, err := tx.DeploymentHealthDispatch(ctx, op.ID, record.Step)
		if err != nil {
			return err
		}
		held, heldFound, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if !found || current != d || !heldFound || held.ID != claim || !held.Sealed {
			return unknownDeploymentHealth()
		}
		return nil
	}
	if err := s.ackStepWithGuard(ctx, op.ID, op.Attempt, record.Step, d.InputsHash, d.ExecutionID, response.Result, platformStepTotal, &record, guard); err != nil {
		return err
	}
	return nil
}

// Only the first execution's acknowledged switch and public verification, with
// the owner still at verification, establish this narrow handoff. A serving
// owner additionally requires the completed serving-route reassertion. A later
// attempt could have overwritten an earlier ambiguous dispatch's step row.
func (s *Service) handoffVerifiedDeployment(ctx context.Context, op contract.Operation, target platform.Target, claim entities.TargetClaimID) error {
	unknown := func() error {
		return contract.NewFault(contract.Unavailable, "deployment recovery is waiting for effect proof", "Keep the target reserved until the original effects can be verified.")
	}
	return s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, found, err := tx.Operations().Lock(ctx, op.ID)
		if err != nil {
			return err
		}
		if !found || current.Attempt != op.Attempt || current.State != contract.Running || current.Target != target.String() || !strings.HasPrefix(claim.Token, "1/") {
			return unknown()
		}
		existing, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if !found || existing.ID != claim || !existing.Sealed || len(existing.Targets) != 1 || existing.Targets[0] != current.Target {
			return unknown()
		}
		if err := verifyDeploymentHandoffTx(ctx, tx, op.ID, target, unknown); err != nil {
			return err
		}
		released, err := tx.ReleaseTargetClaim(ctx, claim)
		if err != nil {
			return err
		}
		if !released {
			return unknown()
		}
		return nil
	})
}

func verifyDeploymentHandoffTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, target platform.Target, unknown func() error) error {
	env, found, err := tx.Platform().Environments().GetByName(ctx, target.App, target.Environment)
	if err != nil {
		return err
	}
	if !found {
		return unknown()
	}
	owner, found, err := tx.Platform().Owners().Get(ctx, env.Environment.ID)
	if err != nil {
		return err
	}
	if !found || owner.OperationID != id || owner.Attempt != 1 || owner.DeploymentID == "" {
		return unknown()
	}
	records, err := tx.Steps().List(ctx, id)
	if err != nil {
		return err
	}
	if owner.Phase == string(deploymentOwnerPrepared) {
		if owner.RouteTarget != owner.RestoreDeployment || env.Environment.ServingDeploymentID != owner.RestoreDeployment {
			return unknown()
		}
		return verifyPreparedHealthHandoff(ctx, tx, id, records, unknown)
	}
	if owner.RouteTarget != owner.DeploymentID ||
		(owner.Phase != string(deploymentOwnerVerifying) && owner.Phase != string(deploymentOwnerServing)) {
		return unknown()
	}
	if !deploymentHandoffStepsComplete(records) {
		return unknown()
	}
	if owner.Phase == string(deploymentOwnerServing) {
		if env.Environment.ServingDeploymentID != owner.DeploymentID || !servingRouteReasserted(records, owner.DeploymentID) {
			return unknown()
		}
	}
	return nil
}

func verifyPreparedHealthHandoff(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, records []entities.StepRecord, unknown func() error) error {
	health := false
	for _, record := range records {
		if record.State != "completed" || record.Attempt != 1 || record.InputsHash == "" || record.Step == deploymentRouteStep || record.Step == deploymentPublicStep || record.Step == deploymentRestoreRouteStep || strings.HasPrefix(record.Step, "platform-cleanup-") {
			return unknown()
		}
		if !strings.HasPrefix(record.Step, "platform-health-") {
			continue
		}
		d, found, err := tx.DeploymentHealthDispatch(ctx, id, record.Step)
		if err != nil {
			return err
		}
		exec := engine.Execution{OperationID: id, Attempt: record.Attempt, Step: record.Step}
		if !found || d.Attempt != record.Attempt || d.ExecutionID != record.ExecutionID || d.ExecutionID != exec.ExecutionID() || d.InputsHash != record.InputsHash || d.Placement.Identity == "" || strings.HasSuffix(d.Placement.Identity, ":unverified") {
			return unknown()
		}
		health = true
	}
	if !health {
		return unknown()
	}
	return nil
}

func servingRouteReasserted(records []entities.StepRecord, deployment platform.DeploymentID) bool {
	raw, err := json.Marshal(map[string]any{"deployment_id": deployment})
	if err != nil {
		return false
	}
	hash := engine.InputsHash(deploymentServingRouteStep, raw)
	for _, record := range records {
		if record.Step == deploymentServingRouteStep && record.InputsHash == hash && record.ExecutionID == "" {
			return true
		}
	}
	return false
}

func deploymentHandoffStepsComplete(records []entities.StepRecord) bool {
	var route, public bool
	for _, record := range records {
		if record.State != "completed" || record.Attempt != 1 || record.InputsHash == "" {
			return false
		}
		if (record.Step == deploymentRouteStep || record.Step == deploymentPublicStep) && record.ExecutionID != "" {
			return false
		}
		route = route || record.Step == deploymentRouteStep
		public = public || record.Step == deploymentPublicStep
	}
	return route && public
}

// An existing executor claim, including a sealed one, excludes this legacy
// recovery path. Recovery must not acquire a second claim around it.
func (s *Service) acquireDeploymentRecoveryTarget(ctx context.Context, app, environment string) (context.Context, entities.TargetClaimID, bool, error) {
	token, err := contract.NewOperationID()
	if err != nil {
		return ctx, entities.TargetClaimID{}, false, err
	}
	claim := entities.TargetClaimID{OperationID: contract.OperationID("deployment-recovery/" + string(token)), Token: string(token)}
	var acquired bool
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		acquired, err = acquirePlatformTargetClaimTx(ctx, tx, claim, []string{"env/" + app + "/" + environment})
		return err
	})
	if err != nil || !acquired {
		return ctx, claim, false, err
	}
	return context.WithValue(ctx, deploymentRouteRecoveryKey{}, true), claim, true, nil
}

func (s *Service) settleDeploymentRecoveryTarget(ctx context.Context, claim entities.TargetClaimID, safe bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.SealTargetClaim(ctx, claim)
		return err
	}); err != nil || !safe {
		return err
	}
	return s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.ReleaseTargetClaim(ctx, claim)
		return err
	})
}
