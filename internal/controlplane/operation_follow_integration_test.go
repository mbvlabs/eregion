package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/viewer"
)

func TestIntegrationOperationFollowSessionReadsSnapshotAndEvents(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op := seedFollowOperation(t, ctx, run, "follow-session", 205)
	app := api.AppService{Operations: run.Service}
	assertFollowSnapshotPages(t, ctx, app, op)
	assertFollowResume(t, ctx, app, op)
	processed := assertFollowDisconnect(t, ctx, app, op)
	assertFollowReconnect(t, ctx, app, op, processed)
	assertFollowCallbackStop(t, ctx, app, op)
	after, err := app.GetOperation(ctx, testActor, op.ID)
	if err != nil || after.State != op.State || after.LastSequence != op.LastSequence {
		t.Fatalf("read changed operation: %+v err=%v", after, err)
	}
}

func seedFollowOperation(t *testing.T, ctx context.Context, run *Runtime, key string, logs int) contract.Operation {
	t.Helper()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: key, Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, found, err := tx.Operations().ClaimAttempt(ctx, op.ID, 1); err != nil || !found {
			return unavailable(err)
		}
		for range logs {
			if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "log", Message: "follow fixture"}); err != nil {
				return err
			}
		}
		op.State, op.Attempt = contract.Succeeded, 1
		if err := tx.Operations().SetState(ctx, op.ID, op.State, ""); err != nil {
			return err
		}
		return appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Succeeded})
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func assertFollowSnapshotPages(t *testing.T, ctx context.Context, app api.AppService, op contract.Operation) {
	t.Helper()
	var sequence uint64
	var snapshots, pages int
	result, err := app.FollowOperation(
		ctx,
		testActor,
		contract.Cursor{OperationID: op.ID},
		200,
		time.Millisecond,
		func(observation api.OperationFollowObservation) error {
			if observation.Snapshot != nil {
				snapshots++
			}
			if observation.Events != nil {
				pages++
				for _, event := range observation.Events {
					sequence++
					if event.Sequence != sequence {
						t.Fatalf("event sequence %d, expected %d", event.Sequence, sequence)
					}
				}
			}
			return nil
		},
	)
	if err != nil || snapshots == 0 || pages < 2 || sequence != op.LastSequence ||
		result.Cursor.After != sequence ||
		result.Operation.State != contract.Succeeded {
		t.Fatalf("result=%+v snapshots=%d pages=%d sequence=%d err=%v", result, snapshots, pages, sequence, err)
	}
}

func assertFollowResume(t *testing.T, ctx context.Context, app api.AppService, op contract.Operation) {
	t.Helper()
	var resumed uint64
	_, err := app.FollowOperation(
		ctx,
		testActor,
		contract.Cursor{OperationID: op.ID, After: 200},
		200,
		time.Millisecond,
		func(observation api.OperationFollowObservation) error {
			for _, event := range observation.Events {
				resumed++
				if event.Sequence != 200+resumed {
					t.Fatalf("unexpected resumed sequence %d", event.Sequence)
				}
			}
			return nil
		},
	)
	if err != nil || resumed != op.LastSequence-200 {
		t.Fatalf("resume count=%d err=%v", resumed, err)
	}
}

func assertFollowDisconnect(t *testing.T, ctx context.Context, app api.AppService, op contract.Operation) uint64 {
	t.Helper()
	disconnect := errors.New("simulated disconnect")
	var processed uint64
	_, err := app.FollowOperation(
		ctx,
		testActor,
		contract.Cursor{OperationID: op.ID},
		100,
		time.Millisecond,
		func(observation api.OperationFollowObservation) error {
			if len(observation.Events) != 0 {
				processed = observation.Cursor.After
				return disconnect
			}
			return nil
		},
	)
	if !errors.Is(err, disconnect) || processed != 100 {
		t.Fatalf("disconnect cursor=%d err=%v", processed, err)
	}
	return processed
}

func assertFollowReconnect(t *testing.T, ctx context.Context, app api.AppService, op contract.Operation, processed uint64) {
	t.Helper()
	replayed := uint64(0)
	_, err := app.FollowOperation(
		ctx,
		testActor,
		contract.Cursor{OperationID: op.ID, After: processed},
		100,
		time.Millisecond,
		func(observation api.OperationFollowObservation) error {
			for _, event := range observation.Events {
				replayed++
				if event.Sequence != processed+replayed {
					t.Fatalf("reconnect replay sequence=%d", event.Sequence)
				}
			}
			return nil
		},
	)
	if err != nil || replayed != op.LastSequence-processed {
		t.Fatalf("reconnect count=%d err=%v", replayed, err)
	}
}

