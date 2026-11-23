package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

type buildTargetContextKey struct{}

type buildTargetExecutor struct {
	claim       entities.TargetClaimID
	target      string
	recover     bool
	dnsReadback bool
}

func usesBuildTargetClaim(kind string) bool {
	return kind == platform.KindBuildRetry || kind == platform.KindBuildStop
}

func (w *platformWorker) workWithBuildTarget(ctx context.Context, op contract.Operation, raw json.RawMessage, attempt uint32) error {
	workCtx, executor, err := w.service.acquireBuildTarget(ctx, op)
	if err != nil {
		return err
	}
	if executor.recover {
		recoveryErr := w.service.reconcilePlatformOperation(ctx, op)
		return errors.Join(recoveryErr, w.service.settleBuildTarget(ctx, executor))
	}
	workErr := w.workClaimed(workCtx, op, raw, attempt)
	if err := w.service.settleBuildTarget(ctx, executor); err != nil {
		return errors.Join(workErr, err)
	}
	return workErr
}

func (s *Service) acquireBuildTarget(ctx context.Context, op contract.Operation) (context.Context, buildTargetExecutor, error) {
	token, err := contract.NewOperationID()
	if err != nil {
		return ctx, buildTargetExecutor{}, err
	}
	executor := buildTargetExecutor{claim: entities.TargetClaimID{OperationID: op.ID, Token: fmt.Sprintf("%d/%s", op.Attempt, token)}, target: op.Target}
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if !usesBuildTargetClaim(current.Kind) || current.Target != executor.target {
			return errNotOwned
		}
		existing, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if found {
			return resumeBuildTarget(ctx, tx, current, existing, &executor)
		}
		targets, err := buildExecutionTargetsTx(ctx, tx, current)
		if err != nil {
			return err
		}
		acquired, err := acquirePlatformTargetClaimTx(ctx, tx, executor.claim, targets)
		if err != nil {
			return err
		}
		if !acquired {
			return river.JobSnooze(deploymentOwnerDefer)
		}
		return nil
	})
	if err != nil {
		return ctx, buildTargetExecutor{}, err
	}
	if !owned {
		return ctx, buildTargetExecutor{}, errNotOwned
	}
	return context.WithValue(ctx, buildTargetContextKey{}, executor), executor, nil
}

func buildExecutionTargetsTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) ([]string, error) {
	if op.Kind != platform.KindBuildRetry {
		return []string{op.Target}, nil
	}
	id := platform.BuildID(strings.TrimPrefix(op.Target, "build/"))
	build, found, err := tx.Platform().Builds().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		// The normal execution path reports the missing build without effects.
		return []string{op.Target}, nil
	}
	return []string{op.Target, "env/" + build.App + "/" + build.Environment}, nil
}

func resumeBuildTarget(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, existing entities.TargetClaim, executor *buildTargetExecutor) error {
	if !slices.Contains(existing.Targets, op.Target) {
		return errNotOwned
	}
	return resumeSealedExecutor(ctx, tx, op, existing, executor)
}

func resumeSealedExecutor(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, existing entities.TargetClaim, executor *buildTargetExecutor) error {
	if !existing.Sealed {
		generation, _, _ := strings.Cut(existing.ID.Token, "/")
		attempt, err := strconv.ParseUint(generation, 10, 32)
		if err != nil || attempt >= uint64(op.Attempt) {
			return river.JobSnooze(deploymentOwnerDefer)
		}
		sealed, err := tx.SealTargetClaim(ctx, existing.ID)
		if err != nil {
			return err
		}
		if !sealed {
			return river.JobSnooze(deploymentOwnerDefer)
		}
	}
	executor.claim, executor.recover = existing.ID, true
	return nil
}

// Check the exact execution token under the same transaction as operation
// writes, including node dispatch intent. An equal River attempt is not a token.
func guardBuildTargetExecutor(ctx context.Context, tx *entities.StoreTx, op contract.Operation) error {
	executor, ok := ctx.Value(buildTargetContextKey{}).(buildTargetExecutor)
	if !ok || executor.claim.OperationID != op.ID {
		return nil
	}
	if executor.dnsReadback {
		claim, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if !domainReadbackTarget(op) || !found || !claim.Sealed || claim.ID != executor.claim {
			return errNotOwned
		}
		return nil
	}
	held, err := tx.LockActiveTargetClaimForTarget(ctx, executor.claim, op.Target)
	if err != nil {
		return err
	}
	if !held {
		return errNotOwned
	}
	return nil
}

