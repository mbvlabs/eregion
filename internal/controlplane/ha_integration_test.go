package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// outageProxy is a loopback TCP forwarder used only by this test. It can drop
// every current connection and refuse new ones, which makes PostgreSQL
// unreachable to the control plane without stopping the external test server or
// touching other databases. Plain TCP passthrough preserves the real
// PostgreSQL wire protocol/TLS; no SQL is parsed or rewritten.
type outageProxy struct {
	target string
	addr   string

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	down     bool
}

func newOutageProxy(t *testing.T, targetDSN string) *outageProxy {
	t.Helper()
	parsed, err := url.Parse(targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &outageProxy{target: parsed.Host, addr: listener.Addr().String(), listener: listener, conns: map[net.Conn]struct{}{}}
	go proxy.serve(listener)
	t.Cleanup(func() { proxy.close() })
	return proxy
}

func (p *outageProxy) dsn(base string) string {
	parsed, _ := url.Parse(base)
	parsed.Host = p.addr
	return parsed.String()
}

func (p *outageProxy) serve(listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.down {
			p.mu.Unlock()
			_ = client.Close()
			continue
		}
		p.conns[client] = struct{}{}
		p.mu.Unlock()
		go p.forward(client)
	}
}

func (p *outageProxy) forward(client net.Conn) {
	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		p.forget(client)
		_ = client.Close()
		return
	}
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		p.forget(client)
		_ = upstream.Close()
		_ = client.Close()
		return
	}
	p.conns[upstream] = struct{}{}
	p.mu.Unlock()
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	p.forget(client)
	p.forget(upstream)
}

func (p *outageProxy) forget(conn net.Conn) {
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
}

// down closes the listening socket and every live connection. The control
// plane sees connection resets and refused new connections, as it would during
// a database restart or network partition.
func (p *outageProxy) downNow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	if p.listener != nil {
		_ = p.listener.Close()
		p.listener = nil
	}
	for conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// up restores the same address so a pool reconnects without reconfiguration.
func (p *outageProxy) up() error {
	listener, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.down = false
	p.listener = listener
	p.mu.Unlock()
	go p.serve(listener)
	return nil
}

func (p *outageProxy) close() {
	p.downNow()
}

// TestIntegrationDatabaseOutageRecoversWithoutDuplicates runs one control plane
// against a connection-level proxy in front of the shared test PostgreSQL. It
// drops the database mid-operation, verifies the operation stays visible and
// non-terminal, reconnects, and requires exactly one effect per step.
func TestIntegrationDatabaseOutageRecoversWithoutDuplicates(t *testing.T) {
	directPool, dsn := testPool(t, true)
	proxy := newOutageProxy(t, dsn)
	keyFile := writeKeyFile(t)
	nodeDir := t.TempDir()
	ctx := context.Background()
	cp := startControlPlane(t, proxy.dsn(dsn), keyFile, nodeDir, 2)

	const steps = 8
	op, err := cp.run.Service.Submit(ctx, submission("database-outage", fmt.Sprintf(`{"steps":%d,"step_delay_ms":150}`, steps)))
	if err != nil {
		t.Fatal(err)
	}
	await(t, 20*time.Second, func() bool {
		var completed int
		err := directPool.QueryRow(ctx, `SELECT completed_steps FROM eregion_operations WHERE id=$1`, op.ID).Scan(&completed)
		return err == nil && completed >= 1
	})

	proxy.downNow()
	// The database is unreachable to the control plane, but the operation row is
	// still visible through the direct test connection and has not been marked
	// terminal or lost.
	await(t, 5*time.Second, func() bool {
		var state string
		err := directPool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, op.ID).Scan(&state)
		return err == nil && !contract.State(state).Terminal()
	})
	time.Sleep(750 * time.Millisecond)
	if err := proxy.up(); err != nil {
		t.Fatal(err)
	}

	await(t, 90*time.Second, func() bool {
		var state string
		err := directPool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, op.ID).Scan(&state)
		return err == nil && contract.State(state) == contract.Succeeded
	})
	if got := effectCount(t, nodeDir, op.ID, steps); got != steps {
		t.Fatalf("node effects after database outage: %d, want %d", got, steps)
	}
	if got := completedStepRows(t, directPool, op.ID); got != steps {
		t.Fatalf("completed step rows after database outage: %d, want %d", got, steps)
	}
	if got := progressEventCount(t, cp.run.Service, op.ID); got != steps {
		t.Fatalf("progress events after database outage: %d, want %d", got, steps)
	}
}

