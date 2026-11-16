package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

type platformFixture struct {
	store    *entities.Store
	app      platform.App
	env      platform.Environment
	revision platform.EnvironmentRevision
	node     string
}

func newPlatformFixture(t *testing.T, pool *pgxpool.Pool, suffix string) *platformFixture {
	t.Helper()
	store := newTestEntities(t, pool)
	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	revisionID, err := platform.NewEnvironmentRevisionID()
	if err != nil {
		t.Fatal(err)
	}
	node := "platform-node-" + suffix
	appName := "platform-app-" + suffix
	envName := "platform-env-" + suffix
	ctx := context.Background()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Nodes().InsertRegistration(ctx, node, []string{platform.RoleApplication, platform.RoleControlPlane}, []string{platform.CapabilityDocker}, ""); err != nil {
			return err
		}
		if _, err := tx.Platform().Apps().Insert(ctx, entities.AppInsert{ID: appID, Name: appName, ImageRepository: "example/" + suffix}); err != nil {
			return err
		}
		if _, err := tx.Platform().Environments().Insert(ctx, entities.EnvironmentInsert{
			ID: envID, AppID: appID, Name: envName, Node: node,
			HostPort: 25000, CandidatePort: 35000, ContainerPort: 8080, Bind: "127.0.0.1",
			HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30,
			Env: map[string]string{}, SecretRefs: map[string]string{}, Protected: true,
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Revisions().Insert(ctx, entities.RevisionInsert{
			ID: revisionID, EnvironmentID: envID, Number: 1,
			Actor: contract.Actor{Kind: "system", Subject: "test"}, Operation: "environment.created", Summary: "first",
			State: platform.RevisionState{Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}}, Config: map[string]string{}, SecretNames: []string{}, Resources: []string{}},
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Environments().SetCurrentRevision(ctx, envID, revisionID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app, found, err := store.Platform().Apps().GetByName(ctx, appName)
	if err != nil || !found {
		t.Fatalf("app = %v, %v", found, err)
	}
	envObservation, found, err := store.Platform().Environments().GetByName(ctx, appName, envName)
	if err != nil || !found {
		t.Fatalf("environment = %v, %v", found, err)
	}
	env := envObservation.Environment
	revision, found, err := store.Platform().Revisions().GetByID(ctx, revisionID)
	if err != nil || !found {
		t.Fatalf("revision = %v, %v", found, err)
	}
	return &platformFixture{store: store, app: app, env: env, revision: revision, node: node}
}

func TestIntegrationEntitiesPlatformStateParity(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "state")
	ctx := context.Background()

	checkPlatformStateMapping(t, ctx, f)
	domainID := seedStateParityDomainChange(t, ctx, f)
	checkStateParityReads(t, ctx, f, domainID)
}

func checkPlatformStateMapping(t *testing.T, ctx context.Context, f *platformFixture) {
	t.Helper()
	if f.env.HostPort != 25000 || f.env.CandidatePort != 35000 || f.env.RevisionID != f.revision.ID {
		t.Fatalf("unexpected environment mapping: %+v", f.env)
	}
	if f.revision.Number != 1 || f.revision.Operation != "environment.created" {
		t.Fatalf("unexpected revision mapping: %+v", f.revision)
	}
	store := f.store
	if resolved, found, err := store.Platform().Environments().ResolveID(ctx, f.app.Name, f.env.Name); err != nil || !found || resolved.ID != f.env.ID {
		t.Fatalf("resolve environment = %+v, %v, %v", resolved, found, err)
	}
	if current, found, err := store.Platform().Environments().CurrentRevisionID(ctx, f.app.Name, f.env.Name); err != nil || !found || current != f.revision.ID {
		t.Fatalf("current revision = %v, %v, %v", current, found, err)
	}
	if count, err := store.Platform().Environments().CountForApp(ctx, f.app.ID); err != nil || count != 1 {
		t.Fatalf("environment count = %d, %v", count, err)
	}
	if found, err := store.Platform().Revisions().ExistsForOperation(ctx, f.revision.OperationID); err != nil || found {
		t.Fatalf("revision operation existence = %v, %v", found, err)
	}
	ports := store.Platform().Environments()
	if err := ports.LockPortAllocation(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if used, err := ports.PortInUse(ctx, 25000); err != nil || !used {
		t.Fatalf("port in use = %v, %v", used, err)
	}
	if next, found, err := ports.NextHostPort(ctx); err != nil || !found || next < 20000 || next > 29999 {
		t.Fatalf("next host port = %d, %v, %v", next, found, err)
	}
	if next, found, err := ports.NextCandidatePort(ctx, 25000); err != nil || !found || next < 30000 || next > 39999 {
		t.Fatalf("next candidate port = %d, %v, %v", next, found, err)
	}
}

func seedStateParityDomainChange(t *testing.T, ctx context.Context, f *platformFixture) platform.DomainID {
	t.Helper()
	domainID, err := platform.NewDomainID()
	if err != nil {
		t.Fatal(err)
	}
	changeOperationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: changeOperationID, Key: entities.OperationKey{Kind: "platform.test", Target: "app/" + f.app.Name, Actor: contract.Actor{Kind: "system", Subject: "test"}, IdempotencyKey: "change-parity"},
			InputHash: "hash", Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Domains().Insert(ctx, entities.DomainInsert{
			ID: domainID, Domain: "state.example.test", EnvironmentID: f.env.ID, Primary: true,
			Addresses: []string{"127.0.0.1"}, CheckPath: "/health",
		}); err != nil {
			return err
		}
		sequence, err := tx.Platform().Changes().NextSequenceForEnvironment(ctx, f.env.ID)
		if err != nil {
			return err
		}
		id, err := platform.NewChangeID()
		if err != nil {
			return err
		}
		_, err = tx.Platform().Changes().Insert(ctx, entities.ChangeInsert{
			ID: id, AppID: f.app.ID, EnvironmentID: f.env.ID, Sequence: sequence, Kind: "test",
			OperationID: changeOperationID, Actor: contract.Actor{Kind: "system", Subject: "test"},
			Summary: "state parity",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return domainID
}

func checkStateParityReads(t *testing.T, ctx context.Context, f *platformFixture, wantDomain platform.DomainID) {
	t.Helper()
	domain, domainEnv, found, err := f.store.Platform().Domains().GetByName(ctx, "state.example.test")
	if err != nil || !found || domain != wantDomain || domainEnv != f.env.ID {
		t.Fatalf("domain = %v, %v, %v, %v", domain, domainEnv, found, err)
	}
	if summary, err := f.store.Platform().Inventory().Summary(ctx); err != nil || summary["apps"] != 1 || summary["environments"] != 1 {
		t.Fatalf("summary = %v, %v", summary, err)
	}
	if changes, err := f.store.Platform().Changes().List(ctx, entities.ChangeFilter{App: f.app.Name, Limit: 10}); err != nil || len(changes) != 1 {
		t.Fatalf("changes = %d, %v", len(changes), err)
	}
	if domains, err := f.store.Platform().Domains().List(ctx, entities.DomainFilter{App: f.app.Name, Limit: 10}); err != nil || len(domains) != 1 {
		t.Fatalf("domains = %d, %v", len(domains), err)
	}
}

func TestIntegrationEntitiesPlatformReleaseDeploymentParity(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "release")
	ctx := context.Background()
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	seedReleaseDeploymentParity(t, ctx, f, operationID)
	checkReleaseDeploymentParityReads(t, ctx, f, operationID)
}

func seedReleaseDeploymentParity(t *testing.T, ctx context.Context, f *platformFixture, operationID contract.OperationID) {
	t.Helper()
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: operationID, Key: entities.OperationKey{Kind: platform.KindDeploy, Target: "env/" + f.app.Name + "/" + f.env.Name, Actor: contract.Actor{Kind: "ci", Subject: "platform"}, IdempotencyKey: "release-parity"},
			InputHash: "hash", Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		releaseID, err := seedReleaseAndBuild(t, ctx, tx, f, operationID)
		if err != nil {
			return err
		}
		return seedDeploymentOwnerAndCommand(t, ctx, tx, f, operationID, releaseID)
	}); err != nil {
		t.Fatal(err)
	}
}

func seedReleaseAndBuild(t *testing.T, ctx context.Context, tx *entities.StoreTx, f *platformFixture, operationID contract.OperationID) (platform.ReleaseID, error) {
	t.Helper()
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		return "", err
	}
	if _, err := tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
		ID: releaseID, OperationID: operationID, AppID: f.app.ID, EnvironmentID: f.env.ID,
		Image: "example/app:tag", ImageID: "sha256:config", Source: json.RawMessage(`{"type":"image"}`),
		Status: platform.ReleaseReady, CompletedAt: timePtr(time.Now().UTC()),
	}); err != nil {
		return "", err
	}
	release, found, err := tx.Platform().Releases().GetByID(ctx, releaseID)
	if err != nil || !found || release.ID != releaseID {
		return "", errors.New("release round trip failed")
	}
	buildID, err := platform.NewBuildID()
	if err != nil {
		return "", err
	}
	if _, err := tx.Platform().Builds().Insert(ctx, entities.BuildInsert{
		ID: buildID, OperationID: operationID, AppID: f.app.ID, EnvironmentID: f.env.ID,
		RevisionID: f.revision.ID, ReleaseID: releaseID, Source: json.RawMessage(`{"type":"git"}`), CommitSHA: "0123456789012345678901234567890123456789",
	}); err != nil {
		return "", err
	}
	return releaseID, nil
}