// Normal dispatch is never a recovery operation. The missing-token check
// prevents an internal caller from dispatching around a sealed reservation.
func guardBuildTargetDispatch(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) error {
	if !usesBuildTargetClaim(op.Kind) {
		return nil
	}
	executor, ok := ctx.Value(buildTargetContextKey{}).(buildTargetExecutor)
	if ok && executor.claim.OperationID == op.ID {
		// mutateOwned already locked this exact claim before invoking admission.
		return nil
	}
	_, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if found {
		return errNotOwned
	}
	// Legacy in-process callers do not carry a claim. Production workers
	// enter through Work, which acquires before allowing any dispatch.
	return nil
}

// Synchronous worker return is not quiescence. Seal first and release only
// when no node dispatch was admitted or all intents have durable completion.
func (s *Service) settleBuildTarget(ctx context.Context, executor buildTargetExecutor) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.SealTargetClaim(ctx, executor.claim)
		return err
	}); err != nil {
		return err
	}
	return s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		op, found, err := tx.Operations().Lock(ctx, executor.claim.OperationID)
		if err != nil || !found {
			return err
		}
		quiescent, err := buildTargetQuiescentTx(ctx, tx, op)
		if err != nil || !quiescent {
			return err
		}
		_, err = tx.ReleaseTargetClaim(ctx, executor.claim)
		return err
	})
}

func buildTargetQuiescentTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation) (bool, error) {
	records, err := tx.Steps().List(ctx, op.ID)
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		err := requireNoBuildTargetIntentTx(ctx, tx, op)
		return err == nil, err
	}
	// Read-only image inspection can run after Pack. A durable step
	// completion does not mean the build's release has been settled.
	if !op.State.Terminal() {
		return false, nil
	}
	for _, record := range records {
		if record.State == "completed" {
			continue
		}
		retired, err := tx.BuildEffectRetired(ctx, record)
		if err != nil || !retired {
			return false, err
		}
	}
	return true, nil
}

func requireNoBuildTargetIntentTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation) error {
	if op.Kind == platform.KindBuildRetry {
		return retryPackHistoryTx(ctx, tx, op.ID, nil)
	}
	_, placed, err := tx.BuildStopPlacement(ctx, op.ID)
	if err != nil {
		return err
	}
	if placed {
		return unknownCancellingBuildStop()
	}
	return nil
}

// The periodic reconciler may take over observation of a discarded job, but
// must seal its executor first so that no new intent can race the observation.
func (s *Service) reconcileBuildTarget(ctx context.Context, op contract.Operation) error {
	var executor buildTargetExecutor
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if err := checkBuildTargetJobEnded(ctx, tx, current.ID); err != nil {
			return err
		}
		claim, found, err := tx.UnreleasedTargetClaimByOperation(ctx, current.ID)
		if err != nil || !found {
			return err
		}
		if !slices.Contains(claim.Targets, current.Target) {
			return errNotOwned
		}
		executor = buildTargetExecutor{claim: claim.ID, target: current.Target}
		if claim.Sealed {
			return nil
		}
		_, err = tx.SealTargetClaim(ctx, claim.ID)
		return err
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	recoveryErr := s.reconcilePlatformOperation(ctx, op)
	if executor.claim.Token == "" {
		return recoveryErr
	}
	return errors.Join(recoveryErr, s.settleBuildTarget(ctx, executor))
}

func checkBuildTargetJobEnded(ctx context.Context, tx *entities.StoreTx, id contract.OperationID) error {
	job, found, err := tx.Operations().JobID(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return errNotOwned
	}
	state, found, err := tx.RiverJobs().GetState(ctx, job)
	if err != nil {
		return err
	}
	if !found {
		return errNotOwned
	}
	switch state {
	case "cancelled", "discarded", "completed":
		return nil
	default:
		return errNotOwned
	}
}

func (s *Service) reconcileSettledTargets(ctx context.Context) error {
	claims, err := s.entities.SettledTargetClaims(ctx, 16)
	if err != nil {
		return err
	}
	var result error
	for _, claim := range claims {
		executor := buildTargetExecutor{claim: claim.ID, target: claim.Target}
		if usesBuildTargetClaim(claim.Kind) {
			result = errors.Join(result, s.settleBuildTarget(ctx, executor))
			continue
		}
		err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
			return checkBuildTargetJobEnded(ctx, tx, claim.ID.OperationID)
		})
		if errors.Is(err, errNotOwned) {
			continue
		}
		if err == nil {
			err = s.settlePlatformExecutor(ctx, executor, nil)
		}
		result = errors.Join(result, err)
	}
	return result
}
