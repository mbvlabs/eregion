package host

import (
	"context"

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
		URLHash string `json:"url_hash"`
		Role    Role   `json:"role"`
	}{engine.HashSecret(s.cfg.DatabaseURL), s.cfg.Role})
}

func (s *databaseStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	return s.inspect(ctx, false)
}

func (s *databaseStep) Apply(ctx context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	if s.database == nil {
		return engine.ApplyResult{}, contractError("no database adapter is configured")
	}
	state, err := s.database.Migrate(ctx, s.cfg.DatabaseURL)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if !state.Ready() {
		return engine.ApplyResult{}, contractError("migrations did not converge: " + state.Detail())
	}
	return engine.ApplyResult{
		Notes:  []string{"database TLS and privileges verified; migrations are current"},
		Result: map[string]any{"postgres": state.ServerVersion, "schema": state.Current, "river": state.RiverCurrent},
	}, nil
}

func (s *databaseStep) inspect(ctx context.Context, _ bool) (engine.CheckResult, error) {
	if s.database == nil {
		return engine.CheckResult{}, contractError("no database adapter is configured")
	}
	state, err := s.database.Inspect(ctx, s.cfg.DatabaseURL)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if state.Ready() {
		return complete(state.Detail())
	}
	detail := state.Detail()
	if !state.CanCreateDatabase && !state.CanCreateSchema {
		detail += "; the role lacks CREATE and cannot apply pending migrations"
	}
	return incomplete(detail)
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
