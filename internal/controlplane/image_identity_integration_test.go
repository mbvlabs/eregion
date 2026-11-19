package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func integrationDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func operationsSubmission(key string) operations.Submission {
	return operations.Submission{
		Kind:           operations.TestKind,
		Target:         operations.TestTarget,
		Actor:          testActor,
		IdempotencyKey: key,
		Input:          []byte(`{}`),
	}
}

// TestIntegrationImageIdentityBuildPersistsManifestAndConfigSeparately checks
// the real worker's build and persistence against the registry's actual digests.
func TestIntegrationImageIdentityBuildPersistsManifestAndConfigSeparately(t *testing.T) {
	testVerifiedLocalPack(t)
}

// captureNodeTransport records real dispatcher requests without executing host
// commands. It is only used to observe the selector a service/worker path sends.
type captureNodeTransport struct {
	steps  []node.Request
	result json.RawMessage
}

func (*captureNodeTransport) Name() string { return "capture" }

func (c *captureNodeTransport) Call(
	_ context.Context,
	request node.Request,
	_ func(node.Event),
) (node.Response, error) {
	if request.Method == node.MethodStatus {
		return node.Response{
			Status:      engine.StatusAbsent,
			ExecutionID: request.Execution.ExecutionID(),
		}, nil
	}
	c.steps = append(c.steps, request)
	return node.Response{
		Status: engine.StatusCompleted, ExecutionID: request.Execution.ExecutionID(),
		Executed: true, Result: c.result,
	}, nil
}

// TestIntegrationImageIdentityReadyReleaseReuseGate exercises the reuse gate
// against a real PG-backed service and dispatcher abstraction. The canonical
// manifest/config pair comes from the existing test-only identity override; the
// external registry observation belongs live. A historical config mismatch is
// refused before any node step; a matching release sends the canonical manifest
// as Image and the config digest as ImageID.
func TestIntegrationImageIdentityReadyReleaseReuseGate(t *testing.T) {
	t.Setenv("EREGION_NODE_STATE_DIR", "")
	t.Setenv("EREGION_DATABASE_URL", "")
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	manifest := "sha256:" + strings.Repeat("b", 64)
	config := "sha256:" + strings.Repeat("a", 64)
	canonical := "registry.example/live@" + manifest
	s.imageIdentityOverrides = map[string][2]string{canonical: {manifest, config}}
	runtimeID := "sha256:" + strings.Repeat("d", 64)

	submitOperation := func(key string) contract.Operation {
		t.Helper()
		op, err := s.Submit(ctx, operationsSubmission(key))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(
			ctx,
			`UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`,
			op.ID,
		); err != nil {
			t.Fatal(err)
		}
		return op
	}

	historical := platform.Release{
		ID: platform.ReleaseID("rel_" + strings.Repeat("1", 32)), Image: canonical,
		ImageID: "sha256:" + strings.Repeat("e", 64), Status: platform.ReleaseReady,
	}
	op := submitOperation("identity-gate-historical")
	recorder := &captureNodeTransport{}
	if err := (&platformWorker{service: s}).materializeReadyReleaseImage(
		ctx,
		op.ID,
		1,
		historical,
		recorder,
	); err == nil {
		t.Fatal("a historical config mismatch was accepted")
	} else if !strings.Contains(
		err.Error(),
		"historical release image_id",
	) {
		t.Fatalf("unexpected mismatch error: %v", err)
	}
	if len(recorder.steps) != 0 {
		t.Fatalf("gate dispatched %d node steps before refusing", len(recorder.steps))
	}
	var stepRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_steps WHERE operation_id=$1`, op.ID).
		Scan(&stepRows); err != nil ||
		stepRows != 0 {
		t.Fatalf("gate wrote %d step rows, err=%v", stepRows, err)
	}

	accepted := platform.Release{
		ID: platform.ReleaseID("rel_" + strings.Repeat("2", 32)), Image: canonical,
		ImageID: config, Status: platform.ReleaseReady,
	}
	acceptedOp := submitOperation("identity-gate-accepted")
	recorder = &captureNodeTransport{
		result: json.RawMessage(
			`{"image":"` + canonical + `","image_id":"` + config + `","runtime_id":"` + runtimeID + `"}`,
		),
	}
	if err := (&platformWorker{service: s}).materializeReadyReleaseImage(
		ctx,
		acceptedOp.ID,
		1,
		accepted,
		recorder,
	); err != nil {
		t.Fatal(err)
	}
	if len(recorder.steps) != 1 {
		t.Fatalf("accepted release dispatched %d node steps, want 1", len(recorder.steps))
	}
	var payload platform.ReadyImagePayload
	if err := json.Unmarshal(recorder.steps[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Step != deploymentReadyImageStep || payload.Image != canonical ||
		payload.ImageID != config {
		t.Fatalf("ready-image payload=%+v, want canonical manifest and config digest", payload)
	}
}
