package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

// These URLs must point at the same disposable, fresh database. The migration
// URL must use a session pool or direct connection; the runtime URL must use a
// distinct role through transaction pooling. This test writes both schemas and
// operation data and deliberately does not create or drop the database.
func TestIntegrationPgBouncerTransactionRuntime(t *testing.T) {
	migrationURL := os.Getenv("EREGION_TEST_PGBOUNCER_MIGRATION_URL")
	runtimeURL := os.Getenv("EREGION_TEST_PGBOUNCER_RUNTIME_URL")
	if migrationURL == "" || runtimeURL == "" {
		t.Skip("set both disposable PgBouncer integration URLs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationPool, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer migrationPool.Close()
	runtimePool, err := Open(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	if runtimePool.Config().ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Fatal("runtime pool must not use session-bound prepared statements")
	}

	migratePgBouncerFixture(t, ctx, migrationPool, runtimePool)
	checkPgBouncerBackendReassignment(t, ctx, runtimePool)
	run := newPgBouncerFixtureRuntime(t, runtimePool)
	defer run.Close()
	checkPgBouncerRiverProcessing(t, ctx, runtimePool, run)
}

func checkPgBouncerBackendReassignment(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	frontend, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frontend.Release()
	const query = "SELECT pg_backend_pid(), $1::integer"
	var initialPID, value int
	if err := frontend.QueryRow(ctx, query, 1).Scan(&initialPID, &value); err != nil {
		t.Fatal(err)
	}
	// Pin the backend just returned to PgBouncer from a different frontend.
	// The original frontend must still execute the same parameterized query.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocker.Rollback(ctx); err != nil {
			t.Errorf("release pinned backend: %v", err)
		}
	}()
	var pinnedPID int
	if err := blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pinnedPID); err != nil {
		t.Fatal(err)
	}
	if initialPID != pinnedPID {
		t.Fatal("fixture did not pin the original frontend's previous backend")
	}
	var reassignedPID int
	if err := frontend.QueryRow(ctx, query, 42).Scan(&reassignedPID, &value); err != nil {
		t.Fatal(err)
	}
	if reassignedPID == initialPID || value != 42 {
		t.Fatalf("expected backend reassignment and value 42, got backend %d and value %d", reassignedPID, value)
	}
}

func migratePgBouncerFixture(t *testing.T, ctx context.Context, migrationPool, runtimePool *pgxpool.Pool) {
	t.Helper()
	var migrationRole, runtimeRole string
	if err := migrationPool.QueryRow(ctx, "SELECT current_user").Scan(&migrationRole); err != nil {
		t.Fatal(err)
	}
	if err := runtimePool.QueryRow(ctx, "SELECT current_user").Scan(&runtimeRole); err != nil {
		t.Fatal(err)
	}
	if migrationRole == runtimeRole {
		t.Fatal("migration and runtime must use different roles")
	}
	var version int
	if err := runtimePool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		t.Fatalf("fixture requires PostgreSQL 18, got version number %d", version)
	}
	plan, err := PlanMigrations(ctx, migrationPool)
	if err != nil {
		t.Fatalf("initial migration plan: %v", err)
	}
	if plan.Current != 0 || plan.RiverCurrent != 0 {
		t.Fatal("fixture requires a fresh disposable database")
	}
	if _, err := Migrate(ctx, migrationPool); err != nil {
		t.Fatalf("session migration: %v: %v", err, errors.Unwrap(err))
	}
	t.Logf("PostgreSQL %d: session migration completed as %s", version, migrationRole)
	if err := Ready(ctx, runtimePool); err != nil {
		t.Fatalf("transaction-pool readiness: %v: %v", err, errors.Unwrap(err))
	}
	t.Logf("transaction-pool readiness succeeded as %s", runtimeRole)
}

func newPgBouncerFixtureRuntime(t *testing.T, runtimePool *pgxpool.Pool) *Runtime {
	t.Helper()
	run, err := New(newTestEntities(t, runtimePool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, NodeStateDir: t.TempDir(), LocalNodeName: "local",
	})
	if err != nil {
		t.Fatalf("runtime startup: %v: %v", err, errors.Unwrap(err))
	}
	return run
}

func checkPgBouncerRiverProcessing(t *testing.T, ctx context.Context, runtimePool *pgxpool.Pool, run *Runtime) {
	t.Helper()
	if err := run.Jobs.Start(ctx); err != nil {
		t.Fatalf("start River: %v", err)
	}
	t.Log("runtime and River workers started through transaction pool")
	defer func() {
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		if err := run.Jobs.StopAndCancel(stopCtx); err != nil {
			t.Errorf("stop River: %v", err)
		}
	}()
	op, err := run.Service.Submit(ctx, submission("pgbouncer-transaction", `{"steps":2}`))
	if err != nil {
		t.Fatalf("submit: %v: %v", err, errors.Unwrap(err))
	}
	await(t, 45*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		if err != nil {
			t.Logf("read operation: %v: %v", err, errors.Unwrap(err))
			return false
		}
		if current.State.Terminal() && current.State != contract.Succeeded {
			t.Fatalf("operation ended in %s", current.State)
		}
		return current.State == contract.Succeeded
	})
	await(t, 10*time.Second, func() bool {
		var state string
		err := runtimePool.QueryRow(ctx, "SELECT state FROM river_job WHERE kind = 'diagnostic_test' AND args->>'operation_id' = $1", op.ID).Scan(&state)
		return err == nil && state == "completed"
	})
	t.Log("diagnostic operation succeeded and River job completed")
}
