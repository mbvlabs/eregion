package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// resourceTestTransport answers the resource node steps without Docker. It
// records payloads so a test can prove a secret value never entered one.
type resourceTestTransport struct {
	mu       sync.Mutex
	payloads []string
	secrets  []map[string]string
}

func (*resourceTestTransport) Name() string         { return "resource-test" }
func (*resourceTestTransport) NodeIdentity() string { return "local" }

func (t *resourceTestTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	switch request.Method {
	case node.MethodStatus:
		return node.Response{Status: engine.StatusAbsent, ExecutionID: request.Execution.ExecutionID()}, nil
	case node.MethodRun:
		t.mu.Lock()
		t.payloads = append(t.payloads, string(request.Payload))
		copied := map[string]string{}
		for key, value := range request.Secrets {
			copied[key] = value
		}
		t.secrets = append(t.secrets, copied)
		t.mu.Unlock()
		result := json.RawMessage(`{}`)
		switch {
		case strings.Contains(request.Execution.Step, "-image-"):
			result, _ = json.Marshal(map[string]string{"stdout": "sha256:" + strings.Repeat("a", 64)})
		case strings.Contains(request.Execution.Step, "-run-"):
			result = json.RawMessage(`{"container_id":"container-from-test"}`)
		}
		return node.Response{Status: engine.StatusCompleted, ExecutionID: request.Execution.ExecutionID(), Executed: true, Result: result}, nil
	default:
		return node.Response{}, fmt.Errorf("unexpected node method %q", request.Method)
	}
}

