package controlplane

import (
	"context"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

func retryStatusMatchesIntent(response node.Response, record StepRecord) bool {
	return (response.ExecutionID == "" || response.ExecutionID == record.ExecutionID) &&
		(response.Attempt == 0 || response.Attempt == record.Attempt) &&
		(response.InputsHash == "" || response.InputsHash == record.InputsHash)
}

// Cancelling is irreversible: Pack admission serializes on the operation row
// and rejects it, including repeated dispatch from an existing step. Retirement
// fences Runs already admitted before cancellation, under the node effect lock.
func (s *Service) retireCancellingRetry(ctx context.Context, op contract.Operation, record StepRecord, placement entities.PackPlacement, tr node.Transport, exec engine.Execution, claim entities.BuildCapacityClaim, claimed bool, status node.Response) (bool, error) {
	if !claimed || !retryStatusMatchesIntent(status, record) {
		return false, unknownRetryPack()
	}
	if err := s.checkCancellingRetryIntent(ctx, op, record, placement, claim, false); err != nil {
		return false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := tr.Call(callCtx, node.Request{Method: node.MethodRetireBuild, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil || response.Status != engine.StatusCompleted || response.ExecutionID != record.ExecutionID {
		return false, unknownRetryPack()
	}
	// An already-admitted Run may have completed before retirement took the
	// effect lock. Recover that completion instead of discarding it.
	status, err = tr.Call(callCtx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil || !retryStatusMatchesIntent(status, record) {
		return false, unknownRetryPack()
	}
	if status.Status == engine.StatusCompleted {
		return false, nil
	}
	if err := s.checkCancellingRetryIntent(ctx, op, record, placement, claim, true); err != nil {
		return false, err
	}
	return true, nil
}

// The first call establishes the cancellation fence before touching the node.
// The second atomically rechecks it, releases the exact claim, and cancels.
func (s *Service) checkCancellingRetryIntent(ctx context.Context, op contract.Operation, record StepRecord, placement entities.PackPlacement, claim entities.BuildCapacityClaim, settle bool) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if err := validateCancellingRetryIntent(ctx, tx, current, record, placement, claim); err != nil {
			return err
		}
		if !settle {
			return nil
		}
		released, err := tx.BuildCapacity().Release(ctx, op.ID, claim.Node, claim.Attempt)
		if err != nil {
			return unavailable(err)
		}
		if !released {
			return unknownRetryPack()
		}
		return s.cancelPlatformTx(ctx, tx, current, op.ID, "Cancellation reconciled after the original builder confirmed Pack retirement.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func validateCancellingRetryIntent(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, record StepRecord, placement entities.PackPlacement, claim entities.BuildCapacityClaim) error {
	if op.Kind != platform.KindBuildRetry || op.State != contract.Cancelling || claim.OperationID != op.ID ||
		claim.Node != placement.Node || claim.Identity != placement.Identity ||
		claim.Attempt < record.Attempt || claim.Attempt > op.Attempt {
		return unknownRetryPack()
	}
	if err := retryPackHistoryTx(ctx, tx, op.ID, &placement); err != nil {
		return err
	}
	step, found, err := tx.Steps().GetForUpdate(ctx, op.ID, record.Step)
	if err != nil {
		return unavailable(err)
	}
	if err := validateStepAcknowledgement(step, found, record.InputsHash, &record); err != nil || step.State != record.State || step.State == "completed" {
		return unknownRetryPack()
	}
	matches, err := tx.BuildCapacity().MatchesForRecovery(ctx, claim)
	if err != nil {
		return unavailable(err)
	}
	if !matches {
		return unknownRetryPack()
	}
	return nil
}
