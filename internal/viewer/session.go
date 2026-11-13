package viewer

import (
	"context"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// SessionObservation is a transport-independent response from a read-only
// follow session. A snapshot precedes the event fragments fetched after it.
type SessionObservation struct {
	Snapshot *contract.Operation
	Events   []contract.Event
	Cursor   contract.Cursor
}

type SessionResult struct {
	Operation contract.Operation
	Cursor    contract.Cursor
}

// SessionClient opens one read session; implementations close it before
// returning on callback failure or cancellation.
type SessionClient interface {
	FollowSession(context.Context, contract.Cursor, int, time.Duration, func(SessionObservation) error) (SessionResult, error)
}

func followSession(ctx context.Context, client SessionClient, id contract.OperationID, opts FollowOptions, emit func(Observation) error) (Summary, error) {
	opts = opts.normalized()
	model := New(id, Options{EventLimit: opts.EventLimit, StaleAfter: opts.StaleAfter, StalePolls: opts.StalePolls, Clean: opts.Clean})
	model.Cursor = opts.After
	send := func(o Observation) error {
		if emit == nil {
			return nil
		}
		o.State, o.Observation, o.Attempt = model.State, model.ObservationState, model.Attempt
		o.Cursor, o.EventCount, o.Age = model.Cursor, model.EventCount, model.Age(o.At)
		return emit(o)
	}
	backoff := opts.PollInterval
	for {
		if ctx.Err() != nil {
			return detach(model, opts.Now(), send, ctx.Err())
		}
		var callbackErr error
		var snapshot *contract.Operation
		result, err := client.FollowSession(ctx, contract.Cursor{OperationID: id, After: model.Cursor}, opts.PageLimit, opts.PollInterval, func(observation SessionObservation) error {
			now := opts.Now()
			fail := func(err error) error { callbackErr = err; return err }
			if observation.Cursor.OperationID != id {
				return fail(followProtocolError())
			}
			if observation.Snapshot != nil {
				op := *observation.Snapshot
				if op.Validate() != nil || op.ID != id || observation.Cursor.After != model.Cursor || observation.Events != nil || op.LastSequence < model.Cursor {
					return fail(followProtocolError())
				}
				snapshot = &op
				model.Snapshot(op, now)
				model.PollSucceeded(now)
				backoff = opts.PollInterval
				if err := send(Observation{Kind: ObservationSnapshot, At: now, Snapshot: &op}); err != nil {
					return fail(err)
				}
				return nil
			}
			if snapshot == nil || len(observation.Events) == 0 || len(observation.Events) > opts.PageLimit {
				return fail(followProtocolError())
			}
			next := model.Cursor
			var previous uint64
			var fresh []contract.Event
			for _, event := range observation.Events {
				if event.Validate() != nil || event.OperationID != id || (previous != 0 && event.Sequence <= previous) {
					return fail(followProtocolError())
				}
				previous = event.Sequence
				if event.Sequence <= model.Cursor {
					continue
				}
				if event.Sequence != next+1 {
					return fail(followProtocolError())
				}
				next = event.Sequence
				fresh = append(fresh, event)
			}
			if observation.Cursor.After != previous {
				return fail(followProtocolError())
			}
			if len(fresh) == 0 {
				return nil
			}
			if err := model.Page(fresh, now); err != nil {
				return fail(followProtocolError())
			}
			model.PollSucceeded(now)
			if err := send(Observation{Kind: ObservationEvents, At: now, Events: fresh}); err != nil {
				return fail(err)
			}
			return nil
		})
		if callbackErr != nil {
			return modelSummary(model), callbackErr
		}
		if err == nil {
			if snapshot == nil || result.Operation.Validate() != nil || result.Operation.ID != id ||
				result.Cursor.OperationID != id || result.Cursor.After != model.Cursor ||
				!result.Operation.State.Terminal() || result.Operation.LastSequence != model.Cursor ||
				*snapshot != result.Operation || model.State != result.Operation.State ||
				model.Attempt != result.Operation.Attempt || model.ErrorCode != result.Operation.ErrorCode {
				return modelSummary(model), followProtocolError()
			}
			model.Close(opts.Now())
			if err := send(Observation{Kind: ObservationClosed, At: opts.Now()}); err != nil {
				return modelSummary(model), err
			}
			summary := modelSummary(model)
			summary.Operation = result.Operation
			return summary, nil
		}
		if fatalFollowError(err) || ctx.Err() != nil {
			return failFollow(ctx, model, opts.Now(), err, send)
		}
		now := opts.Now()
		model.PollFailed(err, now)
		if err := send(Observation{Kind: ObservationFailed, At: now, Err: err}); err != nil {
			return modelSummary(model), err
		}
		backoff = nextBackoff(backoff)
		if !wait(ctx, backoff, opts.Now, model) {
			return detach(model, opts.Now(), send, ctx.Err())
		}
	}
}

func followProtocolError() error {
	return contract.NewFault(contract.ValidationFailed, "invalid operation follow stream", "Check operator and node compatibility; do not assume the operation stopped.")
}
