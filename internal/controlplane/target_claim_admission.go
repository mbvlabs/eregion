package controlplane

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// acquirePlatformTargetClaimTx shares scale's transaction gate. This gate
// serializes a new reservation with submission-time writes; the reservation,
// not the advisory lock, owns targets after the transaction commits.
func acquirePlatformTargetClaimTx(ctx context.Context, tx *entities.StoreTx, claim entities.TargetClaimID, targets []string) (bool, error) {
	targets = uniqueTargets(targets)
	for _, target := range targets {
		locked, err := tx.Platform().Locks().TryTargetXact(ctx, targetLockKey(target))
		if err != nil || !locked {
			return false, err
		}
	}
	return tx.AcquireTargetClaim(ctx, claim, targets)
}

// Protect enumeration of the environment set during runtime maintenance.
// Scope this gate to operations that can add or move environments (or remove
// their parent); ordinary deploy submissions may still queue behind a claim.
func guardTopologyAdmissionTx(ctx context.Context, tx *entities.StoreTx, normalized platform.Normalized) error {
	var targets []string
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		targets = []string{"platform", "app/" + input.Name}
	case *platform.EnvironmentCreateInput:
		targets = []string{"platform", "app/" + input.App}
	case *platform.EnvironmentUpdateInput:
		targets = []string{"platform", normalized.Target.String()}
	case *platform.AppDeleteInput:
		targets = []string{"platform", "app/" + input.App}
	case *platform.EnvironmentDeleteInput:
		targets = []string{"platform", "app/" + input.App, normalized.Target.String()}
	case *platform.ResourceApplyInput:
		targets = []string{"app/" + input.App, normalized.Target.String()}
	case *platform.ResourceDestroyInput:
		targets = []string{"app/" + input.App, normalized.Target.String()}
	default:
		return nil
	}
	for _, target := range uniqueTargets(targets) {
		locked, err := tx.Platform().Locks().TryTargetXact(ctx, targetLockKey(target))
		if err != nil {
			return unavailable(err)
		}
		if locked {
			claimed, err := tx.TargetClaimed(ctx, target)
			if err != nil {
				return unavailable(err)
			}
			if !claimed {
				continue
			}
		}
		return contract.NewFault(contract.Conflict, "target is reserved by runtime work", "Retry after the operation holding the target settles.")
	}
	return nil
}
