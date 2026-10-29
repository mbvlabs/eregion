package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationCreateExposesTheFirstDeployAtAcceptance proves the first
// deploy operation ID is committed with the create transaction: it is readable
// before any worker runs, belongs to the revision the create inserted, and is
// returned unchanged by a same-key replay.
func TestIntegrationCreateExposesTheFirstDeployAtAcceptance(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "node-guided", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	const createKey = "cli:guided:create"
	op := platformSubmit(t, s, platform.KindAppCreate, "platform", createKey, map[string]any{
		"name": "guided",
		"environment": map[string]any{
			"name": "production", "node": "local",
			"source":           map[string]any{"kind": "image", "image": "ghcr.io/acme/api:2026-10-28"},
			"domain":           "guided.example.com",
			"domain_addresses": []string{"203.0.113.10"},
		},
		"deploy": true,
	})
	if op.FirstDeployOperationID == "" {
		t.Fatal("accepted create has no first deploy operation ID")
	}
	var revisionID string
	if err := pool.QueryRow(ctx, `SELECT r.id FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='guided' AND e.name='production' AND r.operation_id=$1`, op.ID).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	var kind, target, state, key string
	if err := pool.QueryRow(ctx, `SELECT kind,target,state,idempotency_key FROM eregion_operations WHERE id=$1`, op.FirstDeployOperationID).
		Scan(&kind, &target, &state, &key); err != nil {
		t.Fatal(err)
	}
	if kind != platform.KindDeploy || target != "env/guided/production" || state != string(contract.Queued) {
		t.Fatalf("first deploy = kind %q target %q state %q", kind, target, state)
	}
	if key != "first-deploy:"+revisionID {
		t.Fatalf("first deploy key = %q, want the create revision %q", key, revisionID)
	}

	replayed, err := s.Submit(ctx, platformSubmission(platform.KindAppCreate, platform.PlatformTarget().String(), createKey, map[string]any{
		"name": "guided",
		"environment": map[string]any{
			"name": "production", "node": "local",
			"source":           map[string]any{"kind": "image", "image": "ghcr.io/acme/api:2026-10-28"},
			"domain":           "guided.example.com",
			"domain_addresses": []string{"203.0.113.10"},
		},
		"deploy": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != op.ID || replayed.FirstDeployOperationID != op.FirstDeployOperationID {
		t.Fatalf("replay = %s/%s, want %s/%s", replayed.ID, replayed.FirstDeployOperationID, op.ID, op.FirstDeployOperationID)
	}
}

func TestIntegrationCreateWithoutDeployHasNoFirstDeployOperation(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "node-plain", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	op := platformSubmit(t, s, platform.KindAppCreate, "platform", "cli:plain:create", map[string]any{
		"name": "plain",
		"environment": map[string]any{
			"name": "production", "node": "local",
			"source": map[string]any{"kind": "image", "image": "ghcr.io/acme/api:2026-10-28"},
		},
	})
	if op.FirstDeployOperationID != "" {
		t.Fatalf("a create without deploy exposed %s", op.FirstDeployOperationID)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_operations WHERE kind=$1 AND target='env/plain/production'`, platform.KindDeploy).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%d deploy operation(s) were created without deploy intent", count)
	}
}
