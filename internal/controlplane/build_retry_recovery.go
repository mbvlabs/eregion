package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

func unknownRetryPack() error {
	return contract.NewFault(contract.Unavailable, "build effect cannot be verified",
		"Inspect the original authenticated builder and its Pack journal; no build was redispatched or retired.")
}

// The caller holds the operation lock. Dispatch takes the same lock before
// inserting any Pack step or placement.
func retryPackHistoryTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, expected *entities.PackPlacement) error {
	placement, placed, err := tx.BuildPlacement(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	records, err := tx.Steps().List(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	if expected == nil {
		if placed {
			return unknownRetryPack()
		}
		for _, record := range records {
			if strings.HasPrefix(record.Step, "platform-pack-") {
				return unknownRetryPack()
			}
		}
		exists, err := tx.BuildCapacity().Exists(ctx, id)
		if err != nil {
			return unavailable(err)
		}
		if exists {
			return unknownRetryPack()
		}
		return nil
	}
	if !placed || placement != *expected || ambiguousPackHistory(records, placed) {
		return unknownRetryPack()
	}
	return nil
}

// recoverBuildRetry is status-only. Neither a dead job nor an absent node
// record permits retirement while a successor could still dispatch.
func (s *Service) recoverBuildRetry(ctx context.Context, op contract.Operation) error {
	record, found, err := s.stepRecord(ctx, op.ID, deploymentPackStep)
	if err != nil {
		return err
	}
	if !found {
		return s.settleRecoveredBuildRetry(ctx, op, nil, nil)
	}
	record, placement, tr, retired, err := s.recoverRetryPackStep(ctx, op, record)
	if err != nil {
		return err
	}
	if retired {
		return nil
	}
	if op.State == contract.Cancelling {
		return s.settleRecoveredBuildRetry(ctx, op, &record, &placement)
	}
	return s.finishRecoveredRetryBuild(ctx, op, record, placement, tr)
}

func (s *Service) recoverRetryPackStep(ctx context.Context, op contract.Operation, record StepRecord) (StepRecord, entities.PackPlacement, node.Transport, bool, error) {
	placement, found, err := s.entities.BuildPlacement(ctx, op.ID)
	if err != nil {
		return record, placement, nil, false, unavailable(err)
	}
	if !found || placement.Identity == "" || strings.HasSuffix(placement.Identity, ":unverified") || s.resolver == nil {
		return record, placement, nil, false, unknownRetryPack()
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: record.Attempt, Step: record.Step, RequestID: newRequestID()}
	hash, err := hex.DecodeString(record.InputsHash)
	if err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != record.InputsHash || record.Attempt == 0 || record.ExecutionID != exec.ExecutionID() {
		return record, placement, nil, false, unknownRetryPack()
	}
	tr, identity, err := s.buildTransportFor(ctx, placement.Node)
	if err != nil || identity != placement.Identity {
		return record, placement, nil, false, unknownRetryPack()
	}
	claim, claimed, err := s.entities.BuildCapacity().Find(ctx, op.ID)
	if err != nil {
		return record, placement, nil, false, unavailable(err)
	}
	record, retired, err := s.ackRecoveredRetryPack(ctx, op, record, placement, tr, exec, claim, claimed)
	if err != nil {
		return record, placement, nil, false, err
	}
	if retired {
		return record, placement, nil, true, nil
	}
	if err := s.releaseRecoveredRetryCapacity(ctx, op, record, placement, claim, claimed); err != nil {
		return record, placement, nil, false, err
	}
	return record, placement, tr, false, nil
}

func (s *Service) ackRecoveredRetryPack(ctx context.Context, op contract.Operation, record StepRecord, placement entities.PackPlacement, tr node.Transport, exec engine.Execution, claim entities.BuildCapacityClaim, claimed bool) (StepRecord, bool, error) {
	if record.State == "completed" {
		return record, false, nil
	}
	response, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil {
		return record, false, unknownRetryPack()
	}
	if response.Status != engine.StatusCompleted && op.State == contract.Cancelling {
		retired, retirementErr := s.retireCancellingRetry(ctx, op, record, placement, tr, exec, claim, claimed, response)
		if retirementErr != nil || retired {
			return record, retired, retirementErr
		}
		response, err = tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	}
	if err != nil || response.Status != engine.StatusCompleted || response.ExecutionID != record.ExecutionID ||
		response.Attempt != record.Attempt || response.InputsHash != record.InputsHash {
		return record, false, unknownRetryPack()
	}
	guard := func(tx *entities.StoreTx) error { return retryPackHistoryTx(ctx, tx, op.ID, &placement) }
	if err := s.ackStepWithGuard(ctx, op.ID, op.Attempt, record.Step, record.InputsHash, response.ExecutionID, response.Result, platformStepTotal, &record, guard); err != nil {
		return record, false, err
	}
	record, found, err := s.stepRecord(ctx, op.ID, deploymentPackStep)
	if err != nil || !found {
		return record, false, unknownRetryPack()
	}
	return record, false, nil
}

func (s *Service) finishRecoveredRetryBuild(ctx context.Context, op contract.Operation, record StepRecord, placement entities.PackPlacement, tr node.Transport) error {
	release, err := s.releaseForOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	build, found, err := s.buildByOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if !found || build.ReleaseID != release.ID || (release.BuildID != "" && release.BuildID != build.ID) {
		return unknownRetryPack()
	}
	if release.Status != platform.ReleaseReady {
		if _, err := (&platformWorker{service: s}).finishBuildRelease(ctx, op.ID, op.Attempt, release, build, tr, record.Result); err != nil {
			return err
		}
	}
	return s.settleRecoveredBuildRetry(ctx, op, &record, &placement)
}

// Release only the exact claim observed before status, after rechecking the
// acknowledged intent and placement under the operation and claim locks.
func (s *Service) releaseRecoveredRetryCapacity(ctx context.Context, op contract.Operation, record StepRecord, placement entities.PackPlacement, claim entities.BuildCapacityClaim, claimed bool) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, _ *contract.Operation) error {
		if err := retryPackHistoryTx(ctx, tx, op.ID, &placement); err != nil {
			return err
		}
		current, found, err := tx.Steps().GetForUpdate(ctx, op.ID, record.Step)
		if err != nil {
			return unavailable(err)
		}
		if err := validateStepAcknowledgement(current, found, record.InputsHash, &record); err != nil || current.State != "completed" {
			return unknownRetryPack()
		}
		return releaseObservedRetryClaim(ctx, tx, op, placement, claim, claimed)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func releaseObservedRetryClaim(ctx context.Context, tx *entities.StoreTx, op contract.Operation, placement entities.PackPlacement, claim entities.BuildCapacityClaim, claimed bool) error {
	if !claimed {
		exists, err := tx.BuildCapacity().Exists(ctx, op.ID)
		if err != nil {
			return unavailable(err)
		}
		if exists {
			return unknownRetryPack()
		}
		return nil
	}
	if claim.Node != placement.Node || claim.Identity != placement.Identity || claim.Attempt > op.Attempt {
		return unknownRetryPack()
	}
	matches, err := tx.BuildCapacity().MatchesForStop(ctx, claim)
	if err != nil {
		return unavailable(err)
	}
	if !matches {
		return unknownRetryPack()
	}
	released, err := tx.BuildCapacity().Release(ctx, op.ID, claim.Node, claim.Attempt)
	if err != nil {
		return unavailable(err)
	}
	if !released {
		return unknownRetryPack()
	}
	return nil
}

// Settlement rechecks all Pack history under the dispatch fence and makes the
// cancellation/success decision using the current operation state.
func (s *Service) settleRecoveredBuildRetry(ctx context.Context, op contract.Operation, record *StepRecord, placement *entities.PackPlacement) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if err := retryPackHistoryTx(ctx, tx, op.ID, placement); err != nil {
			return err
		}
		if err := validateRecoveredRetryStep(ctx, tx, op.ID, record); err != nil {
			return err
		}
		if current.State == contract.Cancelling {
			return s.cancelPlatformTx(ctx, tx, current, op.ID, "Cancellation reconciled at the build boundary.")
		}
		release, found, err := tx.Platform().Releases().GetByOperation(ctx, op.ID)
		if err != nil {
			return unavailable(err)
		}
		if !found || release.Status != platform.ReleaseReady {
			if record != nil {
				return unknownRetryPack()
			}
			return s.failPlatformOperationTx(ctx, tx, current, contract.Unavailable, "release execution ended before the image was verified or built")
		}
		if err := s.appendActionResultTx(ctx, tx, current, contract.Succeeded, "", "Platform action succeeded.", true); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Succeeded, "", "Platform action succeeded; domain state and history are durable.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func validateRecoveredRetryStep(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, record *StepRecord) error {
	if record == nil {
		return nil
	}
	step, found, err := tx.Steps().GetForUpdate(ctx, id, record.Step)
	if err != nil {
		return unavailable(err)
	}
	if err := validateStepAcknowledgement(step, found, record.InputsHash, record); err != nil || step.State != "completed" {
		return unknownRetryPack()
	}
	return nil
}
