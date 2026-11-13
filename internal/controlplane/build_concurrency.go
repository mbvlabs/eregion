package controlplane

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func (s *Service) GetBuildConcurrency(ctx context.Context, actor contract.Actor) (api.BuildConcurrencyResult, error) {
	return s.buildConcurrency(ctx, actor, nil)
}

func (s *Service) SetBuildConcurrency(ctx context.Context, actor contract.Actor, input api.BuildConcurrencyInput) (api.BuildConcurrencyResult, error) {
	if input.Value < 1 || input.Value > platform.MaxBuildConcurrency {
		return api.BuildConcurrencyResult{}, contract.NewFault(contract.ValidationFailed, "invalid build concurrency", "Choose a value from 1 to 16.")
	}
	return s.buildConcurrency(ctx, actor, &input)
}

func (s *Service) buildConcurrency(ctx context.Context, actor contract.Actor, input *api.BuildConcurrencyInput) (api.BuildConcurrencyResult, error) {
	if err := actor.Validate(); err != nil {
		return api.BuildConcurrencyResult{}, err
	}
	if actor.Kind != "operator" {
		return api.BuildConcurrencyResult{}, contract.NewFault(contract.Forbidden, "build concurrency requires an operator", "Use the trusted operator boundary.")
	}
	if s.registryHostIdentity == nil {
		return api.BuildConcurrencyResult{}, contract.NewFault(contract.PreconditionFailed, "the main node identity is not verified", "Use the selected workspace's configured main node.")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.BuildConcurrencyResult{}, unavailable(err)
	}
	defer tx.Rollback(context.Background())
	binding, err := VerifyWorkspaceRegistryHost(ctx, tx, *s.registryHostIdentity)
	if err != nil {
		return api.BuildConcurrencyResult{}, err
	}
	var value int
	var roles []string
	var state, transport string
	err = tx.QueryRow(ctx, `SELECT build_concurrency,roles,enrollment_state,transport FROM eregion_platform_nodes WHERE name=$1 FOR UPDATE`, binding.NodeName).Scan(&value, &roles, &state, &transport)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.BuildConcurrencyResult{}, contract.NewFault(contract.PreconditionFailed, "the verified main node is not registered", "Finish main-node initialization first.")
	}
	if err != nil {
		return api.BuildConcurrencyResult{}, unavailable(err)
	}
	if value < 1 || value > platform.MaxBuildConcurrency || transport != "local" || state != string(platform.EnrollmentRegistered) || !contains(roles, platform.RoleBuilder) {
		return api.BuildConcurrencyResult{}, contract.NewFault(contract.PreconditionFailed, "the main node is not a verified builder", "Finish main-node enrollment and correct invalid node state.")
	}
	result := api.BuildConcurrencyResult{Node: binding.NodeName, Previous: value, Effective: value, Warnings: []string{
		"Higher concurrency consumes additional CPU, memory, disk and cache space.",
		"Worker concurrency and database pool limits can prevent reaching this capacity.",
		"The limit bounds live advisory admissions; a detached build after database session loss may outlive its slot.",
		"Reducing capacity does not cancel running builds or restart workers.",
	}}
	if input != nil {
		result.DryRun = input.DryRun
		result.Effective = input.Value
		if !input.DryRun {
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes SET build_concurrency=$2,updated_at=clock_timestamp() WHERE name=$1`, binding.NodeName, input.Value); err != nil {
				return api.BuildConcurrencyResult{}, unavailable(err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return api.BuildConcurrencyResult{}, unavailable(err)
	}
	return result, nil
}
