package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// TestIntegrationEntitiesRetentionAndResults drives retention run/settings
// persistence and the result projection reads that were still raw.
func TestIntegrationEntitiesRetentionAndResults(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	checkRetentionSettings(t, ctx, store)
	checkRetentionRunLifecycle(t, store)
	checkRetentionResults(t, store)
	checkRetentionPlatformReuse(t, pool, store)
}

func checkRetentionSettings(t *testing.T, ctx context.Context, store *entities.Store) {
	t.Helper()
	if _, found, err := store.Retention().GetServerSettings(ctx); err != nil || found {
		t.Fatalf("empty settings found=%v err=%v", found, err)
	}
	policy := retention.DefaultPolicy()
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Retention().SetServerPolicy(ctx, raw); err != nil {
		t.Fatal(err)
	}
	settings, found, err := store.Retention().GetServerSettings(ctx)
	if err != nil || !found || settings.GCPending || len(settings.Retention) == 0 {
		t.Fatalf("settings=%+v found=%v err=%v", settings, found, err)
	}
	if pending, err := store.Retention().GCPending(ctx); err != nil || pending {
		t.Fatalf("initial gc pending=%v err=%v", pending, err)
	}
	if err := store.Retention().SetGCPending(ctx, true); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Retention().GCPending(ctx); err != nil || !pending {
		t.Fatalf("gc pending=%v err=%v", pending, err)
	}
}

func checkRetentionRunLifecycle(t *testing.T, store *entities.Store) {
	t.Helper()
	ctx := context.Background()
	op := insertDefinitionOperation(t, store, "platform.retention.prune", "node/foundation", "retention-run-1", json.RawMessage(`{"node":"foundation"}`))
	runID := "run_foundation_1"
	started := time.Now().UTC().Add(-time.Minute)
	if err := store.Retention().InsertRun(ctx, entities.RetentionRunInsert{
		ID: runID, OperationID: op, Status: string(retention.RunRunning),
		PlannedItems: json.RawMessage(`[{"kind":"release","reference":"rel-1"}]`),
		RemovedItems: json.RawMessage(`[]`), Errors: json.RawMessage(`[]`),
		StartedAt: started, GCGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	run, found, err := store.Retention().LatestRunForOperation(ctx, op)
	if err != nil || !found || run.ID != runID || run.PlannedBytes != 0 || run.GCGeneration != 1 {
		t.Fatalf("run=%+v found=%v err=%v", run, found, err)
	}
	finished := started.Add(30 * time.Second)
	cacheBytes := int64(2048)
	run.Status = string(retention.RunSucceeded)
	run.FinishedAt = &finished
	run.CacheBytes = &cacheBytes
	run.RemovedItems = json.RawMessage(`[{"kind":"release","reference":"rel-1"}]`)
	if err := store.Retention().UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run, _, err = store.Retention().LatestRunForOperation(ctx, op)
	if err != nil || run.FinishedAt == nil || run.CacheBytes == nil || *run.CacheBytes != 2048 {
		t.Fatalf("updated run=%+v err=%v", run, err)
	}
	facts, found, err := store.Retention().LastRunFacts(ctx)
	if err != nil || !found || facts.Status != string(retention.RunSucceeded) ||
		facts.PlannedItems != 1 || facts.RemovedItems != 1 || facts.CacheBytes == nil {
		t.Fatalf("facts=%+v found=%v err=%v", facts, found, err)
	}
	success, found, err := store.Retention().LastSuccessAt(ctx)
	if err != nil || !found || success == nil || success.Before(started) {
		t.Fatalf("last success=%v found=%v err=%v", success, found, err)
	}
}

func checkRetentionResults(t *testing.T, store *entities.Store) {
	t.Helper()
	ctx := context.Background()
	resultOp := insertDefinitionOperation(t, store, "platform.deploy", "env/results/staging", "results-op", json.RawMessage(`{"app":"results","environment":"staging"}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		op, found, err := tx.Operations().Lock(ctx, resultOp)
		if err != nil || !found {
			t.Fatalf("result op found=%v err=%v", found, err)
		}
		if err := tx.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "warning", Stage: "health", Message: "Node step failed: health check",
			CandidateDiagnostic: &contract.CandidateDiagnostic{
				DeploymentID: "dep-results", Status: "failed", RedactionContext: "logs",
			},
		}); err != nil {
			return err
		}
		if err := tx.Steps().Insert(ctx, resultOp, "platform-health-1", "inputs-health", 0, "exec-health"); err != nil {
			return err
		}
		_, err = tx.Steps().Fail(ctx, resultOp, "platform-health-1", contract.Unavailable, "health failed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.Results().CandidateDiagnostics(ctx, resultOp, 0, 10)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Status != "failed" ||
		diagnostics[0].DeploymentID != "dep-results" || diagnostics[0].RedactionContext != "logs" {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
	if stage, err := store.Results().FailedStage(ctx, resultOp, 0); err != nil || stage != "health" {
		t.Fatalf("failed stage=%q err=%v", stage, err)
	}

	// Without the warning, the same read falls back to the failed step.
	fallbackOp := insertDefinitionOperation(t, store, "platform.deploy", "env/results/staging", "results-fallback", json.RawMessage(`{}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := tx.Steps().Insert(ctx, fallbackOp, "platform-cleanup-1", "inputs-cleanup", 1, "exec-cleanup"); err != nil {
			return err
		}
		_, err := tx.Steps().Fail(ctx, fallbackOp, "platform-cleanup-1", contract.Unavailable, "cleanup failed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stage, err := store.Results().FailedStage(ctx, fallbackOp, 1); err != nil || stage != "platform-cleanup-1" {
		t.Fatalf("fallback stage=%q err=%v", stage, err)
	}
}

func checkRetentionPlatformReuse(t *testing.T, pool *pgxpool.Pool, store *entities.Store) {
	t.Helper()
	ctx := context.Background()
	fixture := newPlatformFixture(t, pool, "retention-reuse")
	if _, err := store.Platform().Environments().RetentionRows(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Platform().Releases().RetentionRows(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Platform().Deployments().RetentionRows(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Platform().Releases().ActiveForNode(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Platform().Environments().RetentionOverride(ctx, fixture.app.Name, fixture.env.Name); err != nil {
		t.Fatal(err)
	}
	_ = platform.ReleaseID("")
}
