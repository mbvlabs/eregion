package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// ensureNoActiveDomainChangeTx serializes a domain binding change with other
// domain changes and with app/environment deletion for the same target. The
// caller has already locked the app and environment rows, so a concurrent
// admission cannot pass this check and then commit behind it.
func (s *Service) ensureNoActiveDomainChangeTx(ctx context.Context, q rowQueryer, id contract.OperationID, app, environment string) error {
	target := "env/" + app + "/" + environment
	var active int
	err := q.QueryRow(ctx, `SELECT count(*) FROM eregion_operations
        WHERE id<>$1 AND state IN ('queued','running','cancelling')
          AND (
            (kind IN ($2,$3) AND target=$4)
            OR (kind=$5 AND target=$6)
            OR (kind=$7 AND target=$8)
          )`,
		id, platform.KindDomainAdd, platform.KindDomainRemove, target,
		platform.KindEnvironmentDelete, target,
		platform.KindAppDelete, "app/"+app).Scan(&active)
	if err != nil {
		return unavailable()
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
	var latest contract.OperationID
	err := s.pool.QueryRow(ctx, `SELECT id FROM eregion_operations
        WHERE target=$1 AND kind IN ($2,$3)
        ORDER BY created_at DESC, id DESC LIMIT 1`,
		op.Target, platform.KindDomainAdd, platform.KindDomainRemove).Scan(&latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return unavailable()
	}
	if err != nil {
		return unavailable()
	}
	var binding *domainBindingIdentity
	var app, environment string
	var primary bool
	var created time.Time
	err = s.pool.QueryRow(ctx, `SELECT a.name,e.name,d.is_primary,d.created_at
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE d.name=$1`, input.Domain).Scan(&app, &environment, &primary, &created)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		binding = nil
	case err != nil:
		return unavailable()
	default:
		binding = &domainBindingIdentity{App: app, Environment: environment, Primary: primary, CreatedAt: created}
	}
	if err := validateDomainOperationIdentity(op, input, latest, binding); err != nil {
		return fmt.Errorf("domain operation %s is stale: %w", op.ID, err)
	}
	return nil
}
