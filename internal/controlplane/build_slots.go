package controlplane

import (
	"context"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
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

func (s *Service) ClearBuildSlot(ctx context.Context, actor contract.Actor, id contract.OperationID, dryRun bool) (api.BuildSlotClearResult, error) {
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
		result.Reason = "would retry retirement on the pinned builder; release requires confirmation of retirement and an unchanged terminal claim"
		return result, nil
	}
	if reason := s.retireBuildOnPinnedNode(ctx, claim); reason != "" {
		result.Reason = reason
		return result, nil
	}
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		deleted, err := tx.BuildCapacity().DeleteTerminal(ctx, claim)
		if err != nil {
			return err
		}
		if !deleted {
			result.Reason = "claim changed or was already released"
			return nil
		}
		if err := tx.Audit().Record(ctx, integrations.AuditEntry{At: time.Now().UTC(), Actor: actor.Subject, Action: "build.slot.clear", Target: string(id), Outcome: "succeeded"}); err != nil {
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
