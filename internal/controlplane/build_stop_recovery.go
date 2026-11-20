package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

var errNotUndispatchedPendingBuild = errors.New("build is not provably pending before dispatch")

// The stop operation and original operation are locked in that order, as in
// beginBuildStopDispatch. Pack admission locks the original operation before
// creating its claim, running state and intent. The conditional pending update
// also fails closed if a legacy writer has already marked the build running.
func (s *Service) stopPendingBuildBeforeDispatch(ctx context.Context, id contract.OperationID, attempt uint32, build platform.BuildRecord) (bool, error) {
	if build.State != platform.BuildPending || build.OperationID == "" || build.OperationID == id {
		return false, nil
	}
	err := s.finishPlatformOperation(ctx, id, attempt, func(tx *entities.StoreTx, stop *contract.Operation) error {
		if stop.State == contract.Cancelling {
			return errPlatformCancelled
		}
		return s.stopPendingBuildTx(ctx, tx, stop, build)
	})
	if errors.Is(err, errNotUndispatchedPendingBuild) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) stopPendingBuildTx(ctx context.Context, tx *entities.StoreTx, stop *contract.Operation, build platform.BuildRecord) error {
	// Existing stop intent or placement is never reinterpreted as a pre-dispatch
	// cancellation, including partially recorded or legacy states.
	if _, found, err := tx.Steps().GetForUpdate(ctx, stop.ID, deploymentBuildStopStep); err != nil {
		return err
	} else if found {
		return errNotUndispatchedPendingBuild
	}
	if _, found, err := tx.BuildStopPlacement(ctx, stop.ID); err != nil {
		return err
	} else if found {
		return errNotUndispatchedPendingBuild
	}
	original, found, err := tx.Operations().Lock(ctx, build.OperationID)
	if err != nil {
		return err
	}
	if !found || original.State.Terminal() {
		return errNotUndispatchedPendingBuild
	}
	if err := requireNoPackDispatchTx(ctx, tx, original.ID); err != nil {
		return err
	}
	if err := lockPendingBuildParentsTx(ctx, tx, build); err != nil {
		return err
	}
	cancelled, err := tx.Platform().Builds().CancelPending(ctx, build.ID, original.ID, contract.Cancelled, "Build stopped by operator.")
	if err != nil {
		return err
	}
	if !cancelled {
		return errNotUndispatchedPendingBuild
	}
	if build.ReleaseID != "" {
		if _, err := tx.Platform().Releases().MarkFailedByID(ctx, build.ReleaseID, contract.Cancelled, "Build stopped by operator."); err != nil {
			return err
		}
	}
	if original.State != contract.Cancelling {
		if err := transition(ctx, tx, &original, contract.Cancelling, "", "Build stop requested before Pack dispatch."); err != nil {
			return err
		}
	}
	return insertPlatformChange(ctx, tx, stop, "build.stopped", build.App, build.Environment,
		"Stopped build "+string(build.ID)+" before Pack dispatch.", map[string]any{"build_id": build.ID}, nil)
}

func requireNoPackDispatchTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID) error {
	claimed, err := tx.BuildCapacity().Exists(ctx, id)
	if err != nil {
		return err
	}
	if claimed {
		return errNotUndispatchedPendingBuild
	}
	records, err := tx.Steps().List(ctx, id)
	if err != nil {
		return err
	}
	for _, record := range records {
		// Include unknown versions: old or future Pack history is ambiguous.
		if strings.HasPrefix(record.Step, "platform-pack-") {
			return errNotUndispatchedPendingBuild
		}
	}
	return nil
}

func lockPendingBuildParentsTx(ctx context.Context, tx *entities.StoreTx, build platform.BuildRecord) error {
	if build.ReleaseID != "" {
		return lockBuildParentsTx(ctx, tx, build.ID, build.ReleaseID)
	}
	return lockOperationParentsTx(ctx, tx, build.OperationID, true)
}

type buildStopDispatch struct {
	placement entities.BuildStopPlacement
	claim     *entities.BuildCapacityClaim
}

func validateBuildStopResponse(response node.Response, err error, dispatch *buildStopDispatch, exec engine.Execution, hash string, begun bool) (node.Response, error) {
	if err == nil && dispatch != nil {
		err = checkBuildStopResponse(response, exec, hash, begun)
	}
	return response, err
}

