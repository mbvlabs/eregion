package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func testRegistryHostIdentity() RegistryHostIdentity {
	return RegistryHostIdentity{
		NodeName:           "main",
		HostKeyType:        "ssh-ed25519",
		HostKeyFingerprint: "SHA256:" + strings.Repeat("A", 43),
		StateDir:           "/var/lib/eregion/node",
	}
}

func testRegistryHostIdentityFor(node, stateDir string) RegistryHostIdentity {
	identity := testRegistryHostIdentity()
	identity.NodeName = node
	identity.StateDir = stateDir
	return identity
}

// ensureTestRegistryHost returns the test database's immutable workspace
// registry-host binding, assigning one through the production path for a fresh
// isolated test database. A caller-supplied state directory is used only for
// that first assignment; once bound, every runtime in the test must reuse the
// exact binding and state directory. Tests that intentionally exercise an
// unbound workspace use testPool and explicit setup instead of this helper.
func ensureTestRegistryHost(t *testing.T, pool *pgxpool.Pool, stateDir string) RegistryHostIdentity {
	t.Helper()
	ctx := context.Background()
	workspace, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Configured {
		if workspace.RegistryHost == nil {
			t.Fatal("test runtime requires a verified registry-host binding; use explicit test setup for an unbound workspace")
		}
		if strings.TrimSpace(stateDir) != "" {
			canonical, err := CanonicalRegistryHostStateDir(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if canonical != workspace.RegistryHost.StateDir {
				t.Fatalf("test runtime state directory %q does not match the existing workspace registry-host binding %q", canonical, workspace.RegistryHost.StateDir)
			}
		}
		return *workspace.RegistryHost
	}
	if strings.TrimSpace(stateDir) == "" {
		stateDir = t.TempDir()
	}
	canonicalStateDir, err := CanonicalRegistryHostStateDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := testRegistryHostIdentityFor("main", canonicalStateDir)
	assigned, err := AssignWorkspace(ctx, pool, "test", identity)
	if err != nil {
		t.Fatal(err)
	}
	if assigned.RegistryHost == nil {
		t.Fatal("workspace assignment did not record the registry-host binding")
	}
	return *assigned.RegistryHost
}

// TestIntegrationTestRuntimeRegistryHostBindingAllowsAppDelete proves the
// shared test runtime composes the same verified public registry-host guard
// production deletion uses. It also proves a second runtime on the same test
// database reuses the exact binding and state directory instead of minting a
// replacement or backfilling a different one.
func TestIntegrationTestRuntimeRegistryHostBindingAllowsAppDelete(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	run := testRuntime(t, pool, 0)
	s := run.Service
	if s.registryHostIdentity == nil {
		t.Fatal("test runtime has no registry-host identity")
	}
	first, err := s.verifyLocalRegistryHost(ctx)
	if err != nil {
		t.Fatalf("test runtime identity did not verify: %v", err)
	}
	s.routeSync = func(context.Context) error { return nil }

	create := platformSubmit(t, s, platform.KindAppCreate, "platform", "runtime-binding-create", map[string]any{
		"name": "bindingdelete",
	})
	if err := runPlatformAttempt(ctx, s, create.ID, 1); err != nil {
		t.Fatalf("create app: %v", err)
	}
	var originalAppID platform.AppID
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name='bindingdelete'`).Scan(&originalAppID); err != nil {
		t.Fatal(err)
	}
	s.humanFactor = func(string) error { return nil }
	deletion := platformSubmission(platform.KindAppDelete, "app/bindingdelete", "runtime-binding-delete", map[string]any{
		"confirm": "bindingdelete",
	})
	deletion.HumanFactor = "test-factor"
	op, err := s.Submit(ctx, deletion)
	if err != nil {
		t.Fatalf("submit app delete: %v", err)
	}
	if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
		t.Fatalf("app delete through registry-host guard: %v", err)
	}
	var apps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_apps WHERE name='bindingdelete'`).Scan(&apps); err != nil {
		t.Fatal(err)
	}
	if apps != 0 {
		t.Fatalf("app delete left %d application row(s)", apps)
	}
	completed, err := s.Get(ctx, op.Actor, op.ID)
	if err != nil || completed.State != contract.Succeeded || completed.LatestResult == nil {
		t.Fatalf("delete result: %+v, %v", completed, err)
	}
	var result struct {
		AppID           platform.AppID `json:"app_id"`
		App             string         `json:"app"`
		AppDeleted      bool           `json:"app_deleted"`
		ResourceVolumes string         `json:"resource_volumes"`
	}
	if err := json.Unmarshal(completed.LatestResult.Data, &result); err != nil ||
		result.AppID != originalAppID || result.App != "bindingdelete" || !result.AppDeleted ||
		result.ResourceVolumes != "unchanged" {
		t.Fatalf("deleted identity was not retained: %+v, %v", result, err)
	}
	replayed, err := s.Submit(ctx, deletion)
	if err != nil || replayed.ID != completed.ID || replayed.LatestResult == nil ||
		string(replayed.LatestResult.Data) != string(completed.LatestResult.Data) ||
		replayed.LastSequence != completed.LastSequence {
		t.Fatalf("terminal deletion replay changed or omitted its result: %+v, %v", replayed, err)
	}

	secondRun := testRuntime(t, pool, 0)
	second, err := secondRun.Service.verifyLocalRegistryHost(ctx)
	if err != nil {
		t.Fatalf("second test runtime identity did not verify: %v", err)
	}
	if !first.Equal(second) {
		t.Fatalf("second runtime binding = %+v, want the exact existing %+v", second, first)
	}
}
