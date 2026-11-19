package host

// This integration test uses only the disposable PostgreSQL target named by
// EREGION_TEST_DATABASE_URL. It creates uniquely named roles, a database and
// an installation schema, then exercises the real host database and
// credentials steps. It never reads a credential file and never contacts a
// live installation.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/riverqueue/river"
)

type splitFixture struct {
	ctx            context.Context
	dbAdmin        *pgxpool.Pool
	schema         string
	otherSchema    string
	runtimeDSN     string
	migrationDSN   string
	wrongSchemaDSN string
	cfg            Config
}

func TestIntegrationSplitDatabaseRoles(t *testing.T) {
	f := newSplitFixture(t)
	t.Run("missing migration credential fails apply", f.checkMissingCredential)
	t.Run("check is read-only and split apply uses migration role", f.checkApply)
	t.Run("inspections separate owner and runtime", f.checkInspections)
	t.Run("same effective role is rejected", f.checkSameRole)
	t.Run("different schema is rejected", f.checkWrongSchema)
	t.Run("runtime role obeys least privilege", f.checkRuntimePrivileges)
	t.Run("intent persists only the split flag", f.checkIntent)
	t.Run("service environment exposes only the runtime DSN", f.checkServiceEnv)
}

func newSplitFixture(t *testing.T) *splitFixture {
	t.Helper()
	baseDSN := os.Getenv("EREGION_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("set EREGION_TEST_DATABASE_URL to run isolated PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	admin, err := controlplane.Open(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)

	suffix := splitTestSuffix(t)
	database := "eregion_split_" + suffix
	ownerRole := "eregion_split_owner_" + suffix
	migrateRole := "eregion_split_migrate_" + suffix
	appRole := "eregion_split_app_" + suffix
	migratePassword := "migrate" + suffix
	appPassword := "app" + suffix
	const schema = "vaire_installation"
	const otherSchema = "vaire_other"

	splitCreateRoles(t, ctx, admin, ownerRole, migrateRole, migratePassword, appRole, appPassword)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		splitDropFixture(t, cleanupCtx, admin, database, appRole, migrateRole, ownerRole)
	})
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	dbAdminDSN := splitTestDSN(t, baseDSN, "", "", database, "", "")
	dbAdmin, err := controlplane.Open(ctx, dbAdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dbAdmin.Close)
	splitPrepareSchema(t, ctx, dbAdmin, schema, otherSchema, ownerRole, migrateRole, appRole)

	return &splitFixture{
		ctx:            ctx,
		dbAdmin:        dbAdmin,
		schema:         schema,
		otherSchema:    otherSchema,
		runtimeDSN:     splitTestDSN(t, baseDSN, appRole, appPassword, database, schema, ""),
		migrationDSN:   splitTestDSN(t, baseDSN, migrateRole, migratePassword, database, schema, ownerRole),
		wrongSchemaDSN: splitTestDSN(t, baseDSN, migrateRole, migratePassword, database, otherSchema, ownerRole),
		cfg: Config{
			Node: "main", Role: RoleMain, Layout: splitTestLayout(t),
			SplitDatabaseRoles: true,
		},
	}
}

func (f *splitFixture) config(migrationDSN string) Config {
	cfg := f.cfg
	cfg.DatabaseURL = f.runtimeDSN
	cfg.MigrationDatabaseURL = migrationDSN
	return cfg
}

func (f *splitFixture) checkMissingCredential(t *testing.T) {
	step := &databaseStep{cfg: f.config(""), database: PostgresDatabase{}}
	check, err := step.Check(f.ctx, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete || !strings.Contains(check.Detail, "node migrate") {
		t.Fatalf("fresh split check = %+v", check)
	}
	if _, err := step.Apply(f.ctx, engine.Runtime{}, nil); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("split apply without migration URL = %v, want input_required", err)
	}
	if splitRelationExists(t, f.ctx, f.dbAdmin, f.schema+".goose_db_version") {
		t.Fatal("a failed split apply created the application migration ledger")
	}
}

