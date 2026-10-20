package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// failingTransport fails every status request for one operation. It is the
// test seam for a node that cannot be reached during reconciliation.
type failingTransport struct {
	inner node.Transport
	fail  contract.OperationID
}

func (f *failingTransport) Name() string { return "failing" }

func (f *failingTransport) Call(ctx context.Context, request node.Request, onEvent func(node.Event)) (node.Response, error) {
	if request.Execution.OperationID == f.fail {
		return node.Response{}, errors.New("the node is unreachable")
	}
	return f.inner.Call(ctx, request, onEvent)
}

// TestIntegrationReconcileContinuesPastPerOperationErrors proves that one
// operation that cannot be reconciled does not hide the others, and that the
// collected failures are reported.
func TestIntegrationReconcileContinuesPastPerOperationErrors(t *testing.T) {
	pool, _ := testPool(t, true)
	nodeDir := t.TempDir()
	run := testRuntime(t, pool, 0, nodeDir)
	ctx := context.Background()
	failing, err := run.Service.Submit(ctx, submission("reconcile-failing", `{"steps":1}`))
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := run.Service.Submit(ctx, submission("reconcile-reconciled", `{"steps":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state='discarded',finalized_at=clock_timestamp() WHERE id IN (SELECT job_id FROM eregion_operations WHERE id IN ($1,$2))`, failing.ID, reconciled.ID); err != nil {
		t.Fatal(err)
	}
	// Give the unreachable operation a step row, so reconciliation has to ask
	// the node about it.
	exec := engine.Execution{OperationID: failing.ID, Attempt: 0, Step: node.DiagnosticStepID(1), RequestID: "reconcile"}
	step, err := node.NewDiagnosticStep(exec, node.DiagnosticPayload{Index: 1, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Service.beginStep(ctx, failing.ID, 0, step.ID(), engine.InputsHash(step.ID(), inputs), exec.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	local, err := node.NewLocal(nodeDir, "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	run.Service.transport = &failingTransport{inner: local, fail: failing.ID}
	err = run.Service.Reconcile(ctx)
	if err == nil {
		t.Fatal("the reconciliation failure was not reported")
	}
	if !strings.Contains(err.Error(), string(failing.ID)) {
		t.Fatalf("the reported failure does not name the operation: %v", err)
	}
	// The other operation was still reconciled to a visible terminal state.
	current, err := run.Service.Get(ctx, testActor, reconciled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contract.Failed {
		t.Fatalf("the second operation was not reconciled: %+v", current)
	}
	if current.ErrorCode != contract.Unavailable {
		t.Fatalf("the reconciled operation has no error code: %+v", current)
	}
	// The operation that could not be reconciled is left visible, not rewritten.
	stuck, err := run.Service.Get(ctx, testActor, failing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stuck.State.Terminal() {
		t.Fatalf("the unreachable operation was terminalised: %+v", stuck)
	}
}
