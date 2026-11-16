package viewer

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
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
	FollowSession(
		context.Context,
		contract.Cursor,
		int,
		time.Duration,
		func(SessionObservation) error,
	) (SessionResult, error)
}

func followSession(
	ctx context.Context,
	client SessionClient,
	id contract.OperationID,
	opts FollowOptions,
	emit func(Observation) error,
) (Summary, error) {
	opts = opts.normalized()
	model := New(
		id,
		Options{
			EventLimit: opts.EventLimit,
			StaleAfter: opts.StaleAfter,
			StalePolls: opts.StalePolls,
			Clean:      opts.Clean,
		},
	)
	model.Cursor = opts.After
	backoff := opts.PollInterval
	send := func(o Observation) error {
		if emit == nil {
			return nil
		}
		o.State, o.Observation, o.Attempt = model.State, model.ObservationState, model.Attempt
		o.Cursor, o.EventCount, o.Age = model.Cursor, model.EventCount, model.Age(o.At)
		return emit(o)
	}
	for {
		if ctx.Err() != nil {
			return detach(model, opts.Now(), send, ctx.Err())
		}
		handler := &sessionHandler{id: id, opts: opts, model: model, emit: send, backoff: backoff}
		var callbackErr error
		result, err := client.FollowSession(
			ctx,
			contract.Cursor{OperationID: id, After: model.Cursor},
			opts.PageLimit,
			opts.PollInterval,
			func(observation SessionObservation) error {
				err := handler.handle(observation)
				if err != nil {
					callbackErr = err
				}
				return err
			},
		)
		if callbackErr != nil {
			return modelSummary(model), callbackErr
		}
		if err == nil {
			return finishSession(model, handler, result, id, opts.Now, send)
		}
		if fatalFollowError(err) || ctx.Err() != nil {
			return failFollow(ctx, model, opts.Now(), err, send)
		}
		now := opts.Now()
		model.PollFailed(err, now)
		if err := send(Observation{Kind: ObservationFailed, At: now, Err: err}); err != nil {
			return modelSummary(model), err
		}
		handler.backoff = nextBackoff(handler.backoff)
		backoff = handler.backoff
		if !wait(ctx, backoff, opts.Now, model) {
			return detach(model, opts.Now(), send, ctx.Err())
		}
	}
}

func finishSession(model *Model, handler *sessionHandler, result SessionResult, id contract.OperationID, now func() time.Time, emit func(Observation) error) (Summary, error) {
	if !validSessionResult(result, handler.snapshot, id, model) {
		return modelSummary(model), followProtocolError()
	}
	model.Close(now())
	if err := emit(Observation{Kind: ObservationClosed, At: now()}); err != nil {
		return modelSummary(model), err
	}
	summary := modelSummary(model)
	summary.Operation = result.Operation
	return summary, nil
}

type sessionHandler struct {
	id       contract.OperationID
	opts     FollowOptions
	model    *Model
	emit     func(Observation) error
	backoff  time.Duration
	snapshot *contract.Operation
}

func (h *sessionHandler) handle(observation SessionObservation) error {
	now := h.opts.Now()
	if observation.Cursor.OperationID != h.id {
		return followProtocolError()
	}
	if observation.Snapshot != nil {
		return h.handleSnapshot(observation, now)
	}
	return h.handleEvents(observation, now)
}

func (h *sessionHandler) handleSnapshot(observation SessionObservation, now time.Time) error {
	op := *observation.Snapshot
	if op.Validate() != nil || op.ID != h.id ||
		observation.Cursor.After != h.model.Cursor ||
		observation.Events != nil ||
		op.LastSequence < h.model.Cursor {
		return followProtocolError()
	}
	h.snapshot = &op
	h.model.Snapshot(op, now)
	h.model.PollSucceeded(now)
	h.backoff = h.opts.PollInterval
	return h.emit(Observation{Kind: ObservationSnapshot, At: now, Snapshot: &op})
}

func (h *sessionHandler) handleEvents(observation SessionObservation, now time.Time) error {
	if h.snapshot == nil || len(observation.Events) == 0 ||
		len(observation.Events) > h.opts.PageLimit {
		return followProtocolError()
	}
	next := h.model.Cursor
	var previous uint64
	var fresh []contract.Event
	for _, event := range observation.Events {
		if event.Validate() != nil || event.OperationID != h.id ||
			(previous != 0 && event.Sequence <= previous) {
			return followProtocolError()
		}
		previous = event.Sequence
		if event.Sequence <= h.model.Cursor {
			continue
		}
		if event.Sequence != next+1 {
			return followProtocolError()
		}
		next = event.Sequence
		fresh = append(fresh, event)
	}
	if observation.Cursor.After != previous {
		return followProtocolError()
	}
	if len(fresh) == 0 {
		return nil
	}
	if err := h.model.Page(fresh, now); err != nil {
		return followProtocolError()
	}
	h.model.PollSucceeded(now)
	return h.emit(Observation{Kind: ObservationEvents, At: now, Events: fresh})
}

func validSessionResult(result SessionResult, snapshot *contract.Operation, id contract.OperationID, model *Model) bool {
	if snapshot == nil {
		return false
	}
	sameOperation, compareErr := sameOperationWire(*snapshot, result.Operation)
	if compareErr != nil || !sameOperation {
		return false
	}
	return result.Operation.Validate() == nil &&
		result.Operation.ID == id &&
		result.Cursor.OperationID == id &&
		result.Cursor.After == model.Cursor &&
		result.Operation.State.Terminal() &&
		result.Operation.LastSequence == model.Cursor &&
		model.State == result.Operation.State &&
		model.Attempt == result.Operation.Attempt &&
		model.ErrorCode == result.Operation.ErrorCode
}

func followProtocolError() error {
	return contract.NewFault(
		contract.ValidationFailed,
		"invalid operation follow stream",
		"Check operator and node compatibility; do not assume the operation stopped.",
	)
}

// sameOperationWire compares the serialized Operation contract by value. The
// final observation and the session result arrive through independent wire
// decoders, so their nested result/evidence pointers are always distinct even
// when the values are identical. Internal PendingResult is excluded (json:"-").
// Numbers decode as json.Number so no precision is lost; object key order and
// insignificant JSON formatting are not semantic differences. A marshal or
// decode failure is a protocol rejection, not equality.
func sameOperationWire(left, right contract.Operation) (bool, error) {
	leftValue, err := operationWireValue(left)
	if err != nil {
		return false, err
	}
	rightValue, err := operationWireValue(right)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(leftValue, rightValue), nil
}

func operationWireValue(op contract.Operation) (any, error) {
	// PendingResult is an internal, unexported wire-invisible hand-off. It is
	// never part of the operation equality contract.
	op.PendingResult = nil
	encoded, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}
