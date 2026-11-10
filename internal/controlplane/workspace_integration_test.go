package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

func TestIntegrationWorkspaceAssignmentIsSingletonIdempotentAndImmutable(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()

	unconfigured, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured.Configured || unconfigured.ID != "" || unconfigured.Name != "" {
		t.Fatalf("a migrated installation started configured: %+v", unconfigured)
	}

	assigned, err := AssignWorkspace(ctx, pool, "thelab")
	if err != nil {
		t.Fatal(err)
	}
	if !assigned.Configured || assigned.Name != "thelab" {
		t.Fatalf("assigned identity: %+v", assigned)
	}
	if err := ValidateWorkspaceID(assigned.ID); err != nil {
		t.Fatal(err)
	}
	replayed, err := AssignWorkspace(ctx, pool, "thelab")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != assigned.ID || replayed.Name != assigned.Name {
		t.Fatalf("same-name replay changed the identity: %+v -> %+v", assigned, replayed)
	}
	if _, err := AssignWorkspace(ctx, pool, "otherlab"); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting assignment = %v, want conflict", err)
	}
	read, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if read != assigned {
		t.Fatalf("read identity = %+v, want %+v", read, assigned)
	}

	// The database refuses a direct rewrite even if application code is buggy.
	if _, err := pool.Exec(ctx, `UPDATE eregion_workspace SET name = 'renamed'`); err == nil {
		t.Fatal("an UPDATE of the immutable workspace identity succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_workspace`); err == nil {
		t.Fatal("a DELETE of the immutable workspace identity succeeded")
	}
}

func TestIntegrationConcurrentWorkspaceAssignmentAgrees(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	const workers = 8
	results := make([]WorkspaceIdentity, workers)
	errs := make([]error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = AssignWorkspace(ctx, pool, "thelab")
		}(i)
	}
	wait.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if results[i].ID != results[0].ID || results[i].Name != "thelab" {
			t.Fatalf("worker %d got %+v, want %+v", i, results[i], results[0])
		}
	}
}

func TestIntegrationConcurrentWorkspaceAssignmentWithDifferentNamesHasOneWinner(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	names := []string{"alpha", "beta"}
	results := make([]WorkspaceIdentity, len(names))
	errs := make([]error, len(names))
	var wait sync.WaitGroup
	for i := range names {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = AssignWorkspace(ctx, pool, names[index])
		}(i)
	}
	wait.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("two assignments won: %d and %d", winner, i)
			}
			winner = i
		case contract.Classify(err).Code != contract.Conflict:
			t.Fatalf("loser %d = %v, want conflict", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no assignment won")
	}
	stored, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != names[winner] || !stored.Configured {
		t.Fatalf("stored workspace = %+v, want %q", stored, names[winner])
	}
}

func TestIntegrationWorkspaceReadBeforeMigrationDoesNotInitialize(t *testing.T) {
	pool, _ := testPool(t, false)
	identity, err := ReadWorkspace(context.Background(), pool)
	if !errors.Is(err, ErrWorkspaceSchemaMissing) {
		t.Fatalf("missing schema error = %v", err)
	}
	if identity.Configured {
		t.Fatalf("a pre-migration database was reported as configured: %+v", identity)
	}
	var table *string
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass('eregion_workspace')::text").Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != nil {
		t.Fatalf("a read created the workspace table: %v", *table)
	}
	if _, err := AssignWorkspace(context.Background(), pool, "thelab"); err == nil {
		t.Fatal("assignment succeeded without migration 028")
	}
}

// applyMigrationsThrough applies the domain migrations up to version and
// records their checksums, as Migrate does, without starting River. It is only
// used to construct an authentic 027 database for the forward-migration test.
func applyMigrationsThrough(t *testing.T, pool *pgxpool.Pool, version int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE eregion_schema_migrations (version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > version {
			break
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("migration %d: %v", m.version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_schema_migrations(version,checksum) VALUES ($1,$2)`, m.version, migrationHash(m)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("record migration %d: %v", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegrationWorkspaceMigration027To028PreservesStateAndDoesNotSeedAnIdentity(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := context.Background()
	applyMigrationsThrough(t, pool, 27)

	plan, err := PlanMigrations(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Current != 27 || plan.Required != 28 {
		t.Fatalf("pre-028 plan = %+v", plan)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES ('op_11111111111111111111111111111111','diagnostic.test','control-plane','ci','tester','key-027','sha256:hash','{}','queued')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_provider_credentials
        (provider, zone_id, zone_name, latest_revision, created_by_kind, created_by_subject, updated_by_kind, updated_by_subject)
        VALUES ('cloudflare','0123456789abcdef0123456789abcdef','example.com',0,'operator','tester','operator','tester')`); err != nil {
		t.Fatal(err)
	}

	// Reading before the forward migration must neither create the table nor
	// add an identity row.
	before, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if before.Configured {
		t.Fatalf("027 read reported a workspace: %+v", before)
	}

	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanMigrations(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Current != 28 || plan.Required != 28 {
		t.Fatalf("post-028 plan = %+v", plan)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_workspace`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("migration 028 seeded %d workspace rows", rows)
	}
	if after, err := ReadWorkspace(ctx, pool); err != nil || after.Configured {
		t.Fatalf("post-028 read = %+v, %v; want explicit unconfigured", after, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_workspace`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a read initialized the workspace table: %d rows", rows)
	}
	var marker int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE id = 'op_11111111111111111111111111111111'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 1 {
		t.Fatal("migration 027 state did not survive 028")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credentials WHERE zone_id = '0123456789abcdef0123456789abcdef'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 1 {
		t.Fatal("provider credential metadata did not survive 028")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_schema_migrations WHERE version <= 27`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 27 {
		t.Fatalf("migration history rows through 027 = %d, want 27", marker)
	}
}

func TestIntegrationAssignWorkspaceRejectsAnInvalidNameBeforeDatabaseAccess(t *testing.T) {
	pool, _ := testPool(t, false)
	_, err := AssignWorkspace(context.Background(), pool, "The Lab")
	if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("invalid name = %v, want validation_failed", err)
	}
}
