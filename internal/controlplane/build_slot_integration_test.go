package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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
	mu        sync.Mutex
	runs      []string
	results   map[string]json.RawMessage
	completed map[string]json.RawMessage
	hashes    map[string]string
}

func (*scriptedPlatformTransport) Name() string         { return "scripted" }
func (*scriptedPlatformTransport) NodeIdentity() string { return "local" }

func (s *scriptedPlatformTransport) Call(
	_ context.Context,
	request node.Request,
	_ func(node.Event),
) (node.Response, error) {
	key := string(request.Execution.OperationID) + "|" + request.Execution.Step
	if request.Method == node.MethodStatus {
		s.mu.Lock()
		result, done := s.completed[key]
		hash := s.hashes[key]
		s.mu.Unlock()
		if !done {
			return node.Response{
				Status:      engine.StatusAbsent,
				ExecutionID: request.Execution.ExecutionID(),
			}, nil
		}
		return node.Response{
			Status: engine.StatusCompleted, ExecutionID: request.Execution.ExecutionID(),
			InputsHash: hash, Executed: true, Result: result,
		}, nil
	}
	hash := scriptedInputsHash(request)
	s.mu.Lock()
	if s.results == nil {
		s.results = map[string]json.RawMessage{}
	}
	if s.completed == nil {
		s.completed = map[string]json.RawMessage{}
	}
	if s.hashes == nil {
		s.hashes = map[string]string{}
	}
	s.runs = append(s.runs, request.Execution.Step)
	result := s.results[request.Execution.Step]
	if result == nil {
		switch {
		case strings.HasPrefix(request.Execution.Step, deploymentContainerStepPrefix):
			result = json.RawMessage(`{"host_port":18080,"container_id":"candidate-container"}`)
		case strings.HasPrefix(request.Execution.Step, deploymentHealthStepPrefix):
			result = json.RawMessage(`{"healthy":true}`)
		}
	}
	s.completed[key] = result
	s.hashes[key] = hash
	s.mu.Unlock()
	return node.Response{
		Status:      engine.StatusCompleted,
		ExecutionID: request.Execution.ExecutionID(),
		InputsHash:  hash,
		Executed:    true,
		Result:      result,
	}, nil
}

func (s *scriptedPlatformTransport) runCount(step string) int { //nolint:unparam // The helper counts any requested step.
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

func (s *scriptedPlatformTransport) totalRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
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
		name, _ := node["name"].(string)
		if _, err := submit(
			"node-"+name,
			platform.KindNodeRegister,
			"platform",
			node,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := submit(
		"app",
		platform.KindAppCreate,
		"platform",
		map[string]any{"name": "live"},
	); err != nil {
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

func assertBuildSlotEvent(t *testing.T, ctx context.Context, s *Service, id contract.OperationID, fragment string) {
	t.Helper()
	page, err := s.Events(ctx, testActor, contract.Cursor{OperationID: id}, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if strings.Contains(event.Message, fragment) {
			return
		}
	}
	t.Fatalf("no %s event was emitted: %+v", fragment, page.Events)
}
