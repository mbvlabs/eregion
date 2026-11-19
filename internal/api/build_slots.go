package api

import (
	"context"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

type BuildSlot struct {
	OperationID contract.OperationID `json:"operation_id"`
	Builder     string               `json:"builder"`
	ClaimedAt   time.Time            `json:"claimed_at"`
	AgeSeconds  int64                `json:"age_seconds"`
	State       string               `json:"state"`
	Reason      string               `json:"reason,omitempty"`
}

type BuildSlotClearResult struct {
	Slot    BuildSlot `json:"slot"`
	DryRun  bool      `json:"dry_run"`
	Cleared bool      `json:"cleared"`
	Reason  string    `json:"reason,omitempty"`
}

type BuildSlotsService interface {
	ListBuildSlots(context.Context, contract.Actor) ([]BuildSlot, error)
	ClearBuildSlot(context.Context, contract.Actor, contract.OperationID, bool, bool) (BuildSlotClearResult, error)
}

func (s AppService) buildSlots(actor contract.Actor) (BuildSlotsService, error) {
	if err := actor.Validate(); err != nil {
		return nil, err
	}
	if actor.Kind != "operator" {
		return nil, contract.NewFault(contract.Forbidden, "build slots require an operator", "Use the trusted operator boundary.")
	}
	service, ok := s.Operations.(BuildSlotsService)
	if !ok {
		return nil, contract.NewFault(contract.Unavailable, "build slots are unavailable", "Check node control-plane configuration.")
	}
	return service, nil
}

func (s AppService) ListBuildSlots(ctx context.Context, actor contract.Actor) ([]BuildSlot, error) {
	service, err := s.buildSlots(actor)
	if err != nil {
		return nil, err
	}
	return service.ListBuildSlots(ctx, actor)
}

func (s AppService) ClearBuildSlot(ctx context.Context, actor contract.Actor, id contract.OperationID, dryRun, legacyLocal bool) (BuildSlotClearResult, error) {
	service, err := s.buildSlots(actor)
	if err != nil {
		return BuildSlotClearResult{}, err
	}
	if err := id.Validate(); err != nil {
		return BuildSlotClearResult{}, err
	}
	return service.ClearBuildSlot(ctx, actor, id, dryRun, legacyLocal)
}
