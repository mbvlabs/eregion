package host

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

type databaseStep struct {
	cfg      Config
	database Database
}

func (s *databaseStep) ID() string { return "host-database-v1" }
func (s *databaseStep) Describe() string {
	return "Validate the external PostgreSQL TLS/privileges and apply migrations"
}
func (s *databaseStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *databaseStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		URLHash          string `json:"url_hash"`
		Role             Role   `json:"role"`
		MigrationURLHash string `json:"migration_url_hash,omitempty"`
	}{engine.HashSecret(s.cfg.DatabaseURL), s.cfg.Role, migrationURLHash(s.cfg.MigrationDatabaseURL)})
}

func migrationURLHash(dsn string) string {
	if dsn == "" {
		return ""
	}
	return engine.HashSecret(dsn)
}

func (s *databaseStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	return s.inspect(ctx)
}

func (s *databaseStep) Apply(ctx context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	if s.database == nil {
		return engine.ApplyResult{}, contractError("no database adapter is configured")
	}
	dsn := s.cfg.DatabaseURL
	if s.cfg.usesSplitDatabaseRoles() {
		dsn = s.cfg.MigrationDatabaseURL
		if dsn == "" {
			return engine.ApplyResult{}, contract.NewFault(contract.InputRequired,
				"pending migrations require the separate migration credential",
				"Run node migrate with --migration-database-url-file on the node, then rerun eregion apply.")
		}
		if _, err := s.inspect(ctx); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	state, err := s.database.Migrate(ctx, dsn)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if s.cfg.usesSplitDatabaseRoles() {
		state, err = s.database.Inspect(ctx, s.cfg.DatabaseURL)
		if err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if !state.Ready() {
		return engine.ApplyResult{}, contract.NewFault(contract.PreconditionFailed,
			"migrations did not converge for the runtime connection: "+state.Detail(),
			"Check that the runtime role can read the migration ledgers and that both roles use the same database and schema.")
	}
	return engine.ApplyResult{
		Notes:  []string{"database TLS and privileges verified; migrations are current"},
		Result: map[string]any{"postgres": state.ServerVersion, "schema": state.Current, "river": state.RiverCurrent},
	}, nil
}

func (s *databaseStep) inspect(ctx context.Context) (engine.CheckResult, error) {
	if s.database == nil {
		return engine.CheckResult{}, contractError("no database adapter is configured")
	}
	state, err := s.database.Inspect(ctx, s.cfg.DatabaseURL)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if s.cfg.MigrationDatabaseURL != "" {
		migration, err := s.inspectMigration(ctx, state)
		if err != nil {
			return engine.CheckResult{}, err
		}
		state.CanCreateDatabase, state.CanCreateSchema = migration.CanCreateDatabase, migration.CanCreateSchema
	}
	if state.Ready() {
		return complete(state.Detail())
	}
	detail := state.Detail()
	if s.cfg.usesSplitDatabaseRoles() && s.cfg.MigrationDatabaseURL == "" {
		detail += "; apply schema updates separately with node migrate --migration-database-url-file"
	} else if !state.CanCreateDatabase && !state.CanCreateSchema {
		detail += "; the role lacks CREATE and cannot apply pending migrations"
	}
	return incomplete(detail)
}

func (s *databaseStep) inspectMigration(ctx context.Context, state DatabaseState) (DatabaseState, error) {
	migration, err := s.database.Inspect(ctx, s.cfg.MigrationDatabaseURL)
	if err != nil {
		return migration, err
	}
	if state.Database != migration.Database || state.Schema != migration.Schema || state.Current != migration.Current || state.RiverCurrent != migration.RiverCurrent {
		return migration, contract.NewFault(contract.PreconditionFailed,
			"the database URLs do not report the same database and schema",
			"Point both URLs at the same writable PostgreSQL database and use the same installation search_path.")
	}
	if state.User == migration.User {
		return migration, contract.NewFault(contract.PreconditionFailed,
			"the database URLs use the same effective PostgreSQL role",
			"Supply distinct migration and runtime roles, or select the one-URL setup.")
	}
	same, err := s.database.SameDatabase(ctx, s.cfg.DatabaseURL, s.cfg.MigrationDatabaseURL)
	if err != nil {
		return migration, err
	}
	if !same {
		return migration, contract.NewFault(contract.PreconditionFailed, "the database URLs reach different PostgreSQL databases",
			"Point both URLs at the same writable PostgreSQL database.")
	}
	return migration, nil
}

func (s *databaseStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	return []string{
		"+ verify the external database uses TLS and the role may migrate",
		"+ apply explicit eregion/River migrations through internal/controlplane",
	}, nil
}

func (s *databaseStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
