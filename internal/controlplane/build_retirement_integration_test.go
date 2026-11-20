package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func assertClaimCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id contract.OperationID, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_build_capacity_claims WHERE operation_id=$1`, id).Scan(&count); err != nil || count != want {
		t.Fatalf("claims for %s = %d, %v; want %d", id, count, err, want)
	}
}

// Uses PostgreSQL, River and the real local node engine; it never runs Pack or
// Docker. The held node effect lock represents a still-live execution.
func TestIntegrationBuildDispatchAndRetirement(t *testing.T) { //nolint:gocognit,gocyclo // One end-to-end scenario covers intent, contention, and retirement.
	pool, _ := testPool(t, true)
	stateDir := t.TempDir()
	identity := testRegistryHostIdentityFor("local", stateDir)
	store := newTestEntities(t, pool)
	if _, err := AssignWorkspace(context.Background(), store, "test", identity); err != nil {
		t.Fatal(err)
	}
	local, err := node.NewLocal(stateDir, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewNodeResolver(store.Nodes(), ResolverConfig{}, func() node.Transport { return local })
	run, err := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: local, Resolver: resolver,
		RegistryEndpoint: "127.0.0.1:5000", RegistryHostIdentity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	ctx := context.Background()
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	submit := func(key string) contract.OperationID {
		t.Helper()
		op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: key, Input: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
			t.Fatalf("claim attempt: %v %v", ok, err)
		}
		return op.ID
	}
	holder, blocked, invalid := submit("retire-holder"), submit("retire-blocked"), submit("retire-invalid")
	w := &platformWorker{service: s}
	_, err = w.executeBuildPack(ctx, blocked, 1, platform.Environment{Node: "local"},
		platform.Release{}, platform.BuildRecord{}, nil, platform.PackPayload{}, nil)
	if err == nil {
		t.Fatal("unavailable transport unexpectedly succeeded")
	}
	assertClaimCount(t, ctx, pool, blocked, 0)
	_, selectedIdentity, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(selectedIdentity, ":unverified") {
		t.Fatalf("injected local transport has unverified identity: %s", selectedIdentity)
	}
	invalidExec := engine.Execution{OperationID: invalid, Attempt: 1, Step: deploymentPackStep}
	if _, err := s.beginStep(ctx, invalid, 1, deploymentPackStep, "original", invalidExec.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginBuildDispatch(ctx, invalid, 1, "local", selectedIdentity, "changed", invalidExec.ExecutionID(), "", ""); err == nil {
		t.Fatal("changed inputs unexpectedly admitted")
	}
	assertClaimCount(t, ctx, pool, invalid, 0)
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='replacement' WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	blockedExec := engine.Execution{OperationID: blocked, Attempt: 1, Step: deploymentPackStep}
	if _, err := s.beginBuildDispatch(ctx, blocked, 1, "local", selectedIdentity, "blocked-input", blockedExec.ExecutionID(), "", ""); err == nil {
		t.Fatal("dispatch admitted with a transport selected before identity changed")
	}
	assertClaimCount(t, ctx, pool, blocked, 0)
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='' WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: holder, Attempt: 1, Step: deploymentPackStep, RequestID: "retire-test"}
	if already, err := s.beginBuildDispatch(ctx, holder, 1, "local", selectedIdentity, "holder-input", exec.ExecutionID(), "", ""); err != nil || already {
		t.Fatalf("begin dispatch: %v %v", already, err)
	}
	if _, err := s.beginBuildDispatch(ctx, blocked, 1, "local", selectedIdentity, "blocked-input", blockedExec.ExecutionID(), "", ""); err == nil {
		t.Fatal("capacity contention was admitted")
	} else {
		var snooze *river.JobSnoozeError
		if !errors.As(err, &snooze) {
			t.Fatalf("capacity contention = %v, want snooze", err)
		}
	}
	assertClaimCount(t, ctx, pool, blocked, 0)
	if _, found, err := s.stepRecord(ctx, blocked, deploymentPackStep); err != nil || found {
		t.Fatalf("busy operation recorded dispatch intent: %v %v", found, err)
	}
	if err := s.failPlatformOperation(ctx, holder, 1, contract.Unavailable, "test retirement"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='replacement' WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileTerminalBuildCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	assertClaimCount(t, ctx, pool, holder, 1)
	e, err := engine.New(stateDir, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := e.IsRetired(exec); err != nil || retired {
		t.Fatalf("replacement identity retired original claim = %v, %v", retired, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='' WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	lock, err := engine.TryEffectLock(ctx, stateDir, exec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileTerminalBuildCapacity(ctx); err != nil {
		lock()
		t.Fatal(err)
	}
	assertClaimCount(t, ctx, pool, holder, 1)
	lock()
	// The real River maintenance worker retries retirement after the node
	// effect is quiescent, then makes capacity available.
	startWorkers(t, run)
	await(t, 15*time.Second, func() bool {
		var count int
		return pool.QueryRow(ctx, `SELECT count(*) FROM eregion_build_capacity_claims WHERE operation_id=$1`, holder).Scan(&count) == nil && count == 0
	})
	payload, err := json.Marshal(platform.PackPayload{
		Step: deploymentPackStep, Image: "127.0.0.1:5000/example:latest",
		Path: t.TempDir(), Builder: "paketobuildpacks/builder-jammy-base", Registry: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := s.resolver.TransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	response, err := tr.Call(ctx, node.Request{Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec, Payload: payload}, nil)
	if err != nil || response.Status != engine.StatusFailed || !strings.Contains(response.Detail, "retired") {
		t.Fatalf("delayed node dispatch after retirement = %+v, %v", response, err)
	}
}

func TestIntegrationBuildIdentityRejectsMismatchedLocalState(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	stateDir := t.TempDir()
	identity := testRegistryHostIdentityFor("local", stateDir)
	store := newTestEntities(t, pool)
	if _, err := AssignWorkspace(ctx, store, "test", identity); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	local, err := node.NewLocal(t.TempDir(), "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Transport: local, NodeStateDir: stateDir, RegistryHostIdentity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	tr, pinned, err := s.buildTransportFor(ctx, "local")
	if err != nil || !strings.HasSuffix(pinned, ":unverified") {
		t.Fatalf("mismatched state directory identity = %q, %v", pinned, err)
	}
	op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "unverified-build", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim attempt: %v %v", ok, err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep}
	if _, err := s.beginBuildDispatch(ctx, op.ID, 1, "local", "", "input", exec.ExecutionID(), "", ""); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("empty identity dispatch = %v, want precondition failure", err)
	}
	if _, err := s.beginBuildDispatch(ctx, op.ID, 1, "local", pinned, "input", exec.ExecutionID(), "", ""); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unverified dispatch = %v, want precondition failure", err)
	}
	w := &platformWorker{service: s}
	payload := platform.PackPayload{Step: deploymentPackStep, Image: "127.0.0.1:5000/example:latest",
		Path: t.TempDir(), Builder: "paketobuildpacks/builder-jammy-base", Registry: "127.0.0.1:5000"}
	if _, err := w.runStepOnBuild(ctx, tr, op.ID, 1, payload, nil, "local", pinned, "", "bld_ffffffffffffffffffffffffffffffff", "rel_ffffffffffffffffffffffffffffffff"); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unverified managed build = %v, want precondition failure", err)
	}
	assertClaimCount(t, ctx, pool, op.ID, 0)
	if _, found, err := s.stepRecord(ctx, op.ID, deploymentPackStep); err != nil || found {
		t.Fatalf("unverified dispatch recorded intent: %v %v", found, err)
	}
}

func TestIntegrationBuildRetirementPinsBuilderAndClaim(t *testing.T) {
	pool, _ := testPool(t, true)
	stateDir := t.TempDir()
	ctx := context.Background()
	if _, err := AssignWorkspace(ctx, newTestEntities(t, pool), "test", testRegistryHostIdentityFor("local", stateDir)); err != nil {
		t.Fatal(err)
	}
	s := testRuntime(t, pool, 0, stateDir).Service
	for _, name := range []string{"local", "other"} {
		if err := insertFixtureNode(ctx, pool, name, []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
			t.Fatal(err)
		}
	}
	op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "retire-pinned", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim attempt: %v %v", ok, err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep, RequestID: "test"}
	_, identity, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginBuildDispatch(ctx, op.ID, 1, "local", identity, "input", exec.ExecutionID(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.adoptBuildCapacity(ctx, op.ID, 1, "other", ""); err == nil {
		t.Fatal("recovery accepted a different builder")
	}
	if err := s.adoptBuildCapacity(ctx, op.ID, 1, "local", ""); err == nil {
		t.Fatal("recovery accepted a name-only builder identity")
	}
	if err := s.failPlatformOperation(ctx, op.ID, 1, contract.Unavailable, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET identity_fingerprint='replacement' WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileTerminalBuildCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	assertClaimCount(t, ctx, pool, op.ID, 1)
	e, err := engine.New(stateDir, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := e.IsRetired(exec); err != nil || retired {
		t.Fatalf("wrong builder retired = %v, %v", retired, err)
	}
	assertStaleRetirementCannotDeleteReplacement(t, ctx, pool, s, op.ID)
}

func assertStaleRetirementCannotDeleteReplacement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, id contract.OperationID) {
	t.Helper()
	claims, err := s.entities.BuildCapacity().TerminalClaims(ctx, 8)
	if err != nil || len(claims) != 1 {
		t.Fatalf("terminal claims = %+v, %v", claims, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_build_capacity_claims WHERE operation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_build_capacity_claims(operation_id,node_name,builder_identity,attempt) VALUES ($1,'other',$2,1)`, id, claims[0].Identity); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.entities.BuildCapacity().DeleteTerminal(ctx, claims[0]); err != nil || deleted {
		t.Fatalf("stale retirement deleted replacement claim: %v, %v", deleted, err)
	}
	assertClaimCount(t, ctx, pool, id, 1)
}
