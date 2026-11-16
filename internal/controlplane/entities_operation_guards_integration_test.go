package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationEntitiesOperationGuardsAndReuse covers the remaining
// operation/domain count and list guards plus the reused platform primitives
// that close the old uncovered call sites.
type operationGuardFixture struct {
	ctx            context.Context
	store          *entities.Store
	platform       *platformFixture
	appTarget      string
	envTarget      string
	resourceTarget string
	domainAdd      contract.OperationID
	domainRemove   contract.OperationID
	parentDelete   contract.OperationID
	resourceOp     contract.OperationID
}

func TestIntegrationEntitiesOperationGuardsAndReuse(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	platformFixture := newPlatformFixture(t, pool, "operation-guards")
	appTarget := "app/" + platformFixture.app.Name
	envTarget := "env/" + platformFixture.app.Name + "/" + platformFixture.env.Name
	resourceTarget := "resource/" + platformFixture.app.Name + "/" + platformFixture.env.Name + "/web"
	g := operationGuardFixture{
		ctx: ctx, store: store, platform: platformFixture,
		appTarget: appTarget, envTarget: envTarget, resourceTarget: resourceTarget,
		domainAdd:    insertDefinitionOperation(t, store, platform.KindDomainAdd, envTarget, "guard-domain-add", json.RawMessage(`{}`)),
		domainRemove: insertDefinitionOperation(t, store, platform.KindDomainRemove, envTarget, "guard-domain-remove", json.RawMessage(`{}`)),
		parentDelete: insertDefinitionOperation(t, store, platform.KindEnvironmentDelete, envTarget, "guard-env-delete", json.RawMessage(`{}`)),
		resourceOp:   insertDefinitionOperation(t, store, platform.KindResourceApply, resourceTarget, "guard-resource", json.RawMessage(`{}`)),
	}

	checkOperationGuardReads(t, g)
	checkOperationGuardTxReads(t, g)
	releaseID := checkBuildReleasePointer(t, g)
	deploymentID, deploymentOp := checkActiveDeployment(t, g, releaseID)
	checkReleaseCommandLive(t, g, releaseID, deploymentOp)
	checkExistingComposites(t, g, deploymentID, deploymentOp)
}

func checkOperationGuardReads(t *testing.T, g operationGuardFixture) {
	t.Helper()
	if count, err := g.store.Operations().CountActiveDomainConflicts(g.ctx, entities.DomainConflictGuard{
		ExcludeID: "op_none", EnvironmentTarget: g.envTarget, AppTarget: g.appTarget,
		DomainAddKind: platform.KindDomainAdd, DomainRemoveKind: platform.KindDomainRemove,
		EnvironmentDeleteKind: platform.KindEnvironmentDelete, AppDeleteKind: platform.KindAppDelete,
	}); err != nil || count != 3 {
		t.Fatalf("domain conflict count=%d err=%v", count, err)
	}
	if latest, found, err := g.store.Operations().LatestDomainOperation(g.ctx, g.envTarget, platform.KindDomainAdd, platform.KindDomainRemove); err != nil || !found || (latest != g.domainAdd && latest != g.domainRemove) {
		t.Fatalf("latest domain operation=%s found=%v err=%v", latest, found, err)
	}
	if count, err := g.store.Operations().CountActiveDeleteTargetsForApp(g.ctx, entities.DeleteTargetGuard{
		ExcludeID: g.parentDelete, AppTarget: g.appTarget, EnvironmentPattern: g.envTarget + "%",
	}); err != nil || count != 2 {
		t.Fatalf("app delete guard count=%d err=%v", count, err)
	}
	if count, err := g.store.Operations().CountActiveDeleteTargetsForEnvironment(g.ctx, entities.EnvironmentDeleteTargetGuard{
		ExcludeID: g.parentDelete, AppTarget: g.appTarget, EnvironmentTarget: g.envTarget,
		EnvironmentDeleteKind: platform.KindEnvironmentDelete, EnvironmentPattern: g.envTarget + "%",
	}); err != nil || count != 2 {
		t.Fatalf("environment delete guard count=%d err=%v", count, err)
	}
	if count, err := g.store.Operations().CountActiveParentDeletes(g.ctx, entities.ParentDeleteGuard{
		ExcludeID: g.resourceOp, AppTarget: g.appTarget, EnvironmentTarget: g.envTarget,
		AppDeleteKind: platform.KindAppDelete, EnvironmentDeleteKind: platform.KindEnvironmentDelete,
	}); err != nil || count != 1 {
		t.Fatalf("parent delete guard count=%d err=%v", count, err)
	}
	if count, err := g.store.Operations().CountActiveResourceOperations(g.ctx, g.resourceOp, g.resourceTarget); err != nil || count != 0 {
		t.Fatalf("resource guard self count=%d err=%v", count, err)
	}
	if count, err := g.store.Operations().CountActiveResourceOperations(g.ctx, "op_none", g.resourceTarget); err != nil || count != 1 {
		t.Fatalf("resource guard count=%d err=%v", count, err)
	}
	active, err := g.store.Operations().ListActiveAppEnvironmentOperations(g.ctx, "op_none", g.envTarget, g.appTarget)
	if err != nil || len(active) != 3 {
		t.Fatalf("active app/environment operations=%+v err=%v", active, err)
	}
}

