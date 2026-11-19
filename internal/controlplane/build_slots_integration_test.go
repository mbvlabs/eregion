package controlplane

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestIntegrationBuildSlotsOperatorCleanup(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	stateDir := t.TempDir()
	identity := testRegistryHostIdentityFor("local", stateDir)
	store := newTestEntities(t, pool)
	if _, err := AssignWorkspace(ctx, store, "test", identity); err != nil {
		t.Fatal(err)
	}
	run, err := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, NodeStateDir: stateDir, LocalNodeName: "local",
		RegistryEndpoint: "127.0.0.1:5000", RegistryHostIdentity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "operator-slots", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	_, pinned, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep}
	if _, err := s.beginBuildDispatch(ctx, op.ID, 1, "local", pinned, "input", exec.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	actor := contract.Actor{Kind: "operator", Subject: "test-operator"}
	slots, err := s.ListBuildSlots(ctx, actor)
	if err != nil || len(slots) != 1 || slots[0].OperationID != op.ID || slots[0].Builder != "local" || slots[0].ClaimedAt.IsZero() {
		t.Fatalf("list: %+v, %v", slots, err)
	}
	other := insertOtherBuildSlot(t, s, pool)
	if _, err := s.ClearBuildSlot(ctx, actor, op.ID, false); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("active clear: %v", err)
	}
	if _, err := s.ClearBuildSlot(ctx, actor, op.ID, true); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("active preview: %v", err)
	}
	if err := s.failPlatformOperation(ctx, op.ID, 1, contract.Unavailable, "test"); err != nil {
		t.Fatal(err)
	}
	assertBuildSlotPreview(t, s, pool, actor, exec, stateDir)
	assertBuildSlotRetirement(t, s, pool, actor, exec, stateDir)
	assertClaimCount(t, ctx, pool, other, 1)
}

func insertOtherBuildSlot(t *testing.T, s *Service, pool *pgxpool.Pool) contract.OperationID {
	t.Helper()
	ctx := context.Background()
	op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "other-operator-slot", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_build_capacity_claims(operation_id,node_name,builder_identity,attempt) VALUES($1,'local','fixture:unverified',1)`, op.ID); err != nil {
		t.Fatal(err)
	}
	return op.ID
}

func assertBuildSlotPreview(t *testing.T, s *Service, pool *pgxpool.Pool, actor contract.Actor, exec engine.Execution, stateDir string) {
	t.Helper()
	ctx := context.Background()
	preview, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, true)
	if err != nil || !preview.DryRun || preview.Cleared {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 1)
	e, err := engine.New(stateDir, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := e.IsRetired(exec); err != nil || retired {
		t.Fatalf("preview retired: %v %v", retired, err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE entry->>'action'='build.slot.clear'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("preview audit: %d %v", audits, err)
	}
}

func assertBuildSlotRetirement(t *testing.T, s *Service, pool *pgxpool.Pool, actor contract.Actor, exec engine.Execution, stateDir string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='replacement' WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	mismatch, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false)
	if err != nil || mismatch.Cleared || mismatch.Reason == "" {
		t.Fatalf("changed identity clear: %+v %v", mismatch, err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 1)
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='' WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	lock, err := engine.TryEffectLock(ctx, stateDir, exec)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false)
	lock()
	if err != nil || blocked.Cleared || blocked.Reason == "" {
		t.Fatalf("locked clear: %+v %v", blocked, err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 1)
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_integration_audit(id,entry,recorded_at) VALUES($1,'{}'::jsonb,clock_timestamp())`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("audit failure: %v", err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 1)
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_integration_audit WHERE id=$1`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false)
	if err != nil || !cleared.Cleared {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 0)
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE entry->>'action'='build.slot.clear' AND entry->>'actor'='test-operator'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("clear audit: %d %v", audits, err)
	}
}
