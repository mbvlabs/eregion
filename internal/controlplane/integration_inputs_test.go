package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestIntegrationInputsMismatchFailsWithoutRetry(t *testing.T) {
	pool, _ := testPool(t, true)
	nodeDir := t.TempDir()
	run := testRuntime(t, pool, 2, nodeDir)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("inputs-mismatch", `{"steps":1}`))
	if err != nil {
		t.Fatal(err)
	}
	local, err := node.NewLocal(nodeDir, "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: node.DiagnosticStepID(1), RequestID: "preexisting"}
	payload, _ := json.Marshal(node.DiagnosticPayload{Index: 1, Total: 2})
	response, err := local.Call(ctx, node.Request{Method: node.MethodRun, RequestID: "preexisting", Execution: exec, Payload: payload}, nil)
	if err != nil || response.Status != engine.StatusCompleted || !response.Executed {
		t.Fatalf("seed effect under different inputs: %+v %v", response, err)
	}
	startWorkers(t, run)
	await(t, 10*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		return err == nil && current.State == contract.Failed
	})
	current, _ := run.Service.Get(ctx, testActor, op.ID)
	if current.Attempt != 1 {
		t.Fatalf("inputs mismatch was retried: attempt %d", current.Attempt)
	}
	if got := effectCount(t, nodeDir, op.ID, 1); got != 1 {
		t.Fatalf("inputs mismatch applied a second effect: %d", got)
	}
	if got := progressEventCount(t, run.Service, op.ID); got != 0 {
		t.Fatalf("inputs mismatch acknowledged progress: %d", got)
	}
}
