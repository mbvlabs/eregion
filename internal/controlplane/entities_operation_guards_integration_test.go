package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationEntitiesOperationGuardsAndReuse covers the remaining
// operation/domain count and list guards plus the reused platform primitives
// that close the old uncovered call sites.
func TestIntegrationEntitiesOperationGuardsAndReuse(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fixture := newPlatformFixture(t, pool, "operation-guards")
	appTarget := "app/" + fixture.app.Name
	envTarget := "env/" + fixture.app.Name + "/" + fixture.env.Name
	domainAdd := insertDefinitionOperation(t, store, platform.KindDomainAdd, envTarget, "guard-domain-add", json.RawMessage(`{}`))
	domainRemove := insertDefinitionOperation(t, store, platform.KindDomainRemove, envTarget, "guard-domain-remove", json.RawMessage(`{}`))
	parentDelete := insertDefinitionOperation(t, store, platform.KindEnvironmentDelete, envTarget, "guard-env-delete", json.RawMessage(`{}`))
	resourceTarget := "resource/" + fixture.app.Name + "/" + fixture.env.Name + "/web"
	resourceOp := insertDefinitionOperation(t, store, platform.KindResourceApply, resourceTarget, "guard-resource", json.RawMessage(`{}`))

	if count, err := store.Operations().CountActiveDomainConflicts(ctx, entities.DomainConflictGuard{
		ExcludeID: "op_none", EnvironmentTarget: envTarget, AppTarget: appTarget,
		DomainAddKind: platform.KindDomainAdd, DomainRemoveKind: platform.KindDomainRemove,
		EnvironmentDeleteKind: platform.KindEnvironmentDelete, AppDeleteKind: platform.KindAppDelete,
	}); err != nil || count != 3 {
		t.Fatalf("domain conflict count=%d err=%v", count, err)
	}
	if latest, found, err := store.Operations().LatestDomainOperation(ctx, envTarget, platform.KindDomainAdd, platform.KindDomainRemove); err != nil || !found || (latest != domainAdd && latest != domainRemove) {
		t.Fatalf("latest domain operation=%s found=%v err=%v", latest, found, err)
	}
	if count, err := store.Operations().CountActiveDeleteTargetsForApp(ctx, entities.DeleteTargetGuard{
		ExcludeID: parentDelete, AppTarget: appTarget, EnvironmentPattern: envTarget[:len(envTarget)] + "%",
	}); err != nil || count != 2 {
		t.Fatalf("app delete guard count=%d err=%v", count, err)
	}
	if count, err := store.Operations().CountActiveDeleteTargetsForEnvironment(ctx, entities.EnvironmentDeleteTargetGuard{
		ExcludeID: parentDelete, AppTarget: appTarget, EnvironmentTarget: envTarget,
		EnvironmentDeleteKind: platform.KindEnvironmentDelete, EnvironmentPattern: envTarget[:len(envTarget)] + "%",
	}); err != nil || count != 2 {
		t.Fatalf("environment delete guard count=%d err=%v", count, err)
	}
	if count, err := store.Operations().CountActiveParentDeletes(ctx, entities.ParentDeleteGuard{
		ExcludeID: resourceOp, AppTarget: appTarget, EnvironmentTarget: envTarget,
		AppDeleteKind: platform.KindAppDelete, EnvironmentDeleteKind: platform.KindEnvironmentDelete,
	}); err != nil || count != 1 {
		t.Fatalf("parent delete guard count=%d err=%v", count, err)
	}
	if count, err := store.Operations().CountActiveResourceOperations(ctx, resourceOp, resourceTarget); err != nil || count != 0 {
		t.Fatalf("resource guard self count=%d err=%v", count, err)
	}
	if count, err := store.Operations().CountActiveResourceOperations(ctx, "op_none", resourceTarget); err != nil || count != 1 {
		t.Fatalf("resource guard count=%d err=%v", count, err)
	}
	active, err := store.Operations().ListActiveAppEnvironmentOperations(ctx, "op_none", envTarget, appTarget)
	if err != nil || len(active) != 3 {
		t.Fatalf("active app/environment operations=%+v err=%v", active, err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if count, err := tx.Operations().CountActiveDomainConflicts(ctx, entities.DomainConflictGuard{
			ExcludeID: "op_none", EnvironmentTarget: envTarget, AppTarget: appTarget,
			DomainAddKind: platform.KindDomainAdd, DomainRemoveKind: platform.KindDomainRemove,
			EnvironmentDeleteKind: platform.KindEnvironmentDelete, AppDeleteKind: platform.KindAppDelete,
		}); err != nil || count != 3 {
			t.Fatalf("tx domain conflict count=%d err=%v", count, err)
		}
		if latest, found, err := tx.Operations().LatestDomainOperation(ctx, envTarget, platform.KindDomainAdd, platform.KindDomainRemove); err != nil || !found || latest == "" {
			t.Fatalf("tx latest domain=%s found=%v err=%v", latest, found, err)
		}
		if count, err := tx.Operations().CountActiveDeleteTargetsForApp(ctx, entities.DeleteTargetGuard{ExcludeID: "op_none", AppTarget: appTarget, EnvironmentPattern: envTarget + "%"}); err != nil || count != 3 {
			t.Fatalf("tx app delete guard=%d err=%v", count, err)
		}
		if count, err := tx.Operations().CountActiveDeleteTargetsForEnvironment(ctx, entities.EnvironmentDeleteTargetGuard{ExcludeID: "op_none", AppTarget: appTarget, EnvironmentTarget: envTarget, EnvironmentDeleteKind: platform.KindEnvironmentDelete, EnvironmentPattern: envTarget + "%"}); err != nil || count != 3 {
			t.Fatalf("tx environment delete guard=%d err=%v", count, err)
		}
		if count, err := tx.Operations().CountActiveParentDeletes(ctx, entities.ParentDeleteGuard{ExcludeID: "op_none", AppTarget: appTarget, EnvironmentTarget: envTarget, AppDeleteKind: platform.KindAppDelete, EnvironmentDeleteKind: platform.KindEnvironmentDelete}); err != nil || count != 1 {
			t.Fatalf("tx parent delete guard=%d err=%v", count, err)
		}
		if count, err := tx.Operations().CountActiveResourceOperations(ctx, "op_none", resourceTarget); err != nil || count != 1 {
			t.Fatalf("tx resource guard=%d err=%v", count, err)
		}
		if rows, err := tx.Operations().ListActiveAppEnvironmentOperations(ctx, "op_none", envTarget, appTarget); err != nil || len(rows) != 3 {
			t.Fatalf("tx active app/environment=%+v err=%v", rows, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Exact build/release pointer facts and environment observations are the
	// approved platform API that replaces broad rows.
	buildID, err := platform.NewBuildID()
	if err != nil {
		t.Fatal(err)
	}
	buildOp := insertDefinitionOperation(t, store, "platform.build", "build/"+string(buildID), "guard-build", json.RawMessage(`{}`))
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Platform().Builds().Insert(ctx, entities.BuildInsert{
			ID: buildID, OperationID: buildOp, AppID: fixture.app.ID,
			EnvironmentID: fixture.env.ID, RevisionID: fixture.revision.ID,
			Source: json.RawMessage(`{}`), CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		})
		if err != nil || !inserted {
			t.Fatalf("build insert=%v err=%v", inserted, err)
		}
		inserted, err = tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
			ID: releaseID, OperationID: buildOp, AppID: fixture.app.ID, EnvironmentID: fixture.env.ID,
			Image: "example/image", ImageID: "sha256:foundation", Source: json.RawMessage(`{}`),
			Build: json.RawMessage(`{}`), Command: json.RawMessage(`{}`),
			Status: platform.ReleaseReady, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		})
		if err != nil || !inserted {
			t.Fatalf("release insert=%v err=%v", inserted, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		pointer, found, err := tx.Platform().Pointers().LockOwnedBuildForRelease(ctx, buildID, fixture.app.ID, fixture.env.ID)
		if err != nil || !found || pointer.AppID != fixture.app.ID || pointer.EnvironmentID != fixture.env.ID {
			t.Fatalf("build-release pointer=%+v found=%v err=%v", pointer, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Active deployment operations share the exact deployment/operation join.
	deploymentID, err := platform.NewDeploymentID()
	if err != nil {
		t.Fatal(err)
	}
	deploymentOp := insertDefinitionOperation(t, store, platform.KindDeploy, envTarget, "guard-deployment", json.RawMessage(`{}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Platform().Deployments().Insert(ctx, entities.DeploymentInsert{
			ID: deploymentID, OperationID: deploymentOp, AppID: fixture.app.ID,
			EnvironmentID: fixture.env.ID, Action: platform.ActionDeploy, ReleaseID: releaseID,
			RevisionID: fixture.revision.ID, SecretRevisions: json.RawMessage(`{}`),
		})
		if err != nil || !inserted {
			t.Fatalf("deployment insert=%v err=%v", inserted, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	activeDeployments, err := store.Operations().ListActiveDeploymentsForEnvironment(ctx, fixture.env.ID)
	if err != nil || len(activeDeployments) != 1 || activeDeployments[0].ID != deploymentOp || activeDeployments[0].DeploymentID != deploymentID {
		t.Fatalf("active deployments=%+v err=%v", activeDeployments, err)
	}
	if count, err := store.Operations().CountActiveDeploymentsForEnvironment(ctx, fixture.env.ID); err != nil || count != 1 {
		t.Fatalf("active deployment count=%d err=%v", count, err)
	}
	if activeRelease, found, err := store.Operations().ActiveDeploymentRelease(ctx, fixture.env.ID); err != nil || !found || activeRelease != releaseID {
		t.Fatalf("active deployment release=%s found=%v err=%v", activeRelease, found, err)
	}

	// Release-command live composite.
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if started, err := tx.Platform().ReleaseCommands().Start(ctx, releaseID, "digest-foundation", deploymentOp); err != nil || started != 1 {
			t.Fatalf("release command start=%d err=%v", started, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	live, found, err := store.Platform().ReleaseCommands().GetWithLive(ctx, releaseID, "digest-foundation")
	if err != nil || !found || !live.Live || live.OperationID != deploymentOp || live.State != "running" {
		t.Fatalf("release command live=%+v found=%v err=%v", live, found, err)
	}
	if missing, found, err := store.Platform().ReleaseCommands().GetWithLive(ctx, releaseID, "absent-digest"); err != nil || found || missing != (entities.ReleaseCommandLive{}) {
		t.Fatalf("absent release command=%+v found=%v err=%v", missing, found, err)
	}

	// Existing composites that close the former uncovered call sites.
	if owners, err := store.Platform().Owners().ListSettleable(ctx); err != nil || len(owners) != 0 {
		t.Fatalf("settleable owners=%+v err=%v", owners, err)
	}
	if _, _, err := store.Platform().Revisions().FirstDeployOperation(ctx, fixture.revision.OperationID, platform.KindDeploy, testActor); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Platform().Revisions().FirstDeployActionRows(ctx, fixture.revision.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Platform().Deployments().ExecutionFacts(ctx, deploymentID, deploymentOp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resources().ReconcileTerminal(ctx); err != nil {
		t.Fatal(err)
	}
}
