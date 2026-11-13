package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/viewer"
)

// OperationFollowObservation is an authorized snapshot or a page of events.
// It is independent of the transport used to deliver those observations.
type OperationFollowObservation struct {
	Snapshot *contract.Operation
	Events   []contract.Event
	Cursor   contract.Cursor
}

// OperationFollowResult records the authoritative terminal snapshot and the
// last processed event cursor. It does not imply the operation succeeded.
type OperationFollowResult struct {
	Operation contract.Operation `json:"operation"`
	Cursor    contract.Cursor    `json:"cursor"`
}

type operationFollowClient struct {
	app       AppService
	authority operations.Authority
}

func (c operationFollowClient) Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	readCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	op, err := c.app.GetOperation(readCtx, c.authority, id)
	if err != nil {
		if readCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, contract.NewFault(contract.Unavailable, "operation snapshot read timed out", "The read will be retried.")
		}
		return nil, err
	}
	return json.Marshal(op)
}

func (c operationFollowClient) Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	readCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	page, err := c.app.OperationEvents(readCtx, c.authority, cursor, limit)
	if err != nil {
		if readCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, contract.NewFault(contract.Unavailable, "operation events read timed out", "The read will be retried.")
		}
		return nil, err
	}
	return json.Marshal(page)
}

// FollowOperation reads through the ordinary authority-checked service for the
// lifetime of one session. Callback failure ends the read; it never cancels the
// durable operation. A zero interval selects the existing 500 ms default.
func (s AppService) FollowOperation(ctx context.Context, authority operations.Authority, cursor contract.Cursor, limit int, interval time.Duration, emit func(OperationFollowObservation) error) (OperationFollowResult, error) {
	if err := cursor.OperationID.Validate(); err != nil {
		return OperationFollowResult{}, err
	}
	if limit < 1 || limit > 200 {
		return OperationFollowResult{}, contract.NewFault(contract.ValidationFailed, "event limit out of range", "Use an event limit from 1 to 200.")
	}
	if interval < 0 {
		return OperationFollowResult{}, contract.NewFault(contract.ValidationFailed, "poll interval must be positive", "Use a positive duration or omit it for the default.")
	}
	var snapshot contract.Operation
	first := true
	summary, err := viewer.Follow(ctx, operationFollowClient{app: s, authority: authority}, cursor.OperationID,
		viewer.FollowOptions{After: cursor.After, PageLimit: limit, PollInterval: interval},
		func(observation viewer.Observation) error {
			switch observation.Kind {
			case viewer.ObservationFailed:
				// Gap refetches stay in the shared reducer. Other transient
				// read failures end this session so the client can show
				// reconnecting and reopen from its processed cursor.
				if viewer.ErrGap(observation.Err) == nil {
					return observation.Err
				}
			case viewer.ObservationSnapshot:
				if first && cursor.After > observation.Snapshot.LastSequence {
					return contract.NewFault(contract.ValidationFailed, "follow cursor exceeds the operation sequence", "Resume from a previously processed event cursor.")
				}
				first = false
				snapshot = *observation.Snapshot
				if emit != nil {
					return emit(OperationFollowObservation{Snapshot: observation.Snapshot, Cursor: contract.Cursor{OperationID: cursor.OperationID, After: observation.Cursor}})
				}
			case viewer.ObservationEvents:
				if emit != nil {
					return emit(OperationFollowObservation{Events: observation.Events, Cursor: contract.Cursor{OperationID: cursor.OperationID, After: observation.Cursor}})
				}
			}
			return nil
		})
	if err != nil {
		return OperationFollowResult{}, err
	}
	return OperationFollowResult{Operation: snapshot, Cursor: contract.Cursor{OperationID: cursor.OperationID, After: summary.Cursor}}, nil
}
