// Package controlplane implements PostgreSQL state and River orchestration.
package controlplane

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/dbschema"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

const migrationLock int64 = 0x657265676d696772

type MigrationPlan struct {
	Current       int `json:"current"`
	Required      int `json:"required"`
	RiverCurrent  int `json:"river_current"`
	RiverRequired int `json:"river_required"`
}

// ParseDatabaseConfig requires an explicit TLS policy and does not echo a DSN.
func ParseDatabaseConfig(dsn string) (*pgxpool.Config, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Path == "" || u.Path == "/" {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid database URL", "Set EREGION_DATABASE_URL to an explicit PostgreSQL URL with a host and database.")
	}
	q := u.Query()
	mode := q.Get("sslmode")
	if len(q["sslmode"]) != 1 || (mode != "verify-full" && !(PlaintextDatabaseHostAllowed(u.Hostname()) && mode == "disable")) || q.Has("host") || q.Has("port") || q.Has("service") {
		return nil, contract.NewFault(contract.ValidationFailed, "database TLS policy is unsafe or ambiguous", "Use sslmode=verify-full; sslmode=disable is allowed only for loopback and tailnet hosts. Host overrides are not supported.")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "could not parse database configuration", "Check EREGION_DATABASE_URL without logging its credentials.")
	}
	config.MaxConns = 12
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.ConnConfig.RuntimeParams["application_name"] = "eregion"
	config.ConnConfig.Tracer = &readinessTracer{}
	return config, nil
}

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := ParseDatabaseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, ControlDatabaseFault(dsn, err)
	}
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		pool.Close()
		return nil, ControlDatabaseFault(dsn, err)
	}
	if version < 170000 {
		pool.Close()
		return nil, contract.NewFault(contract.ValidationFailed, "PostgreSQL 17 or newer is required", "Use a supported external PostgreSQL server.")
	}
	return pool, nil
}