func (f *splitFixture) checkApply(t *testing.T) {
	step := &databaseStep{cfg: f.config(f.migrationDSN), database: PostgresDatabase{}}
	check, err := step.Check(f.ctx, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete {
		t.Fatalf("fresh split check = %+v", check)
	}
	if splitRelationExists(t, f.ctx, f.dbAdmin, f.schema+".goose_db_version") ||
		splitRelationExists(t, f.ctx, f.dbAdmin, f.schema+".river_migration") {
		t.Fatal("database check created migration metadata")
	}
	if _, err := step.Apply(f.ctx, engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	check, err = step.Check(f.ctx, engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("converged split check = %+v, %v", check, err)
	}
	for _, relation := range []string{"goose_db_version", "river_migration", "eregion_operations"} {
		if !splitRelationExists(t, f.ctx, f.dbAdmin, f.schema+"."+relation) {
			t.Fatalf("fresh migrations did not create %s", relation)
		}
	}
}

func (f *splitFixture) checkInspections(t *testing.T) {
	runtimeState, err := (PostgresDatabase{}).Inspect(f.ctx, f.runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.Database != f.dbAdmin.Config().ConnConfig.Database || runtimeState.Schema != f.schema ||
		!runtimeState.Ready() || runtimeState.CanCreateDatabase || runtimeState.CanCreateSchema {
		t.Fatalf("runtime inspection = %+v", runtimeState)
	}
	migrationState, err := (PostgresDatabase{}).Inspect(f.ctx, f.migrationDSN)
	if err != nil {
		t.Fatal(err)
	}
	if migrationState.Schema != f.schema || !migrationState.Ready() ||
		!migrationState.CanCreateDatabase || !migrationState.CanCreateSchema {
		t.Fatalf("migration inspection = %+v", migrationState)
	}
	if runtimeState.User == migrationState.User {
		t.Fatal("migration login did not SET ROLE to a distinct owner")
	}
}

func (f *splitFixture) checkSameRole(t *testing.T) {
	step := &databaseStep{cfg: f.config(f.runtimeDSN), database: PostgresDatabase{}}
	_, err := step.Check(f.ctx, engine.Runtime{})
	if contract.Classify(err).Code != contract.PreconditionFailed || !strings.Contains(err.Error(), "same effective PostgreSQL role") {
		t.Fatalf("same-role split check = %v", err)
	}
}

func (f *splitFixture) checkWrongSchema(t *testing.T) {
	step := &databaseStep{cfg: f.config(f.wrongSchemaDSN), database: PostgresDatabase{}}
	_, err := step.Check(f.ctx, engine.Runtime{})
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("wrong-schema split check = %v", err)
	}
}

func (f *splitFixture) checkRuntimePrivileges(t *testing.T) {
	pool, err := controlplane.Open(f.ctx, f.runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, statement := range []string{
		"CREATE TABLE runtime_ddl_must_fail (id integer)",
		"CREATE SCHEMA runtime_ddl_must_fail",
		"CREATE FUNCTION runtime_ddl_must_fail() RETURNS integer LANGUAGE sql AS 'SELECT 1'",
		"ALTER TABLE eregion_operations ADD COLUMN runtime_ddl_must_fail integer",
		"DROP TABLE eregion_operations",
	} {
		if _, err := pool.Exec(f.ctx, statement); !splitIsPostgresCode(err, "42501") {
			t.Fatalf("%s = %v, want insufficient_privilege", statement, err)
		}
	}
	splitExerciseDML(t, f.ctx, pool)
	splitRunRiverJob(t, f.ctx, pool)
}

func splitExerciseDML(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO eregion_workspace (singleton, workspace_id, name, created_at)
		 VALUES (true, 'ws_0123456789abcdef01234567', 'split-runtime', clock_timestamp())`); err != nil {
		t.Fatalf("runtime insert: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_workspace SET name='split-runtime-updated' WHERE singleton`); err != nil {
		t.Fatalf("runtime update: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_workspace WHERE singleton`); err != nil {
		t.Fatalf("runtime delete: %v", err)
	}
	var sequenceValue int64
	if err := pool.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('river_job','id'))`).Scan(&sequenceValue); err != nil || sequenceValue < 1 {
		t.Fatalf("runtime sequence use = %d, %v", sequenceValue, err)
	}
	var bitmask bool
	if err := pool.QueryRow(ctx,
		`SELECT river_job_state_in_bitmask(B'00000000'::bit(8), 'available'::river_job_state)`).Scan(&bitmask); err != nil {
		t.Fatalf("runtime function use: %v", err)
	}
}

func splitRunRiverJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	store, err := entities.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	worker := &splitDatabaseRiverWorker{ran: make(chan int64, 1)}
	river.AddWorker(workers, worker)
	jobRuntime, err := store.NewJobRuntime(&river.Config{
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			jobargs.DiagnosticQueue: {MaxWorkers: 1},
		},
		TestOnly: true,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stopCancel()
		_ = jobRuntime.StopAndCancel(stopCtx)
	})
	if err := jobRuntime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Jobs().EnqueueDiagnostic(ctx, operationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	select {
	case jobID = <-worker.ran:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(30 * time.Second):
		t.Fatal("River did not run the diagnostic job")
	}
	if jobID < 1 {
		t.Fatalf("River job id = %d", jobID)
	}
	splitAwait(t, ctx, 30*time.Second, func() bool {
		var state string
		return pool.QueryRow(ctx, `SELECT state FROM river_job WHERE id=$1`, jobID).Scan(&state) == nil && state == "completed"
	})
	if err := jobRuntime.StopAndCancel(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
}

func (f *splitFixture) checkIntent(t *testing.T) {
	layout := splitTestLayout(t)
	intentCfg := f.config(f.migrationDSN)
	intentCfg.Layout = layout
	intentCfg.SplitDatabaseRoles = false // derive it from the bootstrap-only URL
	if err := saveIntent(intentCfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(intentPath(layout))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"split_database_roles": true`) {
		t.Fatalf("intent did not record split_database_roles: %s", raw)
	}
	for _, secret := range []string{f.runtimeDSN, f.migrationDSN} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("intent contains a database DSN: %s", raw)
		}
	}
	loaded, err := LoadIntent(layout)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.SplitDatabaseRoles || loaded.DatabaseURL != "" || loaded.MigrationDatabaseURL != "" {
		t.Fatalf("loaded intent = %+v", loaded)
	}
}

