package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// TestIntegrationImageIdentityBuildPersistsManifestAndConfigSeparately drives
// the real service/worker build path with distinct M, C and R observations.
func TestIntegrationImageIdentityBuildPersistsManifestAndConfigSeparately(t *testing.T) {
	t.Setenv("EREGION_NODE_STATE_DIR", "")
	t.Setenv("EREGION_DATABASE_URL", "")
	pool, _ := testPool(t, true)
	manifest := "sha256:" + strings.Repeat("b", 64)
	config := "sha256:" + strings.Repeat("a", 64)
	runtimeID := "sha256:" + strings.Repeat("c", 64)
	commit := "1234567890abcdef1234567890abcdef12345678"
	transport := &scriptedPlatformTransport{results: map[string]json.RawMessage{
		deploymentPackStep: json.RawMessage(
			`{"reference":"127.0.0.1:5000/live@` + manifest + `","image_id":"` + config + `","runtime_id":"` + runtimeID + `","commit":"` + commit + `"}`,
		),
		deploymentImageStep: json.RawMessage(`{"stdout":"` + runtimeID + `"}`),
	}}
	run, err := New(newTestEntities(t, pool), integrationDiscardLogger(), WorkerConfig{
		Concurrency: 2, TestOnly: true, Transport: transport, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.routeSync = func(context.Context) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	ctx := context.Background()
	platformAwait(
		t,
		s,
		platformSubmit(
			t,
			s,
			platform.KindNodeRegister,
			"platform",
			"identity-build-node",
			map[string]any{
				"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
				"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
			},
		),
		contract.Succeeded,
	)
	platformAwait(
		t,
		s,
		platformSubmit(
			t,
			s,
			platform.KindAppCreate,
			"platform",
			"identity-build-app",
			map[string]any{"name": "identity"},
		),
		contract.Succeeded,
	)
	platformAwait(
		t,
		s,
		platformSubmit(
			t,
			s,
			platform.KindEnvironmentCreate,
			"app/identity",
			"identity-build-env",
			map[string]any{
				"name": "production", "node": "local", "container_port": 8080,
				"source": map[string]any{
					"url": "https://github.com/acme/app.git", "ref": commit,
					"builder": "paketobuildpacks/builder-jammy-base",
				},
				"domain": "identity.example.com", "domain_primary": true,
			},
		),
		contract.Succeeded,
	)
	previousWait, previousPoll := buildSlotWait, buildSlotPoll
	buildSlotWait, buildSlotPoll = 250*time.Millisecond, 25*time.Millisecond
	t.Cleanup(func() { buildSlotWait, buildSlotPoll = previousWait, previousPoll })
	deploy := platformSubmit(
		t,
		s,
		platform.KindDeploy,
		"env/identity/production",
		"identity-build-deploy",
		map[string]any{},
	)
	completed := platformAwait(t, s, deploy, contract.Succeeded)
	var image, imageID string
	if err := pool.QueryRow(ctx, `SELECT image,image_id FROM eregion_platform_releases WHERE operation_id=$1`, completed.ID).
		Scan(&image, &imageID); err != nil {
		t.Fatal(err)
	}
	if image != "127.0.0.1:5000/live@"+manifest {
		t.Fatalf("release image=%q, want the canonical manifest reference", image)
	}
	if imageID != config {
		t.Fatalf("release image_id=%q, want manifest config digest %s", imageID, config)
	}
	if imageID == runtimeID || imageID == manifest {
		t.Fatalf("release image_id conflated config with runtime/manifest identity: %q", imageID)
	}
	for _, step := range transport.runs {
		if step == "platform-pack-02-v1" || step == "platform-image-01-v1" {
			t.Fatalf("build executed an obsolete identity generation: %s", step)
		}
	}
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
