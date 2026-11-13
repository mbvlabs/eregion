package controlplane

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// blockingResourceTransport answers resource steps without Docker and holds one
// selected run step until the test releases it with a cancellation error.
type blockingResourceTransport struct {
	base resourceTestTransport

	mu        sync.Mutex
	blockStep string
	started   chan struct{}
	release   chan error
	once      sync.Once
}

func newBlockingResourceTransport(step string) *blockingResourceTransport {
	return &blockingResourceTransport{
		blockStep: step,
		started:   make(chan struct{}),
		release:   make(chan error, 1),
	}
}

func (t *blockingResourceTransport) Name() string         { return "blocking-resource-test" }
func (t *blockingResourceTransport) NodeIdentity() string { return "local" }

func (t *blockingResourceTransport) setBlockStep(step string) {
	t.mu.Lock()
	t.blockStep = step
	t.mu.Unlock()
}

func (t *blockingResourceTransport) Call(ctx context.Context, request node.Request, emit func(node.Event)) (node.Response, error) {
	t.mu.Lock()
	blockStep := t.blockStep
	t.mu.Unlock()
	if request.Method == node.MethodRun && blockStep != "" && strings.Contains(request.Execution.Step, blockStep) {
		t.once.Do(func() { close(t.started) })
		if err := <-t.release; err != nil {
			return node.Response{}, err
		}
	}
	return t.base.Call(ctx, request, emit)
}

func TestIntegrationResourceCancelSettlesStateAndAllowsRetryAndDestroy(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	fake := newBlockingResourceTransport("-run-")
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		RegistryEndpoint: "127.0.0.1:5000",
		SecretResolver:   newSnapshotSecretResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "cancel-resource-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "cancel-resource-app", map[string]any{
		"name": "cancelapi",
		"environments": []map[string]any{
			{"name": "pr", "node": "local", "container_port": 8080, "health": map[string]any{"timeout_seconds": 5}},
			{"name": "pr2", "node": "local", "container_port": 8080, "health": map[string]any{"timeout_seconds": 5}},
		},
	}), contract.Succeeded)

	spec := map[string]any{
		"image":  "postgres:17",
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready"}},
	}
	apply := platformSubmit(t, s, platform.KindResourceApply, "resource/cancelapi/pr/db", "resource-cancel-apply", spec)
	select {
	case <-fake.started:
	case <-time.After(20 * time.Second):
		t.Fatal("resource run step did not start")
	}
	var state, volumes string
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='cancelapi' AND r.name='db'`).Scan(&state, &volumes); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceProvisioning) || !strings.Contains(volumes, "er-res-") {
		t.Fatalf("before cancellation: state=%s volumes=%s", state, volumes)
	}
	if _, err := s.RequestCancel(ctx, testActor, apply.ID); err != nil {
		t.Fatal(err)
	}
	// Let the in-flight step report a safe-boundary cancellation, exactly as a
	// worker does after seeing the cancelling operation.
	fake.setBlockStep("")
	fake.release <- errPlatformCancelled
	cancelled := platformAwait(t, s, apply, contract.Canceled)
	if cancelled.ID != apply.ID {
		t.Fatalf("cancelled operation = %s", cancelled.ID)
	}
	var errorCode string
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text,error_code FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='cancelapi' AND r.name='db'`).Scan(&state, &volumes, &errorCode); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceDegraded) || errorCode != string(contract.Cancelled) {
		t.Fatalf("after cancellation: state=%s error=%s", state, errorCode)
	}
	if !strings.Contains(volumes, "er-res-") {
		t.Fatalf("cancellation lost the volume ledger: %s", volumes)
	}
	retainedVolumes := volumes

	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceApply, "resource/cancelapi/pr/db", "resource-cancel-retry", spec), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='cancelapi' AND r.name='db'`).Scan(&state, &volumes); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceReady) || volumes != retainedVolumes {
		t.Fatalf("retry: state=%s volumes=%s want %s", state, volumes, retainedVolumes)
	}
	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceDestroy, "resource/cancelapi/pr/db", "resource-cancel-destroy", map[string]any{
		"confirm": "cancelapi/pr/db",
	}), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='cancelapi' AND r.name='db'`).Scan(&state, &volumes); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceDestroyed) || volumes != retainedVolumes {
		t.Fatalf("destroy after cancel: state=%s volumes=%s", state, volumes)
	}
	// A destroyed resource no longer blocks the environment deletion guard.
	// The full delete command is not exercised here because this fake node has
	// no Caddy routes file; the resource guard is the state this bug affected.
	var appID string
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name='cancelapi'`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ensureNoLiveResourcesTx(ctx, tx, "cancelapi", "", appID, ""); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("destroyed resource still blocks environment deletion: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationResourceCancelAfterSuccessfulStepIsHonored covers a
// cancellation that arrives while a resource step is in flight and the step
// then succeeds. The cancellation must be observed before the operation is
// recorded successful; otherwise the resource silently becomes ready.
func TestIntegrationResourceCancelAfterSuccessfulStepIsHonored(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	fake := newBlockingResourceTransport("-health-")
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		RegistryEndpoint: "127.0.0.1:5000",
		SecretResolver:   newSnapshotSecretResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "cancel-success-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "cancel-success-app", map[string]any{
		"name": "cancelok",
		"environments": []map[string]any{
			{"name": "pr", "node": "local", "container_port": 8080, "health": map[string]any{"timeout_seconds": 5}},
			{"name": "pr2", "node": "local", "container_port": 8080, "health": map[string]any{"timeout_seconds": 5}},
		},
	}), contract.Succeeded)

	spec := map[string]any{
		"image":  "postgres:17",
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready"}},
	}
	apply := platformSubmit(t, s, platform.KindResourceApply, "resource/cancelok/pr/db", "resource-cancel-success", spec)
	select {
	case <-fake.started:
	case <-time.After(20 * time.Second):
		t.Fatal("resource health step did not start")
	}
	if _, err := s.RequestCancel(ctx, testActor, apply.ID); err != nil {
		t.Fatal(err)
	}
	// Let the in-flight step report success. The worker must still observe the
	// accepted cancellation at the next safe boundary rather than mark the
	// operation successful.
	fake.setBlockStep("")
	fake.release <- nil
	cancelled := platformAwait(t, s, apply, contract.Canceled)
	if cancelled.ID != apply.ID {
		t.Fatalf("cancelled operation = %s", cancelled.ID)
	}
	state, volumes, errorCode, _ := resourceState(t, pool, "cancelok", "db")
	if state != string(platform.ResourceDegraded) || errorCode != string(contract.Cancelled) {
		t.Fatalf("after cancellation: state=%s error=%s", state, errorCode)
	}
	if !strings.Contains(volumes, "er-res-") {
		t.Fatalf("cancellation lost the volume ledger: %s", volumes)
	}
	retainedVolumes := volumes

	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceApply, "resource/cancelok/pr/db", "resource-cancel-success-retry", spec), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='cancelok' AND r.name='db'`).Scan(&state, &volumes); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceReady) || volumes != retainedVolumes {
		t.Fatalf("retry: state=%s volumes=%s want %s", state, volumes, retainedVolumes)
	}
}