func (f *splitFixture) checkServiceEnv(t *testing.T) {
	layout := splitTestLayout(t)
	envCfg := f.config(f.migrationDSN)
	envCfg.Layout = layout
	step := &credentialsStep{cfg: envCfg, credentials: Credentials{}}
	var databaseLines int
	for _, line := range step.managedEnv() {
		if strings.HasPrefix(line, "EREGION_DATABASE_URL=") {
			databaseLines++
			if line != envLine("EREGION_DATABASE_URL", f.runtimeDSN) {
				t.Fatalf("managed runtime URL line = %s", line)
			}
		}
		if strings.Contains(line, f.migrationDSN) || strings.Contains(line, "EREGION_MIGRATION_DATABASE_URL") {
			t.Fatalf("managed environment contains migration material: %s", line)
		}
	}
	if databaseLines != 1 {
		t.Fatalf("managed environment EREGION_DATABASE_URL count = %d", databaseLines)
	}
	if _, err := step.Apply(f.ctx, engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(step.envPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), envLine("EREGION_DATABASE_URL", f.runtimeDSN)) {
		t.Fatal("service environment does not contain the runtime database URL")
	}
	if strings.Contains(string(raw), f.migrationDSN) || strings.Contains(string(raw), "EREGION_MIGRATION_DATABASE_URL") {
		t.Fatal("service environment contains migration material")
	}
	check, err := step.Check(f.ctx, engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("credentials check = %+v, %v", check, err)
	}
}

type splitDatabaseRiverWorker struct {
	river.WorkerDefaults[jobargs.DiagnosticTest]
	ran chan int64
}

