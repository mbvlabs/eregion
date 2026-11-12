package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func imageUploadFixture(t *testing.T) (*Service, *deploymentRetryFixture, RegistryHostIdentity) {
	t.Helper()
	fixture := newDeploymentRetryFixtureWithTransport(t, newScriptedDeployTransport())
	identity := testRegistryHostIdentity()
	identity.StateDir = t.TempDir()
	if _, err := AssignWorkspace(fixture.ctx, fixture.pool, "thelab", identity); err != nil {
		t.Fatal(err)
	}
	fixture.service.registryHostIdentity = &identity
	fixture.service.registryEndpoint = "127.0.0.1:5000"
	return fixture.service, fixture, identity
}

func hostLockContended(t *testing.T, ctx context.Context, lockPath string) bool {
	t.Helper()
	release, err := engine.Acquire(ctx, lockPath, engine.LockExclusive, 0)
	if err == nil {
		release()
		return false
	}
	return errors.Is(err, engine.ErrLockHeld)
}

func TestIntegrationImageUploadLeaseUsesTheSameAppAdvisoryKeyAndHostLock(t *testing.T) {
	service, fixture, identity := imageUploadFixture(t)
	lockPath := node.HostLockPath(identity.StateDir)
	config, err := ParseDatabaseConfig(fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireImageUploadLease(fixture.ctx, config.ConnConfig, identity, "live")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.App != "live" || lease.ImageRepository != "live" {
		t.Fatalf("lease identity = %+v", lease)
	}
	release, acquired, err := service.tryLockTargets(fixture.ctx, []string{"app/live"})
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		release()
		t.Fatal("the upload lease did not hold the app session advisory lock")
	}
	if !hostLockContended(t, fixture.ctx, lockPath) {
		t.Fatal("the upload lease did not hold the shared host lock")
	}
	lease.Release()
	release, acquired, err = service.tryLockTargets(fixture.ctx, []string{"app/live"})
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("releasing the upload lease did not release the app advisory lock")
	}
	release()
	ctx, cancel := context.WithTimeout(fixture.ctx, time.Second)
	defer cancel()
	hostRelease, err := service.acquireRegistryHostLock(ctx)
	if err != nil {
		t.Fatalf("releasing the upload lease did not release the host lock: %v", err)
	}
	hostRelease()
}

