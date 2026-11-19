package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// Two real River build retries run on the verified local node. An actual
// deployment network effect must progress while both build slots are held.
func TestIntegrationBuildSlotConcurrency(t *testing.T) { //nolint:gocognit // The overlap and network progress form one ordered scenario.
	pool, _ := testPool(t, true)
	registry := os.Getenv("EREGION_TEST_REAL_PACK_REGISTRY")
	if registry == "" {
		t.Fatal("set EREGION_TEST_REAL_PACK_REGISTRY to the registry on an isolated real Pack test daemon")
	}
	ctx := context.Background()
	stateDir := t.TempDir()
	if _, err := AssignWorkspace(ctx, newTestEntities(t, pool), "test", testRegistryHostIdentityFor("local", stateDir)); err != nil {
		t.Fatal(err)
	}
	run := testRuntime(t, pool, 2, stateDir)
	s := run.Service
	s.registryEndpoint = registry
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleApplication, platform.RoleBuilder}, []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET build_concurrency=2 WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for name, content := range map[string]string{
		"go.mod": "module example.com/overlapfixture\n\ngo 1.22\n", "main.go": "package main\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	builds := make([]platform.BuildID, 0, 2)
	environments := make([]platform.Environment, 0, 2)
	for _, app := range []string{"overlapone", "overlaptwo"} {
		for _, op := range []contract.Operation{
			platformSubmit(t, s, platform.KindAppCreate, "platform", "app-"+app, map[string]any{"name": app}),
			platformSubmit(t, s, platform.KindEnvironmentCreate, "app/"+app, "env-"+app, map[string]any{"name": "production", "node": "local", "container_port": 8080}),
		} {
			if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
				t.Fatal(err)
			}
			platformAwait(t, s, op, contract.Succeeded)
		}
		env, err := s.environment(ctx, app, "production")
		if err != nil {
			t.Fatal(err)
		}
		environments = append(environments, env)
		seed, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "seed-" + app, Input: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok, err := s.claimAttempt(ctx, seed.ID, 1, "test"); err != nil || !ok {
			t.Fatalf("seed attempt: %v %v", ok, err)
		}
		release := seedDirectoryBuildRelease(t, ctx, s, seed.ID, env, source)
		build, found, err := s.buildByOperation(ctx, seed.ID)
		if err != nil || !found {
			t.Fatalf("seed build: %+v %v", build, err)
		}
		if err := s.markBuildFailed(ctx, build.ID, contract.NewFault(contract.Unavailable, "fixture previous failure", ""), release.ID); err != nil {
			t.Fatal(err)
		}
		builds = append(builds, build.ID)
	}
	startWorkers(t, run)
	a := platformSubmit(t, s, platform.KindBuildRetry, "build/"+string(builds[0]), "overlap-one", map[string]any{})
	b := platformSubmit(t, s, platform.KindBuildRetry, "build/"+string(builds[1]), "overlap-two", map[string]any{})
	tr, _, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	await(t, 60*time.Second, func() bool {
		var count int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_build_capacity_claims c
			JOIN eregion_operation_steps s ON s.operation_id=c.operation_id AND s.step=$3 AND s.state='running'
			WHERE c.operation_id IN ($1,$2)`, a.ID, b.ID, deploymentPackStep).Scan(&count)
		if err != nil || count != 2 {
			return false
		}
		for _, op := range []contract.Operation{a, b} {
			status, err := tr.Call(ctx, node.Request{Method: node.MethodStatus,
				Execution: engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep}}, nil)
			if err != nil || status.Status != engine.StatusRunning || status.Phase != engine.PhaseApplying {
				return false
			}
		}
		return true
	})
	env := environments[0]
	network := platform.EnvironmentNetwork(env.ID)
	payload := platform.ArgvPayload{Step: deploymentNetworkStep,
		Argv:      []string{"docker", "network", "create", "--driver", "bridge", "--label", "io.eregion.app=" + env.App, "--label", "io.eregion.environment=" + env.Name, network},
		CheckArgv: []string{"docker", "network", "inspect", "--format", "{{.Name}}", network}, TimeoutSeconds: 30}
	if _, err := (&platformWorker{service: s}).runStepOn(ctx, tr, a.ID, 1, deploymentNetworkStep, payload, nil); err != nil {
		t.Fatalf("network effect while Pack runs: %v", err)
	}
	assertClaimCount(t, ctx, pool, b.ID, 1)
	awaitRealBuild(t, s, a)
	awaitRealBuild(t, s, b)
	for _, op := range []contract.Operation{a, b} {
		assertBuildSlotEvent(t, ctx, s, op.ID, "build_running")
		assertClaimCount(t, ctx, pool, op.ID, 0)
		release, err := s.releaseForOperation(ctx, op.ID)
		if err != nil || release.Status != platform.ReleaseReady || !platform.IsImageDigestReference(release.Image) {
			t.Fatalf("completed build release: %+v %v", release, err)
		}
	}
}
