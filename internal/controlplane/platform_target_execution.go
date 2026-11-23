package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

// A repeated Run must still belong to the live executor. Checking only at
// Begin would let a retry in the same loop send work after the claim is sealed.
// Requests that already passed this gate are covered by the retained intent.
func (s *Service) checkClaimedNodeDispatch(ctx context.Context, id contract.OperationID, attempt uint32, exec engine.Execution, inputsHash string) error {
	executor, ok := ctx.Value(buildTargetContextKey{}).(buildTargetExecutor)
	if !ok || executor.claim.OperationID != id {
		return nil
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State == contract.Cancelling && !nodeEffectCancellationExempt(exec.Step) {
			return errPlatformCancelled
		}
		record, found, err := tx.Steps().GetForUpdate(ctx, id, exec.Step)
		if err != nil {
			return err
		}
		if !found || record.State != "running" || record.Attempt != attempt ||
			record.ExecutionID != exec.ExecutionID() || record.InputsHash != inputsHash {
			return errNotOwned
		}
		return nil
	})
	if err == nil && !owned {
		return errNotOwned
	}
	return err
}

func platformExecutionTargetsTx(ctx context.Context, tx *entities.StoreTx, normalized platform.Normalized) ([]string, error) {
	targets := []string{normalized.Target.String()}
	switch input := normalized.Input.(type) {
	case *platform.AppDeleteInput:
		// This gate is also held by admissions that add or move descendants.
		locked, err := tx.Platform().Locks().TryTargetXact(ctx, targetLockKey("platform"))
		if err != nil || !locked {
			if err != nil {
				return nil, err
			}
			return nil, river.JobSnooze(deploymentOwnerDefer)
		}
		envs, err := tx.Platform().Environments().ListTargets(ctx, input.App)
		if err != nil {
			return nil, err
		}
		resources, err := tx.Resources().Targets(ctx, input.App, "")
		return append(append(targets, envs...), resources...), err
	case *platform.EnvironmentDeleteInput:
		targets = append(targets, "app/"+input.App)
		resources, err := tx.Resources().Targets(ctx, input.App, input.Environment)
		return append(targets, resources...), err
	case *platform.ResourceApplyInput:
		targets = append(targets, "env/"+input.App+"/"+input.Environment)
	case *platform.ResourceDestroyInput:
		targets = append(targets, "env/"+input.App+"/"+input.Environment)
	}
	return targets, nil
}

func (s *Service) acquirePlatformExecutor(ctx context.Context, op contract.Operation, normalized platform.Normalized) (context.Context, buildTargetExecutor, error) {
	token, err := contract.NewOperationID()
	if err != nil {
		return ctx, buildTargetExecutor{}, err
	}
	executor := buildTargetExecutor{
		claim:  entities.TargetClaimID{OperationID: op.ID, Token: fmt.Sprintf("%d/%s", op.Attempt, token)},
		target: op.Target,
	}
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if current.Kind != op.Kind || current.Target != executor.target {
			return errNotOwned
		}
		if resourceTargetOperation(current.Kind) || domainReadbackTarget(*current) || deploymentLifecycleKind(current.Kind) {
			existing, found, err := tx.UnreleasedTargetClaimByOperation(ctx, current.ID)
			if err != nil {
				return err
			}
			if found {
				return resumePlatformTarget(ctx, tx, current, existing, &executor)
			}
		}
		targets, err := platformExecutionTargetsTx(ctx, tx, normalized)
		if err != nil {
			return err
		}
		acquired, err := acquirePlatformTargetClaimTx(ctx, tx, executor.claim, targets)
		if err == nil && !acquired {
			return river.JobSnooze(deploymentOwnerDefer)
		}
		return err
	})
	if err != nil {
		return ctx, buildTargetExecutor{}, err
	}
	if !owned {
		return ctx, buildTargetExecutor{}, errNotOwned
	}
	return context.WithValue(ctx, buildTargetContextKey{}, executor), executor, nil
}

// Keep failed or interrupted non-build effects reserved unless their specific
// recovery path establishes proof. Node phase or job state alone is not proof.
func (s *Service) settlePlatformExecutor(ctx context.Context, executor buildTargetExecutor, workErr error) error {
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
		records, err := tx.Steps().List(ctx, op.ID)
		if err != nil {
			return err
		}
		var restored bool
		for _, record := range records {
			if record.State != "completed" {
				return nil
			}
			restored = restored || record.Step == deploymentRestoreRouteStep
		}
		if executor.dnsReadback && workErr == nil && op.State.Terminal() {
			if err := guardDefinitionDNSTerminalTx(ctx, tx, &op); err != nil {
				return err
			}
			_, err = tx.ReleaseTargetClaim(ctx, executor.claim)
			return err
		}
		if !mayYieldPlatformTarget(op, workErr, restored) {
			return nil
		}
		_, err = tx.ReleaseTargetClaim(ctx, executor.claim)
		return err
	})
}

func mayYieldPlatformTarget(op contract.Operation, workErr error, restored bool) bool {
	if op.State == contract.Succeeded && workErr == nil {
		return true
	}
	if restored && deploymentLifecycleKind(op.Kind) && op.State.Terminal() && workErr == nil {
		return true
	}
	// These workers have returned, and the caller has checked that every
	// recorded step completed. A later attempt may resume the same operation.
	// Opaque extensions and provider effects need their own settlement proof.
	return op.Kind == platform.KindResourceApply || op.Kind == platform.KindResourceDestroy
}

