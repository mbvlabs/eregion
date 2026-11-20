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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
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
	case node.MethodObserve, node.MethodObserveImage, node.MethodInventory, node.MethodRetireBuild:
		return node.Response{}, fmt.Errorf("unexpected node method %q", request.Method)
	default:
		return node.Response{}, fmt.Errorf("unexpected node method %q", request.Method)
	}
}

// managedResourceTest carries the shared state of one managed-resource
// lifecycle so each phase can assert its own durable boundary.
type managedResourceTest struct {
	t               *testing.T
	ctx             context.Context
	pool            *pgxpool.Pool
	service         *Service
	transport       *resourceTestTransport
	retainedVolumes string
}

func newManagedResourceTest(t *testing.T) *managedResourceTest {
	t.Helper()
	pool, _ := testPool(t, true)
	ctx := context.Background()
	fake := &resourceTestTransport{}
	secretResolver := newSnapshotSecretResolver()
	secretResolver.set("DATABASE_PASSWORD", 1, "hunter2-resource-secret")
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	vaultService, err := newTestVaultService(t, pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		SecretResolver: secretResolver, Vault: vaultService, RegistryEndpoint: "127.0.0.1:5000",
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
	ns, err := vault.NewNamespace("api", "production", "DATABASE_PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := vaultService.Store().Set(ctx, tx.Secrets(), ns, []byte("hunter2-resource-secret"), 0, testActor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return &managedResourceTest{t: t, ctx: ctx, pool: pool, service: s, transport: fake}
}

func managedResourceInput(includeSecret, includeMount bool) map[string]any {
	input := map[string]any{
		"image": "postgres:17",
		"env":   map[string]string{"POSTGRES_DB": "app"},
		"health": map[string]any{
			"type": "exec", "command": []string{"pg_isready", "-U", "app"},
		},
	}
	if includeSecret {
		input["secret_env"] = map[string]string{"POSTGRES_PASSWORD": "DATABASE_PASSWORD"}
	}
	if includeMount {
		input["mounts"] = []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}}
	}
	return input
}

func (m *managedResourceTest) submit(key, kind, target string, input map[string]any) {
	m.t.Helper()
	platformAwait(m.t, m.service, platformSubmit(m.t, m.service, kind, target, key, input), contract.Succeeded)
}

func (m *managedResourceTest) queryResource() (state, imageID, containerName, networkName, specText, volumesText string) {
	m.t.Helper()
	if err := m.pool.QueryRow(m.ctx, `SELECT state,image_id,container_name,network_name,spec::text,volumes::text
        FROM eregion_platform_resources r JOIN eregion_platform_apps a ON a.id=r.app_id
        WHERE a.name='api' AND r.name='db'`).Scan(&state, &imageID, &containerName, &networkName, &specText, &volumesText); err != nil {
		m.t.Fatal(err)
	}
	return state, imageID, containerName, networkName, specText, volumesText
}

func (m *managedResourceTest) applyInitialResource() {
	m.t.Helper()
	m.submit("resource-apply", platform.KindResourceApply, "resource/api/production/db", managedResourceInput(true, true))
}

func (m *managedResourceTest) assertReadyIdentityAndInventory() {
	m.t.Helper()
	state, imageID, containerName, networkName, specText, volumesText := m.queryResource()
	if state != string(platform.ResourceReady) {
		m.t.Fatalf("state=%s", state)
	}
	if !platform.ResourceImageIDValid(imageID) || containerName == "" || !platform.ResourceNetworkValid(networkName) {
		m.t.Fatalf("applied identity: image=%q container=%q network=%q", imageID, containerName, networkName)
	}
	if !strings.Contains(volumesText, "er-res-") || !strings.Contains(volumesText, "/var/lib/postgresql/data") {
		m.t.Fatalf("volumes=%s", volumesText)
	}
	if strings.Contains(specText, "hunter2-resource-secret") {
		m.t.Fatalf("resource spec stored a secret value: %s", specText)
	}
	inventory, err := m.service.Inventory(m.ctx, testActor, platform.InventoryQuery{Collection: "resources", App: "api", Environment: "production", Limit: 10})
	if err != nil {
		m.t.Fatal(err)
	}
	items, ok := inventory.Items.([]platform.Resource)
	if !ok || len(items) != 1 || items[0].State != platform.ResourceReady || items[0].Spec.SecretEnv["POSTGRES_PASSWORD"] != "DATABASE_PASSWORD" {
		m.t.Fatalf("inventory=%+v", inventory.Items)
	}
}

