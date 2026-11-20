package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// Exercises the durable post-Pack boundary without simulating a node or Pack.
func TestIntegrationBuildPlacementSurvivesRelease(t *testing.T) { //nolint:gocognit // Ordered admission, release and retry checks.
	ctx := context.Background()
	pool, _ := testPool(t, true)
	dir := t.TempDir()
	if _, err := AssignWorkspace(ctx, newTestEntities(t, pool), "test", testRegistryHostIdentityFor("local", dir)); err != nil {
		t.Fatal(err)
	}
	s := testRuntime(t, pool, 0, dir).Service
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "placement", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	tr, identity, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	payload := platform.PackPayload{Step: deploymentPackStep, Image: "127.0.0.1:5000/example:latest", Registry: "127.0.0.1:5000", Path: "/source", Builder: "example/builder:latest"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep}
	step, err := node.DefaultRegistry().Build(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	hash := engine.InputsHash(step.ID(), inputs)
	if _, err := s.beginBuildDispatch(ctx, op.ID, 1, "local", identity, hash, exec.ExecutionID(), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='completed', result='{"ok":true}' WHERE operation_id=$1 AND step=$2`, op.ID, deploymentPackStep); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseBuildCapacity(ctx, op.ID, 1, "local"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 2, "test"); err != nil || !ok {
		t.Fatalf("retry claim: %v %v", ok, err)
	}
	w := &platformWorker{service: s}
	result, selected, err := w.executeBuildPack(ctx, op.ID, 2, platform.Environment{Node: "moved"},
		platform.Release{ID: "release"}, platform.BuildRecord{ID: "build"}, tr, payload, nil)
	if err != nil || string(result) != `{"ok": true}` {
		t.Fatalf("completed retry: %s %v", result, err)
	}
	local, ok := selected.(*node.Local)
	if !ok || local.NodeIdentity() != "local" {
		t.Fatalf("retry selected wrong builder: %T", selected)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_build_placements SET node_identity='changed' WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.executeBuildPack(ctx, op.ID, 2, platform.Environment{Node: "local"},
		platform.Release{ID: "release"}, platform.BuildRecord{ID: "build"}, tr, payload, nil); err == nil {
		t.Fatal("changed builder identity accepted")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_build_placements WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.executeBuildPack(ctx, op.ID, 2, platform.Environment{Node: "local"},
		platform.Release{ID: "release"}, platform.BuildRecord{ID: "build"}, tr, payload, nil); err == nil {
		t.Fatal("historical intent without placement accepted")
	}
	if _, err := s.beginBuildDispatch(ctx, op.ID, 2, "local", identity, hash, exec.ExecutionID(), "", ""); err == nil {
		t.Fatal("historical intent was assigned a guessed placement")
	}
}

// A BuildRetry resumes a completed Pack through the real worker entry and local
// node. The deliberately nonexistent image makes the post-Pack observation
// fail, proving the worker reached the original node without running Pack.
func TestIntegrationBuildRetryUsesOriginalBuilderAfterMove(t *testing.T) {
	ctx := context.Background()
	pool, _ := testPool(t, true)
	dir := t.TempDir()
	if _, err := AssignWorkspace(ctx, newTestEntities(t, pool), "test", testRegistryHostIdentityFor("local", dir)); err != nil {
		t.Fatal(err)
	}
	s := testRuntime(t, pool, 0, dir).Service
	s.registryEndpoint = "127.0.0.1:5000"
	for _, name := range []string{"local", "moved"} {
		if err := insertFixtureNode(ctx, pool, name, []string{platform.RoleApplication, platform.RoleBuilder},
			[]string{platform.CapabilityDocker, platform.CapabilityBuildpacks}, ""); err != nil {
			t.Fatal(err)
		}
	}
	platformSubmit(t, s, platform.KindAppCreate, "platform", "placement-app", map[string]any{"name": "placement"})
	platformSubmit(t, s, platform.KindEnvironmentCreate, "app/placement", "placement-env",
		map[string]any{"name": "production", "node": "local", "container_port": 8080})
	env, err := s.environment(ctx, "placement", "production")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "placement-seed", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	release := seedDirectoryBuildRelease(t, ctx, s, seed.ID, env, t.TempDir())
	old, found, err := s.buildByOperation(ctx, seed.ID)
	if err != nil || !found {
		t.Fatalf("old build: found=%v err=%v", found, err)
	}
	if err := s.markBuildFailed(ctx, old.ID, contract.NewFault(contract.Unavailable, "previous failure", ""), release.ID); err != nil {
		t.Fatal(err)
	}
	op := platformSubmit(t, s, platform.KindBuildRetry, "build/"+string(old.ID), "placement-retry", map[string]any{})
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET roles=$1, capabilities=$2 WHERE name='local'`,
		[]string{platform.RoleApplication, platform.RoleBuilder}, []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("first attempt: %v %v", ok, err)
	}
	recordCompletedRetryPack(t, s, pool, op.ID)
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environments SET build_node='moved' WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	moved, err := s.environment(ctx, env.App, env.Name)
	if err != nil || moved.BuildNodeName() != "moved" {
		t.Fatalf("environment move: %+v %v", moved, err)
	}
	err = runPlatformAttempt(ctx, s, op.ID, 2)
	if err == nil || !strings.Contains(err.Error(), "node did not return an image observation") {
		current, _, getErr := s.entities.Operations().Get(ctx, op.ID)
		events, _ := s.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
		t.Fatalf("retry did not reach post-Pack observation on original node: %v; operation=%+v get=%v events=%+v", err, current, getErr, events)
	}
	updated, err := s.releaseForOperation(ctx, op.ID)
	if err != nil || updated.Status != platform.ReleasePending {
		t.Fatalf("unavailable image observation changed release: %+v %v", updated, err)
	}
	assertClaimCount(t, ctx, pool, op.ID, 0)
}

func recordCompletedRetryPack(t *testing.T, s *Service, pool *pgxpool.Pool, id contract.OperationID) {
	t.Helper()
	ctx := context.Background()
	build, found, err := s.buildByOperation(ctx, id)
	if err != nil || !found {
		t.Fatalf("retry build: found=%v err=%v", found, err)
	}
	release, err := s.releaseForOperation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	gitRef := build.CommitSHA
	if gitRef == "" {
		gitRef = build.Source.Ref
	}
	payload, err := packPayloadForBuild(build, release, gitRef, s.registryEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: id, Attempt: 1, Step: deploymentPackStep}
	step, err := node.DefaultRegistry().Build(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginBuildDispatch(ctx, id, 1, "local", identity, engine.InputsHash(step.ID(), inputs), exec.ExecutionID(), build.ID, release.ID); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	result, err := json.Marshal(map[string]string{"reference": "127.0.0.1:5000/placement@" + digest, "image_id": digest, "runtime_id": digest, "commit": build.CommitSHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='completed', result=$3::jsonb WHERE operation_id=$1 AND step=$2`, id, deploymentPackStep, string(result)); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseBuildCapacity(ctx, id, 1, "local"); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationDeploymentArtifactUsesOriginalBuilderAfterMove(t *testing.T) {
	ctx := context.Background()
	f := newPointerBuildFixture(t)
	binding := *f.service.registryHostIdentity
	binding.NodeName = "local"
	f.service.registryHostIdentity = &binding
	if err := insertFixtureNode(ctx, f.pool, "moved", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_nodes SET roles=$1, capabilities=$2 WHERE name='local'`,
		[]string{platform.RoleApplication, platform.RoleBuilder}, []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1); err != nil || !ok {
		t.Fatalf("first attempt: %v %v", ok, err)
	}
	recordCompletedRetryPack(t, f.service, f.pool, f.op.ID)
	if _, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 2); err != nil || !ok {
		t.Fatalf("retry attempt: %v %v", ok, err)
	}
	release, err := f.service.releaseByID(ctx, f.release)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := f.service.resolver.TransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	env := f.env
	env.BuildNode = "moved"
	run := &deploymentRun{id: f.op.ID, attempt: 2, env: env, release: release, tr: tr}
	finished, err := (&platformWorker{service: f.service}).prepareDeploymentArtifact(ctx, run)
	if !finished || err == nil || !strings.Contains(err.Error(), "node did not return an image observation") {
		t.Fatalf("deployment did not reach original node observation: finished=%v err=%v", finished, err)
	}
	assertClaimCount(t, ctx, f.pool, f.op.ID, 0)
}
