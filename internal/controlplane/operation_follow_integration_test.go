package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/viewer"
)

func TestIntegrationOperationFollowSessionReadsSnapshotAndEvents(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "follow-session", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	for range 205 {
		if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "log", Message: "follow fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	op.State, op.Attempt = contract.Succeeded, 1
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET state=$2,attempt=$3 WHERE id=$1", op.ID, op.State, op.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Succeeded}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	app := api.AppService{Operations: run.Service}
	var sequence uint64
	var snapshots, pages int
	result, err := app.FollowOperation(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, func(observation api.OperationFollowObservation) error {
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
	})
	if err != nil || snapshots == 0 || pages < 2 || sequence != op.LastSequence || result.Cursor.After != sequence || result.Operation.State != contract.Succeeded {
		t.Fatalf("result=%+v snapshots=%d pages=%d sequence=%d err=%v", result, snapshots, pages, sequence, err)
	}
	var resumed uint64
	_, err = app.FollowOperation(ctx, testActor, contract.Cursor{OperationID: op.ID, After: 200}, 200, time.Millisecond, func(observation api.OperationFollowObservation) error {
		for _, event := range observation.Events {
			resumed++
			if event.Sequence != 200+resumed {
				t.Fatalf("unexpected resumed sequence %d", event.Sequence)
			}
		}
		return nil
	})
	if err != nil || resumed != op.LastSequence-200 {
		t.Fatalf("resume count=%d err=%v", resumed, err)
	}
	disconnect := errors.New("simulated disconnect")
	var processed uint64
	_, err = app.FollowOperation(ctx, testActor, contract.Cursor{OperationID: op.ID}, 100, time.Millisecond, func(observation api.OperationFollowObservation) error {
		if len(observation.Events) != 0 {
			processed = observation.Cursor.After
			return disconnect
		}
		return nil
	})
	if !errors.Is(err, disconnect) || processed != 100 {
		t.Fatalf("disconnect cursor=%d err=%v", processed, err)
	}
	replayed := uint64(0)
	_, err = app.FollowOperation(ctx, testActor, contract.Cursor{OperationID: op.ID, After: processed}, 100, time.Millisecond, func(observation api.OperationFollowObservation) error {
		for _, event := range observation.Events {
			replayed++
			if event.Sequence != processed+replayed {
				t.Fatalf("reconnect replay sequence=%d", event.Sequence)
			}
		}
		return nil
	})
	if err != nil || replayed != op.LastSequence-processed {
		t.Fatalf("reconnect count=%d err=%v", replayed, err)
	}
	stop := errors.New("reader stopped")
	_, err = app.FollowOperation(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, func(observation api.OperationFollowObservation) error {
		if len(observation.Events) != 0 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("callback error=%v", err)
	}
	after, err := app.GetOperation(ctx, testActor, op.ID)
	if err != nil || after.State != op.State || after.LastSequence != op.LastSequence {
		t.Fatalf("read changed operation: %+v err=%v", after, err)
	}
}

func TestIntegrationOperationFollowSessionTerminalRaceAndDetach(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "follow-race", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	app := api.AppService{Operations: run.Service}
	var snapshots []contract.State
	var last uint64
	result, err := app.FollowOperation(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, func(observation api.OperationFollowObservation) error {
		if observation.Snapshot != nil {
			snapshots = append(snapshots, observation.Snapshot.State)
			if len(snapshots) == 1 {
				tx, err := pool.Begin(ctx)
				if err != nil {
					return err
				}
				defer rollback(tx)
				op.State, op.Attempt = contract.Succeeded, 1
				if _, err = tx.Exec(ctx, "UPDATE eregion_operations SET state=$2,attempt=$3 WHERE id=$1", op.ID, op.State, op.Attempt); err != nil {
					return err
				}
				if err = appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Succeeded}); err != nil {
					return err
				}
				return tx.Commit(ctx)
			}
		}
		for _, event := range observation.Events {
			if event.Sequence != last+1 {
				t.Fatalf("non-monotonic sequence %d after %d", event.Sequence, last)
			}
			last = event.Sequence
		}
		return nil
	})
	if err != nil || len(snapshots) < 2 || snapshots[0] != contract.Queued || snapshots[len(snapshots)-1] != contract.Succeeded ||
		result.Operation.State != contract.Succeeded || result.Operation.LastSequence != last || result.Cursor.After != last {
		t.Fatalf("snapshots=%v result=%+v last=%d err=%v", snapshots, result, last, err)
	}
	other := contract.Actor{Kind: "ci", Subject: "other"}
	_, err = app.FollowOperation(ctx, other, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, nil)
	if err == nil {
		t.Fatal("another actor could follow the operation")
	}
	operator := operations.OperatorAuthority(contract.Actor{Kind: "operator", Subject: "operator"})
	if _, err := app.FollowOperation(ctx, operator, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, nil); err != nil {
		t.Fatalf("operator authority: %v", err)
	}
	if _, err := app.FollowOperation(ctx, testActor, contract.Cursor{OperationID: op.ID, After: op.LastSequence + 1}, 200, time.Millisecond, nil); err == nil {
		t.Fatal("ahead cursor was accepted")
	}
	pending, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "follow-detach", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithCancel(ctx)
	_, err = app.FollowOperation(readCtx, testActor, contract.Cursor{OperationID: pending.ID}, 200, time.Millisecond, func(observation api.OperationFollowObservation) error {
		if observation.Snapshot != nil {
			cancel()
		}
		return nil
	})
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
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "LOCK TABLE eregion_operations IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	start := time.Now()
	_, err = app.FollowOperation(readCtx, testActor, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, nil)
	elapsed := time.Since(start)
	cancel()
	if err == nil || contract.Classify(err).Code != contract.Unavailable || elapsed < 9*time.Second || elapsed >= 15*time.Second {
		t.Fatalf("bounded read elapsed=%s err=%v", elapsed, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	op.State, op.Attempt = contract.Succeeded, 1
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET state=$2,attempt=$3 WHERE id=$1", op.ID, op.State, op.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Succeeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM eregion_operation_events WHERE operation_id=$1 AND sequence=$2", op.ID, int64(op.LastSequence)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel = context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err = app.FollowOperation(readCtx, testActor, contract.Cursor{OperationID: op.ID}, 200, time.Millisecond, nil)
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

func (f *followDelivery) Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	op, err := f.app.GetOperation(ctx, f.actor, id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(op)
}

func (f *followDelivery) Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	page, err := f.app.OperationEvents(ctx, f.actor, cursor, limit)
	if err != nil {
		return nil, err
	}
	return json.Marshal(page)
}

func (f *followDelivery) FollowSession(ctx context.Context, cursor contract.Cursor, limit int, interval time.Duration, emit func(viewer.SessionObservation) error) (viewer.SessionResult, error) {
	serviceLimit := limit
	if f.mixed {
		serviceLimit--
	}
	result, err := f.app.FollowOperation(ctx, f.actor, cursor, serviceLimit, interval, func(o api.OperationFollowObservation) error {
		if f.duplicate && f.last != nil && len(o.Events) != 0 {
			replayed := *f.last
			if err := emit(viewer.SessionObservation{Events: []contract.Event{replayed}, Cursor: contract.Cursor{OperationID: cursor.OperationID, After: replayed.Sequence}}); err != nil {
				return err
			}
		}
		events := append([]contract.Event(nil), o.Events...)
		if f.mixed && f.last != nil && len(events) != 0 {
			events = append([]contract.Event{*f.last}, events...)
			f.mixedSeen = true
			if f.malformed {
				events[len(events)-1].Sequence++
				o.Cursor.After = events[len(events)-1].Sequence
			}
		}
		for i := range events {
			if f.contradict && events[i].Kind == "state" && events[i].State == contract.Succeeded {
				events[i].State, events[i].ErrorCode = contract.Failed, contract.PreconditionFailed
			}
		}
		if len(events) > 0 {
			last := events[len(events)-1]
			f.last = &last
		}
		if err := emit(viewer.SessionObservation{Snapshot: o.Snapshot, Events: events, Cursor: o.Cursor}); err != nil {
			return err
		}
		if f.interrupt && !f.interrupted && len(events) != 0 {
			f.interrupted = true
			return contract.NewFault(contract.Unavailable, "controlled stream interruption", "Reconnect.")
		}
		return nil
	})
	return viewer.SessionResult{Operation: result.Operation, Cursor: result.Cursor}, err
}

func TestIntegrationOperationFollowSessionConsumerReplayAndContradiction(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "follow-consumer", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	for range 4 {
		if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "log", Message: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	op.State, op.Attempt = contract.Succeeded, 1
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET state=$2,attempt=$3 WHERE id=$1", op.ID, op.State, op.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Succeeded}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	client := &followDelivery{app: api.AppService{Operations: run.Service}, actor: testActor, duplicate: true, interrupt: true}
	var last uint64
	summary, err := viewer.Follow(ctx, client, op.ID, viewer.FollowOptions{PageLimit: 2, PollInterval: time.Millisecond}, func(o viewer.Observation) error {
		for _, event := range o.Events {
			if event.Sequence != last+1 {
				t.Fatalf("replayed or missing event after %d: %d", last, event.Sequence)
			}
			last = event.Sequence
		}
		return nil
	})
	if err != nil || last != op.LastSequence || summary.State != contract.Succeeded {
		t.Fatalf("summary=%+v last=%d err=%v", summary, last, err)
	}
	mixedClient := &followDelivery{app: client.app, actor: testActor, mixed: true}
	last = 0
	summary, err = viewer.Follow(ctx, mixedClient, op.ID, viewer.FollowOptions{PageLimit: 3, PollInterval: time.Millisecond}, func(o viewer.Observation) error {
		for _, event := range o.Events {
			if event.Sequence != last+1 {
				t.Fatalf("mixed batch replayed or missed event after %d: %d", last, event.Sequence)
			}
			last = event.Sequence
		}
		return nil
	})
	if err != nil || !mixedClient.mixedSeen || last != op.LastSequence || summary.Cursor != last {
		t.Fatalf("mixed summary=%+v last=%d mixed=%t err=%v", summary, last, mixedClient.mixedSeen, err)
	}
	badClient := &followDelivery{app: client.app, actor: testActor, mixed: true, malformed: true}
	last = 0
	summary, err = viewer.Follow(ctx, badClient, op.ID, viewer.FollowOptions{PageLimit: 3, PollInterval: time.Millisecond}, func(o viewer.Observation) error {
		for _, event := range o.Events {
			last = event.Sequence
		}
		return nil
	})
	if err == nil || contract.Classify(err).Code != contract.ValidationFailed || !badClient.mixedSeen || last != 2 || summary.Cursor != 2 {
		t.Fatalf("malformed mixed batch advanced: summary=%+v last=%d mixed=%t err=%v", summary, last, badClient.mixedSeen, err)
	}
	client.duplicate, client.contradict, client.interrupt = false, true, false
	_, err = viewer.Follow(ctx, client, op.ID, viewer.FollowOptions{PageLimit: 2, PollInterval: time.Millisecond}, nil)
	if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("contradictory terminal event accepted: %v", err)
	}
	failed, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "follow-failed", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	failed.State, failed.Attempt, failed.ErrorCode = contract.Failed, 1, contract.PreconditionFailed
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET state=$2,attempt=$3,error_code=$4 WHERE id=$1", failed.ID, failed.State, failed.Attempt, failed.ErrorCode); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(ctx, tx, &failed, contract.Event{Kind: "state", State: failed.State, ErrorCode: failed.ErrorCode}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	client.last = nil
	failedSummary, err := viewer.Follow(ctx, client, failed.ID, viewer.FollowOptions{PageLimit: 2, PollInterval: time.Millisecond}, nil)
	if err != nil || failedSummary.State != contract.Failed || failedSummary.ErrorCode != contract.PreconditionFailed || failedSummary.Operation.State != contract.Failed {
		t.Fatalf("failed-operation retrieval summary=%+v err=%v", failedSummary, err)
	}
}
