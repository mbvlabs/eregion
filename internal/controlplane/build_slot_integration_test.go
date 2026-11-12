package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// buildSlotCommit is an exact Git commit, so the fixture never reads a remote
// repository to submit a build.
const buildSlotCommit = "1234567890abcdef1234567890abcdef12345678"

// scriptedPlatformTransport completes platform node steps without Docker or
// pack. It implements the authenticated identity so registered-node placement
// still has to match the transport.
type scriptedPlatformTransport struct {
	mu      sync.Mutex
	runs    []string
	results map[string]json.RawMessage
}

func (*scriptedPlatformTransport) Name() string         { return "scripted" }
func (*scriptedPlatformTransport) NodeIdentity() string { return "local" }

func (s *scriptedPlatformTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	if request.Method == node.MethodStatus {
		return node.Response{}, nil
	}
	s.mu.Lock()
	s.runs = append(s.runs, request.Execution.Step)
	result := s.results[request.Execution.Step]
	s.mu.Unlock()
	if result == nil {
		switch {
		case strings.HasPrefix(request.Execution.Step, deploymentContainerStepPrefix):
			result = json.RawMessage(`{"host_port":18080,"container_id":"candidate-container"}`)
		case strings.HasPrefix(request.Execution.Step, deploymentHealthStepPrefix):
			result = json.RawMessage(`{"healthy":true}`)
		}
	}
	return node.Response{
		Status:      engine.StatusCompleted,
		ExecutionID: request.Execution.ExecutionID(),
		Result:      result,
	}, nil
}

func (s *scriptedPlatformTransport) runCount(step string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, run := range s.runs {
		if run == step {
			count++
		}
	}
	return count
}

func TestIntegrationEnvironmentBuildNodeIsValidatedAndGated(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	// Model an insert-only API process: placement facts are recorded, and the
	// worker re-checks them with an authenticated transport before dispatch.
	s.transport = nil
	ctx := context.Background()
	submit := func(key, kind, target string, input any) (contract.Operation, error) {
		return s.Submit(ctx, platformSubmission(kind, target, key, input))
	}
	for _, node := range []map[string]any{
		{"name": "app-1", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker}},
		{"name": "build-1", "roles": []string{platform.RoleBuilder}, "capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}},
		{"name": "app-2", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker}},
	} {
		if _, err := submit("node-"+node["name"].(string), platform.KindNodeRegister, "platform", node); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := submit("app", platform.KindAppCreate, "platform", map[string]any{"name": "live"}); err != nil {
		t.Fatal(err)
	}
	if _, err := submit("bad-env", platform.KindEnvironmentCreate, "app/live", map[string]any{
		"name": "staging", "node": "app-1", "build_node": "app-2",
	}); err == nil {
		t.Fatal("a build node without builder/buildpacks was accepted")
	} else if reason, ok := IsPlacementViolation(err); !ok || reason != ReasonBuilderRequired {
		t.Fatalf("build node refusal=%v reason=%q", err, reason)
	}
	if _, err := submit("env", platform.KindEnvironmentCreate, "app/live", map[string]any{
		"name": "production", "node": "app-1", "build_node": "build-1", "container_port": 8080,
		"source": map[string]any{
			"url": "https://github.com/acme/app.git", "ref": buildSlotCommit,
			"builder": "paketobuildpacks/builder-jammy-base",
		},
		"domain": "live.example.com", "domain_primary": true,
	}); err != nil {
		t.Fatal(err)
	}
	env, err := s.environment(ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	if env.Node != "app-1" || env.BuildNode != "build-1" || env.BuildNodeName() != "build-1" {
		t.Fatalf("placement was not persisted: %+v", env)
	}
	_, err = submit("build", platform.KindDeploy, "env/live/production", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "shared registry") {
		t.Fatalf("a distinct build node was pretended to work: %v", err)
	}
}

func TestIntegrationReleaseBuildSlotQueuesAndRuns(t *testing.T) {
	pool, _ := testPool(t, true)
	transport := &scriptedPlatformTransport{results: map[string]json.RawMessage{
		deploymentPackStep:  json.RawMessage(`{"reference":"127.0.0.1:5000/live@sha256:` + strings.Repeat("b", 64) + `","image_id":"sha256:` + strings.Repeat("a", 64) + `","commit":"` + buildSlotCommit + `"}`),
		deploymentImageStep: json.RawMessage(`{"stdout":"sha256:` + strings.Repeat("a", 64) + `"}`),
	}}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{Concurrency: 2, TestOnly: true, Transport: transport, RegistryEndpoint: "127.0.0.1:5000"})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	// The supported Git deploy path builds before deploying; replace only the
	// host route effects so this fixture exercises build-slot serialization.
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.routeSync = func(context.Context) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	ctx := context.Background()
	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "build-agent", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "build-app", map[string]any{"name": "live"}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", "build-env", map[string]any{
		"name": "production", "node": "local", "container_port": 8080,
		"source": map[string]any{
			"url": "https://github.com/acme/app.git", "ref": buildSlotCommit,
			"builder": "paketobuildpacks/builder-jammy-base",
		},
		"domain": "live.example.com", "domain_primary": true,
	}), contract.Succeeded)

	previousWait, previousPoll := buildSlotWait, buildSlotPoll
	buildSlotWait, buildSlotPoll = 250*time.Millisecond, 25*time.Millisecond
	t.Cleanup(func() { buildSlotWait, buildSlotPoll = previousWait, previousPoll })

	held, err := s.acquireBuildSlot(ctx, "local", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	releaseHeld := func() {
		if released {
			return
		}
		released = true
		held()
	}
	// Cleanup runs before the pool is closed, even after a fatal assertion.
	t.Cleanup(releaseHeld)
	blocked := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "build-blocked", map[string]any{})
	blocked = platformAwait(t, s, blocked, contract.Failed)
	if blocked.ErrorCode != contract.PreconditionFailed {
		t.Fatalf("capacity failure code=%s", blocked.ErrorCode)
	}
	page, err := s.Events(ctx, testActor, contract.Cursor{OperationID: blocked.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	queued := false
	for _, event := range page.Events {
		if strings.Contains(event.Message, "build_queued") {
			queued = true
		}
	}
	if !queued {
		t.Fatalf("no build_queued event was emitted: %+v", page.Events)
	}
	if got := transport.runCount(deploymentPackStep); got != 0 {
		t.Fatalf("a build ran without a slot: %d", got)
	}
	releaseHeld()

	ready := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "build-ready", map[string]any{})
	ready = platformAwait(t, s, ready, contract.Succeeded)
	release, err := s.releaseForOperation(ctx, ready.ID)
	if err != nil || release.Status != platform.ReleaseReady || !strings.HasPrefix(release.ImageID, "sha256:") {
		t.Fatalf("release=%+v err=%v", release, err)
	}
	if !platform.IsImageDigestReference(release.Image) || !strings.HasPrefix(release.Image, "127.0.0.1:5000/live@sha256:") {
		t.Fatalf("release image = %q, want the canonical registry digest reference", release.Image)
	}
	if got := transport.runCount(deploymentPackStep); got != 1 {
		t.Fatalf("pack ran %d times", got)
	}
	page, err = s.Events(ctx, testActor, contract.Cursor{OperationID: ready.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	running := false
	for _, event := range page.Events {
		if strings.Contains(event.Message, "build_running") {
			running = true
		}
	}
	if !running {
		t.Fatalf("no build_running event was emitted: %+v", page.Events)
	}
}