// Only a first, pre-intent absence permits dispatch. After intent, all
// uncertain outcomes (including failures) are status-only, never retried by
// the generic node-effect loop.
func checkBuildStopResponse(response node.Response, exec engine.Execution, hash string, begun bool) error {
	if !begun && response.Status == engine.StatusAbsent {
		return nil
	}
	if begun && response.Status == engine.StatusRunning {
		return nil
	}
	if begun && response.Status == engine.StatusCompleted && response.ExecutionID == exec.ExecutionID() &&
		response.Attempt == exec.Attempt && response.InputsHash == hash {
		return nil
	}
	return contract.NewFault(contract.Unavailable, "build stop outcome is uncertain",
		"Inspect the original node journal; do not redispatch this stop.")
}

func (s *Service) beginBuildStopDispatch(ctx context.Context, exec engine.Execution, hash string, dispatch *buildStopDispatch) (bool, error) {
	already := false
	_, owned, err := s.mutateOwned(ctx, exec.OperationID, exec.Attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if err := lockOriginalBuildStopClaim(ctx, tx, dispatch); err != nil {
			return err
		}
		if err := tx.SetBuildStopPlacement(ctx, exec.OperationID, dispatch.placement); err != nil {
			return err
		}
		var err error
		already, err = beginStepTx(ctx, tx, op, exec.Attempt, exec.Step, hash, exec.ExecutionID())
		return err
	})
	if err != nil {
		return false, err
	}
	if !owned {
		return false, errNotOwned
	}
	return already, nil
}

func lockOriginalBuildStopClaim(ctx context.Context, tx *entities.StoreTx, dispatch *buildStopDispatch) error {
	unknown := contract.NewFault(contract.Unavailable, "original build claim changed",
		"Inspect the original builder before dispatching the stop.")
	if dispatch.claim == nil {
		return unknown
	}
	if _, found, err := tx.Operations().Lock(ctx, dispatch.claim.OperationID); err != nil || !found {
		return unknown
	}
	matches, err := tx.BuildCapacity().MatchesForStop(ctx, *dispatch.claim)
	if err != nil {
		return err
	}
	if !matches {
		return unknown
	}
	rec, found, err := tx.Nodes().Connection(ctx, dispatch.placement.Node)
	if err != nil || !found || !strings.HasPrefix(dispatch.placement.Identity, buildNodeIdentity(rec)+":") {
		return contract.NewFault(contract.Unavailable, "build stop node identity changed", "Restore the original node identity before recovery.")
	}
	return nil
}