// TestIntegrationResourceApplyCancelBeforeFinalizationIsHonored covers the
// final-commit race: cancellation is accepted after the last step-boundary
// check but before the operation row lock. Finalization must see it under the
// lock and settle the operation through the normal cancellation path.
func TestIntegrationResourceApplyCancelBeforeFinalizationIsHonored(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	fake := newBlockingResourceTransport("-health-")
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		RegistryEndpoint: "127.0.0.1:5000",
		SecretResolver:   newSnapshotSecretResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "cancel-finalize-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "cancel-finalize-app", map[string]any{
		"name": "cancelfinalize",
		"environments": []map[string]any{
			{"name": "pr", "node": "local", "container_port": 8080, "health": map[string]any{"timeout_seconds": 5}},
		},
	}), contract.Succeeded)

	spec := map[string]any{
		"image":  "postgres:17",
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready"}},
	}
	apply := platformSubmit(t, s, platform.KindResourceApply, "resource/cancelfinalize/pr/db", "resource-cancel-finalize-apply", spec)
	select {
	case <-fake.started:
	case <-time.After(20 * time.Second):
		t.Fatal("resource health step did not start")
	}
	cancelResult := make(chan error, 1)
	s.beforeResourceFinalize = func() {
		s.beforeResourceFinalize = nil
		_, err := s.RequestCancel(ctx, testActor, apply.ID)
		cancelResult <- err
	}
	fake.setBlockStep("")
	fake.release <- nil
	cancelled := platformAwait(t, s, apply, contract.Canceled)
	if cancelled.ID != apply.ID {
		t.Fatalf("cancelled operation = %s", cancelled.ID)
	}
	if err := <-cancelResult; err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	state, volumes, errorCode, _ := resourceState(t, pool, "cancelfinalize", "db")
	if state != string(platform.ResourceDegraded) || errorCode != string(contract.Cancelled) {
		t.Fatalf("after finalization cancellation: state=%s error=%s", state, errorCode)
	}
	if !strings.Contains(volumes, "er-res-") {
		t.Fatalf("finalization cancellation lost the volume ledger: %s", volumes)
	}
}

