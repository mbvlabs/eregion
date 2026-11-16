package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
)

// mismatchedInputsTransport reports an already-applied node effect under
// different inputs. It is a typed external status result; no node process is
// started.
type mismatchedInputsTransport struct {
	mu   sync.Mutex
	runs int
}

func (*mismatchedInputsTransport) Name() string         { return "mismatched-inputs" }
func (*mismatchedInputsTransport) NodeIdentity() string { return "local" }

func (t *mismatchedInputsTransport) Call(
	_ context.Context,
	request node.Request,
	_ func(node.Event),
) (node.Response, error) {
	if request.Method == node.MethodStatus {
		return node.Response{
			Status:      engine.StatusCompleted,
			ExecutionID: request.Execution.ExecutionID(),
			InputsHash:  strings.Repeat("f", 64),
			Executed:    true,
		}, nil
	}
	t.mu.Lock()
	t.runs++
	t.mu.Unlock()
	return node.Response{}, errors.New("node run must not follow a mismatched status")
}

func (t *mismatchedInputsTransport) runCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs
}

func TestIntegrationInputsMismatchFailsWithoutRetry(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 2)
	transport := &mismatchedInputsTransport{}
	run.Service.transport = transport
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("inputs-mismatch", `{"steps":1}`))
	if err != nil {
		t.Fatal(err)
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
	if got := transport.runCount(); got != 0 {
		t.Fatalf("inputs mismatch applied a second effect: %d runs", got)
	}
	if got := progressEventCount(t, run.Service, op.ID); got != 0 {
		t.Fatalf("inputs mismatch acknowledged progress: %d", got)
	}
}
