package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationFirstDeployRestoreServingPreservesNullPointers exercises the
// production restore bookkeeping for a first deploy with no predecessor. The
// full route restore still needs a live node; this test covers the exact
// environment write and the maintenance read/owner-release path that the
// database-backed control plane uses.
func TestIntegrationFirstDeployRestoreServingPreservesNullPointers(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "first-restore-null")
	ctx := context.Background()
	service := &Service{entities: f.store}

	operationID, deploymentID := seedFirstRestoreDeployment(t, ctx, pool, f)
	seedRestoreOwnerAndCleanup(t, ctx, service, f, operationID, deploymentID)
	// This is the production first-deploy restore write. It previously stored
	// empty strings in the three nullable serving pointers.
	if err := service.recordDeploymentRestoreServing(ctx, f.env, "", ""); err != nil {
		t.Fatal(err)
	}
	env := assertRestoredEnvironmentNulls(t, ctx, pool, service, f)
	assertRestoredCleanupOwner(t, ctx, service, f, env, deploymentID)
}

func seedFirstRestoreDeployment(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	f *platformFixture,
) (contract.OperationID, platform.DeploymentID) {
	t.Helper()
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	deploymentID, err := platform.NewDeploymentID()
	if err != nil {
		t.Fatal(err)
	}
	completed := time.Now().UTC()
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: operationID,
			Key: entities.OperationKey{
				Kind: platform.KindDeploy, Target: "env/" + f.app.Name + "/" + f.env.Name,
				Actor: testActor, IdempotencyKey: "first-restore-null",
			},
			InputHash: "hash", Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		if err := tx.Operations().SetState(ctx, operationID, contract.Failed, contract.PreconditionFailed); err != nil {
			return err
		}
		if _, err := tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
			ID: releaseID, OperationID: operationID, AppID: f.app.ID, EnvironmentID: f.env.ID,
			Image: "registry.example/first-restore:latest", ImageID: "sha256:first-restore",
			Source: json.RawMessage(`{"type":"image"}`), Status: platform.ReleaseReady,
			CompletedAt: &completed,
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Deployments().Insert(ctx, entities.DeploymentInsert{
			ID: deploymentID, OperationID: operationID, AppID: f.app.ID, EnvironmentID: f.env.ID,
			Action: platform.ActionDeploy, ReleaseID: releaseID, RevisionID: f.revision.ID,
			SecretRevisions: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Deployments().MarkFailedByOperation(
			ctx, operationID, contract.PreconditionFailed,
			"public verification failed after the Caddy switch; the recorded serving route was restored",
		); err != nil {
			return err
		}
		_, err := tx.Platform().Environments().SetDesiredRelease(ctx, f.env.ID, releaseID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var previousNull bool
	if err := pool.QueryRow(ctx, `SELECT previous_release_id IS NULL AND previous_deployment_id IS NULL
        FROM eregion_platform_deployments WHERE id=$1`, deploymentID).Scan(&previousNull); err != nil {
		t.Fatal(err)
	}
	if !previousNull {
		t.Fatal("fixture deployment unexpectedly has a predecessor")
	}
	return operationID, deploymentID
}

func seedRestoreOwnerAndCleanup(
	t *testing.T,
	ctx context.Context,
	service *Service,
	f *platformFixture,
	operationID contract.OperationID,
	deploymentID platform.DeploymentID,
) {
	t.Helper()
	inserted, err := f.store.Platform().Owners().Insert(ctx, entities.OwnerInsert{
		EnvironmentID: f.env.ID, OperationID: operationID, DeploymentID: deploymentID, Attempt: 1,
		Phase: string(deploymentOwnerRestored),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("deployment owner was not inserted")
	}
	if err := service.ensureContainerCleanup(
		ctx, "first-restore-candidate", f.env.ID, f.env.Node, deploymentID, "candidate", nil,
	); err != nil {
		t.Fatal(err)
	}
}

func assertRestoredEnvironmentNulls(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	service *Service,
	f *platformFixture,
) platform.Environment {
	t.Helper()
	env, err := service.environment(ctx, f.app.Name, f.env.Name)
	if err != nil {
		t.Fatalf("environment read after restore: %v", err)
	}
	if env.AppliedRelease != "" || env.ServingRelease != "" || env.ServingDeploymentID != "" {
		t.Fatalf("restored serving pointers = %q/%q/%q, want empty", env.AppliedRelease, env.ServingRelease, env.ServingDeploymentID)
	}
	if env.HealthState != platform.HealthUnknown {
		t.Fatalf("restored health state = %q, want unknown", env.HealthState)
	}
	appsInventory, err := service.Inventory(ctx, testActor, platform.InventoryQuery{
		Collection: "apps", App: f.app.Name, Limit: 1,
	})
	if err != nil {
		t.Fatalf("app inventory after restore: %v", err)
	}
	apps, ok := appsInventory.Items.([]platform.App)
	if !ok || len(apps) != 1 || apps[0].ID != f.app.ID {
		t.Fatalf("app inventory items = %#v", appsInventory.Items)
	}
	environmentsInventory, err := service.Inventory(ctx, testActor, platform.InventoryQuery{
		Collection: "environments", App: f.app.Name, Environment: f.env.Name, Limit: 1,
	})
	if err != nil {
		t.Fatalf("environment inventory after restore: %v", err)
	}
	environments, ok := environmentsInventory.Items.([]platform.Environment)
	if !ok || len(environments) != 1 || environments[0].ID != f.env.ID {
		t.Fatalf("environment inventory items = %#v", environmentsInventory.Items)
	}
	if environments[0].AppliedRelease != "" || environments[0].ServingRelease != "" || environments[0].ServingDeploymentID != "" {
		t.Fatalf("inventory serving pointers = %q/%q/%q, want empty", environments[0].AppliedRelease, environments[0].ServingRelease, environments[0].ServingDeploymentID)
	}
	var appliedNull, servingNull, deploymentNull bool
	if err := pool.QueryRow(ctx, `SELECT applied_release_id IS NULL,serving_release_id IS NULL,serving_deployment_id IS NULL
        FROM eregion_platform_environments WHERE id=$1`, f.env.ID).Scan(&appliedNull, &servingNull, &deploymentNull); err != nil {
		t.Fatal(err)
	}
	if !appliedNull || !servingNull || !deploymentNull {
		t.Fatalf("stored serving pointers null = %t/%t/%t, want true/true/true", appliedNull, servingNull, deploymentNull)
	}
	return env
}

func assertRestoredCleanupOwner(
	t *testing.T,
	ctx context.Context,
	service *Service,
	f *platformFixture,
	env platform.Environment,
	deploymentID platform.DeploymentID,
) {
	t.Helper()
	owner, hasOwner, err := f.store.Platform().Owners().Get(ctx, f.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasOwner || owner.Phase != string(deploymentOwnerRestored) {
		t.Fatalf("owner after restore = %+v, has=%t", owner, hasOwner)
	}
	protected, err := service.deploymentCleanupProtected(ctx, env, deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if !protected {
		t.Fatal("candidate cleanup was not protected while the owner was retained")
	}
	if err := service.reconcileDeploymentOwners(ctx); err != nil {
		t.Fatal(err)
	}
	if _, hasOwner, err := f.store.Platform().Owners().Get(ctx, f.env.ID); err != nil || hasOwner {
		t.Fatalf("owner after release: has=%t err=%v", hasOwner, err)
	}
	protected, err = service.deploymentCleanupProtected(ctx, env, deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if protected {
		t.Fatal("candidate cleanup remained protected after owner release")
	}
	cleanups, err := f.store.Platform().Cleanup().ForDeployment(ctx, f.env.ID, deploymentID, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanups) != 1 {
		t.Fatalf("candidate cleanup rows = %d, want 1", len(cleanups))
	}
}
