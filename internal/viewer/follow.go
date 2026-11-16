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
	// After resumes a session after this already-processed event sequence.
	After      uint64
	EventLimit int
	StaleAfter time.Duration
	StalePolls int
	Clean      Cleaner
	Now        func() time.Time
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
	Serving      ServingRelease
	Cleanup      CleanupObservation
}

// Follow polls the authoritative snapshot and event pages until the operation
// is terminal and every event up to the snapshot's last_sequence was applied.
// Context cancellation ends the loop as a detach; it never calls RequestCancel.
func Follow(ctx context.Context, client Client, id contract.OperationID, opts FollowOptions, emit func(Observation) error) (Summary, error) {
	if streaming, ok := client.(SessionClient); ok {
		return followSession(ctx, streaming, id, opts, emit)
	}
	opts = opts.normalized()
	modelOptions := Options{EventLimit: opts.EventLimit, StaleAfter: opts.StaleAfter, StalePolls: opts.StalePolls, Clean: opts.Clean}
	model := New(id, modelOptions)
	model.Cursor = opts.After

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

	follower := &follower{
		client:  client,
		id:      id,
		opts:    opts,
		model:   model,
		emit:    emitObservation,
		backoff: opts.PollInterval,
	}
	for {
		done, err := follower.runOnce(ctx)
		if err != nil {
			var transport *followTransportError
			if errors.As(err, &transport) {
				return failFollow(ctx, model, transport.at, transport.err, emitObservation)
			}
			return modelSummary(model), err
		}
		if done {
			return modelSummary(model), nil
		}
	}
}

// followTransportError marks a poll failure that must pass through failFollow,
// which decides between a retry summary and a detached session.
type followTransportError struct {
	err error
	at  time.Time
}

func (e *followTransportError) Error() string { return e.err.Error() }
func (e *followTransportError) Unwrap() error { return e.err }

type follower struct {
	client Client
	id     contract.OperationID
	opts   FollowOptions
	model  *Model
	emit   func(Observation) error

	backoff        time.Duration
	gapRefetches   int
	tailNoProgress int
}