func seedDeploymentOwnerAndCommand(t *testing.T, ctx context.Context, tx *entities.StoreTx, f *platformFixture, operationID contract.OperationID, releaseID platform.ReleaseID) error {
	t.Helper()
	deploymentID, err := platform.NewDeploymentID()
	if err != nil {
		return err
	}
	if _, err := tx.Platform().Deployments().Insert(ctx, entities.DeploymentInsert{
		ID: deploymentID, OperationID: operationID, AppID: f.app.ID, EnvironmentID: f.env.ID,
		Action: platform.ActionDeploy, ReleaseID: releaseID, RevisionID: f.revision.ID,
		SecretRevisions: json.RawMessage(`{}`),
	}); err != nil {
		return err
	}
	if _, err := tx.Platform().Owners().Insert(ctx, entities.OwnerInsert{
		EnvironmentID: f.env.ID, OperationID: operationID, DeploymentID: deploymentID,
		Attempt: 1, Phase: "prepared", RestoreRelease: "", RestoreDeployment: "",
	}); err != nil {
		return err
	}
	if _, err := tx.Platform().Cleanup().RecordFailure(ctx, entities.CleanupRecord{
		ContainerName: "candidate-" + string(deploymentID), EnvironmentID: f.env.ID, Node: f.node,
		DeploymentID: deploymentID, Reason: "previous", LastError: "test",
	}); err != nil {
		return err
	}
	if _, err := tx.Platform().ReleaseCommands().Start(ctx, releaseID, "digest", operationID); err != nil {
		return err
	}
	if run, found, err := tx.Platform().ReleaseCommands().Get(ctx, releaseID, "digest"); err != nil || !found || run.State != "running" {
		return errors.New("release command start failed")
	}
	_, err = tx.Platform().ReleaseCommands().Finish(ctx, releaseID, "digest", operationID, "succeeded", "", "")
	return err
}