func assertFollowCallbackStop(t *testing.T, ctx context.Context, app api.AppService, op contract.Operation) {
	t.Helper()
	stop := errors.New("reader stopped")
	_, err := app.FollowOperation(
		ctx,
		testActor,
		contract.Cursor{OperationID: op.ID},
		200,
		time.Millisecond,
		func(observation api.OperationFollowObservation) error {
			if len(observation.Events) != 0 {
				return stop
			}
			return nil
		},
	)
	if !errors.Is(err, stop) {
		t.Fatalf("callback error=%v", err)
	}
}

func TestIntegrationOperationFollowSessionTerminalRaceAndDetach(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op := submitQueuedOperation(t, ctx, run, "follow-race")
	app := api.AppService{Operations: run.Service}
	assertFollowTerminalRace(t, ctx, run, app, &op)
	assertFollowAuthority(t, ctx, app, op)
	assertFollowDetach(t, ctx, run, app)
}

func submitQueuedOperation(t *testing.T, ctx context.Context, run *Runtime, key string) contract.Operation {
	t.Helper()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: key, Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func assertFollowTerminalRace(t *testing.T, ctx context.Context, run *Runtime, app api.AppService, op *contract.Operation) {
	t.Helper()
	var snapshots []contract.State
	var last uint64
	result, err := app.FollowOperation(
		ctx,
		testActor,
		contract.Cursor{OperationID: op.ID},
		200,
		time.Millisecond,
		func(observation api.OperationFollowObservation) error {
			if observation.Snapshot != nil {
				snapshots = append(snapshots, observation.Snapshot.State)
				if len(snapshots) == 1 {
					return completeFollowOperation(ctx, run, op)
				}
			}
			for _, event := range observation.Events {
				if event.Sequence != last+1 {
					t.Fatalf("non-monotonic sequence %d after %d", event.Sequence, last)
				}
				last = event.Sequence
			}
			return nil
		},
	)
	if err != nil || len(snapshots) < 2 || snapshots[0] != contract.Queued ||
		snapshots[len(snapshots)-1] != contract.Succeeded ||
		result.Operation.State != contract.Succeeded ||
		result.Operation.LastSequence != last ||
		result.Cursor.After != last {
		t.Fatalf("snapshots=%v result=%+v last=%d err=%v", snapshots, result, last, err)
	}
}

func completeFollowOperation(ctx context.Context, run *Runtime, op *contract.Operation) error {
	return run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, found, err := tx.Operations().ClaimAttempt(ctx, op.ID, 1); err != nil || !found {
			return unavailable(err)
		}
		op.State, op.Attempt = contract.Succeeded, 1
		if err := tx.Operations().SetState(ctx, op.ID, op.State, ""); err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "state", State: contract.Succeeded})
	})
}

func assertFollowAuthority(t *testing.T, ctx context.Context, app api.AppService, op contract.Operation) {
	t.Helper()
	other := contract.Actor{Kind: "ci", Subject: "other"}
	_, err := app.FollowOperation(ctx, other, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, nil)
	if err == nil {
		t.Fatal("another actor could follow the operation")
	}
	operator := operations.OperatorAuthority(contract.Actor{Kind: "operator", Subject: "operator"})
	if _, err := app.FollowOperation(ctx, operator, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, nil); err != nil {
		t.Fatalf("operator authority: %v", err)
	}
	if _, err := app.FollowOperation(
		ctx,
		testActor,
		contract.Cursor{OperationID: op.ID, After: op.LastSequence + 1},
		200,
		time.Millisecond,
		nil,
	); err == nil {
		t.Fatal("ahead cursor was accepted")
	}
}

