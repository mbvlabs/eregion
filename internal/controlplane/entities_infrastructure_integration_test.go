package controlplane

import (
	"database/sql"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
)

func TestIntegrationDatabaseIdentityAcrossPools(t *testing.T) {
	pool, dsn := testPool(t, false)
	other, _ := testPool(t, false)
	ctx := migrationContext(t)

	alias, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("test database URL is invalid")
	}
	q := alias.Query()
	q.Set("application_name", "alternate_endpoint")
	alias.RawQuery = q.Encode()
	alternate, err := Open(ctx, alias.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(alternate.Close)

	same, err := entities.SameDatabase(ctx, pool, alternate)
	if err != nil || !same {
		t.Fatalf("same database: match=%v error=%v", same, err)
	}
	same, err = entities.SameDatabase(ctx, pool, other)
	if err != nil || same {
		t.Fatalf("different database: match=%v error=%v", same, err)
	}
}

func TestIntegrationEntitiesInfrastructureInstallationSchemaPrivileges(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := migrationContext(t)
	role := pool.Config().ConnConfig.Database + "_role"
	if _, err := pool.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS installation CASCADE"); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	if _, err := pool.Exec(ctx, "CREATE SCHEMA installation AUTHORIZATION "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	// A fresh installation may use a dedicated schema and omit public entirely.
	if _, err := pool.Exec(ctx, "DROP SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	config := pool.Config().Copy()
	config.ConnConfig.RuntimeParams["role"] = role
	config.ConnConfig.RuntimeParams["search_path"] = "installation"
	restricted, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restricted.Close)
	facts, err := newTestEntities(t, restricted).DatabaseFacts(ctx)
	if err != nil || facts.User != role || facts.CanCreateDatabase || !facts.CanCreateSchema {
		t.Fatalf("installation privileges = %+v, %v", facts, err)
	}
	plan, err := PlanMigrations(ctx, restricted)
	if err != nil || plan.Current != 0 {
		t.Fatalf("installation plan = %+v, %v", plan, err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = "missing_installation"
	missing, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(missing.Close)
	facts, err = newTestEntities(t, missing).DatabaseFacts(ctx)
	if err != nil || facts.CanCreateSchema {
		t.Fatalf("missing schema facts = %+v, %v", facts, err)
	}
	_, err = PlanMigrations(ctx, missing)
	requireMigrationCode(t, err, contract.PreconditionFailed)
}

func TestIntegrationEntitiesInfrastructureFactsAndSession(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := migrationContext(t)
	store := newTestEntities(t, pool)
	version, err := store.DatabaseVersion(ctx)
	if err != nil || version < 170000 {
		t.Fatalf("version = %d, %v", version, err)
	}
	facts, err := store.DatabaseFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if facts.ServerVersion != version || facts.Database != pool.Config().ConnConfig.Database ||
		facts.User != pool.Config().ConnConfig.User || !facts.CanCreateDatabase || !facts.CanCreateSchema {
		t.Fatalf("database facts = %+v", facts)
	}
	if cfg := pool.Config().ConnConfig; len(cfg.Fallbacks) == 0 && facts.TLS != (cfg.TLSConfig != nil) {
		t.Fatalf("database wire TLS = %v, configured TLS = %v", facts.TLS, cfg.TLSConfig != nil)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	session := entities.NewSessionStore(conn.Conn())
	identity, err := session.DatabaseIdentity(ctx)
	if err != nil || identity.SchemaCount != 1 || identity.Schema != "public" || identity.Role != facts.User {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	const key int64 = 0x656e746974696573
	locked, err := session.TrySessionLock(ctx, key)
	if err != nil || !locked {
		t.Fatalf("lock = %v, %v", locked, err)
	}
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	contender := entities.NewSessionStore(other.Conn())
	locked, err = contender.TrySessionLock(ctx, key)
	if err != nil || locked {
		t.Fatalf("competing lock = %v, %v", locked, err)
	}
	unlocked, err := session.UnlockSession(ctx, key)
	if err != nil || !unlocked {
		t.Fatalf("unlock = %v, %v", unlocked, err)
	}
	locked, err = contender.TrySessionLock(ctx, key)
	if err != nil || !locked {
		t.Fatalf("lock after release = %v, %v", locked, err)
	}
	if _, err := contender.UnlockSession(ctx, key); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationEntitiesInfrastructureMigrationSnapshot(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := migrationContext(t)
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := entities.NewMigrationSnapshot(tx)
	catalog, err := snapshot.Catalog(ctx)
	if err != nil || catalog.LegacyLedger || catalog.LedgerExists || catalog.Identity.Schema != "public" {
		t.Fatalf("fresh catalog = %+v, %v", catalog, err)
	}
	unexpected, err := snapshot.UnexpectedObjects(ctx, nil, nil, true)
	if err != nil || unexpected {
		t.Fatalf("fresh objects = %v, %v", unexpected, err)
	}
	// The application catalog view must use this exact read-only transaction,
	// not a new pooled query after concurrent metadata appears.
	if _, err := pool.Exec(ctx, "CREATE TABLE goose_db_version (version_id bigint)"); err != nil {
		t.Fatal(err)
	}
	catalog, err = snapshot.Catalog(ctx)
	if err != nil || catalog.LedgerExists {
		t.Fatalf("snapshot changed = %+v, %v", catalog, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	next, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Rollback() }()
	catalog, err = entities.NewMigrationSnapshot(next).Catalog(ctx)
	if err != nil || !catalog.LedgerExists {
		t.Fatalf("new snapshot = %+v, %v", catalog, err)
	}
}
