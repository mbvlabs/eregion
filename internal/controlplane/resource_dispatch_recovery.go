package controlplane

import (
	"context"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
)

type resourceDispatchKey struct{}

func requireSettledResourceDispatchesTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) error {
	if !resourceTargetOperation(op.Kind) {
		return nil
	}
	claim, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
	if err != nil || !found {
		return err
	}
	records, err := tx.Steps().List(ctx, claim.ID.OperationID)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.State != "completed" {
			return unknownResourceEffect()
		}
	}
	return nil
}

func pinResourceDispatchTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, placement entities.PackPlacement) error {
	if placement.Node == "" || placement.Identity == "" {
		return unknownResourceEffect()
	}
	original, found, err := tx.ResourceDispatchNode(ctx, id)
	if err != nil {
		return err
	}
	if found {
		if original != placement {
			return unknownResourceEffect()
		}
		return nil
	}
	records, err := tx.Steps().List(ctx, id)
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return unknownResourceEffect()
	}
	return tx.InsertResourceDispatchNode(ctx, id, placement)
}

// An already-admitted step keeps the same authenticated destination even for
// status reads that might acknowledge its completion without a new Begin.
func (s *Service) checkResourceDispatchPlacement(ctx context.Context, id contract.OperationID) error {
	placement, ok := ctx.Value(resourceDispatchKey{}).(entities.PackPlacement)
	if !ok {
		return nil
	}
	return s.entities.WithinReadTx(ctx, func(tx *entities.StoreTx) error {
		original, found, err := tx.ResourceDispatchNode(ctx, id)
		if err != nil {
			return err
		}
		if found && original != placement {
			return unknownResourceEffect()
		}
		return nil
	})
}

func unknownResourceEffect() error {
	return contract.NewFault(contract.Unavailable, "resource recovery is waiting for node effect proof", "Keep the target reserved until the original node effect can be verified.")
}

func (s *Service) recoverResourceDispatches(ctx context.Context, op contract.Operation, claim entities.TargetClaimID) error {
	records, err := s.stepRecords(ctx, op.ID)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.State != "completed" {
			if err := s.recoverResourceDispatch(ctx, op, claim, record); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) recoverResourceDispatch(ctx context.Context, op contract.Operation, owner entities.TargetClaimID, record StepRecord) error {
	var placement entities.PackPlacement
	err := s.entities.WithinReadTx(ctx, func(tx *entities.StoreTx) error {
		var found bool
		var err error
		placement, found, err = tx.ResourceDispatchNode(ctx, op.ID)
		if err == nil && !found {
			return unknownResourceEffect()
		}
		return err
	})
	if err != nil {
		return err
	}
	if placement.Identity == "" || strings.HasSuffix(placement.Identity, ":unverified") {
		return unknownResourceEffect()
	}
	tr, identity, err := s.buildTransportFor(ctx, placement.Node)
	if err != nil || identity != placement.Identity {
		return unknownResourceEffect()
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: record.Attempt, Step: record.Step, RequestID: newRequestID()}
	response, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, Execution: exec, RequestID: exec.RequestID}, nil)
	if err != nil || response.Status != engine.StatusCompleted || record.ExecutionID != exec.ExecutionID() ||
		response.ExecutionID != record.ExecutionID || response.Attempt != record.Attempt || response.InputsHash != record.InputsHash {
		return unknownResourceEffect()
	}
	guard := func(tx *entities.StoreTx) error {
		current, found, err := tx.ResourceDispatchNode(ctx, op.ID)
		if err != nil {
			return err
		}
		if !found || current != placement {
			return unknownResourceEffect()
		}
		claim, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if !found || claim.ID != owner || !claim.Sealed {
			return errNotOwned
		}
		return nil
	}
	return s.ackStepWithGuard(ctx, op.ID, op.Attempt, record.Step, record.InputsHash, response.ExecutionID, response.Result, platformStepTotal, &record, guard)
}
