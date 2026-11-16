package controlplane

import (
	"context"
	"reflect"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

type operationChildPlacement struct{ kind, id, app, environment string }

func operationRequiresPlacement(kind string) bool {
	switch kind {
	case platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote, platform.KindBuildRetry:
		return true
	default:
		return false
	}
}

func operationChildPlacements(ctx context.Context, tx *entities.StoreTx, operation contract.OperationID) ([]operationChildPlacement, error) {
	children, err := tx.Platform().Pointers().OperationChildren(ctx, operation)
	if err != nil {
		return nil, unavailable(err)
	}
	result := make([]operationChildPlacement, 0, len(children))
	for _, child := range children {
		result = append(result, operationChildPlacement{
			kind: child.Kind, id: child.ID, app: string(child.AppID), environment: string(child.EnvironmentID),
		})
	}
	return result, nil
}

// The operation row is already attempt-fenced. Discover without child locks,
// gate the parent, then reload before taking any child mutation lock.
func lockOperationParentsTx(ctx context.Context, tx *entities.StoreTx, operation contract.OperationID, required bool) error {
	before, err := operationChildPlacements(ctx, tx, operation)
	if err != nil {
		return err
	}
	if len(before) == 0 {
		if required {
			return pointerIntegrityFault()
		}
		return nil
	}
	first := before[0]
	for _, item := range before {
		if !validPointerOwnership(first.app, first.environment, item.app, item.environment) {
			return pointerIntegrityFault()
		}
	}
	app, err := lockPointerEnvironmentTx(ctx, tx, platform.EnvironmentID(first.environment))
	if err != nil {
		return err
	}
	if app != first.app {
		return pointerIntegrityFault()
	}
	after, err := operationChildPlacements(ctx, tx, operation)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return pointerIntegrityFault()
	}
	return nil
}

func lockDeploymentOperationParentsTx(ctx context.Context, tx *entities.StoreTx, operation contract.OperationID, env platform.EnvironmentID, deployment platform.DeploymentID, release platform.ReleaseID) error {
	if err := lockOperationParentsTx(ctx, tx, operation, true); err != nil {
		return err
	}
	matches, err := tx.Platform().Pointers().DeploymentMatchesOperation(ctx, operation, deployment, env, release)
	if err != nil {
		return unavailable(err)
	}
	if !matches {
		return pointerIntegrityFault()
	}
	return nil
}

func lockBuildParentsTx(ctx context.Context, tx *entities.StoreTx, build platform.BuildID, release platform.ReleaseID) error {
	recorded, found, err := tx.Platform().Pointers().BuildOwnership(ctx, build)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return pointerIntegrityFault()
	}
	if recorded.ReleaseID == "" || recorded.ReleaseID != release {
		return pointerIntegrityFault()
	}
	lockedApp, err := lockPointerEnvironmentTx(ctx, tx, recorded.EnvironmentID)
	if err != nil {
		return err
	}
	if lockedApp != string(recorded.AppID) {
		return pointerIntegrityFault()
	}
	after, found, err := tx.Platform().Pointers().BuildOwnership(ctx, build)
	if err != nil {
		return unavailable(err)
	}
	if !found || !validPointerOwnership(string(recorded.AppID), string(recorded.EnvironmentID), string(after.AppID), string(after.EnvironmentID)) || after.ReleaseID != recorded.ReleaseID {
		return pointerIntegrityFault()
	}
	if err := lockOwnedReleaseTx(ctx, tx, string(recorded.AppID), recorded.EnvironmentID, release, false); err != nil {
		return err
	}
	reciprocal, found, err := tx.Platform().Pointers().ReleaseBuildID(ctx, release)
	if err != nil {
		return unavailable(err)
	}
	if !found || reciprocal != build {
		return pointerIntegrityFault()
	}
	return nil
}

// Pointer writers gate the app before the environment and referenced rows.
// Locks live only for the surrounding SQL transaction.
func lockPointerEnvironmentTx(ctx context.Context, tx *entities.StoreTx, envID platform.EnvironmentID) (string, error) {
	appID, found, err := tx.Platform().Environments().AppIDByID(ctx, envID)
	if err != nil {
		return "", unavailable(err)
	}
	if !found {
		return "", pointerIntegrityFault()
	}
	if err := lockAppKeyShareTx(ctx, tx, appID); err != nil {
		return "", err
	}
	pointers, found, err := tx.Platform().Pointers().LockEnvironmentPointers(ctx, envID)
	if err != nil {
		return "", unavailable(err)
	}
	if !found {
		return "", pointerIntegrityFault()
	}
	if pointers.AppID != appID {
		return "", pointerIntegrityFault()
	}
	if pointers.CurrentRevision != "" {
		if err := lockRevisionPointerTx(ctx, tx, envID, pointers.CurrentRevision); err != nil {
			return "", err
		}
	}
	for _, release := range []platform.ReleaseID{pointers.DesiredRelease, pointers.AppliedRelease, pointers.ServingRelease} {
		if release != "" {
			if err := lockOwnedReleaseTx(ctx, tx, string(appID), envID, release, false); err != nil {
				return "", err
			}
		}
	}
	if pointers.ServingDeployment != "" {
		if pointers.ServingRelease == "" {
			return "", pointerIntegrityFault()
		}
		if err := lockOwnedDeploymentTx(ctx, tx, string(appID), envID, pointers.ServingRelease, pointers.ServingDeployment); err != nil {
			return "", err
		}
	}
	return string(appID), nil
}

