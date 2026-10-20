package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// seedFailedVerification creates the durable rows a deployment leaves behind
// when the worker dies after a restore. No node effect is executed.
func seedDeployment(t *testing.T, run *Runtime, key string) (contract.Operation, platform.Deployment, platform.Environment, platform.Release, platform.Release) {
	t.Helper()
	ctx := context.Background()
	s := run.Service
	platformAwaitSubmit := func(op contract.Operation) contract.Operation {
		t.Helper()
		await(t, 10*time.Second, func() bool {
			current, err := s.Get(ctx, testActor, op.ID)
			return err == nil && current.State == contract.Queued
		})
		current, err := s.Get(ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	platformAwaitSubmit(platformSubmit(t, s, platform.KindNodeRegister, "platform", key+"-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}))
	platformAwaitSubmit(platformSubmit(t, s, platform.KindAppCreate, "platform", key+"-app", map[string]any{"name": "live"}))
	platformAwaitSubmit(platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", key+"-env", map[string]any{
		"name": "production", "node": "local", "container_port": 8080,
	}))
	createRelease := func(sourceKey string) platform.Release {
		op := platformAwaitSubmit(platformSubmit(t, s, platform.KindReleaseCreate, "env/live/production", key+"-"+sourceKey, map[string]any{
			"source": map[string]any{"type": "image", "image": "caddy:2"},
		}))
		release, err := s.releaseForOperation(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := run.Service.pool.Exec(ctx, `UPDATE eregion_platform_releases SET status='ready',image_id=$2 WHERE id=$1`, release.ID, "sha256:"+strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
		release.Status = platform.ReleaseReady
		release.ImageID = "sha256:" + strings.Repeat("a", 64)
		return release
	}
	previous := createRelease("previous")
	current := createRelease("current")
	deploy := platformAwaitSubmit(platformSubmit(t, s, platform.KindDeploy, "env/live/production", key+"-deploy", map[string]any{
		"release_id": current.ID,
	}))
	dep, has, err := s.deploymentByOperation(ctx, deploy.ID)
	if err != nil || !has {
		t.Fatalf("deployment row: %v %v", has, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_deployments SET previous_release_id=$2 WHERE operation_id=$1`, deploy.ID, previous.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_environments
        SET serving_release_id=$1,applied_release_id=$1,health_state='healthy',health_detail='seeded',health_checked_at=clock_timestamp()
        WHERE name='production'`, previous.ID); err != nil {
		t.Fatal(err)
	}
	dep.PreviousReleaseID = previous.ID
	env, err := s.environment(ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	// No worker ran. Make the operation claimable by reconciliation and discard
	// its River job, which is how a crashed/deleted job appears.
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, deploy.ID); err != nil {
		t.Fatal(err)
	}
	discardPlatformJob(t, s.pool, deploy.ID)
	return deploy, dep, env, previous, current
}

func discardPlatformJob(t *testing.T, pool *pgxpool.Pool, id contract.OperationID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE river_job SET state='discarded',finalized_at=clock_timestamp()
        WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, id); err != nil {
		t.Fatal(err)
	}
}

func seedCompletedStep(t *testing.T, pool *pgxpool.Pool, id contract.OperationID, step string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_operation_steps
        (operation_id,step,inputs_hash,state,attempt,execution_id,completed_at)
        VALUES ($1,$2,$3,'completed',1,'seeded',$4)`,
		id, step, strings.Repeat("0", 64), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func seedRunningStep(t *testing.T, pool *pgxpool.Pool, id contract.OperationID, step string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_operation_steps
        (operation_id,step,inputs_hash,state,attempt,execution_id)
        VALUES ($1,$2,$3,'running',1,'seeded')`,
		id, step, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationReconcileCompletedRestoreRecordsServingPrevious is the
// regression for a worker dying after platform-container-03-v1 restored the
// previous release but before committing failure. Reconciliation must record
// the previous release as serving, not the failed new release.
func TestIntegrationReconcileCompletedRestoreRecordsServingPrevious(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0, t.TempDir())
	deploy, _, _, previous, current := seedDeployment(t, run, "reconcile-restore")
	seedCompletedStep(t, pool, deploy.ID, deploymentCandidateStep)
	seedCompletedStep(t, pool, deploy.ID, deploymentHealthStep)
	seedCompletedStep(t, pool, deploy.ID, deploymentSwitchStep)
	seedCompletedStep(t, pool, deploy.ID, deploymentVerifyStep)
	seedCompletedStep(t, pool, deploy.ID, deploymentRestoreStep)
	seedCompletedStep(t, pool, deploy.ID, deploymentRestoreHealth)

	if err := run.Service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	op, err := run.Service.Get(ctx, testActor, deploy.ID)
	if err != nil || op.State != contract.Failed {
		t.Fatalf("reconciled operation=%+v err=%v", op, err)
	}
	env, err := run.Service.environment(ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	if env.ServingRelease != previous.ID || env.AppliedRelease != previous.ID {
		t.Fatalf("restored release was not recorded as serving: %+v", env)
	}
	if env.HealthState != platform.HealthHealthy {
		t.Fatalf("completed restore health was not recorded: %+v", env)
	}
	if env.ServingRelease == current.ID {
		t.Fatal("failed release was recorded as serving")
	}
	dep, has, err := run.Service.deploymentByOperation(ctx, deploy.ID)
	if err != nil || !has || dep.State != platform.DeploymentFailed {
		t.Fatalf("deployment=%+v has=%v err=%v", dep, has, err)
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: deploy.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range page.Events {
		if event.ServingReleaseID == string(previous.ID) && event.ReleaseID == string(current.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no event named the restored serving release: %+v", page.Events)
	}
	if !run.Service.completedStep(ctx, deploy.ID, deploymentCleanupStep) {
		t.Fatal("reconciliation did not remove the candidate through a durable cleanup step")
	}
}

// TestIntegrationReconcileCancelAfterSwitchRecordsServing covers cancellation
// reconciled after a completed switch: the environment must not keep claiming
// the old release.
func TestIntegrationReconcileCancelAfterSwitchRecordsServing(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0, t.TempDir())
	deploy, _, _, previous, current := seedDeployment(t, run, "reconcile-cancel-switch")
	seedCompletedStep(t, pool, deploy.ID, deploymentCandidateStep)
	seedCompletedStep(t, pool, deploy.ID, deploymentHealthStep)
	seedCompletedStep(t, pool, deploy.ID, deploymentSwitchStep)
	if _, err := pool.Exec(context.Background(), `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, deploy.ID); err != nil {
		t.Fatal(err)
	}

	if err := run.Service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	op, err := run.Service.Get(ctx, testActor, deploy.ID)
	if err != nil || op.State != contract.Canceled {
		t.Fatalf("reconciled cancel=%+v err=%v", op, err)
	}
	env, err := run.Service.environment(ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	if env.ServingRelease != current.ID || env.AppliedRelease != current.ID {
		t.Fatalf("completed switch was ignored: %+v (previous %s)", env, previous.ID)
	}
	if env.HealthState != platform.HealthUnknown {
		t.Fatalf("unverified switch claimed health %q", env.HealthState)
	}
	dep, _, _ := run.Service.deploymentByOperation(ctx, deploy.ID)
	if dep.State != platform.DeploymentCancelled {
		t.Fatalf("deployment state=%s", dep.State)
	}
	if !run.Service.completedStep(ctx, deploy.ID, deploymentCleanupStep) {
		t.Fatal("cancellation reconciliation did not clean up the candidate")
	}
}

// TestIntegrationReconcileCancelBeforeSwitchCleansCandidate proves a cancelled
// candidate leaves no container behind and the serving release is unchanged.
func TestIntegrationReconcileCancelBeforeSwitchCleansCandidate(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0, t.TempDir())
	deploy, _, _, previous, _ := seedDeployment(t, run, "reconcile-cancel-candidate")
	seedRunningStep(t, pool, deploy.ID, deploymentCandidateStep)
	if _, err := pool.Exec(context.Background(), `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, deploy.ID); err != nil {
		t.Fatal(err)
	}

	if err := run.Service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	op, err := run.Service.Get(ctx, testActor, deploy.ID)
	if err != nil || op.State != contract.Canceled {
		t.Fatalf("reconciled cancel=%+v err=%v", op, err)
	}
	if !run.Service.completedStep(ctx, deploy.ID, deploymentCleanupStep) {
		t.Fatal("cancelled candidate was not cleaned up durably")
	}
	env, err := run.Service.environment(ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	if env.ServingRelease != previous.ID {
		t.Fatalf("a cancelled before-switch deployment changed serving: %+v", env)
	}
}
