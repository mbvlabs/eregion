package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func TestIntegrationResourceRecoversCompletedDockerDispatch(t *testing.T) {
	if os.Getenv("EREGION_TEST_PGBOUNCER_DOCKER") != "1" {
		t.Skip("requires a disposable Docker daemon")
	}
	pool, _ := testPool(t, true)
	ctx := t.Context()
	dir := t.TempDir()
	store := newTestEntities(t, pool)
	if _, err := AssignWorkspace(ctx, store, "test", testRegistryHostIdentityFor("local", dir)); err != nil {
		t.Fatal(err)
	}
	run := testRuntime(t, pool, 0, dir)
	s := run.Service
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleApplication}, []string{platform.CapabilityDocker}, ""); err != nil {
		t.Fatal(err)
	}
	app := platformSubmitContext(ctx, t, s, platform.KindAppCreate, "platform", "resource-recovery-app", map[string]any{
		"name":         "recoverfixture",
		"environments": []map[string]any{{"name": "production", "node": "local", "container_port": 8080}},
	})
	if err := runPlatformAttempt(ctx, s, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	op := platformSubmitContext(ctx, t, s, platform.KindResourceApply, "resource/recoverfixture/production/db", "resource-recovery", managedResourceInput(false, true))
	op, raw, owned, err := s.claimPlatform(ctx, op.ID, 1)
	if err != nil || !owned {
		t.Fatalf("claim resource operation: %v %v", owned, err)
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		t.Fatal(err)
	}
	workCtx, executor, err := s.acquirePlatformExecutor(ctx, op, normalized)
	if err != nil {
		t.Fatal(err)
	}
	recordCompletedResourceNetwork(t, workCtx, s, op)
	checkResourceDockerRecovery(t, s, op, executor)
}

func recordCompletedResourceNetwork(t *testing.T, ctx context.Context, s *Service, op contract.Operation) {
	t.Helper()
	tr, identity, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	network := "test-" + string(op.ID)
	stepID := platform.ResourceNetworkStepID(platform.ResourcePlan{StepToken: "a01234567890"})
	execution := engine.Execution{OperationID: op.ID, Attempt: op.Attempt, Step: stepID, RequestID: newRequestID()}
	raw, err := json.Marshal(platform.ArgvPayload{
		Step: stepID, Argv: []string{"docker", "network", "create", network}, TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := node.DefaultRegistry().Build(execution, raw)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, resourceDispatchKey{}, entities.PackPlacement{Node: "local", Identity: identity})
	if _, err := s.beginStep(ctx, op.ID, op.Attempt, stepID, engine.InputsHash(step.ID(), inputs), execution.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	response, err := tr.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, RequestID: execution.RequestID, Payload: raw}, nil)
	if err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("real node effect: %+v %v", response, err)
	}
	t.Cleanup(func() {
		if out, err := exec.CommandContext(context.WithoutCancel(ctx), "docker", "network", "rm", network).CombinedOutput(); err != nil {
			t.Errorf("remove fixture network: %s: %v", out, err)
		}
	})
	// Deliberately leave the database acknowledgement unwritten.
}

func checkResourceDockerRecovery(t *testing.T, s *Service, op contract.Operation, executor buildTargetExecutor) {
	t.Helper()
	ctx := t.Context()
	if err := s.settlePlatformExecutor(ctx, executor, nil); err != nil {
		t.Fatal(err)
	}
	claim := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	competitor := claimID("resource-competitor", "resource-competitor-token")
	if claim.acquire(claim.b, competitor, "env/recoverfixture/production") {
		t.Fatal("unacknowledged effect released the environment")
	}
	if _, err := s.RequestCancel(ctx, testActor, op.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, testActor, op.ID)
	if err != nil || current.State != contract.Cancelling {
		t.Fatalf("cancellation before node status proof: %+v %v", current, err)
	}
	var snooze *river.JobSnoozeError
	if err := s.resumeResourceExecution(ctx, current, executor); !errors.As(err, &snooze) {
		t.Fatalf("resume cancellation: %v", err)
	}
	current, err = s.Get(ctx, testActor, op.ID)
	if err != nil || current.State != contract.Canceled {
		t.Fatalf("cancellation after node status proof: %+v %v", current, err)
	}
	if !claim.acquire(claim.b, competitor, "env/recoverfixture/production") {
		t.Fatal("proven completion did not release the environment")
	}
}
