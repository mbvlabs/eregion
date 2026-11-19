package controlplane

import (
	"context"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/node"
)

func slotView(c entities.BuildCapacityClaim) api.BuildSlot {
	view := api.BuildSlot{OperationID: c.OperationID, Builder: c.Node, ClaimedAt: c.CreatedAt, State: c.State}
	if age := time.Since(c.CreatedAt); age > 0 {
		view.AgeSeconds = int64(age.Seconds())
	}
	if c.Identity == "" || strings.HasSuffix(c.Identity, ":unverified") {
		view.Reason = "builder identity is unverified"
	}
	return view
}

func (s *Service) ListBuildSlots(ctx context.Context, actor contract.Actor) ([]api.BuildSlot, error) {
	if err := actor.Validate(); err != nil {
		return nil, err
	}
	if actor.Kind != "operator" {
		return nil, contract.NewFault(contract.Forbidden, "build slots require an operator", "Use the trusted operator boundary.")
	}
	claims, err := s.entities.BuildCapacity().List(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	slots := make([]api.BuildSlot, 0, len(claims))
	for _, claim := range claims {
		slots = append(slots, slotView(claim))
	}
	return slots, nil
}

func (s *Service) ClearBuildSlot(ctx context.Context, actor contract.Actor, id contract.OperationID, dryRun, legacyLocal bool) (api.BuildSlotClearResult, error) {
	var result api.BuildSlotClearResult
	if err := actor.Validate(); err != nil {
		return result, err
	}
	if actor.Kind != "operator" {
		return result, contract.NewFault(contract.Forbidden, "build slots require an operator", "Use the trusted operator boundary.")
	}
	if err := id.Validate(); err != nil {
		return result, err
	}
	claim, found, err := s.entities.BuildCapacity().Find(ctx, id)
	if err != nil {
		return result, unavailable(err)
	}
	if !found {
		return result, contract.NewFault(contract.NotFound, "build slot not found", "List the current build slots.")
	}
	result = api.BuildSlotClearResult{Slot: slotView(claim), DryRun: dryRun}
	if claim.State != "succeeded" && claim.State != "failed" && claim.State != "cancelled" {
		return result, contract.NewFault(contract.PreconditionFailed, "build operation is still active", "Use eregion build stop or eregion operations cancel and wait for a terminal state.")
	}
	if dryRun {
		if legacyLocal {
			result.Reason = "would verify the legacy local builder and original dispatch, then require locked quiescence proof and an unchanged terminal claim"
		} else {
			result.Reason = "would retry retirement on the pinned builder; release requires confirmation of retirement and an unchanged terminal claim"
		}
		return result, nil
	}
	var proof entities.StepRecord
	var reason string
	if legacyLocal {
		proof, reason = s.retireLegacyLocalBuild(ctx, claim)
	} else {
		reason = s.retireBuildOnPinnedNode(ctx, claim)
	}
	if reason != "" {
		result.Reason = reason
		return result, nil
	}
	return s.deleteBuildSlot(ctx, actor, claim, proof, legacyLocal, result)
}

func (s *Service) deleteBuildSlot(ctx context.Context, actor contract.Actor, claim entities.BuildCapacityClaim, proof entities.StepRecord, legacyLocal bool, result api.BuildSlotClearResult) (api.BuildSlotClearResult, error) {
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if legacyLocal {
			valid, err := legacyBuildProofUnchanged(ctx, tx, claim, proof)
			if err != nil {
				return err
			}
			if !valid {
				result.Reason = "builder or dispatch changed"
				return nil
			}
		}
		deleted, err := tx.BuildCapacity().DeleteTerminal(ctx, claim)
		if err != nil {
			return err
		}
		if !deleted {
			result.Reason = "claim changed or was already released"
			return nil
		}
		detail := ""
		if legacyLocal {
			detail = "legacy local recovery"
		}
		if err := tx.Audit().Record(ctx, integrations.AuditEntry{At: time.Now().UTC(), Actor: actor.Subject, Action: "build.slot.clear", Target: string(claim.OperationID), Outcome: "succeeded", Detail: detail}); err != nil {
			return err
		}
		result.Cleared = true
		return nil
	})
	if err != nil {
		return api.BuildSlotClearResult{}, unavailable(err)
	}
	return result, nil
}

func legacyBuildProofUnchanged(ctx context.Context, tx *entities.StoreTx, claim entities.BuildCapacityClaim, proof entities.StepRecord) (bool, error) {
	_, found, err := tx.Nodes().LockBuildConcurrency(ctx, claim.Node)
	if err != nil || !found {
		return false, err
	}
	rec, found, err := tx.Nodes().Connection(ctx, claim.Node)
	if err != nil || !found {
		return false, err
	}
	step, found, err := tx.Steps().GetForUpdate(ctx, claim.OperationID, deploymentPackStep)
	if err != nil || !found {
		return false, err
	}
	return claim.Identity == buildNodeIdentity(rec)+":unverified" &&
		step.Attempt == proof.Attempt && step.ExecutionID == proof.ExecutionID && step.InputsHash == proof.InputsHash, nil
}

func (s *Service) retireLegacyLocalBuild(ctx context.Context, claim entities.BuildCapacityClaim) (entities.StepRecord, string) {
	var empty entities.StepRecord
	if s.resolver == nil || s.registryHostIdentity == nil {
		return empty, "verified local builder is unavailable"
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tr, identity, err := s.resolver.transportForBuild(callCtx, claim.Node)
	local, ok := tr.(*node.Local)
	binding := s.registryHostIdentity
	if err != nil || !ok || identity != claim.Identity || !strings.HasSuffix(identity, ":unverified") ||
		local.NodeIdentity() != claim.Node || binding.NodeName != claim.Node || local.StateDir() != binding.StateDir {
		return empty, "legacy local builder identity could not be verified"
	}
	step, found, err := s.entities.Steps().Get(callCtx, claim.OperationID, deploymentPackStep)
	exec := engine.Execution{OperationID: claim.OperationID, Attempt: claim.Attempt, Step: deploymentPackStep}
	if err != nil || !found || step.Attempt != claim.Attempt || step.ExecutionID != exec.ExecutionID() || step.InputsHash == "" {
		return empty, "original Pack dispatch could not be verified"
	}
	response, err := local.RetireProvenBuild(callCtx, exec, step.InputsHash)
	if err != nil || response.Status != engine.StatusCompleted || response.ExecutionID != exec.ExecutionID() || response.InputsHash != step.InputsHash {
		return empty, "builder did not prove legacy build retirement"
	}
	return step, ""
}
