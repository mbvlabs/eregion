package host

import (
	"context"
	"errors"
	"fmt"
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
	// ReadWorkspace is a read-only identity observation. It never creates the
	// workspace row, so status, node.info and export remain safe on an
	// unconfigured alpha.6 installation.
	ReadWorkspace(context.Context, string) (controlplane.WorkspaceIdentity, error)
	// AssignWorkspace is the explicit one-time assignment used by apply
	// --workspace. Replaying the same name returns the existing identity; a
	// different name is a conflict.
	AssignWorkspace(context.Context, string, string) (controlplane.WorkspaceIdentity, error)
}

// PostgresDatabase uses controlplane.Open/PlanMigrations/Migrate.
type PostgresDatabase struct{}

func (PostgresDatabase) Inspect(ctx context.Context, dsn string) (DatabaseState, error) {
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return DatabaseState{}, databaseError(dsn, err)
	}
	defer pool.Close()
	return inspectDatabase(ctx, pool, dsn)
}

func (PostgresDatabase) Migrate(ctx context.Context, dsn string) (DatabaseState, error) {
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return DatabaseState{}, databaseError(dsn, err)
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
		return before, databaseError(dsn, err)
	}
	return inspectDatabase(ctx, pool, dsn)
}

func (PostgresDatabase) ReadWorkspace(ctx context.Context, dsn string) (controlplane.WorkspaceIdentity, error) {
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return controlplane.WorkspaceIdentity{}, databaseError(dsn, err)
	}
	defer pool.Close()
	identity, err := controlplane.ReadWorkspace(ctx, pool)
	if err != nil {
		return controlplane.WorkspaceIdentity{}, databaseError(dsn, err)
	}
	return identity, nil
}

func (PostgresDatabase) AssignWorkspace(ctx context.Context, dsn, name string) (controlplane.WorkspaceIdentity, error) {
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return controlplane.WorkspaceIdentity{}, databaseError(dsn, err)
	}
	defer pool.Close()
	identity, err := controlplane.AssignWorkspace(ctx, pool, name)
	if err != nil {
		return controlplane.WorkspaceIdentity{}, databaseError(dsn, err)
	}
	return identity, nil
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
		return DatabaseState{}, databaseError(dsn, err)
	}
	state.TLS = ssl != nil && *ssl
	parsed, err := parseDatabaseURL(dsn)
	if err != nil {
		return DatabaseState{}, err
	}
	if !state.TLS && !parsed.plaintextAllowed {
		return DatabaseState{}, contract.NewFault(contract.PreconditionFailed, "the external database connection is not encrypted", "Use sslmode=verify-full with a trusted CA; plaintext is allowed only for loopback and tailnet databases.")
	}
	plan, err := controlplane.PlanMigrations(ctx, pool)
	if err != nil {
		return DatabaseState{}, databaseError(dsn, err)
	}
	state.Current, state.Required = plan.Current, plan.Required
	state.RiverCurrent, state.RiverRequired = plan.RiverCurrent, plan.RiverRequired
	return state, nil
}

type databaseURL struct {
	password         string
	plaintextAllowed bool
}

// databaseError keeps validation and schema faults intact and classifies a
// connection failure. The returned fault names the class and host:port, never
// the DSN, user, or password.
func databaseError(dsn string, err error) error {
	if err == nil {
		return nil
	}
	var fault *contract.Fault
	if errors.As(err, &fault) && fault.Code != contract.Unavailable && fault.Code != contract.Internal {
		return err
	}
	return controlplane.ControlDatabaseFault(dsn, err)
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
	out.plaintextAllowed = controlplane.PlaintextDatabaseHostAllowed(parsed.Hostname())
	return out, nil
}
