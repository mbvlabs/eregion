package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationReleaseCommandLostResponseStaysRunning proves an unknown node
// outcome does not record the release command as failed. The row stays running
// under the live operation, a competing deploy refuses, and the retry recovers
// the completed effect through node status without running the command twice.
func TestIntegrationReleaseCommandLostResponseStaysRunning(t *testing.T) {
	pool, _ := testPool(t, true)
	fake := newScriptedDeployTransport()
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 2, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Service.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	run.Service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	ctx := context.Background()
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "lost-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "lost-app", map[string]any{"name": "live"}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", "lost-env", map[string]any{
		"name": "production", "node": "local", "container_port": 80, "health": map[string]any{"timeout_seconds": 2},
		"domain": "live.example.com", "domain_primary": true,
	}), contract.Succeeded)

	platformAwait(t, s, platformSubmit(t, s, platform.KindReleaseCreate, "env/live/production", "lost-release", map[string]any{
		"source":  map[string]any{"type": "image", "image": "registry.example/live:v1"},
		"command": map[string]any{"argv": []string{"/bin/migrate"}},
	}), contract.Succeeded)
	releaseID := platform.ReleaseID(latestRelease(t, pool))
	digest := platform.ReleaseCommandDigest(platform.CommandSpec{Argv: []string{"/bin/migrate"}, TimeoutSeconds: 120})

	deploy := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "lost-deploy", map[string]any{"release_id": releaseID})
	fake.lose(deploymentCommandStep, deploy.ID)
	await(t, 30*time.Second, func() bool {
		return fake.runCount(deploy.ID, deploymentCommandStep) > 0
	})

	current, err := s.Get(ctx, testActor, deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("deploy became terminal after a lost response: %+v", current)
	}
	row, err := s.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "running" || !row.Live || row.OperationID != deploy.ID {
		t.Fatalf("lost response did not leave the row running under this operation: %+v", row)
	}
	competing, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if got := releaseCommandAction(row, platform.ActionDeploy, competing); got != releaseCommandRefuse {
		t.Fatalf("competing deploy action = %v, want refuse", got)
	}

	platformAwait(t, s, deploy, contract.Succeeded)
	if got := fake.runCount(deploy.ID, deploymentCommandStep); got != 1 {
		t.Fatalf("release command ran %d times after a lost response", got)
	}
	row, err = s.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "succeeded" {
		t.Fatalf("release command row = %+v, want succeeded", row)
	}
}

// TestIntegrationReleaseCommandTerminalOwnerRecovery proves a later deploy
// recovers the original operation's completed command instead of running it
// again after the original operation became terminal with a lost response.
func TestIntegrationReleaseCommandTerminalOwnerRecovery(t *testing.T) {
	pool, _ := testPool(t, true)
	fake := newScriptedDeployTransport()
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 2, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Service.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	run.Service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	ctx := context.Background()
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "recover-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "recover-app", map[string]any{"name": "live"}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", "recover-env", map[string]any{
		"name": "production", "node": "local", "container_port": 80, "health": map[string]any{"timeout_seconds": 2},
		"domain": "live.example.com", "domain_primary": true,
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindReleaseCreate, "env/live/production", "recover-release", map[string]any{
		"source":  map[string]any{"type": "image", "image": "registry.example/live:v1"},
		"command": map[string]any{"argv": []string{"/bin/migrate"}},
	}), contract.Succeeded)
	releaseID := platform.ReleaseID(latestRelease(t, pool))
	digest := platform.ReleaseCommandDigest(platform.CommandSpec{Argv: []string{"/bin/migrate"}, TimeoutSeconds: 120})

	original := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "recover-deploy-1", map[string]any{"release_id": releaseID})
	fake.lose(deploymentCommandStep, original.ID)
	await(t, 30*time.Second, func() bool {
		return fake.runCount(original.ID, deploymentCommandStep) > 0
	})
	// Simulate retry exhaustion/cancellation: the detached command effect
	// remains unresolved while the operation is terminal.
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='failed' WHERE id=$1`, original.ID); err != nil {
		t.Fatal(err)
	}

	recovered := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "recover-deploy-2", map[string]any{"release_id": releaseID})
	platformAwait(t, s, recovered, contract.Succeeded)
	if got := fake.runCount(recovered.ID, deploymentCommandStep); got != 0 {
		t.Fatalf("recovered deploy dispatched the release command %d times", got)
	}
	row, err := s.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "succeeded" || row.OperationID != original.ID {
		t.Fatalf("recovered row = %+v, want succeeded under the original operation", row)
	}
}
