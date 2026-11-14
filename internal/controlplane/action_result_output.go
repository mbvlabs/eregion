package controlplane

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
)

// appendActionOutputTx retains a secret-free typed effect snapshot in the same
// operation-locked transaction as the effect. It is not an attempt outcome.
// Callers supply trusted typed identities and redact any free text before
// calling; rewriting an authority identity here would corrupt the result.
// One snapshot per operation is sufficient for the single-effect callers.
func (s *Service) appendActionOutputTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, payload any) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events
		WHERE operation_id=$1 AND event ? 'action_output_status')`, op.ID).Scan(&exists); err != nil {
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

func latestActionOutputTx(ctx context.Context, tx pgx.Tx, result *contract.ActionResult) error {
	if result.DataStatus != "" || len(result.Data) != 0 {
		return nil
	}
	var raw []byte
	var status string
	var sequence uint64
	err := tx.QueryRow(ctx, `SELECT sequence,event->'action_output',event->>'action_output_status'
		FROM eregion_operation_events WHERE operation_id=$1 AND event ? 'action_output_status'
		ORDER BY sequence DESC LIMIT 1`, result.OperationID).Scan(&sequence, &raw, &status)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	result.Data, result.DataStatus = raw, status
	result.DataCursor = &contract.Cursor{OperationID: result.OperationID, After: sequence - 1}
	return nil
}

func tryActionOutputTx(ctx context.Context, tx pgx.Tx, result *contract.ActionResult) {
	if result.DataStatus != "" || len(result.Data) != 0 {
		return
	}
	nested, err := tx.Begin(ctx)
	if err != nil {
		result.DataStatus = "unavailable"
		return
	}
	copy := *result
	if err := latestActionOutputTx(ctx, nested, &copy); err != nil {
		_ = nested.Rollback(ctx)
		result.DataStatus = "unavailable"
		return
	}
	if err := nested.Commit(ctx); err != nil {
		result.DataStatus = "unavailable"
		return
	}
	*result = copy
}
