// Package controlplane implements PostgreSQL state and River orchestration.
// It is not imported by rendering or transport-independent domain packages.
package controlplane

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/001_operations.sql
var migration001 string

//go:embed migrations/002_node_execution.sql
var migration002 string

type migration struct {
	version int
	name    string
	sql     string
}

// migrations are applied in order. Never edit an applied migration; add a new
// version instead. Each version has its own checksum so adding 002 does not make
// an existing 001 database look edited.
var migrations = []migration{
	{version: 1, name: "001_operations.sql", sql: migration001},
	{version: 2, name: "002_node_execution.sql", sql: migration002},
}

const migrationLock int64 = 0x657265676d696772 // "eregmigr", separate from River locks.

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
	ip := net.ParseIP(u.Hostname())
	loopback := ip != nil && ip.IsLoopback()
	if len(q["sslmode"]) != 1 || (mode != "verify-full" && !(loopback && mode == "disable")) || q.Has("host") || q.Has("port") || q.Has("service") {
		return nil, contract.NewFault(contract.ValidationFailed, "database TLS policy is unsafe or ambiguous", "Use sslmode=verify-full; sslmode=disable is allowed only for literal loopback development hosts. Host overrides are not supported.")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "could not parse database configuration", "Check EREGION_DATABASE_URL without logging its credentials.")
	}
	config.MaxConns = 12
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.ConnConfig.RuntimeParams["application_name"] = "eregion"
	return config, nil
}

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := ParseDatabaseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, unavailable()
	}
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		pool.Close()
		return nil, unavailable()
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

func migrationHash(m migration) string {
	hash := sha256.Sum256([]byte(m.sql))
	return hex.EncodeToString(hash[:])
}

func requiredVersion() int {
	return migrations[len(migrations)-1].version
}

func migrationByVersion(version int) (migration, bool) {
	for _, m := range migrations {
		if m.version == version {
			return m, true
		}
	}
	return migration{}, false
}

// PlanMigrations performs no DDL. Newer/inconsistent schemas fail closed.
func PlanMigrations(ctx context.Context, pool *pgxpool.Pool) (MigrationPlan, error) {
	plan := MigrationPlan{Required: requiredVersion()}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('eregion_schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return plan, unavailable()
	}
	if exists {
		rows, err := pool.Query(ctx, "SELECT version, checksum FROM eregion_schema_migrations ORDER BY version")
		if err != nil {
			return plan, unavailable()
		}
		defer rows.Close()
		expected := 1
		for rows.Next() {
			var version int
			var checksum string
			if err := rows.Scan(&version, &checksum); err != nil {
				return plan, unavailable()
			}
			applied, known := migrationByVersion(version)
			if !known || version > requiredVersion() {
				return plan, contract.NewFault(contract.PreconditionFailed, "database schema is newer than this binary", "Upgrade the binary before using this database; never rewrite applied migrations.")
			}
			if version != expected {
				return plan, contract.NewFault(contract.PreconditionFailed, "database migration history has a gap", "Restore the missing migration history from a backup; do not guess at applied versions.")
			}
			if checksum != migrationHash(applied) {
				return plan, contract.NewFault(contract.PreconditionFailed, "applied migration content differs", "Use a compatible binary; never rewrite applied migrations.")
			}
			if version > plan.Current {
				plan.Current = version
			}
			expected++
		}
		if err := rows.Err(); err != nil {
			return plan, unavailable()
		}
	}
	m, err := migrator(pool)
	if err != nil {
		return plan, err
	}
	versions := m.AllVersions()
	plan.RiverRequired = versions[len(versions)-1].Version
	if err := pool.QueryRow(ctx, "SELECT to_regclass('river_migration') IS NOT NULL").Scan(&exists); err != nil {
		return plan, unavailable()
	}
	if exists {
		applied, err := m.ExistingVersions(ctx)
		if err != nil {
			return plan, unavailable()
		}
		for _, version := range applied {
			if version.Version > plan.RiverCurrent {
				plan.RiverCurrent = version.Version
			}
		}
		if plan.RiverCurrent > plan.RiverRequired {
			return plan, contract.NewFault(contract.PreconditionFailed, "River schema is newer than this binary", "Upgrade the binary before using this database.")
		}
	}
	return plan, nil
}

func Ready(ctx context.Context, pool *pgxpool.Pool) error {
	plan, err := PlanMigrations(ctx, pool)
	if err != nil {
		return err
	}
	if plan.Current != plan.Required || plan.RiverCurrent != plan.RiverRequired {
		return contract.NewFault(contract.PreconditionFailed, "database migrations are pending", "Run eregion system migrate before starting API or workers.")
	}
	return nil
}

// Migrate serializes across processes using a dedicated session advisory lock.
// River must commit each version separately (enum additions cannot be used in the
// transaction that adds them). Domain DDL is transactional; a partially migrated
// River schema is resumable and never ready. No service migrates automatically.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (MigrationPlan, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return MigrationPlan{}, unavailable()
	}
	defer conn.Release()
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLock); err != nil {
			_ = conn.Conn().Close(unlockCtx) // Never return a lock-holding session to the pool.
		}
	}()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return MigrationPlan{}, unavailable(err)
	}
	plan, err := PlanMigrations(ctx, pool)
	if err != nil {
		return plan, err
	}
	m, err := migrator(pool)
	if err != nil {
		return plan, err
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return plan, unavailable(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS eregion_schema_migrations (version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return plan, unavailable(err)
	}
	for _, m := range migrations {
		if m.version <= plan.Current {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return plan, unavailable(err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			rollback(tx)
			return plan, unavailable(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_schema_migrations(version,checksum) VALUES ($1,$2)`, m.version, migrationHash(m)); err != nil {
			rollback(tx)
			return plan, unavailable(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return plan, unavailable(err)
		}
		plan.Current = m.version
	}
	plan.Current, plan.RiverCurrent = plan.Required, plan.RiverRequired
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
