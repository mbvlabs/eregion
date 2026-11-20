package controlplane

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
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
	if _, err := s.beginBuildDispatch(ctx, op.ID, 1, "local", pinned, "input", exec.ExecutionID(), "", ""); err != nil {
		t.Fatal(err)
	}
	actor := contract.Actor{Kind: "operator", Subject: "test-operator"}
	slots, err := s.ListBuildSlots(ctx, actor)
	if err != nil || len(slots) != 1 || slots[0].OperationID != op.ID || slots[0].Builder != "local" || slots[0].ClaimedAt.IsZero() {
		t.Fatalf("list: %+v, %v", slots, err)
	}
	other := insertOtherBuildSlot(t, s, pool)
	if _, err := s.ClearBuildSlot(ctx, actor, op.ID, false, false); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("active clear: %v", err)
	}
	if _, err := s.ClearBuildSlot(ctx, actor, op.ID, true, false); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("active preview: %v", err)
	}
	if err := s.failPlatformOperation(ctx, op.ID, 1, contract.Unavailable, "test"); err != nil {
		t.Fatal(err)
	}
	assertBuildSlotPreview(t, s, pool, actor, exec, stateDir)
	assertBuildSlotRetirement(t, s, pool, actor, exec, stateDir)
	assertClaimCount(t, ctx, pool, other, 1)
}

func TestIntegrationLegacyLocalBuildSlot(t *testing.T) {
	for _, scenario := range []string{"clear", "no opt in", "wrong node record", "wrong claim identity", "wrong state path", "missing dispatch", "wrong attempt", "wrong execution", "wrong input", "missing proof", "locked"} {
		t.Run(scenario, func(t *testing.T) {
			testLegacyLocalBuildSlot(t, scenario)
		})
	}
}

func testLegacyLocalBuildSlot(t *testing.T, scenario string) {
	t.Helper()
	s, pool, e, exec := setupLegacyLocalBuildSlot(t)
	ctx := context.Background()
	actor := contract.Actor{Kind: "operator", Subject: "legacy-operator"}
	preview, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, true, true)
	if err != nil || !preview.DryRun || preview.Cleared {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 1)
	if retired, err := e.IsRetired(exec); err != nil || retired {
		t.Fatalf("preview fence: %v %v", retired, err)
	}
	modifyLegacyLocalScenario(t, scenario, s, pool, e, exec)
	if scenario == "locked" {
		release, err := engine.TryEffectLock(ctx, e.StateDir, exec)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	result, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false, scenario != "no opt in")
	if err != nil || result.Cleared != (scenario == "clear") {
		t.Fatalf("clear: %+v %v", result, err)
	}
	want := 1
	if scenario == "clear" {
		want = 0
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, want)
	if retired, err := e.IsRetired(exec); err != nil || retired != (scenario == "clear") {
		t.Fatalf("fence: %v %v", retired, err)
	}
	var audits, legacyAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE entry->>'detail'='legacy local recovery')
		FROM eregion_integration_audit WHERE entry->>'action'='build.slot.clear' AND entry->>'actor'='legacy-operator'`).Scan(&audits, &legacyAudits); err != nil || audits != 1-want || legacyAudits != audits {
		t.Fatalf("audit: total=%d legacy=%d %v", audits, legacyAudits, err)
	}
}

func setupLegacyLocalBuildSlot(t *testing.T) (*Service, *pgxpool.Pool, *engine.Engine, engine.Execution) {
	t.Helper()
	pool, _ := testPool(t, true)
	ctx := context.Background()
	dir := t.TempDir()
	identity := testRegistryHostIdentityFor("local", dir)
	store := newTestEntities(t, pool)
	if _, err := AssignWorkspace(ctx, store, "test", identity); err != nil {
		t.Fatal(err)
	}
	run, err := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, NodeStateDir: dir, LocalNodeName: "local",
		RegistryEndpoint: "127.0.0.1:5000", RegistryHostIdentity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "legacy", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep}
	hash := engine.InputsHash(exec.Step, []byte("original"))
	_, pinned, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginBuildDispatch(ctx, op.ID, 1, "local", pinned, hash, exec.ExecutionID(), "", ""); err != nil {
		t.Fatal(err)
	}
	rec, found, err := s.entities.Nodes().Connection(ctx, "local")
	if err != nil || !found {
		t.Fatalf("load local node: found=%v err=%v", found, err)
	}
	// Represent a pre-guard claim; current dispatch cannot create this identity.
	if _, err := pool.Exec(ctx, `UPDATE eregion_build_capacity_claims SET builder_identity=$2 WHERE operation_id=$1`, op.ID, buildNodeIdentity(rec)+":unverified"); err != nil {
		t.Fatal(err)
	}
	if err := s.failPlatformOperation(ctx, op.ID, 1, contract.Unavailable, "test"); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(dir, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := engine.NewJournal("local")
	j.Tasks[exec.EffectKey()] = engine.Task{OperationID: string(op.ID), Step: exec.Step, Attempt: 1, ExecutionID: exec.ExecutionID(), InputsHash: hash, Status: engine.TaskFailed, Phase: engine.PhaseChecking}
	if err := j.Save(e.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	return s, pool, e, exec
}

func modifyLegacyLocalScenario(t *testing.T, scenario string, s *Service, pool *pgxpool.Pool, e *engine.Engine, exec engine.Execution) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch scenario {
	case "wrong node record":
		_, err = pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='replacement' WHERE name='local'`)
	case "wrong claim identity":
		_, err = pool.Exec(ctx, `UPDATE eregion_build_capacity_claims SET builder_identity='other:unverified' WHERE operation_id=$1`, exec.OperationID)
	case "wrong state path":
		s.registryHostIdentity.StateDir = t.TempDir()
	case "missing dispatch":
		_, err = pool.Exec(ctx, `DELETE FROM eregion_operation_steps WHERE operation_id=$1`, exec.OperationID)
	case "wrong attempt":
		_, err = pool.Exec(ctx, `UPDATE eregion_operation_steps SET attempt=2 WHERE operation_id=$1`, exec.OperationID)
	case "wrong execution":
		_, err = pool.Exec(ctx, `UPDATE eregion_operation_steps SET execution_id='other' WHERE operation_id=$1`, exec.OperationID)
	case "wrong input":
		_, err = pool.Exec(ctx, `UPDATE eregion_operation_steps SET inputs_hash=$2 WHERE operation_id=$1`, exec.OperationID, engine.InputsHash(exec.Step, []byte("other")))
	case "missing proof":
		err = os.Remove(e.JournalPath())
	}
	if err != nil {
		t.Fatal(err)
	}
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
	preview, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, true, false)
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
	mismatch, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false, false)
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
	blocked, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false, false)
	lock()
	if err != nil || blocked.Cleared || blocked.Reason == "" {
		t.Fatalf("locked clear: %+v %v", blocked, err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 1)
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_integration_audit(id,entry,recorded_at) VALUES($1,'{}'::jsonb,clock_timestamp())`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false, false); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("audit failure: %v", err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 1)
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_integration_audit WHERE id=$1`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.ClearBuildSlot(ctx, actor, exec.OperationID, false, false)
	if err != nil || !cleared.Cleared {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	assertClaimCount(t, ctx, pool, exec.OperationID, 0)
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE entry->>'action'='build.slot.clear' AND entry->>'actor'='test-operator' AND NOT entry ? 'detail'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("clear audit: %d %v", audits, err)
	}
}
