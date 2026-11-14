package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

type buildLockEvent struct {
	execution engine.Execution
	release   chan struct{}
	interrupt chan struct{}
	finished  <-chan struct{}
}

// buildLockTransport keeps real engine and validated step dispatch for Pack
// and the deployment network. Only external host effects are controlled; the
// remaining effects use the existing scripted deployment transport.
type buildLockTransport struct {
	*scriptedPlatformTransport
	engine      *engine.Engine
	entered     chan buildLockEvent
	script      string
	gateNetwork bool
}

type buildLockEffect struct {
	engine.Step
	execution   engine.Execution
	entered     chan buildLockEvent
	script      string
	finished    chan struct{}
	gateNetwork bool
	checked     chan struct{}
}

func (s *buildLockEffect) SharedHostResources() []string {
	return s.Step.(engine.SharedHostResources).SharedHostResources()
}

func (s *buildLockEffect) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	if s.checked != nil {
		s.checked <- struct{}{}
	}
	return engine.CheckResult{Detail: "controlled external effect"}, nil
}

func (s *buildLockEffect) Apply(ctx context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	event := buildLockEvent{execution: s.execution, finished: s.finished}
	if s.execution.Step == deploymentPackStep {
		event.release = make(chan struct{})
		event.interrupt = make(chan struct{})
	}
	if s.execution.Step == deploymentNetworkStep && s.gateNetwork {
		event.release = make(chan struct{})
	}
	select {
	case s.entered <- event:
	case <-ctx.Done():
		return engine.ApplyResult{}, ctx.Err()
	}
	if event.release != nil {
		select {
		case <-event.release:
		case <-event.interrupt:
			return engine.ApplyResult{}, engine.ErrInterrupted
		case <-ctx.Done():
			return engine.ApplyResult{}, ctx.Err()
		}
	}
	out, err := exec.CommandContext(ctx, s.script, s.execution.Step).Output() //nolint:gosec // G204: test-owned fixture executable.
	if err != nil {
		return engine.ApplyResult{}, err
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Result: result}, nil
}

func (t *buildLockTransport) Call(ctx context.Context, request node.Request, report func(node.Event)) (node.Response, error) {
	if request.Execution.Step != deploymentPackStep && request.Execution.Step != deploymentNetworkStep && request.Execution.Step != deploymentCommandStep {
		return t.scriptedPlatformTransport.Call(ctx, request, report)
	}
	var status engine.TaskStatus
	var err error
	if request.Method == node.MethodStatus {
		status, err = t.engine.Status(ctx, request.Execution)
	} else {
		var step engine.Step
		step, err = node.DefaultRegistry().Build(request.Execution, request.Payload)
		if err == nil {
			finished := make(chan struct{})
			defer close(finished)
			step = &buildLockEffect{Step: step, execution: request.Execution, entered: t.entered, script: t.script, finished: finished, gateNetwork: t.gateNetwork}
			status, err = t.engine.Run(ctx, step, request.Execution, nil)
		}
	}
	if err != nil && status.Status != engine.StatusFailed {
		return node.Response{}, err
	}
	return node.Response{
		Status: status.Status, ExecutionID: status.ExecutionID, Attempt: status.Attempt,
		AppliedAttempt: status.AppliedAttempt, InputsHash: status.InputsHash, Result: status.Result,
		Executed: status.Executed, Phase: status.Phase, WaitMS: status.WaitMS, Detail: status.Detail,
		ErrorCode: status.ErrorCode, Retryable: status.Retryable,
	}, nil
}