// scriptedDeployTransport models a node that commits a container switch and
// then disappears before answering. It records one effect per operation/step,
// returns the authoritatively completed status when it becomes reachable again,
// and never runs the same effect twice.
type scriptedDeployTransport struct {
	mu        sync.Mutex
	runs      map[string]int
	down      map[string]int
	hashes    map[string]string
	lostStep  string
	lostForOp contract.OperationID
}

func newScriptedDeployTransport() *scriptedDeployTransport {
	return &scriptedDeployTransport{runs: map[string]int{}, down: map[string]int{}, hashes: map[string]string{}}
}

// scriptedInputsHash is the hash a real node journals for a run: the step
// built from the request payload, the same way the worker computes it.
func scriptedInputsHash(request node.Request) string {
	step, err := node.DefaultRegistry().Build(request.Execution, request.Payload)
	if err != nil {
		return ""
	}
	inputs, err := step.Inputs()
	if err != nil {
		return ""
	}
	return engine.InputsHash(step.ID(), inputs)
}

func (*scriptedDeployTransport) Name() string         { return "scripted-deploy" }
func (*scriptedDeployTransport) NodeIdentity() string { return "local" }

func (s *scriptedDeployTransport) lose(step string, op contract.OperationID) {
	s.mu.Lock()
	s.lostStep, s.lostForOp = step, op
	s.mu.Unlock()
}

func stepKey(request node.Request) string {
	return string(request.Execution.OperationID) + "|" + request.Execution.Step
}

func scriptedResult(request node.Request) json.RawMessage {
	if strings.HasPrefix(request.Execution.Step, "platform-port-") {
		return json.RawMessage(`{"stdout":"127.0.0.1:18080"}`)
	}
	if request.Execution.Step != "platform-image-01-v1" {
		return json.RawMessage(`{}`)
	}
	var payload platform.ArgvPayload
	_ = json.Unmarshal(request.Payload, &payload)
	image := ""
	if len(payload.Argv) > 0 {
		image = payload.Argv[len(payload.Argv)-1]
	}
	sum := sha256.Sum256([]byte(image))
	result, _ := json.Marshal(map[string]string{"stdout": "sha256:" + hex.EncodeToString(sum[:])})
	return result
}

func (s *scriptedDeployTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	key := stepKey(request)
	switch request.Method {
	case node.MethodStatus:
		s.mu.Lock()
		runs := s.runs[key]
		hash := s.hashes[key]
		failures := s.down[key]
		if failures > 0 {
			s.down[key] = failures - 1
		}
		s.mu.Unlock()
		if failures > 0 {
			return node.Response{}, errors.New("node is unreachable")
		}
		if runs > 0 {
			// Like the node journal, status reports the inputs hash recorded by
			// the run; a status request carries no payload.
			return node.Response{
				Status: engine.StatusCompleted, ExecutionID: request.Execution.ExecutionID(),
				InputsHash: hash, Executed: true, Result: scriptedResult(request),
			}, nil
		}
		return node.Response{Status: engine.StatusAbsent, ExecutionID: request.Execution.ExecutionID()}, nil
	case node.MethodRun:
		hash := scriptedInputsHash(request)
		s.mu.Lock()
		s.runs[key]++
		s.hashes[key] = hash
		first := s.runs[key] == 1
		lost := first && request.Execution.Step == s.lostStep && request.Execution.OperationID == s.lostForOp
		if lost {
			s.down[key] = 1
		}
		s.mu.Unlock()
		if lost {
			return node.Response{}, errors.New("node lost after committing the effect")
		}
		return node.Response{
			Status: engine.StatusCompleted, ExecutionID: request.Execution.ExecutionID(),
			InputsHash: hash, Executed: true, Result: scriptedResult(request),
		}, nil
	default:
		return node.Response{}, fmt.Errorf("unexpected method %q", request.Method)
	}
}

func (s *scriptedDeployTransport) runCount(op contract.OperationID, step string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[string(op)+"|"+step]
}