func assertFollowDetach(t *testing.T, ctx context.Context, run *Runtime, app api.AppService) {
	t.Helper()
	pending := submitQueuedOperation(t, ctx, run, "follow-detach")
	readCtx, cancel := context.WithCancel(ctx)
	_, err := app.FollowOperation(
		readCtx,
		testActor,
		contract.Cursor{OperationID: pending.ID},
		200,
		time.Millisecond,
		func(observation api.OperationFollowObservation) error {
			if observation.Snapshot != nil {
				cancel()
			}
			return nil
		},
	)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("detach error=%v", err)
	}
	after, err := app.GetOperation(ctx, testActor, pending.ID)
	if err != nil || after.State != contract.Queued || after.LastSequence != pending.LastSequence {
		t.Fatalf("detach changed operation: %+v err=%v", after, err)
	}
}

func TestIntegrationOperationFollowSessionReadBudgetAndMissingTail(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "follow-budget", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	app := api.AppService{Operations: run.Service}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(ctx, tx)
	if _, err := tx.Exec(
		ctx,
		"LOCK TABLE eregion_operations IN ACCESS EXCLUSIVE MODE",
	); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	start := time.Now()
	_, err = app.FollowOperation(
		readCtx,
		testActor,
		contract.Cursor{OperationID: op.ID},
		200,
		time.Millisecond,
		nil,
	)
	elapsed := time.Since(start)
	cancel()
	if err == nil || contract.Classify(err).Code != contract.Unavailable ||
		elapsed < 9*time.Second ||
		elapsed >= 15*time.Second {
		t.Fatalf("bounded read elapsed=%s err=%v", elapsed, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, found, err := tx.Operations().ClaimAttempt(ctx, op.ID, 1); err != nil || !found {
			return unavailable(err)
		}
		op.State, op.Attempt = contract.Succeeded, 1
		if err := tx.Operations().SetState(ctx, op.ID, op.State, ""); err != nil {
			return err
		}
		return appendEvent(
			ctx,
			tx,
			&op,
			contract.Event{Kind: "state", State: contract.Succeeded},
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate retention pruning the accepted event: the cursor then reports
	// an explicit gap instead of silently skipping it.
	if _, err := pool.Exec(
		ctx,
		"DELETE FROM eregion_operation_events WHERE operation_id=$1 AND sequence=$2",
		op.ID,
		int64(op.LastSequence),
	); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel = context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err = app.FollowOperation(
		readCtx,
		testActor,
		contract.Cursor{OperationID: op.ID},
		200,
		time.Millisecond,
		nil,
	)
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("missing tail must not finish successfully: %v", err)
	}
}

// followDelivery exercises the real application service while controlling
// delivery at the session-consumer boundary, without a fake operation store.
type followDelivery struct {
	app         api.AppService
	actor       contract.Actor
	duplicate   bool
	contradict  bool
	interrupt   bool
	interrupted bool
	mixed       bool
	malformed   bool
	mixedSeen   bool
	last        *contract.Event
}

func (f *followDelivery) Get(
	ctx context.Context,
	id contract.OperationID,
) (json.RawMessage, error) {
	op, err := f.app.GetOperation(ctx, f.actor, id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(op)
}

func (f *followDelivery) Events(
	ctx context.Context,
	cursor contract.Cursor,
	limit int,
) (json.RawMessage, error) {
	page, err := f.app.OperationEvents(ctx, f.actor, cursor, limit)
	if err != nil {
		return nil, err
	}
	return json.Marshal(page)
}

func (f *followDelivery) FollowSession(
	ctx context.Context,
	cursor contract.Cursor,
	limit int,
	interval time.Duration,
	emit func(viewer.SessionObservation) error,
) (viewer.SessionResult, error) {
	serviceLimit := limit
	if f.mixed {
		serviceLimit--
	}
	result, err := f.app.FollowOperation(
		ctx,
		f.actor,
		cursor,
		serviceLimit,
		interval,
		func(o api.OperationFollowObservation) error {
			if err := f.emitDuplicate(cursor, len(o.Events) != 0, emit); err != nil {
				return err
			}
			events := f.prepareEvents(o.Events, &o.Cursor)
			f.contradictEvents(events)
			if len(events) > 0 {
				last := events[len(events)-1]
				f.last = &last
			}
			if err := emit(
				viewer.SessionObservation{Snapshot: o.Snapshot, Events: events, Cursor: o.Cursor},
			); err != nil {
				return err
			}
			return f.interruptFault(events)
		},
	)
	return viewer.SessionResult{Operation: result.Operation, Cursor: result.Cursor}, err
}

func (f *followDelivery) emitDuplicate(
	cursor contract.Cursor,
	hasEvents bool,
	emit func(viewer.SessionObservation) error,
) error {
	if !f.duplicate || f.last == nil || !hasEvents {
		return nil
	}
	replayed := *f.last
	return emit(viewer.SessionObservation{
		Events: []contract.Event{replayed},
		Cursor: contract.Cursor{
			OperationID: cursor.OperationID,
			After:       replayed.Sequence,
		},
	})
}

func (f *followDelivery) prepareEvents(events []contract.Event, cursor *contract.Cursor) []contract.Event {
	prepared := append([]contract.Event(nil), events...)
	if !f.mixed || f.last == nil || len(prepared) == 0 {
		return prepared
	}
	prepared = append([]contract.Event{*f.last}, prepared...)
	f.mixedSeen = true
	if f.malformed {
		prepared[len(prepared)-1].Sequence++
		cursor.After = prepared[len(prepared)-1].Sequence
	}
	return prepared
}

func (f *followDelivery) contradictEvents(events []contract.Event) {
	if !f.contradict {
		return
	}
	for i := range events {
		if events[i].Kind == "state" && events[i].State == contract.Succeeded {
			events[i].State, events[i].ErrorCode = contract.Failed, contract.PreconditionFailed
		}
	}
}

func (f *followDelivery) interruptFault(events []contract.Event) error {
	if !f.interrupt || f.interrupted || len(events) == 0 {
		return nil
	}
	f.interrupted = true
	return contract.NewFault(
		contract.Unavailable,
		"controlled stream interruption",
		"Reconnect.",
	)
}

func TestIntegrationOperationFollowSessionConsumerReplayAndContradiction(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op := seedFollowOperation(t, ctx, run, "follow-consumer", 4)
	client := &followDelivery{
		app:       api.AppService{Operations: run.Service},
		actor:     testActor,
		duplicate: true,
		interrupt: true,
	}
	assertFollowDuplicateReplay(t, ctx, client, op)
	assertFollowMixedReplay(t, ctx, client, op)
	assertFollowMalformedMixed(t, ctx, client, op)
	assertFollowContradiction(t, ctx, client, op)
	assertFollowFailedRetrieval(t, ctx, run, client)
}

func assertFollowDuplicateReplay(t *testing.T, ctx context.Context, client *followDelivery, op contract.Operation) {
	t.Helper()
	var last uint64
	summary, err := viewer.Follow(
		ctx,
		client,
		op.ID,
		viewer.FollowOptions{PageLimit: 2, PollInterval: time.Millisecond},
		func(o viewer.Observation) error {
			for _, event := range o.Events {
				if event.Sequence != last+1 {
					t.Fatalf("replayed or missing event after %d: %d", last, event.Sequence)
				}
				last = event.Sequence
			}
			return nil
		},
	)
	if err != nil || last != op.LastSequence || summary.State != contract.Succeeded {
		t.Fatalf("summary=%+v last=%d err=%v", summary, last, err)
	}
}

func assertFollowMixedReplay(t *testing.T, ctx context.Context, client *followDelivery, op contract.Operation) {
	t.Helper()
	mixedClient := &followDelivery{app: client.app, actor: testActor, mixed: true}
	var last uint64
	summary, err := viewer.Follow(
		ctx,
		mixedClient,
		op.ID,
		viewer.FollowOptions{PageLimit: 3, PollInterval: time.Millisecond},
		func(o viewer.Observation) error {
			for _, event := range o.Events {
				if event.Sequence != last+1 {
					t.Fatalf("mixed batch replayed or missed event after %d: %d", last, event.Sequence)
				}
				last = event.Sequence
			}
			return nil
		},
	)
	if err != nil || !mixedClient.mixedSeen || last != op.LastSequence || summary.Cursor != last {
		t.Fatalf("mixed summary=%+v last=%d mixed=%t err=%v", summary, last, mixedClient.mixedSeen, err)
	}
}

func assertFollowMalformedMixed(t *testing.T, ctx context.Context, client *followDelivery, op contract.Operation) {
	t.Helper()
	badClient := &followDelivery{app: client.app, actor: testActor, mixed: true, malformed: true}
	var last uint64
	summary, err := viewer.Follow(
		ctx,
		badClient,
		op.ID,
		viewer.FollowOptions{PageLimit: 3, PollInterval: time.Millisecond},
		func(o viewer.Observation) error {
			for _, event := range o.Events {
				last = event.Sequence
			}
			return nil
		},
	)
	if err == nil || contract.Classify(err).Code != contract.ValidationFailed ||
		!badClient.mixedSeen ||
		last != 2 ||
		summary.Cursor != 2 {
		t.Fatalf("malformed mixed batch advanced: summary=%+v last=%d mixed=%t err=%v", summary, last, badClient.mixedSeen, err)
	}
}

func assertFollowContradiction(t *testing.T, ctx context.Context, client *followDelivery, op contract.Operation) {
	t.Helper()
	client.duplicate, client.contradict, client.interrupt = false, true, false
	_, err := viewer.Follow(
		ctx,
		client,
		op.ID,
		viewer.FollowOptions{PageLimit: 2, PollInterval: time.Millisecond},
		nil,
	)
	if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("contradictory terminal event accepted: %v", err)
	}
}

func assertFollowFailedRetrieval(t *testing.T, ctx context.Context, run *Runtime, client *followDelivery) {
	t.Helper()
	failed := submitQueuedOperation(t, ctx, run, "follow-failed")
	err := run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		failed.State, failed.Attempt, failed.ErrorCode = contract.Failed, 1, contract.PreconditionFailed
		if _, found, err := tx.Operations().ClaimAttempt(ctx, failed.ID, 1); err != nil || !found {
			return unavailable(err)
		}
		if err := tx.Operations().SetState(ctx, failed.ID, failed.State, failed.ErrorCode); err != nil {
			return err
		}
		return appendEvent(
			ctx,
			tx,
			&failed,
			contract.Event{Kind: "state", State: failed.State, ErrorCode: failed.ErrorCode},
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	client.last = nil
	failedSummary, err := viewer.Follow(
		ctx,
		client,
		failed.ID,
		viewer.FollowOptions{PageLimit: 2, PollInterval: time.Millisecond},
		nil,
	)
	if err != nil || failedSummary.State != contract.Failed ||
		failedSummary.ErrorCode != contract.PreconditionFailed ||
		failedSummary.Operation.State != contract.Failed {
		t.Fatalf("failed-operation retrieval summary=%+v err=%v", failedSummary, err)
	}
}

// wireFollowDelivery exercises the real application service while crossing an
// independent JSON encoding/decoding boundary for every snapshot, event page and
// final result, exactly as the SSH node RPC wire does. Nested result/evidence
// pointers are therefore freshly allocated instead of shared with the service.
type wireFollowDelivery struct {
	app          api.AppService
	actor        contract.Actor
	mutateSnap   func(*contract.Operation)
	mutatePage   func([]contract.Event) []contract.Event
	mutateResult func(*contract.Operation)
	mutateCursor func(*contract.Cursor)
	lastSnapshot *contract.Operation
	lastResult   *contract.Operation
}

func (f *wireFollowDelivery) Get(
	ctx context.Context,
	id contract.OperationID,
) (json.RawMessage, error) {
	op, err := f.app.GetOperation(ctx, f.actor, id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(op)
}

func (f *wireFollowDelivery) Events(
	ctx context.Context,
	cursor contract.Cursor,
	limit int,
) (json.RawMessage, error) {
	page, err := f.app.OperationEvents(ctx, f.actor, cursor, limit)
	if err != nil {
		return nil, err
	}
	return json.Marshal(page)
}

func (f *wireFollowDelivery) FollowSession(
	ctx context.Context,
	cursor contract.Cursor,
	limit int,
	interval time.Duration,
	emit func(viewer.SessionObservation) error,
) (viewer.SessionResult, error) {
	result, err := f.app.FollowOperation(
		ctx,
		f.actor,
		cursor,
		limit,
		interval,
		func(observation api.OperationFollowObservation) error {
			wire, err := f.observe(observation)
			if err != nil {
				return err
			}
			return emit(wire)
		},
	)
	if err != nil {
		return viewer.SessionResult{}, err
	}
	return f.sessionResult(result)
}

func (f *wireFollowDelivery) observe(
	observation api.OperationFollowObservation,
) (viewer.SessionObservation, error) {
	wire := viewer.SessionObservation{Cursor: observation.Cursor}
	if observation.Snapshot != nil {
		snapshot, err := f.decodeSnapshot(*observation.Snapshot)
		if err != nil {
			return viewer.SessionObservation{}, err
		}
		wire.Snapshot = snapshot
	}
	if observation.Events != nil {
		events, err := f.decodeEvents(observation.Events)
		if err != nil {
			return viewer.SessionObservation{}, err
		}
		wire.Events = events
	}
	return wire, nil
}

func (f *wireFollowDelivery) decodeSnapshot(snapshot contract.Operation) (*contract.Operation, error) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var decoded contract.Operation
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	if f.mutateSnap != nil {
		f.mutateSnap(&decoded)
	}
	f.lastSnapshot = &decoded
	return &decoded, nil
}

func (f *wireFollowDelivery) decodeEvents(events []contract.Event) ([]contract.Event, error) {
	encoded, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	var decoded []contract.Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	if f.mutatePage != nil {
		decoded = f.mutatePage(decoded)
	}
	return decoded, nil
}

func (f *wireFollowDelivery) sessionResult(
	result api.OperationFollowResult,
) (viewer.SessionResult, error) {
	encoded, err := json.Marshal(result.Operation)
	if err != nil {
		return viewer.SessionResult{}, err
	}
	var operation contract.Operation
	if err := json.Unmarshal(encoded, &operation); err != nil {
		return viewer.SessionResult{}, err
	}
	if f.mutateResult != nil {
		f.mutateResult(&operation)
	}
	finalCursor := result.Cursor
	if f.mutateCursor != nil {
		f.mutateCursor(&finalCursor)
	}
	f.lastResult = &operation
	return viewer.SessionResult{Operation: operation, Cursor: finalCursor}, nil
}

// wireFollowFixture persists one terminal operation with real retained result
// and evidence projections; messages and timing are never rewritten.
func wireFollowFixture(
	t *testing.T,
	run *Runtime,
	state contract.State,
	code contract.Code,
	evidenceMode string,
) contract.Operation {
	t.Helper()
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "wire-" + string(state) + "-" + evidenceMode, Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if now.Before(op.CreatedAt) {
		now = op.CreatedAt
	}
	result := &contract.ActionResult{
		OperationID:   op.ID,
		Action:        op.Kind,
		Target:        op.Target,
		Attempt:       1,
		Status:        state,
		Final:         true,
		StartedAt:     op.CreatedAt,
		CompletedAt:   now,
		TimingBasis:   "operation_created",
		Stage:         "action",
		Code:          code,
		Message:       "durable terminal result",
		Excerpt:       "retained failure excerpt",
		ExcerptCursor: &contract.Cursor{OperationID: op.ID, After: 1},
		Data: json.RawMessage(
			`{"tokens":9007199254740993,"nested":{"count":9007199254740995}}`,
		),
		DataStatus: "available",
		DataCursor: &contract.Cursor{OperationID: op.ID, After: 1},
		Evidence:   "not_collected",
		History:    contract.Cursor{OperationID: op.ID, After: 1},
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("fixture result: %v", err)
	}
	stored, err := storeWireFollowFixture(t, ctx, run, op, state, code, result, evidenceMode)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LatestResult == nil || stored.Evidence == nil {
		t.Fatalf("fixture lost result/evidence: %+v", stored)
	}
	return stored
}

func storeWireFollowFixture(
	t *testing.T,
	ctx context.Context,
	run *Runtime,
	op contract.Operation,
	state contract.State,
	code contract.Code,
	result *contract.ActionResult,
	evidenceMode string,
) (contract.Operation, error) {
	t.Helper()
	op.State, op.Attempt, op.ErrorCode = state, 1, code
	err := run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, found, err := tx.Operations().ClaimAttempt(ctx, op.ID, 1); err != nil || !found {
			return unavailable(err)
		}
		if err := tx.Operations().SetState(ctx, op.ID, state, code); err != nil {
			return err
		}
		if err := appendWireEvidence(t, ctx, tx, &op, evidenceMode); err != nil {
			return err
		}
		return appendEvent(
			ctx,
			tx,
			&op,
			contract.Event{Kind: "state", State: state, ErrorCode: code, Result: result},
		)
	})
	if err != nil {
		return contract.Operation{}, err
	}
	return run.Service.Get(ctx, testActor, op.ID)
}

func appendWireEvidence(t *testing.T, ctx context.Context, tx *entities.StoreTx, op *contract.Operation, evidenceMode string) error {
	t.Helper()
	switch evidenceMode {
	case "present":
		evidence := contract.Event{Kind: "step", Stage: "deploy", Message: "candidate failed",
			NodeEvidence: &contract.NodeExecutionEvidence{
				Attempt: 1, Outcome: "failed", Stage: "candidate", OutputAvailability: "available",
				RedactionContext: "not_required", Stdout: "bounded stdout",
				Stderr: "bounded stderr\nsecond line",
			}}
		if err := appendEvent(ctx, tx, op, evidence); err != nil {
			t.Fatal(err)
		}
	case "unavailable":
		loss := contract.Event{
			Kind:    "step",
			Stage:   "evidence",
			Message: "Node execution evidence could not be retained.",
			EvidenceLoss: &contract.NodeEvidenceLoss{
				ExecutionID: string(op.ID) + ":1:capture-v1",
				Attempt:     1,
			},
		}
		if err := appendEvent(ctx, tx, op, loss); err != nil {
			t.Fatal(err)
		}
	}
	return nil
}

func TestIntegrationOperationFollowWireEvidence(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	succeeded, succeededDelivery := runWireFollowCases(t, ctx, run)
	if succeeded.ID == "" {
		t.Fatal("succeeded wire fixture missing")
	}
	otherID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	assertWireFollowMutations(t, ctx, run, succeeded.ID, otherID)
	if !reflect.DeepEqual(
		succeededDelivery.lastSnapshot.LatestResult,
		succeededDelivery.lastResult.LatestResult,
	) {
		t.Fatal("baseline succeeded wire values changed during fault injection")
	}
}

type wireFollowCase struct {
	state        contract.State
	code         contract.Code
	evidence     string
	availability string
}

func runWireFollowCases(
	t *testing.T,
	ctx context.Context,
	run *Runtime,
) (contract.Operation, *wireFollowDelivery) {
	t.Helper()
	cases := []wireFollowCase{
		{contract.Succeeded, "", "present", "partial"},
		{contract.Failed, contract.PreconditionFailed, "absent", "not_collected"},
		{contract.Canceled, "", "unavailable", "unavailable"},
	}
	var succeeded contract.Operation
	var succeededDelivery *wireFollowDelivery
	for _, tc := range cases {
		op, delivery := runWireFollowCase(t, ctx, run, tc)
		if tc.state == contract.Succeeded {
			succeeded, succeededDelivery = op, delivery
		}
	}
	return succeeded, succeededDelivery
}

func runWireFollowCase(
	t *testing.T,
	ctx context.Context,
	run *Runtime,
	tc wireFollowCase,
) (contract.Operation, *wireFollowDelivery) {
	t.Helper()
	op := wireFollowFixture(t, run, tc.state, tc.code, tc.evidence) //nolint:contextcheck // fixture rollback owns a bounded detached cleanup context
	delivery := &wireFollowDelivery{
		app:   api.AppService{Operations: run.Service},
		actor: testActor,
	}
	summary, err := viewer.Follow(
		ctx,
		delivery,
		op.ID,
		viewer.FollowOptions{PageLimit: 50, PollInterval: time.Millisecond},
		nil,
	)
	if err != nil {
		t.Fatalf("%s/%s wire follow: %v", tc.state, tc.evidence, err)
	}
	if summary.State != tc.state || summary.Operation.State != tc.state {
		t.Fatalf("%s wire summary=%+v", tc.state, summary)
	}
	if delivery.lastSnapshot == nil || delivery.lastResult == nil {
		t.Fatalf("%s wire fixture did not cross both boundaries", tc.state)
	}
	assertWireFollowEvidence(t, tc, delivery)
	after, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || after.State != op.State || after.LastSequence != op.LastSequence {
		t.Fatalf("%s follow mutated durable state: %+v err=%v", tc.state, after, err)
	}
	return op, delivery
}

func assertWireFollowEvidence(t *testing.T, tc wireFollowCase, delivery *wireFollowDelivery) {
	t.Helper()
	if delivery.lastSnapshot.Evidence == nil || delivery.lastResult.Evidence == nil ||
		delivery.lastSnapshot.Evidence.Availability != tc.availability ||
		delivery.lastResult.Evidence.Availability != tc.availability {
		t.Fatalf(
			"%s availability snapshot=%+v result=%+v",
			tc.state,
			delivery.lastSnapshot.Evidence,
			delivery.lastResult.Evidence,
		)
	}
	if delivery.lastSnapshot.LatestResult == delivery.lastResult.LatestResult ||
		delivery.lastSnapshot.Evidence == delivery.lastResult.Evidence {
		t.Fatalf("%s reused a nested pointer across independent wire decodes", tc.state)
	}
	if !reflect.DeepEqual(
		delivery.lastSnapshot.LatestResult,
		delivery.lastResult.LatestResult,
	) ||
		!reflect.DeepEqual(delivery.lastSnapshot.Evidence, delivery.lastResult.Evidence) {
		t.Fatalf("%s wire values differ after independent decoding", tc.state)
	}
}

type wireFollowMutation struct {
	name     string
	delivery *wireFollowDelivery
}

func assertWireFollowMutations(
	t *testing.T,
	ctx context.Context,
	run *Runtime,
	operationID contract.OperationID,
	otherID contract.OperationID,
) {
	t.Helper()
	mutations := []wireFollowMutation{
		{"result content", wireFollowResultContentMutation(run)},
		{"evidence content", wireFollowEvidenceMutation(run)},
		{"operation identity", wireFollowIdentityMutation(run, otherID)},
		{"final cursor", wireFollowCursorMutation(run)},
		{"final attempt", wireFollowAttemptMutation(run)},
		{"contradictory error code", wireFollowErrorCodeMutation(run)},
		{"contradictory terminal event", wireFollowTerminalMutation(run)},
	}
	for _, mutation := range mutations {
		_, err := viewer.Follow(
			ctx,
			mutation.delivery,
			operationID,
			viewer.FollowOptions{PageLimit: 50, PollInterval: time.Millisecond},
			nil,
		)
		if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
			t.Fatalf("%s mismatch was accepted: %v", mutation.name, err)
		}
	}
}

