package controlplane

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/dbschema"
)

// These tests use testPool's explicitly configured disposable database only.
// They are not run by the compile-only local check.
func migrationContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func requireMigrationCode(t *testing.T, err error, want contract.Code) {
	t.Helper()
	if err == nil || contract.Classify(err).Code != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
}

func requireMigrationPostgresError(t *testing.T, err error, code string) {
	t.Helper()
	requireMigrationCode(t, err, contract.Unavailable)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want PostgreSQL code %s", err, code)
	}
}

func migrationExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) {
	t.Helper()
	if _, err := pool.Exec(ctx, query); err != nil {
		t.Fatal(err)
	}
}

func migrationExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func migrationPool(t *testing.T, ctx context.Context, dsn, path string, max int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = max
	if path != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = path
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIntegrationGooseFreshPlanReadOnly(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := migrationContext(t)
	plan, err := PlanMigrations(ctx, pool)
	if err != nil || plan.Current != 0 || plan.Required != 1 || plan.RiverCurrent != 0 {
		t.Fatalf("fresh plan: %+v, %v", plan, err)
	}
	if migrationExists(t, ctx, pool, "goose_db_version") || migrationExists(t, ctx, pool, "river_migration") {
		t.Fatal("planning created migration metadata")
	}
	requireMigrationCode(t, Ready(ctx, pool), contract.PreconditionFailed)
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Ready(ctx, pool); err != nil {
		t.Fatalf("migrated database not ready: %v", err)
	}
}

func TestIntegrationGooseRefusesExistingMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, setup, existing string
	}{
		{"legacy", `CREATE TABLE eregion_schema_migrations (version int); INSERT INTO eregion_schema_migrations VALUES (33)`, "eregion_schema_migrations"},
		{"vault", `CREATE TABLE eregion_secrets (marker int); INSERT INTO eregion_secrets VALUES (7)`, "eregion_secrets"},
		{"river-empty", `CREATE TABLE river_migration (version int)`, "river_migration"},
		{"river-populated", `CREATE TABLE river_migration (version int); INSERT INTO river_migration VALUES (1)`, "river_migration"},
		{"river-job", `CREATE TABLE river_job (marker int); INSERT INTO river_job VALUES (9)`, "river_job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := testPool(t, false)
			ctx := migrationContext(t)
			migrationExec(t, ctx, pool, tc.setup)
			var before int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+tc.existing).Scan(&before); err != nil {
				t.Fatal(err)
			}
			_, err := PlanMigrations(ctx, pool)
			requireMigrationCode(t, err, contract.PreconditionFailed)
			_, err = Migrate(ctx, pool)
			requireMigrationCode(t, err, contract.PreconditionFailed)
			var after int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+tc.existing).Scan(&after); err != nil || before != after {
				t.Fatalf("existing rows changed: %d -> %d, %v", before, after, err)
			}
			if migrationExists(t, ctx, pool, "goose_db_version") || (tc.existing != "river_migration" && migrationExists(t, ctx, pool, "river_migration")) {
				t.Fatal("refusal created migration metadata")
			}
		})
	}
}