func latestRelease(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM eregion_platform_releases ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func servingRelease(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id *string
	if err := pool.QueryRow(context.Background(), `SELECT e.serving_release_id
        FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='live' AND e.name='production'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id == nil {
		return ""
	}
	return *id
}

// TestIntegrationNodeLossDuringDeploy exercises a node that commits the
// candidate container and then loses the transport. The operation stays
// visible, a retry asks status instead of repeating the effect, and the
// environment does not report the new release as serving until verification
// committed.
func TestIntegrationNodeLossDuringDeploy(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	fake := newScriptedDeployTransport()
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{Concurrency: 2, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// This test exercises container effect recovery, not Caddy; the test host
	// has no routes file or admin endpoint.
	run.Service.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	run.Service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	startWorkers(t, run)
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "ha-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "ha-app", map[string]any{"name": "live"}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", "ha-env", map[string]any{
		"name": "production", "node": "local", "container_port": 80, "health": map[string]any{"timeout_seconds": 2},
		"domain": "live.example.com", "domain_primary": true,
	}), contract.Succeeded)

	createRelease := func(key, image string) string {
		platformAwait(t, s, platformSubmit(t, s, platform.KindReleaseCreate, "env/live/production", key, map[string]any{
			"source": map[string]any{"type": "image", "image": image},
		}), contract.Succeeded)
		return latestRelease(t, pool)
	}
	first := createRelease("ha-release-1", "registry.example/live:v1")
	platformAwait(t, s, platformSubmit(t, s, platform.KindDeploy, "env/live/production", "ha-deploy-1", map[string]any{"release_id": first}), contract.Succeeded)
	if servingRelease(t, pool) != first {
		t.Fatalf("first deploy did not become serving: %q", servingRelease(t, pool))
	}

	second := createRelease("ha-release-2", "registry.example/live:v2")
	deploy := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "ha-deploy-2", map[string]any{"release_id": second})
	containerStep := containerStepID("web", "1")
	fake.lose(containerStep, deploy.ID)

	// Wait until the node committed the candidate container but the worker has
	// not yet acknowledged it. Serving must still name the old release.
	await(t, 30*time.Second, func() bool {
		return fake.runCount(deploy.ID, containerStep) > 0
	})
	current, err := s.Get(ctx, testActor, deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("deploy became terminal during node loss: %+v", current)
	}
	if got := servingRelease(t, pool); got != first {
		t.Fatalf("serving release changed before verification: %q, want %q", got, first)
	}

	platformAwait(t, s, deploy, contract.Succeeded)
	if got := fake.runCount(deploy.ID, containerStep); got != 1 {
		t.Fatalf("candidate container ran %d times; a retry duplicated the effect", got)
	}
	if got := servingRelease(t, pool); got != second {
		t.Fatalf("serving release after verified deploy = %q, want %q", got, second)
	}
}

// TestIntegrationCompetingSubmissionsAcrossControlPlanes submits the same
// idempotency key through two complete control planes at once. One durable
// operation and one set of node effects must result.
func TestIntegrationCompetingSubmissionsAcrossControlPlanes(t *testing.T) {
	_, dsn := testPool(t, true)
	keyFile := writeKeyFile(t)
	nodeDir := t.TempDir()
	ctx := context.Background()
	first := startControlPlane(t, dsn, keyFile, nodeDir, 1)
	second := startControlPlane(t, dsn, keyFile, nodeDir, 1)

	const steps = 4
	var wait sync.WaitGroup
	ids := make(chan contract.OperationID, 2)
	errs := make(chan error, 2)
	for _, service := range []operations.Service{first.run.Service, second.run.Service} {
		wait.Add(1)
		go func(service operations.Service) {
			defer wait.Done()
			op, err := service.Submit(ctx, submission("competing-control-planes", fmt.Sprintf(`{"steps":%d,"step_delay_ms":100}`, steps)))
			if err != nil {
				errs <- err
				return
			}
			ids <- op.ID
		}(service)
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var id contract.OperationID
	for candidate := range ids {
		if id == "" {
			id = candidate
		} else if candidate != id {
			t.Fatalf("competing submissions created different operations: %s and %s", id, candidate)
		}
	}
	await(t, 30*time.Second, func() bool {
		var state string
		err := first.pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, id).Scan(&state)
		return err == nil && contract.State(state) == contract.Succeeded
	})
	var operations int
	if err := first.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key='competing-control-planes'`).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("competing submissions produced %d operations: %v", operations, err)
	}
	if got := effectCount(t, nodeDir, id, steps); got != steps {
		t.Fatalf("competing submissions produced %d effects, want %d", got, steps)
	}
	if !strings.HasPrefix(string(id), "op_") {
		t.Fatalf("unexpected operation id %q", id)
	}
}