// TestIntegrationReleaseCommandSharesHostFence uses a real worker and engine;
// only the external Docker/Pack effects are replaced by a test-owned script.
func TestIntegrationReleaseCommandSharesHostFence(t *testing.T) {
	pool, _ := testPool(t, true)
	root := t.TempDir()
	eng, err := engine.New(root, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.LockTimeout = 30 * time.Second
	script := filepath.Join(t.TempDir(), "host-effect")
	result, err := json.Marshal(map[string]any{
		"reference": "127.0.0.1:5000/live@sha256:" + strings.Repeat("b", 64),
		"image_id":  "sha256:" + strings.Repeat("a", 64),
		"commit":    buildSlotCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := "#!/bin/sh\ncase \"$1\" in\n" + deploymentPackStep + ") printf '%s\\n' '" + string(result) + "' ;;\n*) printf '%s\\n' '{}' ;;\nesac\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	transport := &buildLockTransport{
		engine: eng, entered: make(chan buildLockEvent, 16), script: script, gateNetwork: true,
		scriptedPlatformTransport: &scriptedPlatformTransport{results: map[string]json.RawMessage{
			deploymentImageStep:      json.RawMessage(`{"stdout":"sha256:` + strings.Repeat("a", 64) + `"}`),
			deploymentReadyImageStep: json.RawMessage(`{"image":"127.0.0.1:5000/live@sha256:` + strings.Repeat("b", 64) + `","image_id":"sha256:` + strings.Repeat("a", 64) + `"}`),
		}},
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{Concurrency: 2, TestOnly: true, Transport: transport, RegistryEndpoint: "127.0.0.1:5000"})
	if err != nil {
		t.Fatal(err)
	}
	service := run.Service
	service.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	service.routeSync = func(context.Context) error { return nil }
	service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	platformAwait(t, service, platformSubmit(t, service, platform.KindNodeRegister, "platform", "shared-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}), contract.Succeeded)
	for _, app := range []string{"packapp", "commandapp"} {
		platformAwait(t, service, platformSubmit(t, service, platform.KindAppCreate, "platform", "app-"+app, map[string]any{"name": app}), contract.Succeeded)
		platformAwait(t, service, platformSubmit(t, service, platform.KindEnvironmentCreate, "app/"+app, "env-"+app, map[string]any{
			"name": "production", "node": "local", "container_port": 8080,
			"source": map[string]any{"url": "https://github.com/acme/app.git", "ref": buildSlotCommit, "builder": "paketobuildpacks/builder-jammy-base"},
			"domain": app + ".example.com", "domain_primary": true,
		}), contract.Succeeded)
	}
	release := seedArtifactRelease(t, service, "commandapp", "production", "registry.example/command:v1",
		withSeededReleaseImageID("sha256:"+strings.Repeat("a", 64)),
		withSeededReleaseCommand(platform.CommandSpec{Argv: []string{"/bin/true"}, TimeoutSeconds: 5}))
	next := func() buildLockEvent {
		t.Helper()
		select {
		case event := <-transport.entered:
			return event
		case <-time.After(30 * time.Second):
			t.Fatal("worker did not reach expected effect")
			return buildLockEvent{}
		}
	}
	command := platformSubmit(t, service, platform.KindDeploy, "env/commandapp/production", "command", map[string]any{"release_id": release.ID})
	network := next()
	if network.execution.OperationID != command.ID || network.execution.Step != deploymentNetworkStep {
		t.Fatalf("expected command deployment network, got %+v", network.execution)
	}
	defer func() {
		select {
		case <-network.release:
		default:
			close(network.release)
		}
	}()
	build := platformSubmit(t, service, platform.KindDeploy, "env/packapp/production", "build", map[string]any{})
	pack := next()
	if pack.execution.OperationID != build.ID || pack.execution.Step != deploymentPackStep {
		t.Fatalf("expected Pack effect, got %+v", pack.execution)
	}
	defer func() {
		select {
		case <-pack.release:
		default:
			close(pack.release)
		}
	}()
	if unlock, err := engine.Acquire(context.Background(), node.HostLockPath(root), engine.LockExclusive, 0); err == nil {
		unlock()
		t.Fatal("active Pack failed to fence an exclusive host mutation")
	} else if !errors.Is(err, engine.ErrLockHeld) {
		t.Fatal(err)
	}
	close(network.release)
	event := next()
	if event.execution.OperationID != command.ID || event.execution.Step != deploymentCommandStep {
		t.Fatalf("release command did not reach effect while Pack was active: %+v", event.execution)
	}
	platformAwait(t, service, command, contract.Succeeded)
	// Long valid app/environment names can truncate away deployment identity.
	app := strings.Repeat("a", 40)
	env1 := platform.Environment{App: app, Name: strings.Repeat("b", 39) + "c"}
	env2 := platform.Environment{App: app, Name: strings.Repeat("b", 39) + "d"}
	name := workloadCommandName(env1, platform.DeploymentID("dep_111111111111"))
	if other := workloadCommandName(env2, platform.DeploymentID("dep_222222222222")); name != other {
		t.Fatalf("expected truncated container-name collision: %q / %q", name, other)
	}
	payload := platform.ReleaseCommandPayload{
		Step: deploymentCommandStep, Name: name, Image: "sha256:" + strings.Repeat("c", 64),
		Network: "er-env-one", Command: []string{"/bin/true"}, TimeoutSeconds: 5,
	}
	colliding := platform.ReleaseCommandPayload{
		Step: deploymentCommandStep, Name: workloadCommandName(env2, platform.DeploymentID("dep_222222222222")),
		Image: "sha256:" + strings.Repeat("d", 64), Network: "er-env-two",
		Command: []string{"/bin/true"}, TimeoutSeconds: 5,
	}
	if payload.Name != colliding.Name || payload.Image == colliding.Image || payload.Network == colliding.Network {
		t.Fatal("collision fixture must share only the container identity")
	}
	for _, resource := range []string{"docker-container\x00" + colliding.Name, "docker-image\x00" + payload.Image, "docker-network\x00" + payload.Network} {
		t.Run(strings.Split(resource, "\x00")[0], func(t *testing.T) {
			// Only this key is held in a fresh engine, so a deferred result
			// proves contention on that exact identity, not a transient wait
			// on another resource in the sorted acquisition sequence.
			root := t.TempDir()
			eng, err := engine.New(root, "local", nil)
			if err != nil {
				t.Fatal(err)
			}
			eng.LockTimeout = 100 * time.Millisecond
			sum := sha256.Sum256([]byte(resource))
			unlock, err := engine.Acquire(context.Background(), filepath.Join(root, "resource-locks", hex.EncodeToString(sum[:])+".lock"), engine.LockExclusive, 0)
			if err != nil {
				t.Fatal(err)
			}
			held := true
			defer func() {
				if held {
					unlock()
				}
			}()
			id, err := contract.NewOperationID()
			if err != nil {
				t.Fatal(err)
			}
			execution := engine.Execution{OperationID: id, Attempt: 1, Step: deploymentCommandStep}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			step, err := node.DefaultRegistry().Build(execution, raw)
			if err != nil {
				t.Fatal(err)
			}
			checked := make(chan struct{}, 1)
			entered := make(chan buildLockEvent, 1)
			effect := &buildLockEffect{Step: step, execution: execution, entered: entered, script: script, checked: checked}
			status, err := eng.Run(context.Background(), effect, execution, nil)
			if !errors.Is(err, engine.ErrLockHeld) || status.Status != engine.StatusDeferred ||
				status.Detail != "exited while waiting for a resource lock before Check/Apply; no effect was attempted" {
				t.Fatalf("expected resource-lock deferral, got %+v, %v", status, err)
			}
			observation, err := eng.Status(context.Background(), execution)
			if err != nil || observation.Status != engine.StatusDeferred || observation.Detail != status.Detail {
				t.Fatalf("resource-lock deferral was not durable: %+v, %v", observation, err)
			}
			select {
			case <-checked:
				t.Fatal("Check ran while resource was locked")
			default:
			}
			select {
			case got := <-entered:
				t.Fatalf("Apply ran while resource was locked: %+v", got.execution)
			default:
			}
			unlock()
			held = false
			status, err = eng.Run(context.Background(), effect, execution, nil)
			if err != nil || status.Status != engine.StatusCompleted || !status.Executed {
				t.Fatalf("same effect did not complete after unlock: %+v, %v", status, err)
			}
			select {
			case <-checked:
			default:
				t.Fatal("retry did not run Check")
			}
			select {
			case got := <-entered:
				if got.execution != execution {
					t.Fatalf("unexpected effect after unlock: %+v", got.execution)
				}
			default:
				t.Fatal("retry did not run Apply")
			}
		})
	}
	close(pack.release)
	event = next()
	if event.execution.OperationID != build.ID || event.execution.Step != deploymentNetworkStep {
		t.Fatalf("expected build deployment network, got %+v", event.execution)
	}
	close(event.release)
	platformAwait(t, service, build, contract.Succeeded)
}

// TestIntegrationBuildLockCompatibility exercises actual service submissions,
// River workers, slot admission, node dispatch, durable steps and engine locks.
// The only external process is a test-owned script; Docker and Pack are never
// invoked.
func TestIntegrationBuildLockCompatibility(t *testing.T) {
	pool, _ := testPool(t, true)
	root := t.TempDir()
	eng, err := engine.New(root, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.LockTimeout = 30 * time.Second
	script := filepath.Join(t.TempDir(), "host-effect")
	result, err := json.Marshal(map[string]any{
		"reference": "127.0.0.1:5000/live@sha256:" + strings.Repeat("b", 64),
		"image_id":  "sha256:" + strings.Repeat("a", 64),
		"commit":    buildSlotCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := "#!/bin/sh\ncase \"$1\" in\n" + deploymentPackStep + ") printf '%s\\n' '" + string(result) + "' ;;\n" + deploymentNetworkStep + ") printf '%s\\n' '{}' ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	transport := &buildLockTransport{
		engine: eng, entered: make(chan buildLockEvent, 8), script: script,
		scriptedPlatformTransport: &scriptedPlatformTransport{results: map[string]json.RawMessage{
			deploymentImageStep:      json.RawMessage(`{"stdout":"sha256:` + strings.Repeat("a", 64) + `"}`),
			deploymentReadyImageStep: json.RawMessage(`{"image":"127.0.0.1:5000/live@sha256:` + strings.Repeat("b", 64) + `","image_id":"sha256:` + strings.Repeat("a", 64) + `"}`),
		}},
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{Concurrency: 2, TestOnly: true, Transport: transport, RegistryEndpoint: "127.0.0.1:5000"})
	if err != nil {
		t.Fatal(err)
	}
	service := run.Service
	service.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	service.routeSync = func(context.Context) error { return nil }
	service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	platformAwait(t, service, platformSubmit(t, service, platform.KindNodeRegister, "platform", "node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}), contract.Succeeded)
	if _, err := pool.Exec(context.Background(), `UPDATE eregion_platform_nodes SET build_concurrency=2 WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"liveone", "livetwo"} {
		platformAwait(t, service, platformSubmit(t, service, platform.KindAppCreate, "platform", "app-"+app, map[string]any{"name": app}), contract.Succeeded)
		platformAwait(t, service, platformSubmit(t, service, platform.KindEnvironmentCreate, "app/"+app, "env-"+app, map[string]any{
			"name": "production", "node": "local", "container_port": 8080,
			"source": map[string]any{"url": "https://github.com/acme/app.git", "ref": buildSlotCommit, "builder": "paketobuildpacks/builder-jammy-base"},
			"domain": app + ".example.com", "domain_primary": true,
		}), contract.Succeeded)
	}
	a := platformSubmit(t, service, platform.KindDeploy, "env/liveone/production", "deploy-one", map[string]any{})
	b := platformSubmit(t, service, platform.KindDeploy, "env/livetwo/production", "deploy-two", map[string]any{})
	next := func() buildLockEvent {
		t.Helper()
		select {
		case event := <-transport.entered:
			return event
		case <-time.After(30 * time.Second):
			t.Fatal("worker did not dispatch the expected host effect")
			return buildLockEvent{}
		}
	}
	first, second := next(), next()
	if first.execution.Step != deploymentPackStep || second.execution.Step != deploymentPackStep || first.execution.OperationID == second.execution.OperationID {
		t.Fatalf("independent Pack effects did not overlap: %+v / %+v", first.execution, second.execution)
	}
	if release, err := engine.Acquire(context.Background(), node.HostLockPath(root), engine.LockExclusive, 0); err == nil {
		release()
		t.Fatal("exclusive host mutation was not fenced by active Pack effects")
	} else if !errors.Is(err, engine.ErrLockHeld) {
		t.Fatalf("checking exclusive host fence: %v", err)
	}
	close(first.release)
	network := next()
	if network.execution.Step != deploymentNetworkStep || network.execution.OperationID != first.execution.OperationID {
		t.Fatalf("deployment network did not proceed while the other Pack remained active: %+v", network.execution)
	}
	close(second.release)
	network = next()
	if network.execution.Step != deploymentNetworkStep || network.execution.OperationID != second.execution.OperationID {
		t.Fatalf("second deployment network: %+v", network.execution)
	}
	platformAwait(t, service, a, contract.Succeeded)
	platformAwait(t, service, b, contract.Succeeded)

	// Hold the first environment's physical cache lock, submit a fresh deploy,
	// and prove its Pack cannot reach the host effect until the lock is released.
	cache := platform.BuildpackCacheVolume("liveone", "production")
	sum := sha256.Sum256([]byte("docker-volume\x00" + cache))
	cacheLock, err := engine.Acquire(context.Background(), filepath.Join(root, "resource-locks", hex.EncodeToString(sum[:])+".lock"), engine.LockExclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	cacheHeld := true
	defer func() {
		if cacheHeld {
			cacheLock()
		}
	}()
	c := platformSubmit(t, service, platform.KindDeploy, "env/liveone/production", "deploy-same-cache", map[string]any{})
	cacheExec := engine.Execution{OperationID: c.ID, Step: deploymentPackStep, Attempt: 1}
	deadline := time.After(20 * time.Second)
	for {
		journal, err := engine.LoadJournal(eng.JournalPath())
		if err != nil {
			t.Fatal(err)
		}
		task := journal.Tasks[cacheExec.EffectKey()]
		if task.Phase == engine.PhaseWaitingHostLock && task.Detail == "waiting for an exclusive resource lock" {
			break
		}
		select {
		case event := <-transport.entered:
			t.Fatalf("same-cache build reached its effect while the cache was locked: %+v", event.execution)
		case <-deadline:
			t.Fatal("worker did not reach the cache resource lock")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cacheLock()
	cacheHeld = false
	event := next()
	if event.execution.Step != deploymentPackStep || event.execution.OperationID != c.ID {
		t.Fatalf("waiting Pack did not resume: %+v", event.execution)
	}
	close(event.release)
	event = next()
	if event.execution.Step != deploymentNetworkStep || event.execution.OperationID != c.ID {
		t.Fatalf("waiting deployment network: %+v", event.execution)
	}
	platformAwait(t, service, c, contract.Succeeded)

	// A cancellation and interrupted Pack must not release another operation's
	// live host fence or alter its eventual successful deployment.
	d := platformSubmit(t, service, platform.KindDeploy, "env/liveone/production", "deploy-interrupted", map[string]any{})
	e := platformSubmit(t, service, platform.KindDeploy, "env/livetwo/production", "deploy-survivor", map[string]any{})
	x, y := next(), next()
	if x.execution.Step != deploymentPackStep || y.execution.Step != deploymentPackStep {
		t.Fatalf("interruption fixture did not start two builds: %+v / %+v", x.execution, y.execution)
	}
	var interrupted, survivor buildLockEvent
	if x.execution.OperationID == d.ID && y.execution.OperationID == e.ID {
		interrupted, survivor = x, y
	} else if y.execution.OperationID == d.ID && x.execution.OperationID == e.ID {
		interrupted, survivor = y, x
	} else {
		t.Fatalf("unexpected build operations: %+v / %+v", x.execution, y.execution)
	}
	if _, err := service.RequestCancel(context.Background(), testActor, d.ID); err != nil {
		t.Fatal(err)
	}
	close(interrupted.interrupt)
	select {
	case <-interrupted.finished:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted engine call did not release its locks")
	}
	deadline = time.After(10 * time.Second)
	for {
		journal, err := engine.LoadJournal(eng.JournalPath())
		if err != nil {
			t.Fatal(err)
		}
		if journal.Tasks[interrupted.execution.EffectKey()].Status == engine.TaskFailed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("interrupted Pack did not record its own failed effect")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if release, err := engine.Acquire(context.Background(), node.HostLockPath(root), engine.LockExclusive, 0); err == nil {
		release()
		t.Fatal("interrupted build released the surviving build's host fence")
	} else if !errors.Is(err, engine.ErrLockHeld) {
		t.Fatalf("checking survivor host fence: %v", err)
	}
	close(survivor.release)
	event = next()
	if event.execution.Step != deploymentNetworkStep || event.execution.OperationID != e.ID {
		t.Fatalf("surviving deployment did not reach its network: %+v", event.execution)
	}
	platformAwait(t, service, e, contract.Succeeded)

	// The exact environment network resource fence serializes network ensure
	// even when the host fence is otherwise shareable.
	environment, err := service.environment(context.Background(), "livetwo", "production")
	if err != nil {
		t.Fatal(err)
	}
	networkSum := sha256.Sum256([]byte("docker-network\x00" + platform.EnvironmentNetwork(environment.ID)))
	networkLock, err := engine.Acquire(context.Background(), filepath.Join(root, "resource-locks", hex.EncodeToString(networkSum[:])+".lock"), engine.LockExclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	networkHeld := true
	defer func() {
		if networkHeld {
			networkLock()
		}
	}()
	f := platformSubmit(t, service, platform.KindDeploy, "env/livetwo/production", "deploy-same-network", map[string]any{})
	event = next()
	if event.execution.Step != deploymentPackStep || event.execution.OperationID != f.ID {
		t.Fatalf("network contention fixture did not build: %+v", event.execution)
	}
	close(event.release)
	networkExec := engine.Execution{OperationID: f.ID, Step: deploymentNetworkStep, Attempt: 1}
	deadline = time.After(20 * time.Second)
	for {
		journal, err := engine.LoadJournal(eng.JournalPath())
		if err != nil {
			t.Fatal(err)
		}
		task := journal.Tasks[networkExec.EffectKey()]
		if task.Phase == engine.PhaseWaitingHostLock && task.Detail == "waiting for an exclusive resource lock" {
			break
		}
		select {
		case got := <-transport.entered:
			t.Fatalf("network effect reached a locked resource: %+v", got.execution)
		case <-deadline:
			t.Fatal("worker did not reach the network resource lock")
		case <-time.After(20 * time.Millisecond):
		}
	}
	networkLock()
	networkHeld = false
	event = next()
	if event.execution.Step != deploymentNetworkStep || event.execution.OperationID != f.ID {
		t.Fatalf("network ensure did not resume: %+v", event.execution)
	}
	platformAwait(t, service, f, contract.Succeeded)
}