func wireFollowDeliveryFor(run *Runtime) *wireFollowDelivery {
	return &wireFollowDelivery{
		app:   api.AppService{Operations: run.Service},
		actor: testActor,
	}
}

func wireFollowResultContentMutation(run *Runtime) *wireFollowDelivery {
	delivery := wireFollowDeliveryFor(run)
	delivery.mutateSnap = func(op *contract.Operation) {
		op.LatestResult.Data = json.RawMessage(`{"tokens":9007199254740994}`)
	}
	return delivery
}

func wireFollowEvidenceMutation(run *Runtime) *wireFollowDelivery {
	delivery := wireFollowDeliveryFor(run)
	delivery.mutateSnap = func(op *contract.Operation) {
		op.Evidence.Availability = "full"
	}
	return delivery
}

func wireFollowIdentityMutation(run *Runtime, otherID contract.OperationID) *wireFollowDelivery {
	delivery := wireFollowDeliveryFor(run)
	delivery.mutateSnap = func(op *contract.Operation) {
		op.ID = otherID
	}
	return delivery
}

func wireFollowCursorMutation(run *Runtime) *wireFollowDelivery {
	delivery := wireFollowDeliveryFor(run)
	delivery.mutateCursor = func(cursor *contract.Cursor) {
		cursor.After++
	}
	return delivery
}

func wireFollowAttemptMutation(run *Runtime) *wireFollowDelivery {
	delivery := wireFollowDeliveryFor(run)
	delivery.mutateResult = func(op *contract.Operation) {
		op.Attempt++
	}
	return delivery
}

func wireFollowErrorCodeMutation(run *Runtime) *wireFollowDelivery {
	delivery := wireFollowDeliveryFor(run)
	delivery.mutateResult = func(op *contract.Operation) {
		op.ErrorCode = contract.Unavailable
	}
	return delivery
}

func wireFollowTerminalMutation(run *Runtime) *wireFollowDelivery {
	delivery := wireFollowDeliveryFor(run)
	delivery.mutatePage = func(events []contract.Event) []contract.Event {
		for index := range events {
			if events[index].Kind == "state" &&
				events[index].State == contract.Succeeded {
				events[index].State = contract.Failed
				events[index].ErrorCode = contract.PreconditionFailed
			}
		}
		return events
	}
	return delivery
}