func (m *managedResourceTest) assertLiveResourceBlocksDeletion() {
	m.t.Helper()
	_, err := m.service.Submit(m.ctx, platformSubmission(platform.KindEnvironmentDelete, "env/api/production", "resource-delete-live", map[string]any{"confirm": "api/production"}))
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		m.t.Fatalf("environment deletion with a live resource: %v", err)
	}
}

func (m *managedResourceTest) assertAttachedResourceLifecycle() {
	m.t.Helper()
	// Attaching the resource is a new environment revision, not a deploy and
	// not proof of readiness. Destroying an attached resource is refused.
	m.submit("resource-attach", platform.KindEnvironmentResourceAttach, "env/api/production", map[string]any{"resource": "db"})
	attachedPreview, err := previewSubmission(m.t, m.service, platform.KindEnvironmentDelete, "env/api/production", map[string]any{})
	if err != nil || !deletionWarningContains(attachedPreview.Warnings, "api/production/db (state: ready)") {
		m.t.Fatalf("attached resource preview: %+v, %v", attachedPreview, err)
	}
	var resources []string
	if err := m.pool.QueryRow(m.ctx, `SELECT resources FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE e.current_revision_id=r.id AND e.name='production'`).Scan(&resources); err != nil {
		m.t.Fatal(err)
	}
	if len(resources) != 1 || resources[0] != "db" {
		m.t.Fatalf("revision resources=%v", resources)
	}
	_, err = m.service.Submit(m.ctx, platformSubmission(platform.KindResourceDestroy, "resource/api/production/db", "resource-destroy-attached", map[string]any{
		"confirm": "api/production/db",
	}))
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		m.t.Fatalf("destroying an attached resource: %v", err)
	}
	m.submit("resource-detach", platform.KindEnvironmentResourceDetach, "env/api/production", map[string]any{"resource": "db"})
	detachedPreview, err := previewSubmission(m.t, m.service, platform.KindEnvironmentDelete, "env/api/production", map[string]any{})
	if err != nil || !deletionWarningContains(detachedPreview.Warnings, "api/production/db (state: ready)") {
		m.t.Fatalf("detached resource preview: %+v, %v", detachedPreview, err)
	}
}

func (m *managedResourceTest) destroyResource(key string, deleteData bool) {
	m.t.Helper()
	input := map[string]any{"confirm": "api/production/db"}
	if deleteData {
		input["delete_data"] = true
	}
	m.submit(key, platform.KindResourceDestroy, "resource/api/production/db", input)
}

func (m *managedResourceTest) assertDestroyRetainsVolumes() {
	m.t.Helper()
	m.destroyResource("resource-destroy", false)
	state, _, containerName, _, _, volumesText := m.queryResource()
	if state != string(platform.ResourceDestroyed) || containerName != "" {
		m.t.Fatalf("destroyed state=%s container=%q", state, containerName)
	}
	if !strings.Contains(volumesText, "er-res-") {
		m.t.Fatalf("named volume identity was lost on destroy: %s", volumesText)
	}
	m.retainedVolumes = volumesText
	destroyedPreview, err := previewSubmission(m.t, m.service, platform.KindEnvironmentDelete, "env/api/production", map[string]any{})
	if err != nil || deletionWarningContains(destroyedPreview.Warnings, "Deletion blocked by resource") {
		m.t.Fatalf("destroyed resource preview: %+v, %v", destroyedPreview, err)
	}
}

