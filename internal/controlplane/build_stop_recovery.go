package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

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
	if _, found, err := tx.BuildPlacement(ctx, id); err != nil {
		return err
	} else if found {
		return errNotUndispatchedPendingBuild
	}
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
	build     platform.BuildRecord
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
		if err := validateBuildStopAdmissionTx(ctx, tx, op, exec, dispatch); err != nil {
			return err
		}
		if err := cancelOriginalBuildForStopTx(ctx, tx, dispatch); err != nil {
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

func validateBuildStopAdmissionTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, exec engine.Execution, dispatch *buildStopDispatch) error {
	if op.State == contract.Cancelling {
		return errPlatformCancelled
	}
	if err := guardBuildTargetDispatch(ctx, tx, op); err != nil {
		return err
	}
	if op.Kind != platform.KindBuildStop || exec.Step != deploymentBuildStopStep {
		return contract.NewFault(contract.PreconditionFailed, "invalid build stop admission", "Use the build stop execution path.")
	}
	raw, found, err := tx.Operations().Input(ctx, op.ID)
	if err != nil || !found {
		return unavailable(err)
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return err
	}
	input, err := typedOperationInput[*platform.BuildStopInput](normalized.Input)
	if err != nil {
		return err
	}
	if input.BuildID != dispatch.build.ID || dispatch.claim == nil || dispatch.claim.OperationID != dispatch.build.OperationID || dispatch.build.OperationID == op.ID {
		return pointerIntegrityFault()
	}
	if _, found, err := tx.Steps().GetForUpdate(ctx, op.ID, exec.Step); err != nil {
		return err
	} else if found {
		return contract.NewFault(contract.Unavailable, "build stop already has a dispatch intent", "Recover the original effect; do not redispatch.")
	}
	return nil
}

func cancelOriginalBuildForStopTx(ctx context.Context, tx *entities.StoreTx, dispatch *buildStopDispatch) error {
	original, err := lockOriginalBuildStopClaim(ctx, tx, dispatch)
	if err != nil {
		return err
	}
	if original.State.Terminal() {
		return contract.NewFault(contract.Conflict, "build operation is already finished", "Inspect the build before requesting another stop.")
	}
	build := dispatch.build
	if err := lockPendingBuildParentsTx(ctx, tx, build); err != nil {
		return err
	}
	current, err := buildByIDTx(ctx, tx, build.ID)
	if err != nil {
		return err
	}
	if current.OperationID != build.OperationID || current.ReleaseID != build.ReleaseID {
		return pointerIntegrityFault()
	}
	if current.State != platform.BuildPending && current.State != platform.BuildRunning {
		return contract.NewFault(contract.Conflict, "build is already finished", "Inspect the build before requesting another stop.")
	}
	if err := cancelBuildAndReleaseForStopTx(ctx, tx, build); err != nil {
		return err
	}
	if original.State != contract.Cancelling {
		return transition(ctx, tx, &original, contract.Cancelling, "", "Build stop requested; cancelling the pack process.")
	}
	return nil
}

func cancelBuildAndReleaseForStopTx(ctx context.Context, tx *entities.StoreTx, build platform.BuildRecord) error {
	rows, err := tx.Platform().Builds().MarkCancelled(ctx, build.ID, contract.Cancelled, "Build stopped by operator.")
	if err != nil {
		return err
	}
	if rows != 1 {
		return pointerIntegrityFault()
	}
	if build.ReleaseID != "" {
		rows, err = tx.Platform().Releases().MarkFailedByID(ctx, build.ReleaseID, contract.Cancelled, "Build stopped by operator.")
		if err != nil {
			return err
		}
		if rows != 1 {
			return pointerIntegrityFault()
		}
	}
	return nil
}

