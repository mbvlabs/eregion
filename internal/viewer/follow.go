package viewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Client is the subset of the authenticated API client the viewer needs. It
// carries raw JSON so the viewer also works with a test double; no viewer path
// can submit, cancel, or reach a database.
type Client interface {
	Get(context.Context, contract.OperationID) (json.RawMessage, error)
	Events(context.Context, contract.Cursor, int) (json.RawMessage, error)
}

// ObservationKind identifies what changed. It is a viewer event, not a domain
// operation event and never a Bubble Tea message.
type ObservationKind string

const (
	ObservationSnapshot  ObservationKind = "snapshot"
	ObservationEvents    ObservationKind = "events"
	ObservationFailed    ObservationKind = "poll_failed"
	ObservationSucceeded ObservationKind = "poll_succeeded"
	ObservationTick      ObservationKind = "tick"
	ObservationClosed    ObservationKind = "closed"
)

// Observation is emitted by Follow. A presenter may choose to render it, apply
// it to its own Model, or ignore it; the reducer remains the source of truth.
type Observation struct {
	Kind ObservationKind
	At   time.Time

	Snapshot *contract.Operation
	Events   []contract.Event
	Err      error

	// A compact view of the reducer at emission time.
	State       contract.State
	Observation ObservationState
	Attempt     uint32
	Cursor      uint64
	EventCount  uint64
	Age         time.Duration
}

type FollowOptions struct {
	PollInterval time.Duration
	PageLimit    int
	EventLimit   int
	StaleAfter   time.Duration
	StalePolls   int
	Clean        Cleaner
	Now          func() time.Time
}

const (
	DefaultPollInterval = 500 * time.Millisecond
	DefaultPageLimit    = 200
	maxBackoff          = 5 * time.Second
)