func (f *follower) runOnce(ctx context.Context) (bool, error) {
	now := f.opts.Now()
	if err := ctx.Err(); err != nil {
		return false, &followTransportError{err: err, at: now}
	}
	f.model.Tick(now)
	if err := f.emit(Observation{Kind: ObservationTick, At: now}); err != nil {
		return false, err
	}

	snapshot, retry, err := f.snapshot(ctx, now)
	if err != nil {
		return false, err
	}
	if retry {
		return false, nil
	}

	cursorBeforePages := f.model.Cursor
	pageFailed, err := f.fetchPages(ctx, now)
	if err != nil {
		return false, err
	}
	if pageFailed {
		return false, nil
	}

	// A terminal event can arrive after the snapshot was fetched. Do not finish
	// until a fresh authoritative terminal snapshot includes it.
	if snapshot.State.Terminal() && f.model.Terminal() && f.model.Cursor == snapshot.LastSequence {
		now = f.opts.Now()
		f.model.Close(now)
		if err := f.emit(Observation{Kind: ObservationClosed, At: now}); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := f.checkTail(snapshot, cursorBeforePages); err != nil {
		return false, err
	}
	if !wait(ctx, f.opts.PollInterval, f.opts.Now, f.model) {
		return false, &followTransportError{err: ctx.Err(), at: f.opts.Now()}
	}
	return false, nil
}

func (f *follower) snapshot(ctx context.Context, now time.Time) (contract.Operation, bool, error) {
	raw, err := f.client.Get(ctx, f.id)
	if err != nil {
		if fatalFollowError(err) || ctx.Err() != nil {
			return contract.Operation{}, false, &followTransportError{err: err, at: now}
		}
		f.model.PollFailed(err, now)
		if emitErr := f.emit(Observation{Kind: ObservationFailed, At: now, Err: err}); emitErr != nil {
			return contract.Operation{}, false, emitErr
		}
		f.backoff = nextBackoff(f.backoff)
		if !wait(ctx, f.backoff, f.opts.Now, f.model) {
			return contract.Operation{}, false, &followTransportError{err: ctx.Err(), at: f.opts.Now()}
		}
		return contract.Operation{}, true, nil
	}
	snapshot, err := decodeOperation(raw)
	if err == nil && snapshot.ID != f.id {
		err = invalidFollowData()
	}
	if err != nil {
		return contract.Operation{}, false, err
	}
	f.model.Snapshot(snapshot, now)
	f.model.PollSucceeded(now)
	f.backoff = f.opts.PollInterval
	if err := f.emit(Observation{Kind: ObservationSnapshot, At: now, Snapshot: &snapshot}); err != nil {
		return contract.Operation{}, false, err
	}
	return snapshot, false, nil
}

func (f *follower) fetchPages(ctx context.Context, now time.Time) (bool, error) {
	pageFailed := false
	for {
		previousCursor := f.model.Cursor
		page, failed, err := f.fetchPage(ctx)
		if err != nil {
			return false, err
		}
		if failed {
			pageFailed = true
			break
		}
		failed, err = f.applyPage(page, now)
		if err != nil {
			return false, err
		}
		if failed {
			pageFailed = true
			break
		}
		if len(page.Events) < f.opts.PageLimit {
			break
		}
		// A full page may have more events immediately; keep reading without
		// waiting for the poll interval. A defensive cursor check prevents a
		// server that returns a full page without progress from spinning.
		if f.model.Cursor <= previousCursor {
			break
		}
	}
	if pageFailed {
		return f.retryPage(ctx)
	}
	return false, nil
}

func (f *follower) fetchPage(ctx context.Context) (eventPage, bool, error) {
	page, err := fetchPage(ctx, f.client, f.id, f.model.Cursor, f.opts.PageLimit)
	if err == nil {
		return page, false, nil
	}
	now := f.opts.Now()
	if fatalFollowError(err) || ctx.Err() != nil {
		return eventPage{}, false, &followTransportError{err: err, at: now}
	}
	f.model.PollFailed(err, now)
	if emitErr := f.emit(Observation{Kind: ObservationFailed, At: now, Err: err}); emitErr != nil {
		return eventPage{}, false, emitErr
	}
	return eventPage{}, true, nil
}

func (f *follower) applyPage(page eventPage, now time.Time) (bool, error) {
	if err := f.model.Page(page.Events, now); err != nil {
		gap := ErrGap(err)
		if gap == nil {
			return false, err
		}
		f.gapRefetches++
		if f.gapRefetches > 2 {
			return false, gap
		}
		f.model.PollFailed(err, now)
		if emitErr := f.emit(Observation{Kind: ObservationFailed, At: now, Err: err}); emitErr != nil {
			return false, emitErr
		}
		return true, nil
	}
	f.gapRefetches = 0
	f.model.PollSucceeded(now)
	if len(page.Events) > 0 {
		if err := f.emit(Observation{Kind: ObservationEvents, At: now, Events: page.Events}); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (f *follower) retryPage(ctx context.Context) (bool, error) {
	f.backoff = nextBackoff(f.backoff)
	if !wait(ctx, f.backoff, f.opts.Now, f.model) {
		return false, &followTransportError{err: ctx.Err(), at: f.opts.Now()}
	}
	return true, nil
}

func (f *follower) checkTail(snapshot contract.Operation, cursorBeforePages uint64) error {
	if snapshot.State.Terminal() && f.model.Cursor < snapshot.LastSequence && f.model.Cursor == cursorBeforePages {
		f.tailNoProgress++
		if f.tailNoProgress > 2 {
			return contract.NewFault(contract.PreconditionFailed, "operation terminal event tail is unavailable", "Inspect the operation history; missing events were not skipped.")
		}
		return nil
	}
	f.tailNoProgress = 0
	return nil
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
	if current >= maxBackoff/2 {
		return maxBackoff
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
		Serving: model.summaryServing(),
		Cleanup: model.Cleanup,
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
	return contract.NewFault(contract.ValidationFailed, "API returned unreadable operation data", "Check binary compatibility; do not assume an unrecognized result means work did not happen.")
}

func fatalFollowError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var fault *contract.FaultError
	if !errors.As(err, &fault) {
		return false
	}
	switch fault.Code {
	case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed, contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.PreconditionFailed, contract.Conflict, contract.NodeVersionMismatch:
		return true
	case contract.MissingTool, contract.DoctorFailed, contract.ConfirmationRequired, contract.RemoteUnreachable, contract.Unavailable, contract.Cancelled, contract.Internal:
		return false
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
		"state":        summary.State,
	}
	if summary.ErrorCode != "" {
		details["error_code"] = summary.ErrorCode
	}
	if summary.Operation.ResultReadError != "" {
		details["result_read_error"] = summary.Operation.ResultReadError
	}
	if summary.Operation.Evidence != nil {
		details["evidence"] = summary.Operation.Evidence
	}
	if summary.Operation.EvidenceReadError != "" {
		details["evidence_read_error"] = summary.Operation.EvidenceReadError
	}
	if result := summary.Operation.LatestResult; result != nil {
		details["result"] = result
	}
	if command != "" {
		details["resume"] = command + " " + string(summary.OperationID)
	}
	return details
}