func checkReleaseDeploymentParityReads(t *testing.T, ctx context.Context, f *platformFixture, operationID contract.OperationID) {
	t.Helper()
	store := f.store
	if release, found, err := store.Platform().Releases().GetByOperation(ctx, operationID); err != nil || !found || release.Status != platform.ReleaseReady {
		t.Fatalf("release by operation = %v, %v, %v", found, err, release.Status)
	}
	if build, found, err := store.Platform().Builds().GetByOperation(ctx, operationID); err != nil || !found || build.ID == "" {
		t.Fatalf("build by operation = %v, %v", found, err)
	}
	if deployment, found, err := store.Platform().Deployments().GetByOperation(ctx, operationID); err != nil || !found || deployment.ID == "" {
		t.Fatalf("deployment by operation = %v, %v", found, err)
	}
	if owner, found, err := store.Platform().Owners().Get(ctx, f.env.ID); err != nil || !found || owner.Attempt != 1 {
		t.Fatalf("owner = %+v, %v, %v", owner, found, err)
	}
	if due, err := store.Platform().Cleanup().Due(ctx, 10); err != nil || len(due) != 1 {
		t.Fatalf("cleanup due = %d, %v", len(due), err)
	}
	if runs, err := store.Platform().Releases().List(ctx, entities.ReleaseFilter{App: f.app.Name, Limit: 10}); err != nil || len(runs) != 1 {
		t.Fatalf("release list = %d, %v", len(runs), err)
	}
}

func TestIntegrationEntitiesPlatformResourceNodeFoundation(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "resource")
	ctx := context.Background()
	checkNodeFoundationReads(t, ctx, f)
	resourceID := seedResourceFoundation(t, ctx, f)
	checkResourceFoundationReads(t, ctx, f, resourceID)
}

func checkNodeFoundationReads(t *testing.T, ctx context.Context, f *platformFixture) {
	t.Helper()
	store := f.store
	if node, found, err := store.Nodes().Get(ctx, f.node); err != nil || !found || node.Name != f.node {
		t.Fatalf("node = %v, %v, %v", found, err, node.Name)
	}
	if connection, found, err := store.Nodes().Connection(ctx, f.node); err != nil || !found || connection.Name != f.node {
		t.Fatalf("node connection = %v, %v, %v", found, err, connection.Name)
	}
	if state, found, err := store.Nodes().LockBuildConcurrency(ctx, f.node); err != nil || !found || state.BuildConcurrency != 1 {
		t.Fatalf("build concurrency = %+v, %v, %v", state, found, err)
	}
	if nodes, err := store.Nodes().List(ctx, entities.NodeFilter{Limit: 10}); err != nil || len(nodes) != 1 {
		t.Fatalf("node list = %d, %v", len(nodes), err)
	}
	if peers, err := store.Nodes().WireGuardPeers(ctx, f.node); err != nil || len(peers) != 0 {
		t.Fatalf("wireguard peers = %d, %v", len(peers), err)
	}
	if addresses, err := store.Nodes().WireGuardAddresses(ctx); err != nil || len(addresses) != 0 {
		t.Fatalf("wireguard addresses = %d, %v", len(addresses), err)
	}
}

