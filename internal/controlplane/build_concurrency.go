package controlplane

import (
	"context"
	"errors"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
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
	var result api.BuildConcurrencyResult
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		binding, err := VerifyWorkspaceRegistryHost(ctx, tx.Workspace(), *s.registryHostIdentity)
		if err != nil {
			return err
		}
		node, found, err := tx.Nodes().LockBuildConcurrency(ctx, binding.NodeName)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return contract.NewFault(contract.PreconditionFailed, "the verified main node is not registered", "Finish main-node initialization first.")
		}
		if node.BuildConcurrency < 1 || node.BuildConcurrency > platform.MaxBuildConcurrency ||
			node.Transport != "local" || node.Enrollment != platform.EnrollmentRegistered || !contains(node.Roles, platform.RoleBuilder) {
			return contract.NewFault(contract.PreconditionFailed, "the main node is not a verified builder", "Finish main-node enrollment and correct invalid node state.")
		}
		result = api.BuildConcurrencyResult{Node: binding.NodeName, Previous: node.BuildConcurrency, Effective: node.BuildConcurrency, Warnings: []string{
			"Higher concurrency consumes additional CPU, memory, disk and cache space.",
			"Worker concurrency and database pool limits can prevent reaching this capacity.",
			"The limit bounds live advisory admissions; a detached build after database session loss may outlive its slot.",
			"Reducing capacity does not cancel running builds or restart workers.",
		}}
		if input != nil {
			result.DryRun = input.DryRun
			result.Effective = input.Value
			if !input.DryRun {
				if _, err := tx.Nodes().SetBuildConcurrency(ctx, binding.NodeName, input.Value); err != nil {
					return unavailable(err)
				}
			}
		}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return api.BuildConcurrencyResult{}, unavailable(err)
		}
		var fault *contract.Fault
		if errors.As(err, &fault) {
			return api.BuildConcurrencyResult{}, err
		}
		return api.BuildConcurrencyResult{}, unavailable(err)
	}
	return result, nil
}
