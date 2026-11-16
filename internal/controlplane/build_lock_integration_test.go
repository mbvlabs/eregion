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
	"github.com/mbvlabs/eregion/internal/platform"
)

// buildSlotGate holds one Pack or network step until the test releases it, so
// overlap is ordered without host processes.
type buildSlotGate struct {
	execution engine.Execution
	release   chan struct{}
}

// buildSlotTransport is the narrow external-effect seam for build slot
// concurrency. Pack and network steps return typed results; PostgreSQL, slot
// admission, River workers and deployment state remain real.
type buildSlotTransport struct {
	*scriptedPlatformTransport
	entered chan buildSlotGate
}

func (t *buildSlotTransport) Call(
	ctx context.Context,
	request node.Request,
	report func(node.Event),
) (node.Response, error) {
	if request.Method == node.MethodRun &&
		(request.Execution.Step == deploymentPackStep ||
			request.Execution.Step == deploymentNetworkStep) {
		release := make(chan struct{})
		select {
		case t.entered <- buildSlotGate{execution: request.Execution, release: release}:
		case <-ctx.Done():
			return node.Response{}, ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return node.Response{}, ctx.Err()
		}
	}
	return t.scriptedPlatformTransport.Call(ctx, request, report)
}

func releaseBuildSlot(gate buildSlotGate) {
	select {
	case <-gate.release:
	default:
		close(gate.release)
	}
}

// TestIntegrationBuildSlotConcurrency exercises actual service submissions,
// River workers, slot admission and deployment state. Two independent Pack
// steps must overlap on a node with two build slots; releasing one must let its
// network ensure proceed while the other Pack remains in flight.
func TestIntegrationBuildSlotConcurrency(t *testing.T) {
	pool, _ := testPool(t, true)
	packResult, err := json.Marshal(map[string]any{
		"reference":  "127.0.0.1:5000/live@sha256:" + strings.Repeat("b", 64),
		"image_id":   "sha256:" + strings.Repeat("a", 64),
		"runtime_id": "sha256:" + strings.Repeat("a", 64),
		"commit":     buildSlotCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := &buildSlotTransport{
		entered: make(chan buildSlotGate, 8),
		scriptedPlatformTransport: &scriptedPlatformTransport{results: map[string]json.RawMessage{
			deploymentPackStep: packResult,
			deploymentImageStep: json.RawMessage(
				`{"stdout":"sha256:` + strings.Repeat("a", 64) + `"}`,
			),
			deploymentReadyImageStep: json.RawMessage(
				`{"image":"127.0.0.1:5000/live@sha256:` + strings.Repeat("b", 64) +
					`","image_id":"sha256:` + strings.Repeat("a", 64) + `"}`,
			),
		}},
	}
	run, err := New(
		newTestEntities(t, pool),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WorkerConfig{
			Concurrency:      2,
			TestOnly:         true,
			Transport:        transport,
			RegistryEndpoint: "127.0.0.1:5000",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	service := run.Service
	service.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	service.routeSync = func(context.Context) error { return nil }
	service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	platformAwait(
		t,
		service,
		platformSubmit(t, service, platform.KindNodeRegister, "platform", "node", map[string]any{
			"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
			"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
		}),
		contract.Succeeded,
	)
	if _, err := pool.Exec(
		context.Background(),
		`UPDATE eregion_platform_nodes SET build_concurrency=2 WHERE name='local'`,
	); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"liveone", "livetwo"} {
		platformAwait(
			t,
			service,
			platformSubmit(
				t,
				service,
				platform.KindAppCreate,
				"platform",
				"app-"+app,
				map[string]any{"name": app},
			),
			contract.Succeeded,
		)
		platformAwait(
			t,
			service,
			platformSubmit(
				t,
				service,
				platform.KindEnvironmentCreate,
				"app/"+app,
				"env-"+app,
				map[string]any{
					"name":           "production",
					"node":           "local",
					"container_port": 8080,
					"source": map[string]any{
						"url":     "https://github.com/acme/app.git",
						"ref":     buildSlotCommit,
						"builder": "paketobuildpacks/builder-jammy-base",
					},
					"domain":         app + ".example.com",
					"domain_primary": true,
				},
			),
			contract.Succeeded,
		)
	}
	a := platformSubmit(
		t,
		service,
		platform.KindDeploy,
		"env/liveone/production",
		"deploy-one",
		map[string]any{},
	)
	b := platformSubmit(
		t,
		service,
		platform.KindDeploy,
		"env/livetwo/production",
		"deploy-two",
		map[string]any{},
	)
	next := func() buildSlotGate {
		t.Helper()
		select {
		case gate := <-transport.entered:
			return gate
		case <-time.After(30 * time.Second):
			t.Fatal("worker did not dispatch the expected build step")
			return buildSlotGate{}
		}
	}
	first, second := next(), next()
	defer releaseBuildSlot(first)
	defer releaseBuildSlot(second)
	if first.execution.Step != deploymentPackStep || second.execution.Step != deploymentPackStep ||
		first.execution.OperationID == second.execution.OperationID {
		t.Fatalf("independent Pack effects did not overlap: %+v / %+v", first.execution, second.execution)
	}
	releaseBuildSlot(first)
	network := next()
	if network.execution.Step != deploymentNetworkStep ||
		network.execution.OperationID != first.execution.OperationID {
		t.Fatalf("first deployment network did not proceed while the other Pack remained active: %+v", network.execution)
	}
	releaseBuildSlot(network)
	releaseBuildSlot(second)
	network = next()
	if network.execution.Step != deploymentNetworkStep ||
		network.execution.OperationID != second.execution.OperationID {
		t.Fatalf("second deployment network: %+v", network.execution)
	}
	releaseBuildSlot(network)
	platformAwait(t, service, a, contract.Succeeded)
	platformAwait(t, service, b, contract.Succeeded)
}