func lockOriginalBuildStopClaim(ctx context.Context, tx *entities.StoreTx, dispatch *buildStopDispatch) (contract.Operation, error) {
	unknown := contract.NewFault(contract.Unavailable, "original build claim changed",
		"Inspect the original builder before dispatching the stop.")
	if dispatch.claim == nil {
		return contract.Operation{}, unknown
	}
	original, found, err := tx.Operations().Lock(ctx, dispatch.claim.OperationID)
	if err != nil || !found {
		return contract.Operation{}, unknown
	}
	matches, err := tx.BuildCapacity().MatchesForStop(ctx, *dispatch.claim)
	if err != nil {
		return contract.Operation{}, err
	}
	if !matches {
		return contract.Operation{}, unknown
	}
	rec, found, err := tx.Nodes().Connection(ctx, dispatch.placement.Node)
	if err != nil || !found || !strings.HasPrefix(dispatch.placement.Identity, buildNodeIdentity(rec)+":") {
		return contract.Operation{}, contract.NewFault(contract.Unavailable, "build stop node identity changed", "Restore the original node identity before recovery.")
	}
	return original, nil
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
	return tr, &buildStopDispatch{placement: placement, claim: claim, build: build}, nil
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
func (s *Service) failReconciledBuildStop(ctx context.Context, op contract.Operation, message string) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		record, found, err := tx.Steps().GetForUpdate(ctx, op.ID, deploymentBuildStopStep)
		if err != nil {
			return err
		}
		if found && record.State != "completed" {
			return contract.NewFault(contract.Unavailable, "build stop has an unacknowledged dispatch intent",
				"Recover the original authenticated node effect before settling the operation.")
		}
		if current.State == contract.Cancelling {
			return errPlatformCancelled
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

func (s *Service) cancelRecoveredBuildStop(ctx context.Context, id contract.OperationID, attempt uint32) error { //nolint:gocognit,gocyclo // Keep the cancellation fence, node call and guarded settlement in order.
	op, found, err := s.entities.Operations().Get(ctx, id)
	if err != nil {
		return err
	}
	if !found || op.Attempt != attempt {
		return errNotOwned
	}
	if op.Kind != platform.KindBuildStop || op.State != contract.Cancelling {
		return unknownCancellingBuildStop()
	}
	record, found, err := s.stepRecord(ctx, id, deploymentBuildStopStep)
	if err != nil {
		return err
	}
	if !found {
		return s.settleCancellingBuildStop(ctx, op, nil, nil, "Cancellation reconciled before build stop dispatch.")
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
	exec := engine.Execution{OperationID: id, Attempt: record.Attempt, Step: record.Step, RequestID: newRequestID()}
	raw, err := json.Marshal(platform.BuildStopPayload{Step: record.Step, OperationID: string(build.OperationID), PackStep: deploymentPackStep})
	if err != nil {
		return err
	}
	step, err := node.DefaultRegistry().Build(exec, raw)
	if err != nil {
		return unknownCancellingBuildStop()
	}
	inputs, err := step.Inputs()
	if err != nil {
		return err
	}
	if record.Step != deploymentBuildStopStep || record.Attempt == 0 || record.Attempt > attempt ||
		record.ExecutionID != exec.ExecutionID() || record.InputsHash != engine.InputsHash(step.ID(), inputs) {
		return unknownCancellingBuildStop()
	}
	placement, found, err := s.entities.BuildStopPlacement(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	if !found || placement.Node == "" || placement.Identity == "" || strings.HasSuffix(placement.Identity, ":unverified") {
		return unknownCancellingBuildStop()
	}
	if err := s.checkCancellingBuildStop(ctx, op, &record, &placement); err != nil {
		return err
	}
	tr, identity, err := s.buildTransportFor(ctx, placement.Node)
	if err != nil || identity != placement.Identity {
		return unknownCancellingBuildStop()
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := tr.Call(callCtx, node.Request{Method: node.MethodRetireBuildStopMarker, RequestID: exec.RequestID, Execution: exec, InputsHash: record.InputsHash}, nil)
	if err != nil || response.Status != engine.StatusCompleted || response.ExecutionID != record.ExecutionID ||
		response.Attempt != record.Attempt || response.InputsHash != record.InputsHash {
		return unknownCancellingBuildStop()
	}
	status, err := tr.Call(callCtx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil {
		return unknownCancellingBuildStop()
	}
	message, err := s.ackCancellingBuildStop(ctx, op, &record, &placement, status)
	if err != nil {
		return err
	}
	return s.settleCancellingBuildStop(ctx, op, &record, &placement, message)
}

func (s *Service) ackCancellingBuildStop(ctx context.Context, op contract.Operation, record *StepRecord, placement *entities.BuildStopPlacement, status node.Response) (string, error) {
	if status.Status != engine.StatusCompleted {
		if (status.Status != engine.StatusAbsent && status.Status != engine.StatusStale &&
			status.Status != engine.StatusFailed && status.Status != engine.StatusDeferred) ||
			record.State == "completed" ||
			(status.ExecutionID != "" && status.ExecutionID != record.ExecutionID) ||
			(status.Attempt != 0 && status.Attempt != record.Attempt) ||
			(status.InputsHash != "" && status.InputsHash != record.InputsHash) {
			return "", unknownCancellingBuildStop()
		}
		return "Build stop dispatch was fenced; a prior cancellation marker may have been written. Pack is not confirmed stopped.", nil
	}
	if status.ExecutionID != record.ExecutionID || status.Attempt != record.Attempt || status.InputsHash != record.InputsHash {
		return "", unknownCancellingBuildStop()
	}
	guard := func(tx *entities.StoreTx) error {
		current, found, err := tx.Operations().Lock(ctx, op.ID)
		if err != nil || !found {
			return unknownCancellingBuildStop()
		}
		return validateCancellingBuildStopTx(ctx, tx, current, record, placement)
	}
	if err := s.ackStepWithGuard(ctx, op.ID, op.Attempt, record.Step, record.InputsHash, status.ExecutionID, status.Result, platformStepTotal, record, guard); err != nil {
		return "", err
	}
	record.State = "completed"
	return "Build stop cancellation marker was recorded before dispatch was fenced. Pack is not confirmed stopped.", nil
}

func unknownCancellingBuildStop() error {
	return contract.NewFault(contract.Unavailable, "build stop cancellation cannot be verified",
		"Inspect the original authenticated node and dispatch intent; no stop was redispatched.")
}

func validateCancellingBuildStopTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, record *StepRecord, placement *entities.BuildStopPlacement) error {
	if op.Kind != platform.KindBuildStop || op.State != contract.Cancelling {
		return unknownCancellingBuildStop()
	}
	records, err := tx.Steps().List(ctx, op.ID)
	if err != nil {
		return unavailable(err)
	}
	for _, other := range records {
		if strings.HasPrefix(other.Step, "platform-build-stop-") && other.Step != deploymentBuildStopStep {
			return unknownCancellingBuildStop()
		}
	}
	step, found, err := tx.Steps().GetForUpdate(ctx, op.ID, deploymentBuildStopStep)
	if err != nil {
		return unavailable(err)
	}
	actual, placed, err := tx.BuildStopPlacement(ctx, op.ID)
	if err != nil {
		return unavailable(err)
	}
	if record == nil {
		if found || placed {
			return unknownCancellingBuildStop()
		}
		return nil
	}
	if !found || !placed || placement == nil || actual != *placement ||
		step.Step != record.Step || step.Attempt != record.Attempt || step.ExecutionID != record.ExecutionID ||
		step.InputsHash != record.InputsHash || step.State != record.State {
		return unknownCancellingBuildStop()
	}
	rec, found, err := tx.Nodes().Connection(ctx, actual.Node)
	if err != nil || !found || !strings.HasPrefix(actual.Identity, buildNodeIdentity(rec)+":") {
		return unknownCancellingBuildStop()
	}
	return nil
}

func (s *Service) checkCancellingBuildStop(ctx context.Context, op contract.Operation, record *StepRecord, placement *entities.BuildStopPlacement) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		return validateCancellingBuildStopTx(ctx, tx, *current, record, placement)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) settleCancellingBuildStop(ctx context.Context, op contract.Operation, record *StepRecord, placement *entities.BuildStopPlacement, message string) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if err := validateCancellingBuildStopTx(ctx, tx, *current, record, placement); err != nil {
			return err
		}
		if record != nil && record.State != "completed" {
			if err := tx.RecordBuildEffectRetirement(ctx, op.ID, record.Step, record.ExecutionID, record.InputsHash); err != nil {
				return unavailable(err)
			}
		}
		return s.cancelPlatformTx(ctx, tx, current, op.ID, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}
