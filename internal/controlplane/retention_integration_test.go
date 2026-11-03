package controlplane

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// TestIntegrationRetentionPolicyRepository proves migration 017 stores the
// server default and the environment override, that other revision changes
// carry the override forward, and that a run row is insertable.
func TestIntegrationRetentionPolicyRepository(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "retention-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "retention-create", map[string]any{
		"name": "retention",
		"environment": map[string]any{
			"name": "production", "node": "local",
			"source":           map[string]any{"kind": "image", "image": "ghcr.io/acme/api:2026-11-03"},
			"domain":           "retention.example.com",
			"domain_addresses": []string{"203.0.113.10"},
			"retention":        map[string]any{"keep_releases": 3},
		},
	})

	server, err := s.ServerRetentionPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if server != retention.DefaultPolicy() {
		t.Fatalf("server default = %+v, want D30 defaults", server)
	}
	stored := retention.DefaultPolicy()
	stored.KeepReleases = 8
	stored.ArtifactGraceDays = 30
	if err := s.SetServerRetentionPolicy(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if read, err := s.ServerRetentionPolicy(ctx); err != nil || read != stored {
		t.Fatalf("stored server policy = %+v err=%v, want %+v", read, err, stored)
	}
	var invalidWrites int
	badServer := retention.DefaultPolicy()
	badServer.KeepReleases = 0
	if err := s.SetServerRetentionPolicy(ctx, badServer); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("invalid policy error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_server_settings`).Scan(&invalidWrites); err != nil || invalidWrites != 1 {
		t.Fatalf("settings rows = %d err=%v, want the single valid row", invalidWrites, err)
	}

	effective, err := s.EffectiveRetentionPolicy(ctx, "retention", "production")
	if err != nil {
		t.Fatal(err)
	}
	if effective.KeepReleases != 3 || effective.ArtifactGraceDays != 30 {
		t.Fatalf("effective = %+v, want keep_releases=3 artifact_grace_days=30", effective)
	}
	if effective.KeepNewerThanDays != retention.DefaultKeepNewerThanDays || effective.BuildpackCacheMaxBytes != retention.DefaultBuildpackCacheMaxBytes {
		t.Fatalf("unoverridden fields did not inherit the server default: %+v", effective)
	}
	if _, err := s.EffectiveRetentionPolicy(ctx, "retention", "missing"); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("unknown environment error = %v", err)
	}

	// A config change commits another revision. The retention block must be
	// carried forward unchanged by that unrelated desired-state change.
	platformSubmit(t, s, platform.KindConfigSet, "env/retention/production", "retention-config", map[string]any{
		"config": map[string]string{"LOG_LEVEL": "debug"},
	})
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT r.retention FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='retention' AND e.name='production' AND r.id=e.current_revision_id`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var carried retention.Override
	if err := json.Unmarshal(raw, &carried); err != nil {
		t.Fatal(err)
	}
	if carried.KeepReleases == nil || *carried.KeepReleases != 3 {
		t.Fatalf("current revision retention = %s, want the carried keep_releases=3 override", raw)
	}
	effective, err = s.EffectiveRetentionPolicy(ctx, "retention", "production")
	if err != nil {
		t.Fatal(err)
	}
	if effective.KeepReleases != 3 {
		t.Fatalf("effective after a config change = %+v, want keep_releases=3", effective)
	}

	runID, err := retention.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_retention_runs
        (id,status,planned_items,removed_items,planned_bytes,reclaimed_bytes,errors,finished_at)
        VALUES($1,'succeeded','[{"kind":"release","reference":"rel_1"}]'::jsonb,'[]'::jsonb,2048,1024,'[]'::jsonb,clock_timestamp())`,
		runID); err != nil {
		t.Fatal(err)
	}
	var reclaimed int64
	if err := pool.QueryRow(ctx, `SELECT reclaimed_bytes FROM eregion_retention_runs WHERE id=$1`, runID).Scan(&reclaimed); err != nil || reclaimed != 1024 {
		t.Fatalf("retention run reclaimed = %d err=%v", reclaimed, err)
	}
}
