package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
)

func mustOperationID(t *testing.T) contract.OperationID {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestIntegrationEntitiesOperationEventStepFoundation drives the real
// operation, event and step named queries against isolated PostgreSQL. It
// proves the typed read and transaction views map every column, guard the
// integer ranges, preserve the admission conflict, and keep step completion
// idempotent at the persistence layer.
func TestIntegrationEntitiesOperationEventStepFoundation(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := mustOperationID(t)
	key := entities.OperationKey{
		Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
		IdempotencyKey: "foundation-op-1",
	}
	input := json.RawMessage(`{"steps":2,"step_delay_ms":0,"fail_until_attempt":0}`)

	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id, Key: key, InputHash: "hash-one", Input: input,
		})
		if err != nil {
			return err
		}
		if !inserted {
			t.Fatalf("first operation insert reported a conflict")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The uniqueness constraint arbitrates a replay; the typed identity read
	// returns the accepted operation and hash for the caller's comparison.
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: mustOperationID(t), Key: key, InputHash: "hash-two", Input: input,
		})
		if err != nil {
			return err
		}
		if inserted {
			t.Fatalf("duplicate operation insert was not rejected")
		}
		identity, found, err := tx.Operations().Identity(ctx, key)
		if err != nil {
			return err
		}
		if !found || identity.ID != id || identity.InputHash != "hash-one" {
			t.Fatalf("identity = %+v, found=%v, want %s/hash-one", identity, found, id)
		}
		raw, found, err := tx.Operations().ClaimAttempt(ctx, id, 1)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("claim input was not found")
		}
		var decoded, expected any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		if err := json.Unmarshal(input, &expected); err != nil {
			return err
		}
		if !reflect.DeepEqual(decoded, expected) {
			t.Fatalf("claim input = %s, want %s", raw, input)
		}
		op, found, err := tx.Operations().Lock(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("locked operation disappeared")
		}
		if op.Attempt != 1 || op.LastSequence != 0 {
			t.Fatalf("locked operation = attempt %d sequence %d", op.Attempt, op.LastSequence)
		}
		if job, bound, err := tx.Operations().JobID(ctx, id); err != nil || bound || job != 0 {
			t.Fatalf("unbound job = %d bound=%v err=%v", job, bound, err)
		}
		if err := tx.Operations().SetJob(ctx, id, 4242); err != nil {
			return err
		}
		if job, bound, err := tx.Operations().JobID(ctx, id); err != nil || !bound || job != 4242 {
			t.Fatalf("bound job = %d bound=%v err=%v", job, bound, err)
		}
		if err := tx.Operations().SetState(ctx, id, contract.Running, ""); err != nil {
			return err
		}
		if err := tx.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "step", Stage: "foundation", Message: "foundation step started",
		}); err != nil {
			return err
		}
		if op.LastSequence != 1 {
			t.Fatalf("append advanced sequence to %d", op.LastSequence)
		}
		result := &contract.ActionResult{
			OperationID: id, Action: "diagnostic.test", Target: "control-plane",
			Attempt: 1, Status: contract.Failed, Final: true,
			StartedAt: op.CreatedAt, CompletedAt: time.Now().UTC(),
			History: contract.Cursor{OperationID: id},
		}
		if err := tx.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "state", State: contract.Failed, Result: result,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The share-locking replay read returns the same accepted identity.
	if shared, found, err := store.Operations().IdentityForShare(ctx, key); err != nil || !found ||
		shared.ID != id || shared.InputHash != "hash-one" {
		t.Fatalf("shared identity = %+v found=%v err=%v", shared, found, err)
	}

	op, found, err := store.Operations().Get(ctx, id)
	if err != nil || !found {
		t.Fatalf("get operation: %v found=%v", err, found)
	}
	if op.State != contract.Running || op.Attempt != 1 || op.LastSequence != 2 || op.ErrorCode != "" {
		t.Fatalf("operation snapshot = %+v", op)
	}
	if op.Actor != testActor || op.CreatedAt.IsZero() || op.UpdatedAt.IsZero() {
		t.Fatalf("operation mapping lost actor or timestamps: %+v", op)
	}

	if _, found, err := store.Operations().Get(ctx, mustOperationID(t)); err != nil || found {
		t.Fatalf("missing operation = found %v err %v", found, err)
	}

	events, err := store.Operations().Events(ctx, contract.Cursor{OperationID: id}, op.LastSequence, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 || events[1].State != contract.Failed {
		t.Fatalf("events = %+v", events)
	}
	if exists, err := store.Operations().EventStageExists(ctx, id, "foundation"); err != nil || !exists {
		t.Fatalf("event stage exists = %v err %v", exists, err)
	}
	if message, err := store.Operations().LatestEventMessageForStage(ctx, id, "foundation"); err != nil || message != "foundation step started" {
		t.Fatalf("latest stage message = %q err %v", message, err)
	}

	exists, err := store.Operations().ResultExists(ctx, id, 1, contract.Failed, true)
	if err != nil || !exists {
		t.Fatalf("result exists = %v err %v", exists, err)
	}
	latest, found, err := store.Operations().LatestResult(ctx, id, op.LastSequence)
	if err != nil || !found || latest.Status != contract.Failed || latest.Attempt != 1 {
		t.Fatalf("latest result = %+v found=%v err=%v", latest, found, err)
	}
	if started, found, err := store.Operations().AttemptStartedAt(ctx, id, 1); err != nil || !found || started.After(time.Now()) {
		t.Fatalf("attempt start = %v found=%v err=%v", started, found, err)
	}
	if failed, found, err := store.Operations().FailedStateEvent(ctx, id, op.LastSequence, 1); err != nil || !found || failed.State != contract.Failed {
		t.Fatalf("failed state event = %+v found=%v err=%v", failed, found, err)
	}
	if outputExists, err := store.Operations().ActionOutputExists(ctx, id); err != nil || outputExists {
		t.Fatalf("action output exists = %v err %v", outputExists, err)
	}

	// Steps are persistence primitives: insert a running step, complete it once,
	// count the acknowledgement, then fail a second step that never completed.
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		steps := tx.Steps()
		if err := steps.Insert(ctx, id, "platform-foundation-1", "inputs-1", 1, "exec-one"); err != nil {
			return err
		}
		completed, err := steps.Complete(ctx, id, "platform-foundation-1", "exec-one", json.RawMessage(`{"ok":true}`))
		if err != nil || !completed {
			t.Fatalf("complete step = %v err %v", completed, err)
		}
		if err := steps.Insert(ctx, id, "platform-foundation-2", "inputs-2", 1, "exec-two"); err != nil {
			return err
		}
		failed, err := steps.Fail(ctx, id, "platform-foundation-2", contract.Unavailable, "foundation failure")
		if err != nil || !failed {
			t.Fatalf("fail step = %v err %v", failed, err)
		}
		count, err := steps.CountCompleted(ctx, id)
		if err != nil || count != 1 {
			t.Fatalf("completed count = %d err %v", count, err)
		}
		// FailIncomplete includes an already-failed row, exactly like the
		// existing markIncompleteSteps statement; only completed rows survive.
		if failedCount, err := steps.FailIncomplete(ctx, id, contract.Unavailable, "foundation drain"); err != nil || failedCount != 1 {
			t.Fatalf("fail incomplete = %d err %v", failedCount, err)
		}
		if completedSteps, err := tx.Steps().CountCompleted(ctx, id); err != nil || completedSteps != 1 {
			t.Fatalf("duplicate count = %d err %v", completedSteps, err)
		}
		if err := tx.Operations().SetCompletedSteps(ctx, id, 1); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	first, found, err := store.Steps().Get(ctx, id, "platform-foundation-1")
	if err != nil || !found {
		t.Fatalf("get step: %v found=%v", err, found)
	}
	if first.State != "completed" || first.Attempt != 1 || first.ExecutionID != "exec-one" ||
		first.CompletedAt == nil || first.StartedAt.IsZero() {
		t.Fatalf("step mapping = %+v", first)
	}
	var firstResult map[string]bool
	if err := json.Unmarshal(first.Result, &firstResult); err != nil || !firstResult["ok"] {
		t.Fatalf("step result = %s err %v", first.Result, err)
	}
	second, found, err := store.Steps().Get(ctx, id, "platform-foundation-2")
	if err != nil || !found || second.State != "failed" || second.ErrorCode != contract.Unavailable ||
		second.Error != "foundation drain" || len(second.Result) != 0 {
		t.Fatalf("failed step = %+v found=%v err=%v", second, found, err)
	}
	all, err := store.Steps().List(ctx, id)
	if err != nil || len(all) != 2 {
		t.Fatalf("list steps = %d err %v", len(all), err)
	}
	prefixed, err := store.Steps().ListByPattern(ctx, id, []string{"platform-foundation-%"}, []string{"platform-foundation-1"})
	if err != nil || len(prefixed) != 2 {
		t.Fatalf("list steps by prefix = %d err %v", len(prefixed), err)
	}

	// The row lock serializes two claimers: the later attempt is the one that
	// remains, and neither transaction observes a partial operation.
	locked := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var firstErr, secondErr error
	go func() {
		defer wg.Done()
		firstErr = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, _, err := tx.Operations().Lock(ctx, id); err != nil {
				return err
			}
			close(locked)
			<-release
			return tx.Operations().SetAttempt(ctx, id, 2)
		})
	}()
	<-locked
	go func() {
		defer wg.Done()
		secondErr = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, found, err := tx.Operations().Lock(ctx, id); err != nil {
				return err
			} else if !found {
				return errors.New("locked operation disappeared")
			}
			return tx.Operations().SetAttempt(ctx, id, 3)
		})
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if firstErr != nil || secondErr != nil {
		t.Fatalf("claim race errors: %v / %v", firstErr, secondErr)
	}
	op, found, err = store.Operations().Get(ctx, id)
	if err != nil || !found || op.Attempt != 3 {
		t.Fatalf("final attempt = %+v found=%v err=%v", op, found, err)
	}

	// History filters are SQL parameters, not assembled statement text.
	otherID := mustOperationID(t)
	otherKey := entities.OperationKey{Kind: "diagnostic.test", Target: "app/demo", Actor: testActor, IdempotencyKey: "foundation-op-2"}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: otherID, Key: otherKey, InputHash: "hash-other", Input: input,
		})
		if err != nil {
			return err
		}
		if !inserted {
			return errors.New("second operation insert conflicted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	byActor, err := store.Operations().List(ctx, entities.OperationFilter{Actor: testActor, Limit: 10})
	if err != nil || len(byActor) != 2 {
		t.Fatalf("actor list = %d err %v", len(byActor), err)
	}
	byForeignActor, err := store.Operations().List(ctx, entities.OperationFilter{
		Actor: contract.Actor{Kind: "ci", Subject: "someone-else"}, Limit: 10,
	})
	if err != nil || len(byForeignActor) != 0 {
		t.Fatalf("foreign actor list = %d err %v", len(byForeignActor), err)
	}
	byApp, err := store.Operations().List(ctx, entities.OperationFilter{AllActors: true, App: "demo", Limit: 10})
	if err != nil || len(byApp) != 1 || byApp[0].ID != otherID {
		t.Fatalf("app list = %+v err %v", byApp, err)
	}
	byState, err := store.Operations().List(ctx, entities.OperationFilter{AllActors: true, State: "running", Limit: 10})
	if err != nil || len(byState) != 1 || byState[0].ID != id {
		t.Fatalf("state list = %+v err %v", byState, err)
	}
	if _, err := store.Operations().List(ctx, entities.OperationFilter{AllActors: true}); err == nil {
		t.Fatalf("unbounded operation list was accepted")
	}

	// Concurrent first admissions of one key: exactly one insert wins, every
	// loser reads the same accepted identity.
	raceKey := entities.OperationKey{Kind: "diagnostic.test", Target: "control-plane", Actor: testActor, IdempotencyKey: "foundation-race"}
	const writers = 8
	results := make(chan contract.OperationID, writers)
	insertedCount := make(chan bool, writers)
	errs := make(chan error, writers)
	var raceWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		raceWG.Add(1)
		go func() {
			defer raceWG.Done()
			candidate, err := contract.NewOperationID()
			if err != nil {
				errs <- err
				return
			}
			err = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
				inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
					ID: candidate, Key: raceKey, InputHash: "race-hash", Input: input,
				})
				if err != nil {
					return err
				}
				insertedCount <- inserted
				identity, found, err := tx.Operations().Identity(ctx, raceKey)
				if err != nil {
					return err
				}
				if !found {
					return errors.New("race identity missing")
				}
				results <- identity.ID
				return nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	raceWG.Wait()
	close(results)
	close(insertedCount)
	close(errs)
	for err := range errs {
		t.Fatalf("race writer: %v", err)
	}
	winners := 0
	for inserted := range insertedCount {
		if inserted {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("admission race winners = %d", winners)
	}
	var accepted contract.OperationID
	for identity := range results {
		if accepted == "" {
			accepted = identity
		}
		if identity != accepted {
			t.Fatalf("race identities diverged: %s vs %s", identity, accepted)
		}
	}
}
