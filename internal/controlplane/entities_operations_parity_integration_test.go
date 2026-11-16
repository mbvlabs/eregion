package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
)

type operationHistoryFixture struct {
	id     contract.OperationID
	kind   string
	target string
	actor  contract.Actor
	key    string
	state  contract.State
}

// TestIntegrationEntitiesOperationHistoryParity proves the four typed history
// branches match the existing product read exactly: unfiltered actor/all,
// app-only and app+environment. App filters bypass the actor filter, and the
// environment branch excludes the app-wide operation.
func TestIntegrationEntitiesOperationHistoryParity(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	service := &Service{entities: newTestEntities(t, pool)}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := insertFixtureNode(ctx, pool, "parity-node", []string{"platform"}, []string{"build"}, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureApp(ctx, pool, "app-demo", "demo", "example.com/demo"); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, "env-demo-staging", "app-demo", "staging", "parity-node", 18100, 18101, 8080); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, "env-demo-production", "app-demo", "production", "parity-node", 18102, 18103, 8080); err != nil {
		t.Fatal(err)
	}

	foreignCI := contract.Actor{Kind: "ci", Subject: "foreign-ci"}
	foreignWebhook := contract.Actor{Kind: "webhook", Subject: "foreign-webhook"}
	fixtures := seedOperationHistoryFixtures(t, ctx, store, []operationHistoryFixture{
		{kind: "diagnostic.test", target: "control-plane", actor: testActor, key: "own", state: contract.Queued},
		{kind: "diagnostic.test", target: "control-plane", actor: testActor, key: "own-running", state: contract.Running},
		{kind: "platform.deploy", target: "env/demo/staging", actor: foreignCI, key: "ci-env", state: contract.Running},
		{kind: "platform.deploy", target: "env/demo/staging", actor: foreignWebhook, key: "webhook-env", state: contract.Queued},
		{kind: "platform.deploy", target: "app/demo", actor: foreignCI, key: "app-wide", state: contract.Queued},
		{kind: "platform.deploy", target: "env/demo/production", actor: foreignCI, key: "prod-env", state: contract.Queued},
		{kind: "platform.build", target: "build/build-1", actor: foreignCI, key: "build-env", state: contract.Queued},
	})
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_builds
        (id, operation_id, app_id, environment_id, source, commit_sha, state, error_code, error, created_at)
        VALUES ('build-1',$1,'app-demo','env-demo-staging','{}','0123456789abcdef0123456789abcdef01234567','queued','','',clock_timestamp())`,
		fixtures[6].id); err != nil {
		t.Fatal(err)
	}

	checkOperationHistoryBranches(t, ctx, store, service, []operationHistoryBranch{
		{
			name:   "actor-only",
			filter: entities.OperationFilter{Actor: testActor, Limit: 200},
			raw:    operations.ListQuery{Limit: 200},
		},
		{
			name:   "all-actors",
			filter: entities.OperationFilter{AllActors: true, Limit: 200},
			raw:    operations.ListQuery{Limit: 200},
			all:    true,
		},
		{
			name:   "app-only",
			filter: entities.OperationFilter{Actor: testActor, App: "demo", Limit: 200},
			raw:    operations.ListQuery{App: "demo", Limit: 200},
		},
		{
			name:   "app-environment",
			filter: entities.OperationFilter{Actor: testActor, App: "demo", Environment: "staging", Limit: 200},
			raw:    operations.ListQuery{App: "demo", Environment: "staging", Limit: 200},
		},
		{
			name:   "app-environment-running",
			filter: entities.OperationFilter{Actor: testActor, App: "demo", Environment: "staging", State: string(contract.Running), Limit: 200},
			raw:    operations.ListQuery{App: "demo", Environment: "staging", State: string(contract.Running), Limit: 200},
		},
	})
	checkOperationHistoryPolicy(t, ctx, store, fixtures)
}

type operationHistoryBranch struct {
	name   string
	filter entities.OperationFilter
	raw    operations.ListQuery
	all    bool
}

func seedOperationHistoryFixtures(t *testing.T, ctx context.Context, store *entities.Store, fixtures []operationHistoryFixture) []operationHistoryFixture {
	t.Helper()
	for index := range fixtures {
		fixtures[index].id = mustOperationID(t)
		fixture := &fixtures[index]
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
				ID: fixture.id,
				Key: entities.OperationKey{
					Kind: fixture.kind, Target: fixture.target, Actor: fixture.actor,
					IdempotencyKey: fixture.key,
				},
				InputHash: "parity-" + fixture.key,
				Input:     json.RawMessage(`{"app":"demo"}`),
			})
			if err != nil || !inserted {
				t.Fatalf("insert %s = %v err %v", fixture.key, inserted, err)
			}
			if fixture.state != contract.Queued {
				return tx.Operations().SetState(ctx, fixture.id, fixture.state, "")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return fixtures
}

func checkOperationHistoryBranches(t *testing.T, ctx context.Context, store *entities.Store, service *Service, branches []operationHistoryBranch) {
	t.Helper()
	for _, branch := range branches {
		t.Run(branch.name, func(t *testing.T) {
			typed, err := store.Operations().List(ctx, branch.filter)
			if err != nil {
				t.Fatal(err)
			}
			var authority operations.Authority = testActor
			if branch.all {
				authority = operations.OperatorAuthority(testActor)
			}
			page, err := service.ListOperations(ctx, authority, branch.raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(typed) != len(page.Items) {
				t.Fatalf("typed %d items, raw %d items; typed=%v raw=%v",
					len(typed), len(page.Items), operationIDs(typed), operationIDs(page.Items))
			}
			for index := range typed {
				if typed[index].ID != page.Items[index].ID {
					t.Fatalf("item %d typed=%s raw=%s; typed=%v raw=%v",
						index, typed[index].ID, page.Items[index].ID,
						operationIDs(typed), operationIDs(page.Items))
				}
			}
		})
	}
}

func checkOperationHistoryPolicy(t *testing.T, ctx context.Context, store *entities.Store, fixtures []operationHistoryFixture) {
	t.Helper()
	actorOnly, err := store.Operations().List(ctx, entities.OperationFilter{Actor: testActor, Limit: 200})
	if err != nil || len(actorOnly) != 2 {
		t.Fatalf("actor-only items = %v err %v", operationIDs(actorOnly), err)
	}
	appOnly, err := store.Operations().List(ctx, entities.OperationFilter{Actor: testActor, App: "demo", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if containsOperation(appOnly, fixtures[0].id) || containsOperation(appOnly, fixtures[1].id) {
		t.Fatalf("app-only filter included the control-plane operation: %v", operationIDs(appOnly))
	}
	if !containsOperation(appOnly, fixtures[4].id) || !containsOperation(appOnly, fixtures[6].id) {
		t.Fatalf("app-only filter omitted the app-wide or build operation: %v", operationIDs(appOnly))
	}
	environment, err := store.Operations().List(ctx, entities.OperationFilter{
		Actor: testActor, App: "demo", Environment: "staging", Limit: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsOperation(environment, fixtures[4].id) {
		t.Fatalf("environment filter included the app-wide operation: %v", operationIDs(environment))
	}
	if containsOperation(environment, fixtures[5].id) {
		t.Fatalf("environment filter included the other environment: %v", operationIDs(environment))
	}
	for _, index := range []int{2, 3, 6} {
		if !containsOperation(environment, fixtures[index].id) {
			t.Fatalf("environment filter omitted fixture %d: %v", index, operationIDs(environment))
		}
	}
}

func operationIDs(items []contract.Operation) []contract.OperationID {
	ids := make([]contract.OperationID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func containsOperation(items []contract.Operation, id contract.OperationID) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

// TestIntegrationEntitiesTypedInputAndIdentityPattern covers the narrow typed
// adapters for the existing pool-side input and retention-day history reads.
func TestIntegrationEntitiesTypedInputAndIdentityPattern(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first := mustOperationID(t)
	second := mustOperationID(t)
	other := mustOperationID(t)
	input := json.RawMessage(`{"steps":2}`)
	key := func(suffix string) entities.OperationKey {
		return entities.OperationKey{
			Kind: "platform.retention_prune", Target: "node/parity", Actor: testActor,
			IdempotencyKey: "daily-" + suffix,
		}
	}
	for _, fixture := range []struct {
		id  contract.OperationID
		key entities.OperationKey
	}{{first, key("one")}, {second, key("two")}} {
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
				ID: fixture.id, Key: fixture.key, InputHash: "retention", Input: input,
			})
			if err != nil || !inserted {
				t.Fatalf("retention insert = %v err %v", inserted, err)
			}
			raw, found, err := tx.Operations().Input(ctx, fixture.id)
			if err != nil || !found || !sameJSONInput(raw, input) {
				t.Fatalf("transaction input = %s found=%v err=%v", raw, found, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: other,
			Key: entities.OperationKey{
				Kind: "platform.retention_prune", Target: "node/other", Actor: testActor,
				IdempotencyKey: "daily-other",
			},
			InputHash: "other", Input: input,
		})
		if err != nil || !inserted {
			t.Fatalf("other insert = %v err %v", inserted, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	raw, found, err := store.Operations().Input(ctx, first)
	if err != nil || !found || !sameJSONInput(raw, input) {
		t.Fatalf("pool input = %s found=%v err=%v", raw, found, err)
	}
	pattern, err := store.Operations().ListByIdentityPattern(ctx, entities.OperationIdentityPattern{
		Kind: "platform.retention_prune", Target: "node/parity", Actor: testActor,
		IdempotencyPattern: "daily-%",
	})
	if err != nil || len(pattern) != 2 || pattern[0].ID != first || pattern[1].ID != second {
		t.Fatalf("identity-pattern history = %v err %v", operationIDs(pattern), err)
	}
}

// TestIntegrationEntitiesTransactionIdentityForShareLocks proves the
// transaction-scoped share read holds the row lock until the caller commits.
// The lock_timeout is a server-side PostgreSQL stimulus, not a sleep or poll.
func TestIntegrationEntitiesTransactionIdentityForShareLocks(t *testing.T) {
	_, dsn := testPool(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["lock_timeout"] = "250ms"
	lockPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lockPool.Close)
	store, err := entities.NewStore(lockPool)
	if err != nil {
		t.Fatal(err)
	}

	id := mustOperationID(t)
	key := entities.OperationKey{
		Kind: "platform.deploy", Target: "env/demo/staging", Actor: testActor,
		IdempotencyKey: "share-lock",
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id, Key: key, InputHash: "share-lock", Input: json.RawMessage(`{}`),
		})
		if err != nil || !inserted {
			t.Fatalf("share-lock insert = %v err %v", inserted, err)
		}
		return tx.Operations().SetJob(ctx, id, 1)
	}); err != nil {
		t.Fatal(err)
	}

	checkShareLockConflict(t, ctx, store, id, key)
}

func checkShareLockConflict(t *testing.T, ctx context.Context, store *entities.Store, id contract.OperationID, key entities.OperationKey) {
	t.Helper()
	holderReady := make(chan struct{})
	competingDone := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, found, err := tx.Operations().IdentityForShare(ctx, key); err != nil || !found {
				if err != nil {
					return err
				}
				return errors.New("share-lock identity not found")
			}
			close(holderReady)
			<-competingDone
			return tx.Operations().SetJob(ctx, id, 2)
		})
	}()
	select {
	case <-holderReady:
	case err := <-holderDone:
		t.Fatalf("holder failed before ready: %v", err)
	}

	var lockErr error
	conflictErr := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, _, err := tx.Operations().Lock(ctx, id)
		lockErr = err
		return err
	})
	// Release the holder before asserting so a RED ordinary-SELECT run cannot
	// leave the holder transaction blocked on competingDone.
	close(competingDone)
	holderErr := <-holderDone

	var pgErr *pgconn.PgError
	if !errors.As(lockErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("conflicting lock error = %v, want SQLSTATE 55P03", lockErr)
	}
	if !errors.As(conflictErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("conflicting transaction error = %v", conflictErr)
	}
	if holderErr != nil {
		t.Fatal(holderErr)
	}
	var jobID int64
	var bound bool
	var err error
	if err = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, found, err := tx.Operations().Lock(ctx, id); err != nil || !found {
			return err
		}
		jobID, bound, err = tx.Operations().JobID(ctx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !bound || jobID != 2 {
		t.Fatalf("rolled-forward job = %d bound=%v", jobID, bound)
	}
}

func sameJSONInput(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