func (w *platformWorker) workWithPlatformTarget(ctx context.Context, op contract.Operation, raw []byte, attempt uint32) error {
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return w.settle(ctx, op.ID, attempt, err)
	}
	workCtx, executor, err := w.service.acquirePlatformExecutor(ctx, op, normalized)
	if err != nil {
		return err
	}
	if executor.recover && deploymentLifecycleKind(op.Kind) {
		if err := w.service.recoverDeploymentHealth(ctx, op, executor.claim); err != nil {
			return err
		}
		if err := w.service.handoffVerifiedDeployment(ctx, op, normalized.Target, executor.claim); err != nil {
			return err
		}
		return river.JobSnooze(deploymentOwnerDefer)
	}
	if executor.recover {
		if domainReadbackTarget(op) {
			executor.dnsReadback = true
			workCtx = context.WithValue(workCtx, buildTargetContextKey{}, executor)
			recoveryErr := w.resumeDomainReadback(workCtx, op, attempt, normalized)
			return errors.Join(recoveryErr, w.service.settlePlatformExecutor(ctx, executor, recoveryErr))
		}
		return w.service.resumeResourceExecution(ctx, op, executor)
	}
	workErr := w.workClaimed(workCtx, op, raw, attempt)
	return errors.Join(workErr, w.service.settlePlatformExecutor(ctx, executor, workErr))
}

func domainReadbackTarget(op contract.Operation) bool {
	return op.Kind == platform.KindDomainAdd || op.Kind == platform.KindDomainRemove
}

func (w *platformWorker) resumeDomainReadback(ctx context.Context, op contract.Operation, attempt uint32, normalized platform.Normalized) error {
	if op.Kind == platform.KindDomainRemove && op.State != contract.Cancelling {
		// Reuse the existing cleanup latch and its read-only provider probe.
		// Never fall through to dispatch when the latch finds no cleanup.
		if err := w.guardDomainRemoveRecovery(ctx, op, attempt); err != nil {
			return err
		}
		current, _, err := w.service.operationSnapshot(ctx, op.ID)
		if err != nil || current.State.Terminal() {
			return err
		}
		return contract.NewFault(contract.Unavailable, "domain cleanup has no approved readback evidence", "Keep the reservation until the original provider effect can be verified.")
	}
	if op.State == contract.Cancelling {
		if handled, err := w.stopDefinitionDNSChild(ctx, op, attempt); handled {
			return err
		}
	} else {
		child, err := w.service.definitionDNSChild(ctx, op.ID)
		if err != nil {
			return err
		}
		// A pending provider intent proves this attempt had not reached route
		// publication. With no pending intent we cannot make that inference.
		if child != nil && child.Plan != nil && len(child.Progress) > 0 && child.Progress[len(child.Progress)-1].Result == nil {
			return w.runDomain(ctx, op.ID, attempt, normalized)
		}
	}
	return contract.NewFault(contract.Unavailable, "domain effect has no approved readback plan", "Keep its reservation until the original effect can be verified.")
}

func (s *Service) resumeResourceExecution(ctx context.Context, op contract.Operation, executor buildTargetExecutor) error {
	if err := s.recoverResourceDispatches(ctx, op, executor.claim); err != nil {
		return err
	}
	if op.State == contract.Cancelling {
		if err := s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled after the original resource effects completed."); err != nil {
			return err
		}
	}
	if err := s.settlePlatformExecutor(ctx, executor, nil); err != nil {
		return err
	}
	return river.JobSnooze(deploymentOwnerDefer)
}

func resumePlatformTarget(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, existing entities.TargetClaim, executor *buildTargetExecutor) error {
	if !slices.Contains(existing.Targets, op.Target) {
		return errNotOwned
	}
	return resumeSealedExecutor(ctx, tx, op, existing, executor)
}

func resourceTargetOperation(kind string) bool {
	return kind == platform.KindResourceApply || kind == platform.KindResourceDestroy
}

// Resource execution only mutates Docker through journalled node steps.
// If recovery cannot account for all of them, keep the reservation instead
// of treating a discarded job as permission to retry against the same target.
func (s *Service) reconcileResourceTarget(ctx context.Context, op contract.Operation) error {
	var claim entities.TargetClaim
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if err := checkBuildTargetJobEnded(ctx, tx, current.ID); err != nil {
			return err
		}
		record, found, err := tx.UnreleasedTargetClaimByOperation(ctx, current.ID)
		if err != nil || !found {
			return err
		}
		claim = record
		if !record.Sealed {
			_, err = tx.SealTargetClaim(ctx, record.ID)
		}
		return err
	})
	if err != nil || !owned {
		return err
	}
	if claim.ID.Token == "" {
		// A fresh install must never infer that an old unowned resource
		// operation did not dispatch an effect.
		return contract.NewFault(contract.Unavailable, "resource recovery has no target reservation", "Inspect the node effect before resubmitting this operation.")
	}
	if err := s.recoverResourceDispatches(ctx, op, claim.ID); err != nil {
		return err
	}
	if err := s.reconcilePlatformOperation(ctx, op); err != nil {
		return err
	}
	return s.settlePlatformExecutor(ctx, buildTargetExecutor{claim: claim.ID, target: op.Target}, nil)
}
