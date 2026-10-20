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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func platformSubmission(kind, target, key string, input any) operations.Submission {
	raw, _ := json.Marshal(input)
	return operations.Submission{Kind: kind, Target: target, Actor: testActor, IdempotencyKey: key, Input: raw}
}

func platformSubmit(t *testing.T, s *Service, kind, target, key string, input any) contract.Operation {
	t.Helper()
	op, err := s.Submit(context.Background(), platformSubmission(kind, target, key, input))
	if err != nil {
		t.Fatalf("submit %s: %v (%v)", kind, err, errors.Unwrap(err))
	}
	return op
}

func platformAwait(t *testing.T, s *Service, op contract.Operation, want contract.State) contract.Operation {
	t.Helper()
	await(t, 60*time.Second, func() bool {
		current, err := s.Get(context.Background(), testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		op = current
		return op.State.Terminal()
	})
	if op.State != want {
		page, _ := s.Events(context.Background(), testActor, contract.Cursor{OperationID: op.ID}, 200)
		raw, _ := json.Marshal(page)
		t.Fatalf("%s: got %s, want %s; events=%s", op.Kind, op.State, want, raw)
	}
	return op
}

// dropPlatformAck models a worker losing the response after the real node
// committed its effect. The next River attempt must recover status, not repeat
// the Docker switch. It retains the local transport's authenticated identity.
type dropPlatformAck struct {
	node.Transport
	mu         sync.Mutex
	dropped    bool
	switchRuns int
}

func (*dropPlatformAck) NodeIdentity() string { return "local" }
func (d *dropPlatformAck) Call(ctx context.Context, req node.Request, report func(node.Event)) (node.Response, error) {
	response, err := d.Transport.Call(ctx, req, report)
	if req.Method == node.MethodRun && req.Execution.Step == "platform-container-02-v1" {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.switchRuns++
		if !d.dropped && err == nil && response.Status == engine.StatusCompleted {
			d.dropped = true
			return node.Response{}, errors.New("injected response loss after durable Docker switch")
		}
	}
	return response, err
}

func TestIntegrationPlatformLocalDockerDeployRollbackRecovery(t *testing.T) {
	if os.Getenv("EREGION_TEST_DOCKER") != "1" {
		t.Skip("set EREGION_TEST_DOCKER=1 for real Docker deployment")
	}
	pool, _ := testPool(t, true)
	ctx := context.Background()
	for _, image := range []string{"caddy:2", "debian:bookworm-slim"} {
		if out, err := exec.Command("docker", "image", "inspect", image).CombinedOutput(); err != nil {
			t.Fatalf("local fixture %s: %v %s", image, err, out)
		}
	}
	stateDir := t.TempDir()
	local, err := node.NewLocal(stateDir, "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	fault := &dropPlatformAck{Transport: local}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{Concurrency: 2, TestOnly: true, Transport: fault})
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	s := run.Service
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "app", map[string]any{"name": "live"}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", "env", map[string]any{
		"name": "production", "node": "local", "container_port": 80, "health": map[string]any{"timeout_seconds": 2},
	}), contract.Succeeded)
	env, err := s.environment(ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Scoped to this test's unguessable environment ID, never other apps.
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=io.eregion.environment=production", "--filter", "label=io.eregion.app=live").Output()
		for _, id := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	})
	n, err := s.node(ctx, "local")
	if err != nil || !contains(n.Roles, platform.RoleControlPlane) || contains(n.Capabilities, platform.CapabilityRegistry) || contains(n.Capabilities, platform.CapabilityHTTPIngress) {
		t.Fatalf("unfactual local node: %+v %v", n, err)
	}
	createRelease := func(key, image string) platform.Release {
		op := platformSubmit(t, s, platform.KindReleaseCreate, "env/live/production", key, map[string]any{"source": map[string]any{"type": "image", "image": image}})
		platformAwait(t, s, op, contract.Succeeded)
		r, err := s.releaseForOperation(ctx, op.ID)
		if err != nil || !strings.HasPrefix(r.ImageID, "sha256:") {
			t.Fatalf("release=%+v err=%v", r, err)
		}
		return r
	}
	r1 := createRelease("release1", "caddy:2")
	deploy := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "deploy1", map[string]any{"release_id": r1.ID})
	op := platformAwait(t, s, deploy, contract.Succeeded)
	if op.Attempt < 2 {
		t.Fatal("response loss did not force worker recovery")
	}
	fault.mu.Lock()
	runs := fault.switchRuns
	fault.mu.Unlock()
	if runs != 1 {
		t.Fatalf("recovery repeated the switch %d times", runs)
	}
	assertServing := func(want platform.ReleaseID) {
		t.Helper()
		env, err = s.environment(ctx, "live", "production")
		if err != nil || env.ServingRelease != want || env.AppliedRelease != want {
			t.Fatalf("serving=%+v want=%s err=%v", env, want, err)
		}
		client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", env.HostPort))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("serving HTTP=%d", response.StatusCode)
		}
	}
	assertServing(r1.ID)
	// Idempotent replay is the same operation and never starts another effect.
	replay := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "deploy1", map[string]any{"release_id": r1.ID})
	if replay.ID != deploy.ID {
		t.Fatal("idempotent submission duplicated deployment")
	}
	bad := createRelease("badrelease", "debian:bookworm-slim")
	platformAwait(t, s, platformSubmit(t, s, platform.KindDeploy, "env/live/production", "baddeploy", map[string]any{"release_id": bad.ID}), contract.Failed)
	assertServing(r1.ID)
	if env.DesiredRelease != bad.ID {
		t.Fatal("failed desired state was lost")
	}
	// A second successful release may reference the same local image; rollback
	// still changes the durable release identity and running container labels.
	r2 := createRelease("release2", "caddy:2")
	platformAwait(t, s, platformSubmit(t, s, platform.KindDeploy, "env/live/production", "deploy2", map[string]any{"release_id": r2.ID}), contract.Succeeded)
	assertServing(r2.ID)
	platformAwait(t, s, platformSubmit(t, s, platform.KindRollback, "env/live/production", "rollback", map[string]any{}), contract.Succeeded)
	assertServing(r1.ID)
	label, err := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "io.eregion.release"}}`, env.ID.Container("serving")).Output()
	if err != nil || strings.TrimSpace(string(label)) != string(r1.ID) {
		t.Fatalf("running release label=%s err=%v", label, err)
	}
	changes, err := s.inventoryChanges(ctx, platform.InventoryQuery{App: "live", Environment: "production", Limit: 50})
	if err != nil || len(changes) < 8 {
		t.Fatalf("changes=%d err=%v", len(changes), err)
	}

	// Exercise the shared HTTP/client contract against real durable inventory.
	token := strings.Repeat("a", 64)
	tokenHash := sha256.Sum256([]byte(token))
	auth, err := api.NewAuth([]api.Principal{{Actor: testActor, TokenSHA256: hex.EncodeToString(tokenHash[:]), Scopes: []string{"operations:read", "operations:submit"}, ActionGrants: []api.ActionGrant{{Kind: platform.KindDeploy, Target: "env/live/production"}}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.NewHandler(auth, s, func(context.Context) error { return nil }))
	defer server.Close()
	client, err := api.NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PlatformInventory(ctx, "environments", url.Values{"app": {"live"}, "environment": {"production"}}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationPlatformAtomicAndTargetLock(t *testing.T) {
	pool, dsn := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	original := run.Service.enqueuePlatform
	run.Service.enqueuePlatform = func(ctx context.Context, tx pgx.Tx, op contract.Operation) (int64, error) {
		_, err := original(ctx, tx, op)
		if err != nil {
			return 0, err
		}
		return 0, errors.New("injected failure after job insertion")
	}
	_, err := run.Service.Submit(ctx, platformSubmission(platform.KindAppCreate, "platform", "atomic", map[string]any{"name": "atomic"}))
	if err == nil {
		t.Fatal("partial submission succeeded")
	}
	for _, table := range []string{"eregion_operations", "eregion_platform_apps", "eregion_operation_events", "river_job"} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s count=%d err=%v", table, count, err)
		}
	}
	otherPool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	other := &Service{pool: otherPool}
	for _, target := range []string{"env/live/production", "node/local", "platform"} {
		release, err := run.Service.lockTarget(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		unlock, err := other.lockTarget(blocked, target)
		cancel()
		if err == nil {
			unlock()
			release()
			t.Fatalf("target %s was not serialized across sessions", target)
		}
		release()
		unlock, err = other.lockTarget(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
}