func TestIntegrationImageUploadLeaseKeepsHostFenceAfterAdvisorySessionLoss(t *testing.T) {
	service, fixture, identity := imageUploadFixture(t)
	lockPath := node.HostLockPath(identity.StateDir)
	config, err := ParseDatabaseConfig(fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireImageUploadLease(fixture.ctx, config.ConnConfig, identity, "live")
	if err != nil {
		t.Fatal(err)
	}
	if lease.conn == nil {
		t.Fatal("lease has no dedicated connection")
	}
	// Simulate the PostgreSQL session dying while the upload child is still
	// running. The host lock must still block final metadata deletion.
	if err := lease.conn.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !hostLockContended(t, fixture.ctx, lockPath) {
		t.Fatal("final metadata deletion ignored the held host lock after advisory session loss")
	}
	lease.Release()
	ctx, cancel := context.WithTimeout(fixture.ctx, time.Second)
	defer cancel()
	hostRelease, err := service.acquireRegistryHostLock(ctx)
	if err != nil {
		t.Fatalf("host lock was not released after session loss: %v", err)
	}
	hostRelease()
}

func TestIntegrationImageUploadLeaseRefusesStaleDeleteRecreateBeforePayload(t *testing.T) {
	_, fixture, identity := imageUploadFixture(t)
	config, err := ParseDatabaseConfig(fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	recreated := "app_" + strings.Repeat("b", 32)
	lease, err := acquireImageUploadLease(fixture.ctx, config.ConnConfig, identity, "live", func(ctx context.Context) error {
		if _, err := fixture.pool.Exec(ctx, `DELETE FROM eregion_platform_apps WHERE name='live'`); err != nil {
			return err
		}
		_, err := fixture.pool.Exec(ctx, `INSERT INTO eregion_platform_apps(id,name,image_repository) VALUES($1,'live','live')`, recreated)
		return err
	})
	if lease != nil {
		lease.Release()
	}
	if err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("stale delete/recreate = %v, want conflict before payload", err)
	}
}

func TestIntegrationFinalMetadataHostLockWaitsForUploadLease(t *testing.T) {
	service, fixture, identity := imageUploadFixture(t)
	config, err := ParseDatabaseConfig(fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireImageUploadLease(fixture.ctx, config.ConnConfig, identity, "live")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
		defer cancel()
		release, err := service.acquireRegistryHostLock(ctx)
		if err == nil {
			release()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		lease.Release()
		t.Fatalf("final metadata host lock did not wait for the upload: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("final metadata host lock did not acquire after upload release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("final metadata host lock stayed blocked after upload release")
	}
}

func TestIntegrationCrossHostUploadIdentityCannotAuthorizeAnotherHost(t *testing.T) {
	serviceA, fixture, identityA := imageUploadFixture(t)
	lockPathA := node.HostLockPath(identityA.StateDir)
	config, err := ParseDatabaseConfig(fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	leaseA, err := AcquireImageUploadLease(fixture.ctx, config.ConnConfig, identityA, "live")
	if err != nil {
		t.Fatal(err)
	}
	defer leaseA.Release()
	if leaseA.conn == nil {
		t.Fatal("lease has no dedicated connection")
	}
	// The advisory session dies, but the A host lock and payload work remain.
	if err := leaseA.conn.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	identityB := identityA
	identityB.HostKeyFingerprint = "SHA256:" + strings.Repeat("B", 43)
	identityB.StateDir = t.TempDir()
	if _, err := VerifyWorkspaceRegistryHost(fixture.ctx, fixture.pool, identityB); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("B with A's name and a different fingerprint = %v, want conflict", err)
	}
	serviceB := &Service{pool: fixture.pool, registryHostIdentity: &identityB, registryEndpoint: serviceA.registryEndpoint}
	if _, err := serviceB.verifyLocalRegistryHost(fixture.ctx); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("B local verification = %v, want conflict", err)
	}
	if !hostLockContended(t, fixture.ctx, lockPathA) {
		t.Fatal("A upload lease did not hold the A host lock")
	}
	// B can acquire the app advisory lock after A's PostgreSQL session dies;
	// that alone must not authorize B's deletion.
	release, acquired, err := serviceB.tryLockTargets(fixture.ctx, []string{"app/live"})
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("B could not acquire the app advisory lock after A's session loss")
	}
	release()
	var originalID platform.AppID
	var originalRepo string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT id,image_repository FROM eregion_platform_apps WHERE name='live'`).Scan(&originalID, &originalRepo); err != nil {
		t.Fatal(err)
	}
	appOp := "op_" + strings.Repeat("a", 32)
	envOp := "op_" + strings.Repeat("b", 32)
	for _, row := range []struct{ id, kind, target string }{
		{appOp, platform.KindAppDelete, "app/live"},
		{envOp, platform.KindEnvironmentDelete, "env/live/production"},
	} {
		if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_operations
            (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt)
            VALUES($1,$2,$3,'operator','cross-host',$1,'sha256:hash','{}','running',1)`,
			row.id, row.kind, row.target); err != nil {
			t.Fatal(err)
		}
	}
	workerB := &platformWorker{service: serviceB}
	// deleteApplication/deleteEnvironment settle a terminal guard refusal
	// durably and return nil to their worker caller. The operation row is the
	// observable outcome: it must be failed/conflict and the app/environment
	// rows must survive.
	if err := workerB.deleteApplication(fixture.ctx, contract.OperationID(appOp), 1, "live"); err != nil {
		t.Fatalf("B app delete worker returned an unclassified error: %v", err)
	}
	if err := workerB.deleteEnvironment(fixture.ctx, contract.OperationID(envOp), 1, "live", "production"); err != nil {
		t.Fatalf("B environment delete worker returned an unclassified error: %v", err)
	}
	for _, row := range []struct{ id, kind string }{
		{appOp, platform.KindAppDelete},
		{envOp, platform.KindEnvironmentDelete},
	} {
		var state, code string
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,error_code FROM eregion_operations WHERE id=$1`, row.id).Scan(&state, &code); err != nil {
			t.Fatal(err)
		}
		if state != string(contract.Failed) || code != string(contract.Conflict) {
			t.Fatalf("%s operation state/code = %s/%s, want failed/conflict", row.kind, state, code)
		}
	}
	var cleanupRows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup c
        JOIN eregion_platform_environments e ON e.id=c.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='live'`).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("cross-host refusal recorded %d durable cleanup row(s)", cleanupRows)
	}
	var appID platform.AppID
	var repo string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT id,image_repository FROM eregion_platform_apps WHERE name='live'`).Scan(&appID, &repo); err != nil {
		t.Fatal(err)
	}
	if appID != originalID || repo != originalRepo {
		t.Fatalf("cross-host deletion changed app identity/repository: %s/%s -> %s/%s", originalID, originalRepo, appID, repo)
	}
	// The unique repository remains owned by A; B cannot claim it by name.
	raw, _ := json.Marshal(map[string]any{"name": "other", "image_repository": originalRepo})
	if _, err := serviceB.Submit(fixture.ctx, operations.Submission{
		Kind: platform.KindAppCreate, Target: platform.PlatformTarget().String(),
		Actor: testActor, IdempotencyKey: "cross-host-repo", Input: raw,
	}); err == nil {
		t.Fatal("B claimed A's unique app repository")
	}
}

func TestIntegrationImageUploadLeaseRefusesMissingOrEmptyRepository(t *testing.T) {
	service, fixture, identity := imageUploadFixture(t)
	config, err := ParseDatabaseConfig(fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireImageUploadLease(fixture.ctx, config.ConnConfig, identity, "missing"); err == nil || contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("missing app = %v", err)
	}
	if _, err := service.pool.Exec(fixture.ctx, `UPDATE eregion_platform_apps SET image_repository='' WHERE name='live'`); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireImageUploadLease(fixture.ctx, config.ConnConfig, identity, "live"); err == nil || !strings.Contains(err.Error(), "no image repository") {
		t.Fatalf("empty repository = %v", err)
	}
}