func checkOperationGuardTxReads(t *testing.T, g operationGuardFixture) {
	t.Helper()
	if err := g.store.WithinTx(g.ctx, func(tx *entities.StoreTx) error {
		checkOperationGuardTxCounts(t, tx, g)
		checkOperationGuardTxLists(t, tx, g)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func checkOperationGuardTxCounts(t *testing.T, tx *entities.StoreTx, g operationGuardFixture) {
	t.Helper()
	if count, err := tx.Operations().CountActiveDomainConflicts(g.ctx, entities.DomainConflictGuard{
		ExcludeID: "op_none", EnvironmentTarget: g.envTarget, AppTarget: g.appTarget,
		DomainAddKind: platform.KindDomainAdd, DomainRemoveKind: platform.KindDomainRemove,
		EnvironmentDeleteKind: platform.KindEnvironmentDelete, AppDeleteKind: platform.KindAppDelete,
	}); err != nil || count != 3 {
		t.Fatalf("tx domain conflict count=%d err=%v", count, err)
	}
	if latest, found, err := tx.Operations().LatestDomainOperation(g.ctx, g.envTarget, platform.KindDomainAdd, platform.KindDomainRemove); err != nil || !found || latest == "" {
		t.Fatalf("tx latest domain=%s found=%v err=%v", latest, found, err)
	}
	if count, err := tx.Operations().CountActiveDeleteTargetsForApp(g.ctx, entities.DeleteTargetGuard{ExcludeID: "op_none", AppTarget: g.appTarget, EnvironmentPattern: g.envTarget + "%"}); err != nil || count != 3 {
		t.Fatalf("tx app delete guard=%d err=%v", count, err)
	}
	if count, err := tx.Operations().CountActiveDeleteTargetsForEnvironment(g.ctx, entities.EnvironmentDeleteTargetGuard{ExcludeID: "op_none", AppTarget: g.appTarget, EnvironmentTarget: g.envTarget, EnvironmentDeleteKind: platform.KindEnvironmentDelete, EnvironmentPattern: g.envTarget + "%"}); err != nil || count != 3 {
		t.Fatalf("tx environment delete guard=%d err=%v", count, err)
	}
	if count, err := tx.Operations().CountActiveParentDeletes(g.ctx, entities.ParentDeleteGuard{ExcludeID: "op_none", AppTarget: g.appTarget, EnvironmentTarget: g.envTarget, AppDeleteKind: platform.KindAppDelete, EnvironmentDeleteKind: platform.KindEnvironmentDelete}); err != nil || count != 1 {
		t.Fatalf("tx parent delete guard=%d err=%v", count, err)
	}
}

func checkOperationGuardTxLists(t *testing.T, tx *entities.StoreTx, g operationGuardFixture) {
	t.Helper()
	if count, err := tx.Operations().CountActiveResourceOperations(g.ctx, "op_none", g.resourceTarget); err != nil || count != 1 {
		t.Fatalf("tx resource guard=%d err=%v", count, err)
	}
	if rows, err := tx.Operations().ListActiveAppEnvironmentOperations(g.ctx, "op_none", g.envTarget, g.appTarget); err != nil || len(rows) != 3 {
		t.Fatalf("tx active app/environment=%+v err=%v", rows, err)
	}
}

func checkBuildReleasePointer(t *testing.T, g operationGuardFixture) platform.ReleaseID {
	t.Helper()
	buildID, err := platform.NewBuildID()
	if err != nil {
		t.Fatal(err)
	}
	buildOp := insertDefinitionOperation(t, g.store, "platform.build", "build/"+string(buildID), "guard-build", json.RawMessage(`{}`))
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	if err := g.store.WithinTx(g.ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Platform().Builds().Insert(g.ctx, entities.BuildInsert{
			ID: buildID, OperationID: buildOp, AppID: g.platform.app.ID,
			EnvironmentID: g.platform.env.ID, RevisionID: g.platform.revision.ID,
			Source: json.RawMessage(`{}`), CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		})
		if err != nil || !inserted {
			t.Fatalf("build insert=%v err=%v", inserted, err)
		}
		inserted, err = tx.Platform().Releases().Insert(g.ctx, entities.ReleaseInsert{
			ID: releaseID, OperationID: buildOp, AppID: g.platform.app.ID, EnvironmentID: g.platform.env.ID,
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
	if err := g.store.WithinTx(g.ctx, func(tx *entities.StoreTx) error {
		pointer, found, err := tx.Platform().Pointers().LockOwnedBuildForRelease(g.ctx, buildID, g.platform.app.ID, g.platform.env.ID)
		if err != nil || !found || pointer.AppID != g.platform.app.ID || pointer.EnvironmentID != g.platform.env.ID {
			t.Fatalf("build-release pointer=%+v found=%v err=%v", pointer, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return releaseID
}

func checkActiveDeployment(t *testing.T, g operationGuardFixture, releaseID platform.ReleaseID) (platform.DeploymentID, contract.OperationID) {
	t.Helper()
	deploymentID, err := platform.NewDeploymentID()
	if err != nil {
		t.Fatal(err)
	}
	deploymentOp := insertDefinitionOperation(t, g.store, platform.KindDeploy, g.envTarget, "guard-deployment", json.RawMessage(`{}`))
	if err := g.store.WithinTx(g.ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Platform().Deployments().Insert(g.ctx, entities.DeploymentInsert{
			ID: deploymentID, OperationID: deploymentOp, AppID: g.platform.app.ID,
			EnvironmentID: g.platform.env.ID, Action: platform.ActionDeploy, ReleaseID: releaseID,
			RevisionID: g.platform.revision.ID, SecretRevisions: json.RawMessage(`{}`),
		})
		if err != nil || !inserted {
			t.Fatalf("deployment insert=%v err=%v", inserted, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	activeDeployments, err := g.store.Operations().ListActiveDeploymentsForEnvironment(g.ctx, g.platform.env.ID)
	if err != nil || len(activeDeployments) != 1 || activeDeployments[0].ID != deploymentOp || activeDeployments[0].DeploymentID != deploymentID {
		t.Fatalf("active deployments=%+v err=%v", activeDeployments, err)
	}
	if count, err := g.store.Operations().CountActiveDeploymentsForEnvironment(g.ctx, g.platform.env.ID); err != nil || count != 1 {
		t.Fatalf("active deployment count=%d err=%v", count, err)
	}
	if activeRelease, found, err := g.store.Operations().ActiveDeploymentRelease(g.ctx, g.platform.env.ID); err != nil || !found || activeRelease != releaseID {
		t.Fatalf("active deployment release=%s found=%v err=%v", activeRelease, found, err)
	}
	return deploymentID, deploymentOp
}

func checkReleaseCommandLive(t *testing.T, g operationGuardFixture, releaseID platform.ReleaseID, deploymentOp contract.OperationID) {
	t.Helper()
	if err := g.store.WithinTx(g.ctx, func(tx *entities.StoreTx) error {
		if started, err := tx.Platform().ReleaseCommands().Start(g.ctx, releaseID, "digest-foundation", deploymentOp); err != nil || started != 1 {
			t.Fatalf("release command start=%d err=%v", started, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	live, found, err := g.store.Platform().ReleaseCommands().GetWithLive(g.ctx, releaseID, "digest-foundation")
	if err != nil || !found || !live.Live || live.OperationID != deploymentOp || live.State != "running" {
		t.Fatalf("release command live=%+v found=%v err=%v", live, found, err)
	}
	if missing, found, err := g.store.Platform().ReleaseCommands().GetWithLive(g.ctx, releaseID, "absent-digest"); err != nil || found || missing != (entities.ReleaseCommandLive{}) {
		t.Fatalf("absent release command=%+v found=%v err=%v", missing, found, err)
	}
}

func checkExistingComposites(t *testing.T, g operationGuardFixture, deploymentID platform.DeploymentID, deploymentOp contract.OperationID) {
	t.Helper()
	if owners, err := g.store.Platform().Owners().ListSettleable(g.ctx); err != nil || len(owners) != 0 {
		t.Fatalf("settleable owners=%+v err=%v", owners, err)
	}
	if _, _, err := g.store.Platform().Revisions().FirstDeployOperation(g.ctx, g.platform.revision.OperationID, platform.KindDeploy, testActor); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.Platform().Revisions().FirstDeployActionRows(g.ctx, g.platform.revision.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.store.Platform().Deployments().ExecutionFacts(g.ctx, deploymentID, deploymentOp); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.Resources().ReconcileTerminal(g.ctx); err != nil {
		t.Fatal(err)
	}
}
