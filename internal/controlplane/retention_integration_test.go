package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"domain":           "retention.example.com",
			"domain_addresses": []string{"203.0.113.10"},
			"retention":        map[string]any{"keep_releases": 3},
		}},
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

// TestIntegrationPrunedReleaseRefusesDeployAndRollback proves D30: a release
// row stays, but deploying it explicitly or rolling back to it refuses with
// precondition_failed before any deployment is recorded.
func TestIntegrationPrunedReleaseRefusesDeployAndRollback(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "pruned-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "pruned-create", map[string]any{
		"name": "pruned",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"domain":           "pruned.example.com",
			"domain_addresses": []string{"203.0.113.10"},
		}},
	})
	prunedAt := time.Date(2026, 11, 3, 10, 30, 0, 0, time.UTC)
	releaseID := insertRetentionRelease(t, pool, "pruned", "production", &prunedAt)

	for _, test := range []struct {
		name  string
		kind  string
		key   string
		input map[string]any
	}{
		{name: "deploy explicit release", kind: platform.KindDeploy, key: "pruned-deploy", input: map[string]any{"release_id": releaseID}},
		{name: "rollback", kind: platform.KindRollback, key: "pruned-rollback", input: map[string]any{"to_release_id": releaseID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.Submit(ctx, platformSubmission(test.kind, "env/pruned/production", test.key, test.input))
			if err == nil {
				t.Fatal("submission to a pruned release was accepted")
			}
			fault := contract.Classify(err)
			if fault.Code != contract.PreconditionFailed {
				t.Fatalf("code = %s, want %s: %v", fault.Code, contract.PreconditionFailed, err)
			}
			for _, want := range []string{string(releaseID), "2026-11-03T10:30:00Z"} {
				if !strings.Contains(fault.Message, want) {
					t.Fatalf("message %q does not name %q", fault.Message, want)
				}
			}
			if !strings.Contains(strings.ToLower(fault.Hint), "deploy a new build") {
				t.Fatalf("hint %q does not tell the operator to deploy a new build", fault.Hint)
			}
			var operations, deployments int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, test.key).Scan(&operations); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_deployments d
                JOIN eregion_platform_environments e ON e.id=d.environment_id
                JOIN eregion_platform_apps a ON a.id=e.app_id
                WHERE a.name='pruned' AND e.name='production'`).Scan(&deployments); err != nil {
				t.Fatal(err)
			}
			if operations != 0 || deployments != 0 {
				t.Fatalf("refused submission wrote %d operation(s) and %d deployment(s)", operations, deployments)
			}
		})
	}
}

func insertRetentionRelease(t *testing.T, pool *pgxpool.Pool, app, environment string, prunedAt *time.Time) platform.ReleaseID {
	t.Helper()
	ctx := context.Background()
	var appID, environmentID string
	if err := pool.QueryRow(ctx, `SELECT e.app_id,e.id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2`, app, environment).
		Scan(&appID, &environmentID); err != nil {
		t.Fatal(err)
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES($1,$2,$3,'system','retention-test',$4,'retention-test','{}'::jsonb,'succeeded')`,
		operationID, platform.KindDeploy, "env/"+app+"/"+environment, "retention-release:"+string(releaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_releases
        (id,operation_id,app_id,environment_id,image,source,status,pruned_at)
        VALUES($1,$2,$3,$4,'ghcr.io/acme/pruned:2026-11-03','{"type":"image"}'::jsonb,'ready',$5)`,
		releaseID, operationID, appID, environmentID, prunedAt); err != nil {
		t.Fatal(err)
	}
	return releaseID
}

// TestIntegrationPrunedReleasePreviewsRefuse proves the dry-run planner applies
// the same D30 refusal as submission: an explicit deploy release preview, an
// explicit rollback preview, and a default rollback preview whose default
// target is pruned all return precondition_failed. The default rollback
// submission refuses the same target, so preview and submit agree.
func TestIntegrationPrunedReleasePreviewsRefuse(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "preview-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "preview-create", map[string]any{
		"name": "preview",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"domain":           "preview.example.com",
			"domain_addresses": []string{"203.0.113.10"},
		}},
	})
	prunedAt := time.Date(2026, 11, 3, 10, 30, 0, 0, time.UTC)
	releaseID := insertRetentionRelease(t, pool, "preview", "production", &prunedAt)

	assertPruned := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("a pruned release was accepted")
		}
		fault := contract.Classify(err)
		if fault.Code != contract.PreconditionFailed {
			t.Fatalf("code = %s, want %s: %v", fault.Code, contract.PreconditionFailed, err)
		}
		for _, want := range []string{string(releaseID), "2026-11-03T10:30:00Z"} {
			if !strings.Contains(fault.Message, want) {
				t.Fatalf("message %q does not name %q", fault.Message, want)
			}
		}
		if !strings.Contains(strings.ToLower(fault.Hint), "deploy a new build") {
			t.Fatalf("hint %q does not tell the operator to deploy a new build", fault.Hint)
		}
	}
	preview := func(kind string, input any) error {
		t.Helper()
		_, err := previewSubmission(t, s, kind, "env/preview/production", input)
		return err
	}

	t.Run("deploy release preview", func(t *testing.T) {
		assertPruned(t, preview(platform.KindDeploy, map[string]any{"release_id": releaseID}))
	})
	t.Run("explicit rollback preview", func(t *testing.T) {
		assertPruned(t, preview(platform.KindRollback, map[string]any{"to_release_id": releaseID}))
	})

	// A successful deployment of the pruned release makes it the default
	// rollback target (previousRollbackRelease's one-step history fallback).
	insertRetentionDeployment(t, pool, "preview", "production", releaseID)
	t.Run("default rollback preview", func(t *testing.T) {
		assertPruned(t, preview(platform.KindRollback, map[string]any{}))
	})
	t.Run("default rollback submission", func(t *testing.T) {
		_, err := s.Submit(ctx, platformSubmission(platform.KindRollback, "env/preview/production", "preview-default-rollback", map[string]any{}))
		assertPruned(t, err)
	})
	var operations, deployments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key='preview-default-rollback'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_deployments d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='preview' AND e.name='production'`).Scan(&deployments); err != nil {
		t.Fatal(err)
	}
	if operations != 0 || deployments != 1 {
		t.Fatalf("refused default rollback wrote %d operation(s); deployments = %d, want only the one-step history row", operations, deployments)
	}
}

func previewSubmission(t *testing.T, s *Service, kind, target string, input any) (platform.Preview, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := platform.Default().Normalize(kind, target, raw)
	if err != nil {
		t.Fatal(err)
	}
	return s.Preview(context.Background(), testActor, normalized, "")
}

func insertRetentionDeployment(t *testing.T, pool *pgxpool.Pool, app, environment string, releaseID platform.ReleaseID) {
	t.Helper()
	ctx := context.Background()
	var appID, environmentID string
	if err := pool.QueryRow(ctx, `SELECT e.app_id,e.id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2`, app, environment).
		Scan(&appID, &environmentID); err != nil {
		t.Fatal(err)
	}
	deploymentID, err := platform.NewDeploymentID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES($1,$2,$3,'system','retention-test',$4,'retention-test','{}'::jsonb,'succeeded')`,
		operationID, platform.KindDeploy, "env/"+app+"/"+environment, "retention-deployment:"+string(deploymentID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployments
        (id,operation_id,app_id,environment_id,action,release_id,state)
        VALUES($1,$2,$3,$4,'deploy',$5,'succeeded')`,
		deploymentID, operationID, appID, environmentID, releaseID); err != nil {
		t.Fatal(err)
	}
}