func (w *splitDatabaseRiverWorker) Work(_ context.Context, job *river.Job[jobargs.DiagnosticTest]) error {
	w.ran <- job.ID
	return nil
}

func splitTestSuffix(t *testing.T) string {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(string(id), "op_")
}

func splitTestLayout(t *testing.T) Layout {
	t.Helper()
	base := t.TempDir()
	return Layout{
		StateDir:     filepath.Join(base, "state"),
		NodeStateDir: filepath.Join(base, "node"),
		ConfigDir:    filepath.Join(base, "config"),
		DataDir:      filepath.Join(base, "data"),
		BinDir:       filepath.Join(base, "bin"),
		SystemdDir:   filepath.Join(base, "systemd"),
		RegistryDir:  filepath.Join(base, "registry"),
	}
}

func splitTestDSN(t *testing.T, base, user, password, database, searchPath, role string) string {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		parsed.User = url.UserPassword(user, password)
	}
	parsed.Path = "/" + database
	query := parsed.Query()
	if searchPath == "" {
		query.Del("search_path")
	} else {
		query.Set("search_path", searchPath)
	}
	if role == "" {
		query.Del("role")
	} else {
		query.Set("role", role)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func splitCreateRoles(t *testing.T, ctx context.Context, admin *pgxpool.Pool, ownerRole, migrateRole, migratePassword, appRole, appPassword string) {
	t.Helper()
	for _, statement := range []string{
		"CREATE ROLE " + pgx.Identifier{ownerRole}.Sanitize() + " NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE",
		"CREATE ROLE " + pgx.Identifier{migrateRole}.Sanitize() + " LOGIN PASSWORD " + splitLiteral(migratePassword) + " NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE",
		"GRANT " + pgx.Identifier{ownerRole}.Sanitize() + " TO " + pgx.Identifier{migrateRole}.Sanitize(),
		"CREATE ROLE " + pgx.Identifier{appRole}.Sanitize() + " LOGIN PASSWORD " + splitLiteral(appPassword) + " NOSUPERUSER NOCREATEDB NOCREATEROLE",
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func splitPrepareSchema(t *testing.T, ctx context.Context, admin *pgxpool.Pool, schema, otherSchema, ownerRole, migrateRole, appRole string) {
	t.Helper()
	for _, statement := range []string{
		"CREATE SCHEMA " + pgx.Identifier{schema}.Sanitize() + " AUTHORIZATION " + pgx.Identifier{ownerRole}.Sanitize(),
		"CREATE SCHEMA " + pgx.Identifier{otherSchema}.Sanitize() + " AUTHORIZATION " + pgx.Identifier{ownerRole}.Sanitize(),
		"GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO " + pgx.Identifier{appRole}.Sanitize(),
		"GRANT USAGE ON SCHEMA " + pgx.Identifier{otherSchema}.Sanitize() + " TO " + pgx.Identifier{appRole}.Sanitize(),
		"GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO " + pgx.Identifier{migrateRole}.Sanitize(),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{ownerRole}.Sanitize() + " IN SCHEMA " + pgx.Identifier{schema}.Sanitize() +
			" GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + pgx.Identifier{appRole}.Sanitize(),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{ownerRole}.Sanitize() + " IN SCHEMA " + pgx.Identifier{schema}.Sanitize() +
			" GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO " + pgx.Identifier{appRole}.Sanitize(),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + pgx.Identifier{ownerRole}.Sanitize() + " IN SCHEMA " + pgx.Identifier{schema}.Sanitize() +
			" GRANT EXECUTE ON FUNCTIONS TO " + pgx.Identifier{appRole}.Sanitize(),
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func splitLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func splitDropFixture(t *testing.T, ctx context.Context, admin *pgxpool.Pool, database string, roles ...string) {
	t.Helper()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
		t.Errorf("could not remove test database: %v", err)
	}
	for _, role := range roles {
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Errorf("could not remove test role: %v", err)
		}
	}
}

func splitRelationExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func splitIsPostgresCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func splitAwait(t *testing.T, ctx context.Context, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	t.Fatal("timed out waiting for PostgreSQL state")
}