func TestIntegrationManagedResourceLifecycleRetainsVolumesAndSecrets(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	fake := &resourceTestTransport{}
	secretResolver := newSnapshotSecretResolver()
	secretResolver.set("DATABASE_PASSWORD", 1, "hunter2-resource-secret")
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		SecretResolver: secretResolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	s := run.Service

	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "resource-node", map[string]any{
		"name": "local",
		"roles": []string{
			platform.RoleApplication, platform.RoleControlPlane,
		},
		"capabilities": []string{platform.CapabilityDocker},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "resource-app", map[string]any{
		"name": "api",
		"environments": []map[string]any{{
			"name": "production", "node": "local", "container_port": 8080,
			"health": map[string]any{"timeout_seconds": 5},
		}},
	}), contract.Succeeded)

	apply := platformSubmit(t, s, platform.KindResourceApply, "resource/api/production/db", "resource-apply", map[string]any{
		"image": "postgres:17",
		"env":   map[string]string{"POSTGRES_DB": "app"},
		"secret_env": map[string]string{
			"POSTGRES_PASSWORD": "DATABASE_PASSWORD",
		},
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready", "-U", "app"}},
	})
	platformAwait(t, s, apply, contract.Succeeded)

	var state, imageID, containerName, networkName, specText, volumesText string
	if err := pool.QueryRow(ctx, `SELECT state,image_id,container_name,network_name,spec::text,volumes::text
        FROM eregion_platform_resources r JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='api' AND r.name='db'`).Scan(&state, &imageID, &containerName, &networkName, &specText, &volumesText); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceReady) {
		t.Fatalf("state=%s", state)
	}
	if !platform.ResourceImageIDValid(imageID) || containerName == "" || !platform.ResourceNetworkValid(networkName) {
		t.Fatalf("applied identity: image=%q container=%q network=%q", imageID, containerName, networkName)
	}
	if !strings.Contains(volumesText, "er-res-") || !strings.Contains(volumesText, "/var/lib/postgresql/data") {
		t.Fatalf("volumes=%s", volumesText)
	}
	if strings.Contains(specText, "hunter2-resource-secret") {
		t.Fatalf("resource spec stored a secret value: %s", specText)
	}

	inventory, err := s.Inventory(ctx, testActor, platform.InventoryQuery{Collection: "resources", App: "api", Environment: "production", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := inventory.Items.([]platform.Resource)
	if !ok || len(items) != 1 || items[0].State != platform.ResourceReady || items[0].Spec.SecretEnv["POSTGRES_PASSWORD"] != "DATABASE_PASSWORD" {
		t.Fatalf("inventory=%+v", inventory.Items)
	}

	// A live resource blocks environment deletion before any container cleanup.
	_, err = s.Submit(ctx, platformSubmission(platform.KindEnvironmentDelete, "env/api/production", "resource-delete-live", map[string]any{"confirm": "api/production"}))
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("environment deletion with a live resource: %v", err)
	}

	// Attaching the resource is a new environment revision, not a deploy and
	// not proof of readiness. Destroying an attached resource is refused.
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentResourceAttach, "env/api/production", "resource-attach", map[string]any{
		"resource": "db",
	}), contract.Succeeded)
	var resources []string
	if err := pool.QueryRow(ctx, `SELECT resources FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE e.current_revision_id=r.id AND e.name='production'`).Scan(&resources); err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0] != "db" {
		t.Fatalf("revision resources=%v", resources)
	}
	_, err = s.Submit(ctx, platformSubmission(platform.KindResourceDestroy, "resource/api/production/db", "resource-destroy-attached", map[string]any{
		"confirm": "api/production/db",
	}))
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("destroying an attached resource: %v", err)
	}
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentResourceDetach, "env/api/production", "resource-detach", map[string]any{
		"resource": "db",
	}), contract.Succeeded)

	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceDestroy, "resource/api/production/db", "resource-destroy", map[string]any{
		"confirm": "api/production/db",
	}), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,container_name,volumes::text
        FROM eregion_platform_resources r JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='api' AND r.name='db'`).Scan(&state, &containerName, &volumesText); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceDestroyed) || containerName != "" {
		t.Fatalf("destroyed state=%s container=%q", state, containerName)
	}
	if !strings.Contains(volumesText, "er-res-") {
		t.Fatalf("named volume identity was lost on destroy: %s", volumesText)
	}
	retainedVolumes := volumesText

	// Re-applying the same identity reactivates the retained volumes instead of
	// creating a second resource.
	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceApply, "resource/api/production/db", "resource-reapply", map[string]any{
		"image": "postgres:17",
		"env":   map[string]string{"POSTGRES_DB": "app"},
		"secret_env": map[string]string{
			"POSTGRES_PASSWORD": "DATABASE_PASSWORD",
		},
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready", "-U", "app"}},
	}), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text
        FROM eregion_platform_resources r JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='api' AND r.name='db'`).Scan(&state, &volumesText); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceReady) || volumesText != retainedVolumes {
		t.Fatalf("re-apply did not reuse the same retained volumes: state=%s volumes=%s", state, volumesText)
	}

	// A declaration that stops mounting the data volume must not drop its
	// durable identity. The old volume stays owned even though the running
	// container no longer mounts it.
	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceApply, "resource/api/production/db", "resource-remove-mount", map[string]any{
		"image":  "postgres:17",
		"env":    map[string]string{"POSTGRES_DB": "app"},
		"health": map[string]any{"type": "exec", "command": []string{"pg_isready", "-U", "app"}},
	}), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text
        FROM eregion_platform_resources r JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='api' AND r.name='db'`).Scan(&state, &volumesText); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceReady) || volumesText != retainedVolumes {
		t.Fatalf("mount removal forgot the owned volume identity: state=%s volumes=%s", state, volumesText)
	}

	// Default destroy retains the ledger; a later explicit --delete-data is a
	// supported, separate operation and clears it only after the owned volumes
	// were checked and removed.
	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceDestroy, "resource/api/production/db", "resource-destroy-keep-data", map[string]any{
		"confirm": "api/production/db",
	}), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text
        FROM eregion_platform_resources r JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='api' AND r.name='db'`).Scan(&state, &volumesText); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceDestroyed) || volumesText != retainedVolumes {
		t.Fatalf("default destroy lost retained volume identities: state=%s volumes=%s", state, volumesText)
	}
	platformAwait(t, s, platformSubmit(t, s, platform.KindResourceDestroy, "resource/api/production/db", "resource-destroy-delete-data", map[string]any{
		"confirm": "api/production/db", "delete_data": true,
	}), contract.Succeeded)
	if err := pool.QueryRow(ctx, `SELECT state,volumes::text
        FROM eregion_platform_resources r JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='api' AND r.name='db'`).Scan(&state, &volumesText); err != nil {
		t.Fatal(err)
	}
	if state != string(platform.ResourceDestroyed) || volumesText != "[]" {
		t.Fatalf("explicit data destruction did not clear the owned ledger: state=%s volumes=%s", state, volumesText)
	}

	// No event, operation input or node payload may carry the resolved value.
	rows, err := pool.Query(ctx, `SELECT coalesce(o.input::text,''), coalesce(e.event::text,'')
        FROM eregion_operations o LEFT JOIN eregion_operation_events e ON e.operation_id=o.id
        WHERE o.target='resource/api/production/db'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var input, event string
		if err := rows.Scan(&input, &event); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(input, "hunter2-resource-secret") || strings.Contains(event, "hunter2-resource-secret") {
			t.Fatal("resource secret value entered durable operation history")
		}
	}
	fake.mu.Lock()
	payloads, secrets := append([]string(nil), fake.payloads...), append([]map[string]string(nil), fake.secrets...)
	fake.mu.Unlock()
	for _, payload := range payloads {
		if strings.Contains(payload, "hunter2-resource-secret") {
			t.Fatal("resource secret value entered a node payload")
		}
	}
	foundSecret := false
	for _, secret := range secrets {
		if secret["POSTGRES_PASSWORD"] == "hunter2-resource-secret" {
			foundSecret = true
		}
	}
	if !foundSecret {
		t.Fatal("the resource run step never received the transient secret value")
	}
}