func migrator(pool *pgxpool.Pool) (*rivermigrate.Migrator[pgx.Tx], error) {
	return rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func schemaMigrator(pool *pgxpool.Pool, schema string) (*rivermigrate.Migrator[pgx.Tx], error) {
	return rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func gooseProvider(db *sql.DB) (*goose.Provider, error) {
	migrations, err := fs.Sub(dbschema.GooseFS, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, migrations)
}

func schemaFault(message string) error {
	return contract.NewFault(contract.PreconditionFailed, message, "Use a fresh database; existing application schemas cannot be adopted or upgraded.")
}

type migrationIdentity struct{ schema, role string }

const identityQuery = `SELECT cardinality(current_schemas(false)), current_schema(), current_user`

func checkedIdentity(count int, schema, role string) (migrationIdentity, error) {
	if count != 1 || schema == "" || role == "" {
		return migrationIdentity{}, contract.NewFault(contract.PreconditionFailed, "unsupported database search_path", "Configure exactly one existing installation schema in search_path for every pool connection.")
	}
	return migrationIdentity{schema: schema, role: role}, nil
}

// Every connection Goose acquires is checked before it can issue any SQL.
type migrationConnector struct {
	driver.Connector
	want migrationIdentity
}

func (c migrationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pgxConn, ok := conn.(*stdlib.Conn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("unexpected PostgreSQL connector")
	}
	var count int
	var schema, role string
	err = pgxConn.Conn().QueryRow(ctx, identityQuery).Scan(&count, &schema, &role)
	if err == nil {
		var got migrationIdentity
		got, err = checkedIdentity(count, schema, role)
		if err == nil && got != c.want {
			err = contract.NewFault(contract.PreconditionFailed, "inconsistent database pool schema or role", "Use the same single-schema search_path and role on every pool connection.")
		}
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// PlanMigrations reads the Goose ledger in a read-only snapshot. Provider status
// methods are deliberately not used: they can initialize the version table.
func PlanMigrations(ctx context.Context, pool *pgxpool.Pool) (MigrationPlan, error) {
	plan, _, err := planMigrations(ctx, pool)
	return plan, err
}

func planMigrations(ctx context.Context, pool *pgxpool.Pool) (MigrationPlan, migrationIdentity, error) {
	plan, identity, err := inspectApplicationMigrations(ctx, pool)
	if err != nil {
		return plan, identity, err
	}
	m, err := schemaMigrator(pool, identity.schema)
	if err != nil {
		return plan, identity, err
	}
	riverVersions := m.AllVersions()
	plan.RiverRequired = riverVersions[len(riverVersions)-1].Version
	if err := readRiverMigrations(ctx, pool, m, identity, &plan); err != nil {
		return plan, identity, err
	}
	return plan, identity, nil
}

func inspectApplicationMigrations(ctx context.Context, pool *pgxpool.Pool) (plan MigrationPlan, identity migrationIdentity, resultErr error) {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close() // releases wrapper connections, not the underlying pgx pool
	provider, err := gooseProvider(db)
	if err != nil {
		return plan, identity, err
	}
	sources := provider.ListSources()
	versions := make([]int64, len(sources))
	for i, source := range sources {
		versions[i] = source.Version
		if source.Version != int64(i+1) {
			return plan, identity, schemaFault("invalid application migration sources")
		}
	}
	if len(versions) == 0 {
		return plan, identity, schemaFault("missing application migration sources")
	}
	if len(versions) > 0 {
		plan.Required = int(versions[len(versions)-1])
	}
	store, err := database.NewStore(database.DialectPostgres, goose.DefaultTablename)
	if err != nil {
		return plan, identity, err
	}
	extended, ok := store.(database.StoreExtender)
	if !ok {
		return plan, identity, schemaFault("Goose store cannot inspect its ledger")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return plan, identity, unavailable(err)
	}
	defer tx.Rollback()
	var count int
	var schema, role sql.NullString
	if err := tx.QueryRowContext(ctx, identityQuery).Scan(&count, &schema, &role); err != nil {
		return plan, identity, unavailable(err)
	}
	identity, err = checkedIdentity(count, schema.String, role.String)
	if err != nil {
		return plan, identity, err
	}
	var old bool
	if err := tx.QueryRowContext(ctx, "SELECT to_regclass('eregion_schema_migrations') IS NOT NULL").Scan(&old); err != nil {
		return plan, identity, unavailable(err)
	}
	if old {
		return plan, identity, schemaFault("legacy application migration ledger exists")
	}
	exists, err := extended.TableExists(ctx, tx)
	if err != nil {
		return plan, identity, unavailable(err)
	}
	if exists {
		history, err := store.ListMigrations(ctx, tx)
		if err != nil {
			return plan, identity, unavailable(err)
		}
		plan.Current, err = validateGooseHistory(history, versions)
		if err != nil {
			return plan, identity, schemaFault("invalid application migration history")
		}
	}
	{
		tables, indexes, err := applicationInventory(plan.Current)
		if err != nil {
			return plan, identity, schemaFault("invalid application migration inventory")
		}
		var orphan bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = ANY(current_schemas(false))
			AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S', 'i', 'I')
			AND (($3 AND c.relname LIKE 'river\_%' ESCAPE '\') OR
			  (c.relname LIKE 'eregion\_%' ESCAPE '\' AND NOT (
			    (c.relkind IN ('r', 'p') AND c.relname = ANY($1::text[]))
			    OR (c.relkind IN ('i', 'I') AND EXISTS (
			      SELECT 1 FROM pg_catalog.pg_index i
			      JOIN pg_catalog.pg_class owner ON owner.oid = i.indrelid
			      WHERE i.indexrelid = c.oid AND owner.relname = ANY($1::text[])
			      AND (c.relname || ':' || owner.relname = ANY($2::text[]) OR EXISTS (
			        SELECT 1 FROM pg_catalog.pg_constraint con
			        WHERE con.conindid = c.oid AND con.conrelid = owner.oid AND con.contype IN ('p', 'u')
			      ))
			    ))
			  )))
			UNION ALL
			SELECT 1 FROM pg_catalog.pg_type t
			JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
			WHERE n.nspname = ANY(current_schemas(false))
			AND (($3 AND t.typname LIKE 'river\_%' ESCAPE '\') OR
			  (t.typname LIKE 'eregion\_%' ESCAPE '\' AND NOT (
			    t.typtype = 'c' AND t.typname = ANY($1::text[]) AND EXISTS (
			      SELECT 1 FROM pg_catalog.pg_class owner
			      WHERE owner.oid = t.typrelid AND owner.relkind IN ('r', 'p') AND owner.relname = t.typname
			    )
			  )))
			UNION ALL
			SELECT 1 FROM pg_catalog.pg_proc p
			JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = ANY(current_schemas(false))
			AND (p.proname LIKE 'eregion\_%' ESCAPE '\' OR ($3 AND p.proname LIKE 'river\_%' ESCAPE '\'))
			UNION ALL
			SELECT 1 FROM pg_catalog.pg_trigger tr
			JOIN pg_catalog.pg_class owner ON owner.oid = tr.tgrelid
			JOIN pg_catalog.pg_namespace n ON n.oid = owner.relnamespace
			WHERE n.nspname = ANY(current_schemas(false)) AND NOT tr.tgisinternal
			AND (tr.tgname LIKE 'eregion\_%' ESCAPE '\' OR owner.relname = ANY($1::text[]))
		)`, tables, indexes, plan.Current < plan.Required).Scan(&orphan)
		if err != nil {
			return plan, identity, unavailable(err)
		}
		if orphan {
			return plan, identity, schemaFault("unexpected application or River metadata before application migrations complete")
		}
	}
	if err := tx.Commit(); err != nil {
		return plan, identity, unavailable(err)
	}
	return plan, identity, nil
}

// River's pgx transaction API cannot share the database/sql Goose transaction;
// its status is a separate read-only snapshot, not an atomic whole-plan view.
func readRiverMigrations(ctx context.Context, pool *pgxpool.Pool, m *rivermigrate.Migrator[pgx.Tx], identity migrationIdentity, plan *MigrationPlan) (err error) {
	ctx, end := beginReadyStage(ctx, "river_versions")
	defer func() { end(err) }()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	var count int
	var schema, role string
	if err := tx.QueryRow(ctx, identityQuery).Scan(&count, &schema, &role); err != nil {
		return unavailable(err)
	}
	got, err := checkedIdentity(count, schema, role)
	if err != nil {
		return err
	}
	if got != identity {
		return contract.NewFault(contract.PreconditionFailed, "inconsistent database pool schema or role", "Use the same single-schema search_path and role on every pool connection.")
	}
	applied, err := m.ExistingVersionsTx(ctx, tx)
	if err != nil {
		return unavailable(err)
	}
	for _, version := range applied {
		if version.Version > plan.RiverCurrent {
			plan.RiverCurrent = version.Version
		}
	}
	if err := checkRiverPlan(*plan); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

func checkRiverPlan(plan MigrationPlan) error {
	if plan.RiverCurrent > plan.RiverRequired {
		return contract.NewFault(contract.PreconditionFailed, "River schema is newer than this binary", "Upgrade the binary before using this database.")
	}
	return nil
}

// Do not wait for the advisory lock while holding a pooled connection:
// concurrent migrators must leave a connection available to the lock owner.
func acquireMigrationLock(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	for {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, unavailable(err)
		}
		var locked bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", migrationLock).Scan(&locked); err != nil {
			// A lost response can leave the session locked; discard it.
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = conn.Conn().Close(closeCtx)
			cancel()
			conn.Release()
			return nil, unavailable(err)
		}
		if locked {
			return conn, nil
		}
		conn.Release()
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, unavailable(ctx.Err())
		case <-timer.C:
		}
	}
}

// Migrate holds the session lock across inspection, Goose and River writes.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (MigrationPlan, error) {
	migrations, err := fs.Sub(dbschema.GooseFS, "migrations")
	if err != nil {
		return MigrationPlan{}, err
	}
	return migrateWithFS(ctx, pool, migrations, nil)
}

// migrateWithFS lets tests exercise a transactional baseline failure without
// changing the production embedded migration inventory.
func migrateWithFS(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS, afterGoose func(context.Context, *pgxpool.Pool) error) (MigrationPlan, error) {
	if pool.Config().MaxConns < 2 {
		return MigrationPlan{}, contract.NewFault(contract.PreconditionFailed, "migration requires at least two database connections", "Increase the database pool capacity before migrating.")
	}
	conn, err := acquireMigrationLock(ctx, pool)
	if err != nil {
		return MigrationPlan{}, unavailable(err)
	}
	defer conn.Release()
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLock); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	plan, identity, err := planMigrations(ctx, pool)
	if err != nil {
		return plan, err
	}
	var count int
	var schema, role string
	if err := conn.QueryRow(ctx, identityQuery).Scan(&count, &schema, &role); err != nil {
		return plan, unavailable(err)
	}
	lockIdentity, err := checkedIdentity(count, schema, role)
	if err != nil {
		return plan, err
	}
	if lockIdentity != identity {
		return plan, contract.NewFault(contract.PreconditionFailed, "inconsistent database pool schema or role", "Use the same single-schema search_path and role on every pool connection.")
	}
	db := sql.OpenDB(migrationConnector{Connector: stdlib.GetPoolConnector(pool), want: identity})
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		return plan, err
	}
	if _, err := provider.Up(ctx); err != nil {
		var fault *contract.Fault
		if errors.As(err, &fault) {
			return plan, fault
		}
		return plan, unavailable(err)
	}
	// Release the checked Goose session before River acquires the second pool
	// connection. Closing the wrapper never closes the pgx pool.
	if err := db.Close(); err != nil {
		return plan, unavailable(err)
	}
	plan.Current = plan.Required
	if afterGoose != nil {
		if err := afterGoose(ctx, pool); err != nil {
			return plan, err
		}
	}
	m, err := schemaMigrator(pool, identity.schema)
	if err != nil {
		return plan, err
	}
	riverConn, err := pool.Acquire(ctx)
	if err != nil {
		return plan, unavailable(err)
	}
	defer riverConn.Release()
	if err := riverConn.QueryRow(ctx, identityQuery).Scan(&count, &schema, &role); err != nil {
		return plan, unavailable(err)
	}
	riverIdentity, err := checkedIdentity(count, schema, role)
	if err != nil {
		return plan, err
	}
	if riverIdentity != identity {
		return plan, contract.NewFault(contract.PreconditionFailed, "inconsistent database pool schema or role", "Use the same single-schema search_path and role on every pool connection.")
	}
	// River versions require separate commits (some use new enum values).
	// One version per outer transaction pins all work to the checked session.
	for {
		tx, err := riverConn.Begin(ctx)
		if err != nil {
			return plan, unavailable(err)
		}
		result, err := m.MigrateTx(ctx, tx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{MaxSteps: 1})
		if err != nil {
			rollback(tx)
			return plan, unavailable(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return plan, unavailable(err)
		}
		if len(result.Versions) == 0 {
			break
		}
	}
	plan.RiverCurrent = plan.RiverRequired
	return plan, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func unavailable(causes ...error) *contract.Fault {
	fault := contract.NewFault(contract.Unavailable, "control-plane database is unavailable", "Check database connectivity and schema readiness. Reuse the same idempotency key after an ambiguous submission.")
	if len(causes) > 0 {
		fault.Cause = causes[0]
	}
	return fault
}

var tailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

func PlaintextDatabaseHostAllowed(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if addr.IsLoopback() {
			return true
		}
		for _, prefix := range tailnetPrefixes {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".ts.net")
}