func (m *managedResourceTest) assertReapplyReusesVolumes() {
	m.t.Helper()
	// Re-applying the same identity reactivates the retained volumes instead of
	// creating a second resource.
	m.submit("resource-reapply", platform.KindResourceApply, "resource/api/production/db", managedResourceInput(true, true))
	state, _, _, _, _, volumesText := m.queryResource()
	if state != string(platform.ResourceReady) || volumesText != m.retainedVolumes {
		m.t.Fatalf("re-apply did not reuse the same retained volumes: state=%s volumes=%s", state, volumesText)
	}
}

func (m *managedResourceTest) assertMountRemovalRetainsVolumes() {
	m.t.Helper()
	// A declaration that stops mounting the data volume must not drop its
	// durable identity.
	m.submit("resource-remove-mount", platform.KindResourceApply, "resource/api/production/db", managedResourceInput(false, false))
	state, _, _, _, _, volumesText := m.queryResource()
	if state != string(platform.ResourceReady) || volumesText != m.retainedVolumes {
		m.t.Fatalf("mount removal forgot the owned volume identity: state=%s volumes=%s", state, volumesText)
	}
}

func (m *managedResourceTest) assertDataDestructionClearsVolumes() {
	m.t.Helper()
	// Default destroy retains the ledger; a later explicit --delete-data is a
	// supported, separate operation and clears it only after the owned volumes
	// were checked and removed.
	m.destroyResource("resource-destroy-keep-data", false)
	state, _, _, _, _, volumesText := m.queryResource()
	if state != string(platform.ResourceDestroyed) || volumesText != m.retainedVolumes {
		m.t.Fatalf("default destroy lost retained volume identities: state=%s volumes=%s", state, volumesText)
	}
	m.destroyResource("resource-destroy-delete-data", true)
	state, _, _, _, _, volumesText = m.queryResource()
	if state != string(platform.ResourceDestroyed) || volumesText != "[]" {
		m.t.Fatalf("explicit data destruction did not clear the owned ledger: state=%s volumes=%s", state, volumesText)
	}
}

func (m *managedResourceTest) assertNoSecretLeak() {
	m.t.Helper()
	// No event, operation input or node payload may carry the resolved value.
	rows, err := m.pool.Query(m.ctx, `SELECT coalesce(o.input::text,''), coalesce(e.event::text,'')
        FROM eregion_operations o LEFT JOIN eregion_operation_events e ON e.operation_id=o.id
        WHERE o.target='resource/api/production/db'`)
	if err != nil {
		m.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var input, event string
		if err := rows.Scan(&input, &event); err != nil {
			m.t.Fatal(err)
		}
		if strings.Contains(input, "hunter2-resource-secret") || strings.Contains(event, "hunter2-resource-secret") {
			m.t.Fatal("resource secret value entered durable operation history")
		}
	}
	if err := rows.Err(); err != nil {
		m.t.Fatal(err)
	}
	m.transport.mu.Lock()
	payloads, secrets := append([]string(nil), m.transport.payloads...), append([]map[string]string(nil), m.transport.secrets...)
	m.transport.mu.Unlock()
	for _, payload := range payloads {
		if strings.Contains(payload, "hunter2-resource-secret") {
			m.t.Fatal("resource secret value entered a node payload")
		}
	}
	foundSecret := false
	for _, secret := range secrets {
		if secret["POSTGRES_PASSWORD"] == "hunter2-resource-secret" {
			foundSecret = true
		}
	}
	if !foundSecret {
		m.t.Fatal("the resource run step never received the transient secret value")
	}
}

func TestIntegrationManagedResourceLifecycleRetainsVolumesAndSecrets(t *testing.T) {
	m := newManagedResourceTest(t)
	m.applyInitialResource()
	m.assertReadyIdentityAndInventory()
	m.assertLiveResourceBlocksDeletion()
	m.assertAttachedResourceLifecycle()
	m.assertDestroyRetainsVolumes()
	m.assertReapplyReusesVolumes()
	m.assertMountRemovalRetainsVolumes()
	m.assertDataDestructionClearsVolumes()
	m.assertNoSecretLeak()
}