func (o FollowOptions) normalized() FollowOptions {
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.PageLimit <= 0 {
		o.PageLimit = DefaultPageLimit
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Summary is the stable result of a follow session. Detached means the viewer
// left early; the operation may still be running and was never cancelled.
type Summary struct {
	OperationID  contract.OperationID
	State        contract.State
	Attempt      uint32
	ErrorCode    contract.Code
	Events       uint64
	Cursor       uint64
	LastSequence uint64
	Observation  ObservationState
	Detached     bool
	Operation    contract.Operation
}

// Follow polls the authoritative snapshot and event pages until the operation
// is terminal and every event up to the snapshot's last_sequence was applied.
// Context cancellation ends the loop as a detach; it never calls RequestCancel.
func Follow(ctx context.Context, client Client, id contract.OperationID, opts FollowOptions, emit func(Observation) error) (Summary, error) {
	opts = opts.normalized()
	modelOptions := Options{EventLimit: opts.EventLimit, StaleAfter: opts.StaleAfter, StalePolls: opts.StalePolls, Clean: opts.Clean}
	model := New(id, modelOptions)

	emitObservation := func(observation Observation) error {
		if emit == nil {
			return nil
		}
		observation.State = model.State
		observation.Observation = model.ObservationState
		observation.Attempt = model.Attempt
		observation.Cursor = model.Cursor
		observation.EventCount = model.EventCount
		observation.Age = model.Age(observation.At)
		return emit(observation)
	}

	backoff := opts.PollInterval
	gapRefetches := 0
	for {
		now := opts.Now()
		if err := ctx.Err(); err != nil {
			return detach(model, now, emitObservation, err)
		}
		model.Tick(now)
		if err := emitObservation(Observation{Kind: ObservationTick, At: now}); err != nil {
			return modelSummary(model), err
		}

		raw, err := client.Get(ctx, id)
		if err != nil {
			if fatalFollowError(err) || ctx.Err() != nil {
				return failFollow(ctx, model, now, err, emitObservation)
			}
			model.PollFailed(err, now)
			if emitErr := emitObservation(Observation{Kind: ObservationFailed, At: now, Err: err}); emitErr != nil {
				return modelSummary(model), emitErr
			}
			backoff = nextBackoff(backoff)
			if !wait(ctx, backoff, opts.Now, model) {
				return detach(model, opts.Now(), emitObservation, ctx.Err())
			}
			continue
		}
		snapshot, err := decodeOperation(raw)
		if err != nil {
			model.PollFailed(err, now)
			if emitErr := emitObservation(Observation{Kind: ObservationFailed, At: now, Err: err}); emitErr != nil {
				return modelSummary(model), emitErr
			}
			backoff = nextBackoff(backoff)
			if !wait(ctx, backoff, opts.Now, model) {
				return detach(model, opts.Now(), emitObservation, ctx.Err())
			}
			continue
		}
		model.Snapshot(snapshot, now)
		model.PollSucceeded(now)
		backoff = opts.PollInterval
		if err := emitObservation(Observation{Kind: ObservationSnapshot, At: now, Snapshot: &snapshot}); err != nil {
			return modelSummary(model), err
		}

		pageFailed := false
		for {
			previousCursor := model.Cursor
			page, err := fetchPage(ctx, client, id, model.Cursor, opts.PageLimit)
			if err != nil {
				now = opts.Now()
				if fatalFollowError(err) || ctx.Err() != nil {
					return failFollow(ctx, model, now, err, emitObservation)
				}
				model.PollFailed(err, now)
				if emitErr := emitObservation(Observation{Kind: ObservationFailed, At: now, Err: err}); emitErr != nil {
					return modelSummary(model), emitErr
				}
				pageFailed = true
				break
			}
			if err := model.Page(page.Events, now); err != nil {
				gap := ErrGap(err)
				if gap == nil {
					return modelSummary(model), err
				}
				gapRefetches++
				if gapRefetches > 2 {
					return modelSummary(model), gap
				}
				model.PollFailed(err, now)
				if emitErr := emitObservation(Observation{Kind: ObservationFailed, At: now, Err: err}); emitErr != nil {
					return modelSummary(model), emitErr
				}
				pageFailed = true
				break
			}
			gapRefetches = 0
			model.PollSucceeded(now)
			if len(page.Events) > 0 {
				if err := emitObservation(Observation{Kind: ObservationEvents, At: now, Events: page.Events}); err != nil {
					return modelSummary(model), err
				}
			}
			if len(page.Events) < opts.PageLimit {
				break
			}
			// A full page may have more events immediately; keep reading without
			// waiting for the poll interval. A defensive cursor check prevents a
			// server that returns a full page without progress from spinning.
			if model.Cursor <= previousCursor {
				break
			}
		}
		if pageFailed {
			backoff = nextBackoff(backoff)
			if !wait(ctx, backoff, opts.Now, model) {
				return detach(model, opts.Now(), emitObservation, ctx.Err())
			}
			continue
		}

		if model.Terminal() && model.Cursor >= model.LastSequence {
			now = opts.Now()
			model.Close(now)
			if err := emitObservation(Observation{Kind: ObservationClosed, At: now}); err != nil {
				return modelSummary(model), err
			}
			return modelSummary(model), nil
		}
		if !wait(ctx, opts.PollInterval, opts.Now, model) {
			return detach(model, opts.Now(), emitObservation, ctx.Err())
		}
	}
}

func failFollow(ctx context.Context, model *Model, now time.Time, err error, emit func(Observation) error) (Summary, error) {
	if ctx.Err() != nil {
		return detach(model, now, emit, ctx.Err())
	}
	model.PollFailed(err, now)
	_ = emit(Observation{Kind: ObservationFailed, At: now, Err: err})
	return modelSummary(model), err
}

func detach(model *Model, now time.Time, emit func(Observation) error, err error) (Summary, error) {
	model.Close(now)
	_ = emit(Observation{Kind: ObservationClosed, At: now, Err: err})
	summary := modelSummary(model)
	summary.Detached = true
	if err == nil {
		err = context.Canceled
	}
	return summary, err
}

func wait(ctx context.Context, d time.Duration, now func() time.Time, model *Model) bool {
	if d <= 0 {
		d = time.Millisecond
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		model.Tick(now())
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		current = DefaultPollInterval
	}
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

func modelSummary(model *Model) Summary {
	return Summary{
		OperationID:  model.OperationID,
		State:        model.State,
		Attempt:      model.Attempt,
		ErrorCode:    model.ErrorCode,
		Events:       model.EventCount,
		Cursor:       model.Cursor,
		LastSequence: model.LastSequence,
		Observation:  model.ObservationState,
		Operation: contract.Operation{
			ID:           model.OperationID,
			Kind:         model.Kind,
			Target:       model.Target,
			Actor:        model.Actor,
			State:        model.State,
			Attempt:      model.Attempt,
			LastSequence: model.LastSequence,
			ErrorCode:    model.ErrorCode,
			CreatedAt:    model.CreatedAt,
			UpdatedAt:    model.UpdatedAt,
		},
	}
}

func decodeOperation(raw json.RawMessage) (contract.Operation, error) {
	var op contract.Operation
	if err := json.Unmarshal(raw, &op); err != nil || op.Validate() != nil {
		return contract.Operation{}, invalidFollowData()
	}
	return op, nil
}

type eventPage struct {
	Events []contract.Event `json:"events"`
	Cursor contract.Cursor  `json:"cursor"`
}

func fetchPage(ctx context.Context, client Client, id contract.OperationID, after uint64, limit int) (eventPage, error) {
	raw, err := client.Events(ctx, contract.Cursor{OperationID: id, After: after}, limit)
	if err != nil {
		return eventPage{}, err
	}
	var page eventPage
	if json.Unmarshal(raw, &page) != nil || page.Events == nil || len(page.Events) > limit || page.Cursor.OperationID != id || page.Cursor.After < after {
		return eventPage{}, invalidFollowData()
	}
	next := after
	for _, event := range page.Events {
		if event.Validate() != nil || event.OperationID != id || event.Sequence <= next {
			return eventPage{}, invalidFollowData()
		}
		next = event.Sequence
	}
	if page.Cursor.After != next {
		return eventPage{}, invalidFollowData()
	}
	return page, nil
}

func invalidFollowData() error {
	return contract.NewFault(contract.Unavailable, "API returned unreadable operation data", "Check binary compatibility; do not assume an unrecognized result means work did not happen.")
}

func fatalFollowError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		return false
	}
	switch fault.Code {
	case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed, contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.PreconditionFailed, contract.Conflict:
		return true
	default:
		return false
	}
}

// FaultDetails adds the operation identity to a follow error without exposing
// transport internals. It is safe for both JSON and plain output after the
// normal redaction boundary.
func FaultDetails(summary Summary, command string) map[string]any {
	details := map[string]any{
		"operation_id": string(summary.OperationID),
		"events":       summary.Events,
		"cursor":       fmt.Sprintf("%d", summary.Cursor),
	}
	if command != "" {
		details["resume"] = command + " " + string(summary.OperationID)
	}
	return details
}
