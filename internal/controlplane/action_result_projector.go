package controlplane

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
)

// A payload read is optional enrichment, not permission to stop settlement.
// Savepoints keep a failed projection from aborting its surrounding transaction.
func tryActionPayloadTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, result *contract.ActionResult, service *Service) {
	if result.DataStatus != "" || len(result.Data) != 0 {
		return
	}
	if service == nil {
		// Generic finalizers project only allowlisted durable facts. Free text
		// in those records has already crossed its producer's redaction boundary.
		service = &Service{}
	}
	nested, err := tx.Begin(ctx)
	if err != nil {
		result.DataStatus = "unavailable"
		return
	}
	raw, err := service.actionPayloadTx(ctx, nested, op, result.Status)
	if err != nil {
		_ = nested.Rollback(ctx)
		result.DataStatus = "unavailable"
		return
	}
	if err := nested.Commit(ctx); err != nil {
		result.DataStatus = "unavailable"
		return
	}
	// Reuse the established candidate-capture bound.
	if len(raw) > 64*1024 {
		result.DataStatus = "omitted_too_large"
		return
	}
	if !json.Valid(raw) {
		result.DataStatus = "omitted_unserializable"
		return
	}
	var availability struct {
		Availability string `json:"availability"`
	}
	_ = json.Unmarshal(raw, &availability)
	result.Data, result.DataStatus = raw, "available"
	switch availability.Availability {
	case "partial", "unavailable":
		result.DataStatus = availability.Availability
	}
}