func lockAppKeyShareTx(ctx context.Context, tx *entities.StoreTx, app platform.AppID) error {
	locked, err := tx.Platform().Pointers().LockAppKeyShare(ctx, app)
	if err != nil {
		return unavailable(err)
	}
	if !locked {
		return pointerIntegrityFault()
	}
	return nil
}

func pointerIntegrityFault() error {
	return contract.NewFault(contract.Unavailable, "platform reference is missing or belongs to another environment", "Inspect the recorded identities before retrying; no reference was changed.")
}

func validPointerOwnership(app, env, actualApp, actualEnv string) bool {
	return app != "" && env != "" && app == actualApp && env == actualEnv
}

func lockRevisionPointerTx(ctx context.Context, tx *entities.StoreTx, envID platform.EnvironmentID, revisionID platform.RevisionID) error {
	if revisionID == "" {
		return pointerIntegrityFault()
	}
	locked, err := tx.Platform().Pointers().LockRevisionPointer(ctx, revisionID, envID)
	if err != nil {
		return unavailable(err)
	}
	if !locked {
		return pointerIntegrityFault()
	}
	return nil
}

func lockReleasePointerTx(ctx context.Context, tx *entities.StoreTx, app string, envID platform.EnvironmentID, releaseID platform.ReleaseID) error {
	return lockOwnedReleaseTx(ctx, tx, app, envID, releaseID, true)
}

func lockOwnedReleaseTx(ctx context.Context, tx *entities.StoreTx, app string, envID platform.EnvironmentID, releaseID platform.ReleaseID, requireUnpruned bool) error {
	if releaseID == "" {
		return pointerIntegrityFault()
	}
	pointer, found, err := tx.Platform().Pointers().LockOwnedRelease(ctx, releaseID, platform.AppID(app), envID)
	if err != nil {
		return unavailable(err)
	}
	if !found || pointer.AppID != platform.AppID(app) || pointer.EnvironmentID != envID || (requireUnpruned && pointer.Pruned) {
		return pointerIntegrityFault()
	}
	if pointer.BuildID != "" {
		build, found, err := tx.Platform().Pointers().LockOwnedBuildForRelease(ctx, pointer.BuildID, platform.AppID(app), envID)
		if err != nil {
			return unavailable(err)
		}
		if !found || build.AppID != platform.AppID(app) || build.EnvironmentID != envID || build.ReleaseID != releaseID {
			return pointerIntegrityFault()
		}
	}
	return nil
}

func lockServingPointersTx(ctx context.Context, tx *entities.StoreTx, envID platform.EnvironmentID, release platform.ReleaseID, deployment platform.DeploymentID) error {
	app, err := lockPointerEnvironmentTx(ctx, tx, envID)
	if err != nil {
		return err
	}
	// Empty/empty means that the frozen predecessor had no serving workload.
	if release == "" && deployment == "" {
		return nil
	}
	if release == "" || deployment == "" {
		return pointerIntegrityFault()
	}
	if err := lockReleasePointerTx(ctx, tx, app, envID, release); err != nil {
		return err
	}
	return lockOwnedDeploymentTx(ctx, tx, app, envID, release, deployment)
}

func lockOwnedDeploymentTx(ctx context.Context, tx *entities.StoreTx, app string, envID platform.EnvironmentID, release platform.ReleaseID, deployment platform.DeploymentID) error {
	locked, err := tx.Platform().Pointers().LockOwnedDeployment(ctx, deployment, platform.AppID(app), envID, release)
	if err != nil {
		return unavailable(err)
	}
	if !locked {
		return pointerIntegrityFault()
	}
	return nil
}

func setReleaseBuildPointerTx(ctx context.Context, tx *entities.StoreTx, envID platform.EnvironmentID, release platform.ReleaseID, build platform.BuildID) error {
	app, err := lockPointerEnvironmentTx(ctx, tx, envID)
	if err != nil {
		return err
	}
	if err := lockReleasePointerTx(ctx, tx, app, envID, release); err != nil {
		return err
	}
	pointer, found, err := tx.Platform().Pointers().LockOwnedBuildForRelease(ctx, build, platform.AppID(app), envID)
	if err != nil {
		return unavailable(err)
	}
	if !found || pointer.AppID != platform.AppID(app) || pointer.EnvironmentID != envID || pointer.ReleaseID != release {
		return pointerIntegrityFault()
	}
	rows, err := tx.Platform().Pointers().SetReleaseBuild(ctx, release, build)
	if err != nil {
		return unavailable(err)
	}
	if rows != 1 {
		return pointerIntegrityFault()
	}
	return nil
}
