package controlplane

import (
	"context"
	"encoding/json"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
)

// appendActionOutputTx retains a secret-free typed effect snapshot in the same
// operation-locked transaction as the effect. It is not an attempt outcome.
// Callers supply trusted typed identities and redact any free text before
// calling; rewriting an authority identity here would corrupt the result.
// One snapshot per operation is sufficient for the single-effect callers.
func (s *Service) appendActionOutputTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, payload any) error {
	exists, err := tx.Operations().ActionOutputExists(ctx, op.ID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	event := contract.Event{Kind: "step", Stage: "action-output", ActionOutputStatus: "available"}
	raw, err := json.Marshal(payload)
	if err != nil {
		event.ActionOutputStatus = "omitted_unserializable"
	} else {
		switch {
		// Reuse the established 64 KiB candidate capture bound.
		case len(raw) > 64*1024:
			event.ActionOutputStatus = "omitted_too_large"
		default:
			event.ActionOutput = raw
		}
	}
	return appendEvent(ctx, tx, op, event)
}

func latestActionOutputTx(ctx context.Context, tx *entities.StoreTx, result *contract.ActionResult) error {
	if result.DataStatus != "" || len(result.Data) != 0 {
		return nil
	}
	sequence, raw, status, found, err := tx.Operations().LatestActionOutputEvent(ctx, result.OperationID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	result.Data, result.DataStatus = raw, status
	result.DataCursor = &contract.Cursor{OperationID: result.OperationID, After: sequence - 1}
	return nil
}

func tryActionOutputTx(ctx context.Context, tx *entities.StoreTx, result *contract.ActionResult) {
	if result.DataStatus != "" || len(result.Data) != 0 {
		return
	}
	if tx == nil {
		result.DataStatus = "unavailable"
		return
	}
	copy := *result
	if err := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
		return latestActionOutputTx(ctx, nested, &copy)
	}); err != nil {
		result.DataStatus = "unavailable"
		return
	}
	*result = copy
}