// TestIntegrationResourceDestroyCancelBeforeFinalizationIsHonored covers the
// same final-commit race for destroy. The container effect has already
// completed; cancellation before the locked finalization must leave the
// resource degraded with its ledger, not destroyed or succeeded.
func TestIntegrationResourceDestroyCancelBeforeFinalizationIsHonored(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	fake := newBlockingResourceTransport("")
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		RegistryEndpoint: "127.0.0.1:5000",
		SecretResolver:   newSnapshotSecretResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "destroy-cancel-finalize-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "destroy-cancel-finalize-app", map[string]any{
		"name": "destroycancelfinalize",
		"environments": []map[string]any{
			{"name": "pr", "node": "local", "container_port": 8080, "health": map[string]any{"timeout_seconds": 5}},
		},
	}), contract.Succeeded)
	spec := map[string]any{
		"image":  "postgres:17",
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready"}},
	}
	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceApply, "resource/destroycancelfinalize/pr/db", "destroy-cancel-finalize-apply", spec), contract.Succeeded)

	fake.setBlockStep("-destroy-")
	destroy := platformSubmit(t, s, platform.KindResourceDestroy, "resource/destroycancelfinalize/pr/db", "destroy-cancel-finalize-destroy", map[string]any{"confirm": "destroycancelfinalize/pr/db"})
	select {
	case <-fake.started:
	case <-time.After(20 * time.Second):
		t.Fatal("resource destroy step did not start")
	}
	cancelResult := make(chan error, 1)
	s.beforeResourceFinalize = func() {
		s.beforeResourceFinalize = nil
		_, err := s.RequestCancel(ctx, testActor, destroy.ID)
		cancelResult <- err
	}
	fake.setBlockStep("")
	fake.release <- nil
	cancelled := platformAwait(t, s, destroy, contract.Canceled)
	if cancelled.ID != destroy.ID {
		t.Fatalf("cancelled operation = %s", cancelled.ID)
	}
	if err := <-cancelResult; err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	state, volumes, errorCode, _ := resourceState(t, pool, "destroycancelfinalize", "db")
	if state != string(platform.ResourceDegraded) || errorCode != string(contract.Cancelled) {
		t.Fatalf("after destroy finalization cancellation: state=%s error=%s", state, errorCode)
	}
	if !strings.Contains(volumes, "er-res-") {
		t.Fatalf("destroy finalization cancellation lost the volume ledger: %s", volumes)
	}
}

// TestIntegrationReconcileTerminalResourceStatesRepairsExistingStuckRows
// reproduces a row left provisioning/destroying by an operation that was
// already terminal before this fix, then runs the ordinary maintenance
// reconciliation only.
func TestIntegrationReconcileTerminalResourceStatesRepairsExistingStuckRows(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	run := testRuntime(t, pool, 0)
	s := run.Service
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "stuck-resource-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "stuck-resource-app", map[string]any{
		"name": "stuckapi",
		"environments": []map[string]any{{
			"name": "production", "node": "local", "container_port": 8080,
			"health": map[string]any{"timeout_seconds": 5},
		}},
	})
	spec := map[string]any{
		"image":  "postgres:17",
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready"}},
	}
	apply := platformSubmit(t, s, platform.KindResourceApply, "resource/stuckapi/production/db", "stuck-resource-apply", spec)
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='cancelled' WHERE id=$1`, apply.ID); err != nil {
		t.Fatal(err)
	}
	var resourceID string
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_platform_resources WHERE operation_id=$1`, apply.ID).Scan(&resourceID); err != nil {
		t.Fatal(err)
	}
	volumeName := "er-res-" + strings.TrimPrefix(resourceID, "res_") + "-data"
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources
        SET state='provisioning',
            volumes=jsonb_build_array(jsonb_build_object('name',$2::text,'destination','/var/lib/postgresql/data'))
        WHERE operation_id=$1`, apply.ID, volumeName); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, volumes, errorCode, detail := resourceState(t, pool, "stuckapi", "db")
	if state != string(platform.ResourceDegraded) || errorCode != string(contract.Cancelled) || !strings.Contains(volumes, volumeName) || !strings.Contains(detail, "apply") {
		t.Fatalf("cancelled stuck row = state=%s error=%s detail=%q volumes=%s", state, errorCode, detail, volumes)
	}

	// A failed destroy row is repaired too, with the destroy-specific detail.
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='failed',error_code='unavailable' WHERE id=$1`, apply.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state='destroying' WHERE operation_id=$1`, apply.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, volumes, errorCode, detail = resourceState(t, pool, "stuckapi", "db")
	if state != string(platform.ResourceDegraded) || errorCode != string(contract.Unavailable) || !strings.Contains(volumes, volumeName) || !strings.Contains(detail, "destroy") {
		t.Fatalf("failed stuck row = state=%s error=%s detail=%q volumes=%s", state, errorCode, detail, volumes)
	}

	// A newer operation owns the row; reconciliation must not degrade its
	// pending state just because an older terminal operation still exists.
	platformSubmit(t, s, platform.KindResourceApply, "resource/stuckapi/production/db", "stuck-resource-newer", spec)
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, _, _ = resourceState(t, pool, "stuckapi", "db")
	if state != string(platform.ResourcePending) {
		t.Fatalf("newer operation was clobbered: state=%s", state)
	}
}

func resourceState(t *testing.T, pool *pgxpool.Pool, app, name string) (state, volumes, errorCode, detail string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT r.state,r.volumes::text,r.error_code,r.health_detail
        FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name=$1 AND r.name=$2`, app, name).Scan(&state, &volumes, &errorCode, &detail); err != nil {
		t.Fatal(err)
	}
	return state, volumes, errorCode, detail
}
