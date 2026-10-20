package controlplane

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

// recoveryDrillPrefix is the only database-name prefix this test creates or
// drops. A name outside it is refused before any SQL runs.
const recoveryDrillPrefix = "eregion_test_recovery_"

func recoveryDrillEnv(t *testing.T, dsn, database string) []string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" {
		t.Fatalf("invalid test database URL")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	query := parsed.Query()
	env := append(os.Environ(),
		"PGHOST="+parsed.Hostname(),
		"PGPORT="+port,
		"PGDATABASE="+database,
		"PGCONNECT_TIMEOUT=10",
		"PGAPPNAME=eregion-cp-recovery-test",
	)
	if parsed.User != nil {
		env = append(env, "PGUSER="+parsed.User.Username())
		if password, ok := parsed.User.Password(); ok {
			env = append(env, "PGPASSWORD="+password)
		}
	}
	for _, key := range []string{"sslmode", "sslrootcert", "sslcert", "sslkey", "sslcrl"} {
		if value := query.Get(key); value != "" {
			env = append(env, "PG"+strings.ToUpper(key)+"="+value)
		}
	}
	return env
}

func recoveryDrillRun(t *testing.T, env []string, name string, args ...string) []byte {
	t.Helper()
	command := exec.Command(name, args...)
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v: %s", name, err, boundedOutput(output))
	}
	return output
}

func boundedOutput(value []byte) string {
	if len(value) > 2048 {
		return string(value[:2048]) + "…"
	}
	return string(value)
}

func recoveryDrillCreate(t *testing.T, ctx context.Context, admin *pgxpool.Pool, created map[string]bool) string {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	name := recoveryDrillPrefix + strings.TrimPrefix(string(id), "op_")
	if !strings.HasPrefix(name, recoveryDrillPrefix) {
		t.Fatalf("generated name %q is outside the drill prefix", name)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	created[name] = true
	return name
}

func recoveryDrillDrop(t *testing.T, ctx context.Context, admin *pgxpool.Pool, created map[string]bool, name string) {
	t.Helper()
	if !strings.HasPrefix(name, recoveryDrillPrefix) || !created[name] {
		t.Fatalf("refusing to drop database %q: it is outside this drill", name)
	}
	if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		t.Errorf("could not drop drill database %s: %v", name, err)
	}
	delete(created, name)
}

func recoveryDrillDSN(t *testing.T, base *url.URL, database string) string {
	t.Helper()
	value := *base
	value.Path = "/" + database
	return value.String()
}

func recoveryDrillRestore(t *testing.T, dsn, database, archive string) string {
	t.Helper()
	env := recoveryDrillEnv(t, dsn, database)
	recoveryDrillRun(t, env, "pg_restore", "--no-owner", "--no-privileges", "--exit-on-error", "--dbname", database, archive)
	return recoveryDrillDSN(t, mustParseURL(t, dsn), database)
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func recoveryDrillVerify(t *testing.T, ctx context.Context, dsn string) (string, MigrationPlan) {
	t.Helper()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Ready(ctx, pool); err != nil {
		t.Fatalf("restored database is not ready: %v", err)
	}
	plan, err := PlanMigrations(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Current != plan.Required || plan.RiverCurrent != plan.RiverRequired {
		t.Fatalf("restored migration plan = %+v", plan)
	}
	for _, table := range []string{
		"eregion_schema_migrations", "eregion_operations", "eregion_operation_events",
		"eregion_operation_steps", "eregion_platform_nodes", "river_job", "river_migration",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("restored database is missing key table %s", table)
		}
	}
	var fingerprint string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(version::text || ':' || checksum, ',' ORDER BY version), '') FROM eregion_schema_migrations`).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	return fingerprint, plan
}

// TestIntegrationControlPlaneRecoveryDrill exercises the documented separate
// control-plane recovery procedure against a disposable database: dump a
// migrated source, restore it into two new databases on the same server, and
// verify migration/River readiness, key tables, and an equivalent second
// restore. It never writes to or drops the source database.
func TestIntegrationControlPlaneRecoveryDrill(t *testing.T) {
	if os.Getenv("EREGION_TEST_DATABASE_URL") == "" {
		t.Skip("set EREGION_TEST_DATABASE_URL to run isolated PostgreSQL integration tests")
	}
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the control-plane recovery drill needs PostgreSQL client tools", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sourcePool, sourceDSN := testPool(t, true)
	sourceFingerprint, sourcePlan := func() (string, MigrationPlan) {
		plan, err := PlanMigrations(ctx, sourcePool)
		if err != nil {
			t.Fatal(err)
		}
		var fingerprint string
		if err := sourcePool.QueryRow(ctx, `SELECT coalesce(string_agg(version::text || ':' || checksum, ',' ORDER BY version), '') FROM eregion_schema_migrations`).Scan(&fingerprint); err != nil {
			t.Fatal(err)
		}
		return fingerprint, plan
	}()

	admin, err := Open(ctx, os.Getenv("EREGION_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	created := map[string]bool{}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for name := range created {
			if strings.HasPrefix(name, recoveryDrillPrefix) {
				_, _ = admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
			}
		}
	})

	sourceName := strings.TrimPrefix(mustParseURL(t, sourceDSN).Path, "/")
	archive := filepath.Join(t.TempDir(), "control-plane.dump")
	env := recoveryDrillEnv(t, sourceDSN, sourceName)
	recoveryDrillRun(t, env, "pg_dump", "--format=custom", "--no-owner", "--no-privileges", "--file", archive)
	info, err := os.Stat(archive)
	if err != nil || info.Size() == 0 {
		t.Fatalf("pg_dump archive is missing or empty: %v", err)
	}
	listing := recoveryDrillRun(t, env, "pg_restore", "--list", archive)
	if len(listing) == 0 || !strings.Contains(strings.ToUpper(string(listing)), "TABLE") {
		t.Fatal("pg_restore --list returned an unusable table of contents")
	}

	first := recoveryDrillCreate(t, ctx, admin, created)
	firstDSN := recoveryDrillRestore(t, sourceDSN, first, archive)
	firstFingerprint, firstPlan := recoveryDrillVerify(t, ctx, firstDSN)

	second := recoveryDrillCreate(t, ctx, admin, created)
	secondDSN := recoveryDrillRestore(t, sourceDSN, second, archive)
	secondFingerprint, secondPlan := recoveryDrillVerify(t, ctx, secondDSN)

	if firstFingerprint != sourceFingerprint || secondFingerprint != sourceFingerprint {
		t.Fatal("restored migration history differs from the source")
	}
	if firstPlan != sourcePlan || secondPlan != sourcePlan {
		t.Fatalf("restored migration plans differ from the source: %+v %+v %+v", sourcePlan, firstPlan, secondPlan)
	}

	recoveryDrillDrop(t, ctx, admin, created, first)
	recoveryDrillDrop(t, ctx, admin, created, second)

	// The source must still be ready and unchanged; the drill only read it.
	afterFingerprint, afterPlan := func() (string, MigrationPlan) {
		if err := Ready(ctx, sourcePool); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanMigrations(ctx, sourcePool)
		if err != nil {
			t.Fatal(err)
		}
		var fingerprint string
		if err := sourcePool.QueryRow(ctx, `SELECT coalesce(string_agg(version::text || ':' || checksum, ',' ORDER BY version), '') FROM eregion_schema_migrations`).Scan(&fingerprint); err != nil {
			t.Fatal(err)
		}
		return fingerprint, plan
	}()
	if afterFingerprint != sourceFingerprint || afterPlan != sourcePlan {
		t.Fatal("the recovery drill changed the source database")
	}
}
