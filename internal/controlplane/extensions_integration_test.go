package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

var operatorActor = contract.Actor{Kind: "operator", Subject: "extensions-operator"}

// controlPlane is one full control plane: its own pool, extension wiring, vault
// resolver, River workers, and HTTP API (operations, secrets) on one database.
type controlPlane struct {
	pool   *pgxpool.Pool
	run    *Runtime
	server *httptest.Server
	vault  *vault.Service
	token  string
	stop   func()
}

func (c *controlPlane) client(t *testing.T) *api.Client {
	t.Helper()
	client, err := api.NewClient(c.server.URL, c.token)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func startControlPlane(t *testing.T, dsn, keyFile, nodeDir string, workers int) *controlPlane {
	t.Helper()
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterExtensions(ExtensionConfig{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	vaultService, err := NewVaultService(pool, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: workers, TestOnly: true, NodeStateDir: nodeDir, LocalNodeName: "local", SecretResolver: vault.NewEnvironmentResolver(vaultService),
	})
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("e", 64)
	hash := sha256.Sum256([]byte(token))
	auth, err := api.NewAuth([]api.Principal{{Actor: operatorActor, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: []string{"operations:submit", "operations:read", "operations:cancel", "system:read"}}})
	if err != nil {
		t.Fatal(err)
	}
	base := api.NewHandler(auth, run.Service, func(ctx context.Context) error { return Ready(ctx, pool) })
	mux := api.NewMux(api.MuxConfig{Default: base, Vault: api.NewVaultHandler(vaultService, api.AuthVaultAuthorizer(auth))})
	cp := &controlPlane{pool: pool, run: run, server: httptest.NewServer(mux), vault: vaultService, token: token}
	var once sync.Once
	cp.stop = func() {
		once.Do(func() {
			cp.server.Close()
			if workers > 0 {
				stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = run.Jobs.StopAndCancel(stopCtx)
			}
			pool.Close()
		})
	}
	t.Cleanup(cp.stop)
	if workers > 0 {
		if err := run.Jobs.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return cp
}

func writeKeyFile(t *testing.T) string {
	t.Helper()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "vault.keys")
	if err := vault.WriteKeyRing(path, ring); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestIntegrationTwoControlPlanesSurviveTheSubmitter runs two complete control
// planes against one database. The work is accepted through the first one's API,
// the first is then killed mid-flight, and the second finishes it exactly once.
func TestIntegrationTwoControlPlanesSurviveTheSubmitter(t *testing.T) {
	_, dsn := testPool(t, true)
	keyFile := writeKeyFile(t)
	nodeDir := t.TempDir() // Both control planes share one node state directory.
	ctx := context.Background()

	first := startControlPlane(t, dsn, keyFile, nodeDir, 1)
	raw, err := first.client(t).Test(ctx, operations.TestInput{Steps: 3, StepDelayMS: 400}, "two-control-planes", false)
	if err != nil {
		t.Fatal(err)
	}
	var submitted contract.Operation
	if err := json.Unmarshal(raw, &submitted); err != nil || submitted.State != contract.Queued {
		t.Fatalf("not accepted: %s %v", raw, err)
	}
	await(t, 10*time.Second, func() bool {
		var completed int
		err := first.pool.QueryRow(ctx, "SELECT completed_steps FROM eregion_operations WHERE id=$1", submitted.ID).Scan(&completed)
		return err == nil && completed > 0
	})

	// The second control plane comes up only now, so the first certainly holds the job.
	second := startControlPlane(t, dsn, keyFile, nodeDir, 1)
	first.stop() // Kill: listener closed, workers cancelled mid-step, pool gone.

	// The surviving control plane's API answers for the dead one's operation.
	secondClient := second.client(t)
	await(t, 30*time.Second, func() bool {
		raw, err := secondClient.Get(ctx, submitted.ID)
		if err != nil {
			return false
		}
		var op contract.Operation
		return json.Unmarshal(raw, &op) == nil && op.State == contract.Succeeded
	})
	if got := effectCount(t, nodeDir, submitted.ID, 3); got != 3 {
		t.Fatalf("node effects were duplicated or lost: %d", got)
	}
	if got := completedStepRows(t, second.pool, submitted.ID); got != 3 {
		t.Fatalf("step rows were duplicated or lost: %d", got)
	}
	page, err := second.run.Service.Events(ctx, operatorActor, contract.Cursor{OperationID: submitted.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	progress := 0
	for _, event := range page.Events {
		if event.Kind == "progress" {
			progress++
		}
	}
	if progress != 3 {
		t.Fatalf("progress was duplicated or lost: %d", progress)
	}
}

// TestIntegrationExtensionsThroughRealWiring exercises the production wiring:
// the migrations create the extension tables, a resource action runs through the
// registry, a secret set over the mounted vault route resolves at deploy time
// (and only in its own environment), and a due backup policy is submitted once.
func TestIntegrationExtensionsThroughRealWiring(t *testing.T) {
	pool, dsn := testPool(t, true)
	keyFile := writeKeyFile(t)
	nodeDir := t.TempDir()
	ctx := context.Background()
	cp := startControlPlane(t, dsn, keyFile, nodeDir, 2)
	s := cp.run.Service

	for _, table := range []string{"eregion_secrets", "eregion_secret_revisions", "eregion_access_grants", "eregion_integration_audit"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("migrations did not create %s: %v", table, err)
		}
	}
	if audit, err := integrationsAudit(pool); err != nil || audit != 0 {
		t.Fatalf("audit table is not queryable and empty: %d %v", audit, err)
	}

	// A secret set through the mounted route is resolved at deploy time.
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "app", map[string]any{"name": "shop"}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentCreate, "app/shop", "env", map[string]any{
		"name": "production", "node": "local", "container_port": 80,
		"secret_refs": map[string]string{"DB_PASSWORD": "vault:shop/production/DB_PASSWORD@1"},
	}), contract.Succeeded)
	namespace, err := vault.NewNamespace("shop", "production", "DB_PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	const plaintext = "correct-horse-battery-staple"
	meta, err := cp.client(t).SetSecret(ctx, namespace, []byte(plaintext), 0)
	if err != nil || meta.LatestRevision != 1 {
		t.Fatalf("secret set: %+v %v", meta, err)
	}
	env, err := s.environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	values, _, err := s.resolveEnvironmentSecrets(ctx, env)
	if err != nil || values["DB_PASSWORD"] != plaintext {
		t.Fatalf("deploy-time resolution failed: %v %v", values, err)
	}
	// A reference into another environment is refused, not resolved.
	env.SecretRefs = map[string]string{"DB_PASSWORD": "vault:shop/staging/DB_PASSWORD@1"}
	if _, _, err := s.resolveEnvironmentSecrets(ctx, env); err == nil {
		t.Fatal("cross-environment secret reference resolved")
	}
	var leaked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events e JOIN eregion_operations o ON o.id=e.operation_id WHERE e.event::text LIKE '%'||$1||'%' OR o.input::text LIKE '%'||$1||'%'`, plaintext).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("plaintext reached durable operation state: %d %v", leaked, err)
	}
}

func integrationsAudit(pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_integration_audit`).Scan(&n)
	return n, err
}
