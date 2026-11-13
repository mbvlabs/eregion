package controlplane

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
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

func operationChildPlacements(ctx context.Context, tx pgx.Tx, operation contract.OperationID) ([]operationChildPlacement, error) {
	rows, err := tx.Query(ctx, `SELECT kind,id,app_id,environment_id FROM (
		SELECT 'build' AS kind,id,app_id,environment_id FROM eregion_platform_builds WHERE operation_id=$1
		UNION ALL SELECT 'release',id,app_id,environment_id FROM eregion_platform_releases WHERE operation_id=$1
		UNION ALL SELECT 'deployment',id,app_id,environment_id FROM eregion_platform_deployments WHERE operation_id=$1
	) x ORDER BY kind,id`, operation)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var result []operationChildPlacement
	for rows.Next() {
		var item operationChildPlacement
		if err := rows.Scan(&item.kind, &item.id, &item.app, &item.environment); err != nil {
			return nil, unavailable(err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return result, nil
}

// The operation row is already attempt-fenced. Discover without child locks,
// gate the parent, then reload before taking any child mutation lock.
func lockOperationParentsTx(ctx context.Context, tx pgx.Tx, operation contract.OperationID, required bool) error {
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

func lockDeploymentOperationParentsTx(ctx context.Context, tx pgx.Tx, operation contract.OperationID, env platform.EnvironmentID, deployment platform.DeploymentID, release platform.ReleaseID) error {
	if err := lockOperationParentsTx(ctx, tx, operation, true); err != nil {
		return err
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM eregion_platform_deployments
		WHERE operation_id=$1 AND id=$2 AND environment_id=$3 AND release_id=$4)`,
		operation, deployment, env, release).Scan(&matches); err != nil {
		return unavailable(err)
	}
	if !matches {
		return pointerIntegrityFault()
	}
	return nil
}

func lockBuildParentsTx(ctx context.Context, tx pgx.Tx, build platform.BuildID, release platform.ReleaseID) error {
	var app, env, recordedRelease string
	err := tx.QueryRow(ctx, `SELECT app_id,environment_id,COALESCE(release_id,'') FROM eregion_platform_builds WHERE id=$1`, build).Scan(&app, &env, &recordedRelease)
	if errors.Is(err, pgx.ErrNoRows) {
		return pointerIntegrityFault()
	}
	if err != nil {
		return unavailable(err)
	}
	if recordedRelease == "" || recordedRelease != string(release) {
		return pointerIntegrityFault()
	}
	lockedApp, err := lockPointerEnvironmentTx(ctx, tx, platform.EnvironmentID(env))
	if err != nil {
		return err
	}
	if lockedApp != app {
		return pointerIntegrityFault()
	}
	var newApp, newEnv, newRelease string
	err = tx.QueryRow(ctx, `SELECT app_id,environment_id,COALESCE(release_id,'') FROM eregion_platform_builds WHERE id=$1`, build).Scan(&newApp, &newEnv, &newRelease)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!validPointerOwnership(app, env, newApp, newEnv) || newRelease != recordedRelease)) {
		return pointerIntegrityFault()
	}
	if err != nil {
		return unavailable(err)
	}
	if err := lockOwnedReleaseTx(ctx, tx, app, platform.EnvironmentID(env), release, false); err != nil {
		return err
	}
	var reciprocal *string
	if err := tx.QueryRow(ctx, `SELECT build_id FROM eregion_platform_releases WHERE id=$1`, release).Scan(&reciprocal); err != nil {
		return unavailable(err)
	}
	if reciprocal == nil || *reciprocal != string(build) {
		return pointerIntegrityFault()
	}
	return nil
}

// Pointer writers gate the app before the environment and referenced rows.
// Locks live only for the surrounding SQL transaction.
func lockPointerEnvironmentTx(ctx context.Context, tx pgx.Tx, envID platform.EnvironmentID) (string, error) {
	var app string
	if err := tx.QueryRow(ctx, `SELECT app_id FROM eregion_platform_environments WHERE id=$1`, envID).Scan(&app); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", pointerIntegrityFault()
		}
		return "", unavailable(err)
	}
	if err := lockAppKeyShareTx(ctx, tx, app); err != nil {
		return "", err
	}
	var lockedApp string
	var revision, desired, applied, serving, deployment *string
	err := tx.QueryRow(ctx, `SELECT app_id,current_revision_id,desired_release_id,applied_release_id,serving_release_id,serving_deployment_id FROM eregion_platform_environments WHERE id=$1 FOR UPDATE`, envID).Scan(&lockedApp, &revision, &desired, &applied, &serving, &deployment)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", pointerIntegrityFault()
	}
	if err != nil {
		return "", unavailable(err)
	}
	if lockedApp != app {
		return "", pointerIntegrityFault()
	}
	if revision != nil {
		if err := lockRevisionPointerTx(ctx, tx, envID, platform.RevisionID(*revision)); err != nil {
			return "", err
		}
	}
	for _, id := range []*string{desired, applied, serving} {
		if id != nil {
			if err := lockOwnedReleaseTx(ctx, tx, app, envID, platform.ReleaseID(*id), false); err != nil {
				return "", err
			}
		}
	}
	if deployment != nil {
		if serving == nil {
			return "", pointerIntegrityFault()
		}
		if err := lockOwnedDeploymentTx(ctx, tx, app, envID, platform.ReleaseID(*serving), platform.DeploymentID(*deployment)); err != nil {
			return "", err
		}
	}
	return app, nil
}

func lockAppKeyShareTx(ctx context.Context, tx pgx.Tx, app string) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE id=$1 FOR KEY SHARE`, app).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return pointerIntegrityFault()
	}
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func pointerIntegrityFault() error {
	return contract.NewFault(contract.Unavailable, "platform reference is missing or belongs to another environment", "Inspect the recorded identities before retrying; no reference was changed.")
}

func validPointerOwnership(app, env, actualApp, actualEnv string) bool {
	return app != "" && env != "" && app == actualApp && env == actualEnv
}

func lockRevisionPointerTx(ctx context.Context, tx pgx.Tx, envID platform.EnvironmentID, revisionID platform.RevisionID) error {
	if revisionID == "" {
		return pointerIntegrityFault()
	}
	var owner string
	err := tx.QueryRow(ctx, `SELECT environment_id FROM eregion_platform_environment_revisions WHERE id=$1 AND environment_id=$2 FOR KEY SHARE`, revisionID, envID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != string(envID)) {
		return pointerIntegrityFault()
	}
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func lockReleasePointerTx(ctx context.Context, tx pgx.Tx, app string, envID platform.EnvironmentID, releaseID platform.ReleaseID) error {
	return lockOwnedReleaseTx(ctx, tx, app, envID, releaseID, true)
}

func lockOwnedReleaseTx(ctx context.Context, tx pgx.Tx, app string, envID platform.EnvironmentID, releaseID platform.ReleaseID, requireUnpruned bool) error {
	if releaseID == "" {
		return pointerIntegrityFault()
	}
	var actualApp, actualEnv string
	var pruned bool
	var buildID *string
	err := tx.QueryRow(ctx, `SELECT app_id,environment_id,pruned_at IS NOT NULL,build_id FROM eregion_platform_releases WHERE id=$1 AND app_id=$2 AND environment_id=$3 FOR KEY SHARE`, releaseID, app, envID).Scan(&actualApp, &actualEnv, &pruned, &buildID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!validPointerOwnership(app, string(envID), actualApp, actualEnv) || (requireUnpruned && pruned))) {
		return pointerIntegrityFault()
	}
	if err != nil {
		return unavailable(err)
	}
	if buildID != nil {
		var buildApp, buildEnv, buildRelease string
		err := tx.QueryRow(ctx, `SELECT app_id,environment_id,COALESCE(release_id,'') FROM eregion_platform_builds WHERE id=$1 AND app_id=$2 AND environment_id=$3 FOR KEY SHARE`, *buildID, app, envID).Scan(&buildApp, &buildEnv, &buildRelease)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!validPointerOwnership(app, string(envID), buildApp, buildEnv) || buildRelease != string(releaseID))) {
			return pointerIntegrityFault()
		}
		if err != nil {
			return unavailable(err)
		}
	}
	return nil
}

func lockServingPointersTx(ctx context.Context, tx pgx.Tx, envID platform.EnvironmentID, release platform.ReleaseID, deployment platform.DeploymentID) error {
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

func lockOwnedDeploymentTx(ctx context.Context, tx pgx.Tx, app string, envID platform.EnvironmentID, release platform.ReleaseID, deployment platform.DeploymentID) error {
	var actualApp, actualEnv, actualRelease string
	err := tx.QueryRow(ctx, `SELECT app_id,environment_id,release_id FROM eregion_platform_deployments WHERE id=$1 AND app_id=$2 AND environment_id=$3 FOR KEY SHARE`, deployment, app, envID).Scan(&actualApp, &actualEnv, &actualRelease)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!validPointerOwnership(app, string(envID), actualApp, actualEnv) || actualRelease != string(release))) {
		return pointerIntegrityFault()
	}
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func setReleaseBuildPointerTx(ctx context.Context, tx pgx.Tx, envID platform.EnvironmentID, release platform.ReleaseID, build platform.BuildID) error {
	app, err := lockPointerEnvironmentTx(ctx, tx, envID)
	if err != nil {
		return err
	}
	if err := lockReleasePointerTx(ctx, tx, app, envID, release); err != nil {
		return err
	}
	var actualApp, actualEnv, actualRelease string
	err = tx.QueryRow(ctx, `SELECT app_id,environment_id,COALESCE(release_id,'') FROM eregion_platform_builds WHERE id=$1 AND app_id=$2 AND environment_id=$3 FOR KEY SHARE`, build, app, envID).Scan(&actualApp, &actualEnv, &actualRelease)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!validPointerOwnership(app, string(envID), actualApp, actualEnv) || actualRelease != string(release))) {
		return pointerIntegrityFault()
	}
	if err != nil {
		return unavailable(err)
	}
	tag, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET build_id=$2 WHERE id=$1`, release, build)
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() != 1 {
		return pointerIntegrityFault()
	}
	return nil
}
