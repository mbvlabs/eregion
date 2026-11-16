package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// ensureNoActiveDomainChangeTx serializes a domain binding change with other
// domain changes and with app/environment deletion for the same target. The
// caller has already locked the app and environment rows, so a concurrent
// admission cannot pass this check and then commit behind it.
func (s *Service) ensureNoActiveDomainChangeTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, app, environment string) error {
	target := "env/" + app + "/" + environment
	active, err := tx.Operations().CountActiveDomainConflicts(ctx, entities.DomainConflictGuard{
		ExcludeID: id, EnvironmentTarget: target, AppTarget: "app/" + app,
		DomainAddKind: platform.KindDomainAdd, DomainRemoveKind: platform.KindDomainRemove,
		EnvironmentDeleteKind: platform.KindEnvironmentDelete, AppDeleteKind: platform.KindAppDelete,
	})
	if err != nil {
		return unavailable(err)
	}
	if active > 0 {
		return contract.NewFault(contract.Conflict,
			"another domain or deletion operation is pending for this environment",
			"Wait for the pending operation to finish, then retry; domain binding changes are serialized.")
	}
	return nil
}

type domainBindingIdentity struct {
	App         string
	Environment string
	Primary     bool
	CreatedAt   time.Time
}

// validateDomainOperationIdentity is the pure durable-identity check. It
// requires the operation to still be the newest domain intent for its target
// and refuses a removal whose binding was promoted or re-created after
// admission.
func validateDomainOperationIdentity(op contract.Operation, input *platform.DomainInput, latest contract.OperationID, binding *domainBindingIdentity) error {
	if latest != op.ID {
		return contract.NewFault(contract.Conflict,
			"a newer domain operation exists for this environment",
			"The delayed operation is stale and will not delete or change the binding; retry the current intent.")
	}
	remove := op.Kind == platform.KindDomainRemove
	if binding == nil {
		if remove {
			return nil
		}
		return contract.NewFault(contract.Conflict,
			"the domain binding disappeared before the add ran",
			"Re-add the binding explicitly; no provider change was made by this stale operation.")
	}
	if binding.App != input.App || binding.Environment != input.Environment {
		return contract.NewFault(contract.Conflict,
			"the hostname now belongs to a different environment",
			"This operation is stale; inspect domain list and retry the intended binding.")
	}
	if !remove {
		return nil
	}
	if binding.Primary {
		return contract.NewFault(contract.PreconditionFailed,
			"the domain was promoted to primary after removal was admitted",
			"Retry with a fresh removal plan; the delayed removal did not delete the binding.")
	}
	if binding.CreatedAt.After(op.CreatedAt) {
		return contract.NewFault(contract.Conflict,
			"the domain binding was re-added after removal was admitted",
			"Retry with a fresh removal plan; the delayed removal did not delete the new binding.")
	}
	return nil
}

// revalidateDomainOperation runs under the same target lock as the worker and
// checks the durable operation order and binding identity before any provider
// effect or row deletion.
func (s *Service) revalidateDomainOperation(ctx context.Context, op contract.Operation, input *platform.DomainInput) error {
	latest, found, err := s.entities.Operations().LatestDomainOperation(ctx, op.Target, platform.KindDomainAdd, platform.KindDomainRemove)
	if err != nil || !found {
		return unavailable(err)
	}
	var binding *domainBindingIdentity
	summary, found, err := s.entities.Platform().Domains().BindingSummary(ctx, input.Domain)
	if err != nil {
		return unavailable(err)
	}
	if found {
		binding = &domainBindingIdentity{
			App: summary.App, Environment: summary.Environment, Primary: summary.Primary, CreatedAt: summary.CreatedAt,
		}
	}
	if err := validateDomainOperationIdentity(op, input, latest, binding); err != nil {
		return fmt.Errorf("domain operation %s is stale: %w", op.ID, err)
	}
	return nil
}