func TestIntegrationGooseRejectsMalformedHistory(t *testing.T) {
	for _, tc := range []struct{ name, rows string }{
		{"empty", ""},
		{"missing-zero", "(1,true)"},
		{"duplicate", "(0,true),(1,true),(1,true)"},
		{"down", "(0,true),(1,false)"},
		{"unknown", "(0,true),(2,true)"},
		{"out-of-order", "(1,true),(0,true)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := testPool(t, false)
			ctx := migrationContext(t)
			migrationExec(t, ctx, pool, `CREATE TABLE goose_db_version (id bigserial PRIMARY KEY, version_id bigint NOT NULL, is_applied boolean NOT NULL)`)
			if tc.rows != "" {
				migrationExec(t, ctx, pool, `INSERT INTO goose_db_version(version_id,is_applied) VALUES `+tc.rows)
			}
			var before, after int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			_, err := PlanMigrations(ctx, pool)
			requireMigrationCode(t, err, contract.PreconditionFailed)
			_, err = Migrate(ctx, pool)
			requireMigrationCode(t, err, contract.PreconditionFailed)
			if migrationExists(t, ctx, pool, "river_migration") || migrationExists(t, ctx, pool, "eregion_operations") {
				t.Fatal("malformed ledger caused writes")
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version`).Scan(&after); err != nil || before != after {
				t.Fatalf("history changed: %d -> %d, %v", before, after, err)
			}
		})
	}
}

func TestIntegrationGooseZeroHistoryRefusesExistingRiver(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := migrationContext(t)
	migrationExec(t, ctx, pool, `CREATE TABLE goose_db_version (id bigserial PRIMARY KEY, version_id bigint NOT NULL, is_applied boolean NOT NULL);
		INSERT INTO goose_db_version(version_id,is_applied) VALUES (0,true);
		CREATE TABLE river_migration (version int)`)
	_, err := PlanMigrations(ctx, pool)
	requireMigrationCode(t, err, contract.PreconditionFailed)
	_, err = Migrate(ctx, pool)
	requireMigrationCode(t, err, contract.PreconditionFailed)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("Goose ledger changed: %d %v", count, err)
	}
	if migrationExists(t, ctx, pool, "eregion_operations") {
		t.Fatal("refusal created application tables")
	}
}

func TestIntegrationGooseTransactionalFailureAndRetry(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := migrationContext(t)
	broken := fstest.MapFS{"00001_baseline.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE eregion_failure_marker (id int);\nSELECT no_such_migration_function();\n")}}
	_, err := migrateWithFS(ctx, pool, broken, nil)
	requireMigrationPostgresError(t, err, "42883")
	if migrationExists(t, ctx, pool, "eregion_failure_marker") || migrationExists(t, ctx, pool, "river_migration") {
		t.Fatal("failed baseline left application or River objects")
	}
	var rows, version int
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(version_id),-1) FROM goose_db_version`).Scan(&rows, &version); err != nil || rows != 1 || version != 0 {
		t.Fatalf("failed baseline history: rows=%d version=%d error=%v", rows, version, err)
	}
	plan, err := PlanMigrations(ctx, pool)
	if err != nil || plan.Current != 0 || plan.Required != 1 {
		t.Fatalf("zero-only retry plan: %+v %v", plan, err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestIntegrationRiverPartialFailureRetry(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := migrationContext(t)
	migrations, err := fs.Sub(dbschema.GooseFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	_, err = migrateWithFS(ctx, pool, migrations, func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pool.Exec(ctx, `CREATE TABLE river_job (marker int)`)
		return err
	})
	requireMigrationPostgresError(t, err, "42P07")
	plan, err := PlanMigrations(ctx, pool)
	if err != nil || plan.Current != 1 || plan.RiverCurrent != 1 {
		t.Fatalf("partial River plan: %+v %v", plan, err)
	}
	migrationExec(t, ctx, pool, `DROP TABLE river_job`)
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("River retry: %v", err)
	}
	if err := Ready(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationMigrationSingleSchemaSearchPath(t *testing.T) {
	base, dsn := testPool(t, false)
	ctx := migrationContext(t)
	migrationExec(t, ctx, base, `CREATE SCHEMA tenant`)
	migrationExec(t, ctx, base, `CREATE TABLE public.eregion_schema_migrations (version int)`)
	ambiguous := migrationPool(t, ctx, dsn, "tenant,public", 2)
	_, err := PlanMigrations(ctx, ambiguous)
	requireMigrationCode(t, err, contract.PreconditionFailed)
	if !strings.Contains(err.Error(), "search_path") {
		t.Fatalf("ambiguous path plan: %v", err)
	}
	_, err = Migrate(ctx, ambiguous)
	requireMigrationCode(t, err, contract.PreconditionFailed)
	if !strings.Contains(err.Error(), "search_path") {
		t.Fatalf("ambiguous path migration: %v", err)
	}
	if migrationExists(t, ctx, base, "tenant.goose_db_version") || migrationExists(t, ctx, base, "public.goose_db_version") || migrationExists(t, ctx, base, "tenant.river_migration") {
		t.Fatal("ambiguous search_path created metadata")
	}
	tenant := migrationPool(t, ctx, dsn, "tenant", 2)
	if _, err := Migrate(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if !migrationExists(t, ctx, base, "tenant.goose_db_version") || !migrationExists(t, ctx, base, "tenant.river_migration") || migrationExists(t, ctx, base, "public.goose_db_version") {
		t.Fatal("migration did not stay in the tenant schema")
	}
	if err := Ready(ctx, tenant); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationMigrationTwoConnectionsConcurrencyAndCancellation(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "separate"}[separate], func(t *testing.T) {
			_, dsn := testPool(t, false)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			first := migrationPool(t, ctx, dsn, "", 2)
			second := first
			if separate {
				second = migrationPool(t, ctx, dsn, "", 2)
			}
			var wg sync.WaitGroup
			for _, p := range []*pgxpool.Pool{first, second} {
				wg.Add(1)
				go func(p *pgxpool.Pool) {
					defer wg.Done()
					if _, err := Migrate(ctx, p); err != nil {
						t.Errorf("concurrent migration: %v", err)
					}
				}(p)
			}
			wg.Wait()
			if err := Ready(ctx, first); err != nil {
				t.Fatal(err)
			}
			lock, err := first.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			locked := false
			released := false
			defer func() {
				if released {
					return
				}
				if locked {
					cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if _, err := lock.Exec(cleanupCtx, `SELECT pg_advisory_unlock($1)`, migrationLock); err != nil {
						_ = lock.Conn().Close(cleanupCtx)
					}
				}
				lock.Release()
			}()
			if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
				t.Fatal(err)
			}
			locked = true
			waitCtx, stop := context.WithTimeout(ctx, 250*time.Millisecond)
			_, err = Migrate(waitCtx, second)
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("lock wait error = %v, want deadline exceeded", err)
			}
			requireMigrationCode(t, err, contract.Unavailable)
			if _, err := lock.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLock); err != nil {
				t.Fatal(err)
			}
			locked = false
			lock.Release()
			released = true
			if _, err := Migrate(ctx, first); err != nil {
				t.Fatalf("pool unusable after wrapper close/cancellation: %v", err)
			}
		})
	}
}
