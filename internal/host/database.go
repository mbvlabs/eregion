package host

import (
	"context"
	"fmt"
	"net"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
)

// DatabaseState is a read-only observation of the external control-plane
// database. It never contains the DSN or a password.
type DatabaseState struct {
	ServerVersion     int    `json:"server_version"`
	User              string `json:"user"`
	Database          string `json:"database"`
	TLS               bool   `json:"tls"`
	CanCreateDatabase bool   `json:"can_create_database"`
	CanCreateSchema   bool   `json:"can_create_schema"`
	Current           int    `json:"current"`
	Required          int    `json:"required"`
	RiverCurrent      int    `json:"river_current"`
	RiverRequired     int    `json:"river_required"`
}

func (s DatabaseState) Ready() bool {
	return s.Current == s.Required && s.RiverCurrent == s.RiverRequired
}

func (s DatabaseState) Detail() string {
	if s.Ready() {
		return fmt.Sprintf("PostgreSQL %d is ready; schema %d/%d and River %d/%d requested", s.ServerVersion, s.Current, s.Required, s.RiverCurrent, s.RiverRequired)
	}
	return fmt.Sprintf("schema %d/%d, River %d/%d; migrations are pending", s.Current, s.Required, s.RiverCurrent, s.RiverRequired)
}

// Database is the test seam for the database step. The production
// implementation reuses the existing controlplane services; it does not
// duplicate migration logic.
type Database interface {
	Inspect(context.Context, string) (DatabaseState, error)
	Migrate(context.Context, string) (DatabaseState, error)
}

// PostgresDatabase uses controlplane.Open/PlanMigrations/Migrate.
type PostgresDatabase struct{}

func (PostgresDatabase) Inspect(ctx context.Context, dsn string) (DatabaseState, error) {
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return DatabaseState{}, err
	}
	defer pool.Close()
	return inspectDatabase(ctx, pool, dsn)
}

func (PostgresDatabase) Migrate(ctx context.Context, dsn string) (DatabaseState, error) {
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return DatabaseState{}, err
	}
	defer pool.Close()
	before, err := inspectDatabase(ctx, pool, dsn)
	if err != nil {
		return DatabaseState{}, err
	}
	if !before.Ready() && !before.CanCreateDatabase && !before.CanCreateSchema {
		return before, contract.NewFault(contract.PreconditionFailed, "the database role cannot apply pending migrations", "Grant the migration role CREATE on the database and schema, or apply migrations with an operator role before bootstrap.")
	}
	if _, err := controlplane.Migrate(ctx, pool); err != nil {
		return before, err
	}
	return inspectDatabase(ctx, pool, dsn)
}

func inspectDatabase(ctx context.Context, pool *pgxpool.Pool, dsn string) (DatabaseState, error) {
	state := DatabaseState{}
	var ssl *bool
	err := pool.QueryRow(ctx, `SELECT current_user::text, current_database()::text,
        current_setting('server_version_num')::integer,
        (SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()),
        has_database_privilege(current_user, current_database(), 'CREATE'),
        has_schema_privilege(current_user, 'public', 'CREATE')`).Scan(
		&state.User, &state.Database, &state.ServerVersion, &ssl, &state.CanCreateDatabase, &state.CanCreateSchema)
	if err != nil {
		return DatabaseState{}, contract.NewFault(contract.Unavailable, "could not inspect the control-plane database", "Check connectivity, PostgreSQL version 17 or newer, and the migration role's privileges.")
	}
	state.TLS = ssl != nil && *ssl
	parsed, err := parseDatabaseURL(dsn)
	if err != nil {
		return DatabaseState{}, err
	}
	if !state.TLS && !parsed.loopback {
		return DatabaseState{}, contract.NewFault(contract.PreconditionFailed, "the external database connection is not encrypted", "Use sslmode=verify-full with a trusted CA; plaintext is allowed only for a literal loopback development database.")
	}
	plan, err := controlplane.PlanMigrations(ctx, pool)
	if err != nil {
		return DatabaseState{}, err
	}
	state.Current, state.Required = plan.Current, plan.Required
	state.RiverCurrent, state.RiverRequired = plan.RiverCurrent, plan.RiverRequired
	return state, nil
}

type databaseURL struct {
	password string
	loopback bool
}

func parseDatabaseURL(dsn string) (databaseURL, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" {
		return databaseURL{}, contract.NewFault(contract.ValidationFailed, "invalid database URL", "Use a PostgreSQL URL with an explicit TLS policy; the URL is never logged.")
	}
	out := databaseURL{}
	if parsed.User != nil {
		out.password, _ = parsed.User.Password()
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && ip.IsLoopback() {
		out.loopback = true
	}
	return out, nil
}