// New stops go to the original Pack claimant, never the mutable environment.
// A missing or unverified legacy claim cannot prove the Pack location.
func (s *Service) prepareBuildStopDispatch(ctx context.Context, id contract.OperationID, build platform.BuildRecord) (node.Transport, *buildStopDispatch, error) {
	placement, found, err := s.entities.BuildStopPlacement(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var claim *entities.BuildCapacityClaim
	if found {
		if err := s.recoverExistingBuildStop(ctx, id); err != nil {
			return nil, nil, err
		}
	} else {
		placement, claim, err = s.originalBuildStopPlacement(ctx, id, build)
		if err != nil {
			return nil, nil, err
		}
	}
	tr, identity, err := s.buildTransportFor(ctx, placement.Node)
	if err != nil {
		return nil, nil, err
	}
	if identity == "" || strings.HasSuffix(identity, ":unverified") || identity != placement.Identity {
		return nil, nil, contract.NewFault(contract.Unavailable, "build stop node identity cannot be verified",
			"Restore the original authenticated node before recovering this stop.")
	}
	return tr, &buildStopDispatch{placement: placement, claim: claim}, nil
}

func (s *Service) recoverExistingBuildStop(ctx context.Context, id contract.OperationID) error {
	if _, exists, err := s.stepRecord(ctx, id, deploymentBuildStopStep); err != nil {
		return err
	} else if !exists {
		return contract.NewFault(contract.Unavailable, "build stop intent is incomplete", "Inspect the original node journal; do not redispatch.")
	}
	op, exists, err := s.entities.Operations().Get(ctx, id)
	if err != nil || !exists {
		return contract.NewFault(contract.Unavailable, "build stop operation is unavailable", "Retry recovery later.")
	}
	return s.recoverBuildStopEffect(ctx, op)
}

func (s *Service) originalBuildStopPlacement(ctx context.Context, id contract.OperationID, build platform.BuildRecord) (entities.BuildStopPlacement, *entities.BuildCapacityClaim, error) {
	empty := entities.BuildStopPlacement{}
	if _, found, err := s.stepRecord(ctx, id, deploymentBuildStopStep); err != nil {
		return empty, nil, err
	} else if found {
		return empty, nil, contract.NewFault(contract.Unavailable, "build stop dispatch placement is unknown", "Inspect the original node journal; do not dispatch this stop again.")
	}
	if build.State == platform.BuildSucceeded {
		return empty, nil, contract.NewFault(contract.Conflict, "build already succeeded", "Retry with a new build after changing the source if another artifact is needed.")
	}
	claim, found, err := s.entities.BuildCapacity().Find(ctx, build.OperationID)
	if err != nil {
		return empty, nil, err
	}
	if !found || claim.Node == "" || claim.Identity == "" || strings.HasSuffix(claim.Identity, ":unverified") {
		return empty, nil, contract.NewFault(contract.Unavailable, "original build placement is unknown", "Inspect the original builder and its Pack effect; do not guess from the current environment.")
	}
	return entities.BuildStopPlacement{Node: claim.Node, Identity: claim.Identity}, &claim, nil
}

// recoverBuildStopEffect is status-only. In particular, absence, a dead River
// job or a newer attempt is not permission to dispatch or release anything.
func (s *Service) recoverBuildStopEffect(ctx context.Context, op contract.Operation) error {
	record, found, err := s.stepRecord(ctx, op.ID, deploymentBuildStopStep)
	if err != nil || !found || record.State == "completed" {
		return err
	}
	unknown := func() error {
		return contract.NewFault(contract.Unavailable, "build stop effect cannot be verified",
			"Inspect the original authenticated node and its journal; no stop was redispatched.")
	}
	placement, found, err := s.entities.BuildStopPlacement(ctx, op.ID)
	if err != nil {
		return err
	}
	if !found || placement.Identity == "" || strings.HasSuffix(placement.Identity, ":unverified") {
		return unknown()
	}
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
	exec := engine.Execution{OperationID: op.ID, Attempt: record.Attempt, Step: record.Step, RequestID: newRequestID()}
	payload, err := json.Marshal(platform.BuildStopPayload{Step: record.Step, OperationID: string(build.OperationID), PackStep: deploymentPackStep})
	if err != nil {
		return err
	}
	step, err := node.DefaultRegistry().Build(exec, payload)
	if err != nil {
		return err
	}
	inputs, err := step.Inputs()
	if err != nil {
		return err
	}
	if record.ExecutionID != exec.ExecutionID() || record.InputsHash != engine.InputsHash(step.ID(), inputs) {
		return unknown()
	}
	tr, identity, err := s.buildTransportFor(ctx, placement.Node)
	if err != nil || identity != placement.Identity {
		return unknown()
	}
	response, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil || response.Status != engine.StatusCompleted || response.ExecutionID != record.ExecutionID ||
		response.Attempt != record.Attempt || response.InputsHash != record.InputsHash {
		return unknown()
	}
	return s.ackStepWithExpectedExecution(ctx, op.ID, op.Attempt, record.Step, record.InputsHash, response.ExecutionID, response.Result, platformStepTotal, &record)
}

// Recheck intent under the operation lock used by beginStepTx. A worker may
// have committed its intent after the initial recovery read.
func (s *Service) settleReconciledBuildStop(ctx context.Context, op contract.Operation, cancel bool, message string) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		record, found, err := tx.Steps().GetForUpdate(ctx, op.ID, deploymentBuildStopStep)
		if err != nil {
			return err
		}
		if found && record.State != "completed" {
			return contract.NewFault(contract.Unavailable, "build stop has an unacknowledged dispatch intent",
				"Recover the original authenticated node effect before settling the operation.")
		}
		if cancel {
			return s.cancelPlatformTx(ctx, tx, current, op.ID, "Cancellation reconciled at the build stop boundary.")
		}
		return s.failPlatformOperationTx(ctx, tx, current, contract.Unavailable, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) cancelRecoveredBuildStop(ctx context.Context, id contract.OperationID, attempt uint32) error {
	op, found, err := s.entities.Operations().Get(ctx, id)
	if err != nil {
		return err
	}
	if !found || op.Attempt != attempt {
		return errNotOwned
	}
	if err := s.recoverBuildStopEffect(ctx, op); err != nil {
		return err
	}
	return s.settleReconciledBuildStop(ctx, op, true, "")
}