func seedResourceFoundation(t *testing.T, ctx context.Context, f *platformFixture) platform.ResourceID {
	t.Helper()
	resourceID, err := platform.NewResourceID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: operationID, Key: entities.OperationKey{Kind: platform.KindResourceApply, Target: "resource/" + f.app.Name + "/" + f.env.Name + "/cache", Actor: contract.Actor{Kind: "ci", Subject: "platform"}, IdempotencyKey: "resource-parity"},
			InputHash: "hash", Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		if _, err := tx.Resources().Insert(ctx, entities.ResourceInsert{
			ID: resourceID, AppID: f.app.ID, EnvironmentID: f.env.ID, Name: "cache", Node: f.node,
			Spec: platform.ResourceSpec{Image: "redis:7"}, Image: "redis:7", OperationID: operationID,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return resourceID
}

func checkResourceFoundationReads(t *testing.T, ctx context.Context, f *platformFixture, resourceID platform.ResourceID) {
	t.Helper()
	store := f.store
	resource, found, err := store.Resources().GetByName(ctx, f.app.Name, f.env.Name, "cache", false)
	if err != nil || !found || resource.ID != resourceID {
		t.Fatalf("resource = %v, %v, %v", found, err, resource.ID)
	}
	if state, found, err := store.Resources().LockState(ctx, f.env.ID, "cache"); err != nil || !found || state != platform.ResourcePending {
		t.Fatalf("resource state = %v, %v, %v", state, found, err)
	}
	if targets, err := store.Resources().Targets(ctx, f.app.Name, f.env.Name); err != nil || len(targets) != 1 {
		t.Fatalf("resource targets = %v, %v", targets, err)
	}
	if live, err := store.Resources().LiveForEnvironment(ctx, f.env.ID); err != nil || len(live) != 1 {
		t.Fatalf("live resources = %d, %v", len(live), err)
	}
	if deployability, found, err := store.Resources().State(ctx, f.env.ID, "cache"); err != nil || !found || deployability.State != platform.ResourcePending {
		t.Fatalf("resource deployability = %+v, %v, %v", deployability, found, err)
	}
}

func TestIntegrationEntitiesPlatformAtomicRollback(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx := context.Background()
	appID, _ := platform.NewAppID()
	envID, _ := platform.NewEnvironmentID()
	revisionID, _ := platform.NewEnvironmentRevisionID()
	node := "rollback-node"
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Nodes().InsertRegistration(ctx, node, []string{platform.RoleApplication}, []string{platform.CapabilityDocker}, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback platform foundation")
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Platform().Apps().Insert(ctx, entities.AppInsert{ID: appID, Name: "rollback-app", ImageRepository: "example/rollback"}); err != nil {
			return err
		}
		if _, err := tx.Platform().Environments().Insert(ctx, entities.EnvironmentInsert{
			ID: envID, AppID: appID, Name: "production", Node: node, HostPort: 26000, CandidatePort: 36000,
			ContainerPort: 8080, Bind: "127.0.0.1", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30,
			Env: map[string]string{}, SecretRefs: map[string]string{},
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Revisions().Insert(ctx, entities.RevisionInsert{
			ID: revisionID, EnvironmentID: envID, Number: 1, Actor: contract.Actor{Kind: "system", Subject: "test"},
			Operation: "environment.created", Summary: "rollback",
			State: platform.RevisionState{Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}}, Config: map[string]string{}, SecretNames: []string{}, Resources: []string{}},
		}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback error = %v", err)
	}
	if found, err := store.Platform().Apps().Exists(ctx, "rollback-app"); err != nil || found {
		t.Fatalf("app survived rollback = %v, %v", found, err)
	}
	if found, err := store.Platform().Environments().Exists(ctx, "rollback-app", "production"); err != nil || found {
		t.Fatalf("environment survived rollback = %v, %v", found, err)
	}
	if found, err := store.Platform().Revisions().ExistsForOperation(ctx, ""); err != nil || found {
		t.Fatalf("revision survived rollback = %v, %v", found, err)
	}
}

func timePtr(value time.Time) *time.Time { return &value }

var _ = pgxpool.Pool{}
