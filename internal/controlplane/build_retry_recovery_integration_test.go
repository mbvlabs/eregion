package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

func newRetryRecoveryFixture(t *testing.T) pointerBuildFixture {
	t.Helper()
	ctx := context.Background()
	f := newPointerBuildFixture(t)
	binding := *f.service.registryHostIdentity
	binding.NodeName = "local"
	f.service.registryHostIdentity = &binding
	f.service.registryEndpoint = "127.0.0.1:5000"
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_nodes SET roles=$1, capabilities=$2 WHERE name='local'`,
		[]string{platform.RoleApplication, platform.RoleBuilder}, []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.markBuildFailed(ctx, f.build, contract.NewFault(contract.Unavailable, "previous failure", ""), f.release); err != nil {
		t.Fatal(err)
	}
	f.op = platformSubmit(t, f.service, platform.KindBuildRetry, "build/"+string(f.build), "retry-recovery", map[string]any{})
	build, found, err := f.service.buildByOperation(ctx, f.op.ID)
	if err != nil || !found {
		t.Fatalf("retry build: %v %v", found, err)
	}
	f.build, f.release = build.ID, build.ReleaseID
	return f
}

func TestIntegrationCancellingBuildRetryRetiresDelayedRun(t *testing.T) {
	ctx := context.Background()
	f := newRetryRecoveryFixture(t)
	op, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	recordCompletedRetryPack(t, f.service, f.pool, op.ID)
	record, _, err := f.service.stepRecord(ctx, op.ID, deploymentPackStep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='running', result=NULL WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
		t.Fatal(err)
	}
	tr, identity, err := f.service.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.beginBuildDispatch(ctx, op.ID, 1, "local", identity, record.InputsHash, record.ExecutionID, f.build, f.release); err != nil {
		t.Fatal(err)
	}
	build, found, err := f.service.buildByOperation(ctx, op.ID)
	if err != nil || !found {
		t.Fatalf("build: %v %v", found, err)
	}
	release, err := f.service.releaseForOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref := build.CommitSHA
	if ref == "" {
		ref = build.Source.Ref
	}
	payload, err := packPayloadForBuild(build, release, ref, f.service.registryEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep, RequestID: "delayed-run"}
	if _, err := f.service.RequestCancel(ctx, testActor, op.ID); err != nil {
		t.Fatal(err)
	}
	// Even a replay with an existing same-attempt intent must re-enter
	// beginBuildDispatch; it cannot admit a new Run after cancellation.
	if _, err := f.service.beginBuildDispatch(ctx, op.ID, 1, "local", identity, record.InputsHash, record.ExecutionID, f.build, f.release); !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("replay admitted after cancellation: %v", err)
	}
	if _, err := (&platformWorker{service: f.service}).runStepOnBuild(ctx, tr, op.ID, 1, payload, nil, "local", identity, build.Source.Type, f.build, f.release); !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("existing-intent worker replay after cancellation: %v", err)
	}
	// Exercise the active worker's cancellation path, not only maintenance.
	if err := (&platformWorker{service: f.service}).settle(ctx, op.ID, 1, errPlatformCancelled); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.Get(ctx, testActor, op.ID)
	if err != nil || current.State != contract.Canceled {
		t.Fatalf("cancelled operation: %+v %v", current, err)
	}
	assertClaimCount(t, ctx, f.pool, op.ID, 0)
	response, err := tr.Call(ctx, node.Request{Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec, Payload: raw}, nil)
	if err != nil || response.Status != engine.StatusFailed || !strings.Contains(response.Detail, "retired") {
		t.Fatalf("already-admitted delayed Run: %+v %v", response, err)
	}
}

func TestIntegrationCancellingBuildRetryRetainsUncertainApply(t *testing.T) {
	ctx := context.Background()
	f := newRetryRecoveryFixture(t)
	op, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	recordCompletedRetryPack(t, f.service, f.pool, op.ID)
	record, _, err := f.service.stepRecord(ctx, op.ID, deploymentPackStep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='running', result=NULL WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
		t.Fatal(err)
	}
	tr, identity, err := f.service.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.beginBuildDispatch(ctx, op.ID, 1, "local", identity, record.InputsHash, record.ExecutionID, f.build, f.release); err != nil {
		t.Fatal(err)
	}
	local, ok := tr.(*node.Local)
	if !ok {
		t.Fatalf("expected local node, got %T", tr)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentPackStep}
	unlock, err := engine.TryEffectLock(ctx, local.StateDir(), exec)
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.service.RequestCancel(ctx, testActor, op.ID)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	err = f.service.reconcilePlatformOperation(ctx, op)
	unlock()
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("locked effect settled: %v", err)
	}
	assertClaimCount(t, ctx, f.pool, op.ID, 1)
	sum := sha256.Sum256([]byte(exec.EffectKey()))
	if err := os.WriteFile(filepath.Join(local.StateDir(), "build-applying-"+hex.EncodeToString(sum[:])), []byte("uncertain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcilePlatformOperation(ctx, op); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("uncertain apply settled: %v", err)
	}
	current, err := f.service.Get(ctx, testActor, op.ID)
	if err != nil || current.State != contract.Cancelling {
		t.Fatalf("uncertain state: %+v %v", current, err)
	}
	assertClaimCount(t, ctx, f.pool, op.ID, 1)
}

// Seed the durable node effect directly: this exercises the real local node
// journal/status boundary, but does not claim that Pack or an image was built.
func TestIntegrationBuildRetryRecoversDurablePackAcknowledgement(t *testing.T) { //nolint:gocognit,gocyclo // Ordered boundary and mismatch checks.
	for _, cancelling := range []bool{false, true} {
		name := "running"
		if cancelling {
			name = "cancelling"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newRetryRecoveryFixture(t)
			op, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1)
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			recordCompletedRetryPack(t, f.service, f.pool, op.ID)
			record, _, err := f.service.stepRecord(ctx, op.ID, deploymentPackStep)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='running', result=NULL WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
				t.Fatal(err)
			}
			tr, identity, err := f.service.buildTransportFor(ctx, "local")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.beginBuildDispatch(ctx, op.ID, 1, "local", identity, record.InputsHash, record.ExecutionID, f.build, f.release); err != nil {
				t.Fatal(err)
			}
			local, ok := tr.(*node.Local)
			if !ok {
				t.Fatalf("expected local node, got %T", tr)
			}
			e, err := engine.New(local.StateDir(), "local", nil)
			if err != nil {
				t.Fatal(err)
			}
			exec := engine.Execution{OperationID: op.ID, Attempt: record.Attempt, Step: record.Step}
			path := e.EffectRecordPath(exec)
			if err := engine.EnsureDir(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.SaveEffectRecord(path, engine.EffectRecord{OperationID: string(op.ID), Step: record.Step,
				InputsHash: record.InputsHash, AppliedAttempt: record.Attempt, ExecutionID: record.ExecutionID,
				CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Result: record.Result}); err != nil {
				t.Fatal(err)
			}
			op, _, ok, err = f.service.claimPlatform(ctx, op.ID, 2)
			if err != nil || !ok {
				t.Fatalf("claim successor: %v %v", ok, err)
			}
			if cancelling {
				if _, err := f.service.RequestCancel(ctx, testActor, op.ID); err != nil {
					t.Fatal(err)
				}
				op, err = f.service.Get(ctx, testActor, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_build_placements SET node_identity='wrong' WHERE operation_id=$1`, op.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.service.reconcilePlatformOperation(ctx, op); contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("identity mismatch: %v", err)
			}
			assertClaimCount(t, ctx, f.pool, op.ID, 1)
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_build_placements SET node_identity=$2 WHERE operation_id=$1`, op.ID, identity); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET inputs_hash='wrong' WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
				t.Fatal(err)
			}
			if err := f.service.reconcilePlatformOperation(ctx, op); contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("hash mismatch: %v", err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET inputs_hash=$3 WHERE operation_id=$1 AND step=$2`, op.ID, record.Step, record.InputsHash); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET execution_id='wrong' WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
				t.Fatal(err)
			}
			if err := f.service.reconcilePlatformOperation(ctx, op); contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("execution mismatch: %v", err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET execution_id=$3 WHERE operation_id=$1 AND step=$2`, op.ID, record.Step, record.ExecutionID); err != nil {
				t.Fatal(err)
			}
			if err := insertFixtureNode(ctx, f.pool, "moved", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_environments SET build_node='moved' WHERE id=$1`, f.env.ID); err != nil {
				t.Fatal(err)
			}
			// Neither current registry configuration nor the mutable release
			// image is the retained dispatch hash.
			f.service.registryEndpoint = "127.0.0.1:5999"
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_releases SET image='changed/image:tag' WHERE id=$1`, f.release); err != nil {
				t.Fatal(err)
			}
			if cancelling {
				_, err = (&platformWorker{service: f.service}).handleCancellingPlatformWork(ctx, op, nil, op.Attempt)
			} else {
				err = f.service.reconcilePlatformOperation(ctx, op)
			}
			if cancelling && err != nil {
				t.Fatalf("cancel recovery: %v", err)
			}
			if !cancelling && err == nil {
				t.Fatal("nonexistent image was accepted")
			}
			recovered, found, getErr := f.service.stepRecord(ctx, op.ID, record.Step)
			if getErr != nil || !found || recovered.State != "completed" || recovered.ExecutionID != record.ExecutionID {
				t.Fatalf("acknowledgement: %+v %v %v", recovered, found, getErr)
			}
			assertClaimCount(t, ctx, f.pool, op.ID, 0)
			current, getErr := f.service.Get(ctx, testActor, op.ID)
			if getErr != nil || (cancelling && current.State != contract.Canceled) || (!cancelling && current.State == contract.Succeeded) {
				t.Fatalf("state: %+v %v", current, getErr)
			}
		})
	}
}

func TestIntegrationBuildRetryNoIntentRequiresEmptyHistory(t *testing.T) { //nolint:gocognit // Independent retained-history scenarios.
	for _, kind := range []string{"legacy-step", "placement", "claim"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newRetryRecoveryFixture(t)
			op, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1)
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			if kind == "legacy-step" { //nolint:nestif // Build each distinct historical boundary.
				exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: "platform-pack-02-v1"}
				if _, err := f.service.beginStep(ctx, op.ID, 1, exec.Step, "legacy", exec.ExecutionID()); err != nil {
					t.Fatal(err)
				}
			} else {
				recordCompletedRetryPack(t, f.service, f.pool, op.ID)
				record, _, err := f.service.stepRecord(ctx, op.ID, deploymentPackStep)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "claim" {
					if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='running' WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
						t.Fatal(err)
					}
					_, identity, err := f.service.buildTransportFor(ctx, "local")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.beginBuildDispatch(ctx, op.ID, 1, "local", identity, record.InputsHash, record.ExecutionID, f.build, f.release); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.pool.Exec(ctx, `DELETE FROM eregion_operation_steps WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
					t.Fatal(err)
				}
				if kind == "claim" {
					if _, err := f.pool.Exec(ctx, `DELETE FROM eregion_build_placements WHERE operation_id=$1`, op.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.service.reconcilePlatformOperation(ctx, op); contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("ambiguous history settled: %v", err)
			}
			current, err := f.service.Get(ctx, testActor, op.ID)
			if err != nil || current.State.Terminal() {
				t.Fatalf("operation: %+v %v", current, err)
			}
		})
	}
}

func TestIntegrationBuildRetrySettlementHonorsCurrentCancellation(t *testing.T) {
	ctx := context.Background()
	f := newRetryRecoveryFixture(t)
	op, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_releases SET status='ready' WHERE id=$1`, f.release); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RequestCancel(ctx, testActor, op.ID); err != nil {
		t.Fatal(err)
	}
	// Deliberately pass the pre-cancellation snapshot.
	if err := f.service.reconcilePlatformOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.Get(ctx, testActor, op.ID)
	if err != nil || current.State != contract.Canceled {
		t.Fatalf("cancellation lost: %+v %v", current, err)
	}
}

func TestIntegrationBuildRetryDoesNotReleaseChangedClaim(t *testing.T) {
	ctx := context.Background()
	f := newRetryRecoveryFixture(t)
	op, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	recordCompletedRetryPack(t, f.service, f.pool, op.ID)
	record, _, err := f.service.stepRecord(ctx, op.ID, deploymentPackStep)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := f.service.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an acknowledged completion with a retained claim.
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='running' WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.beginBuildDispatch(ctx, op.ID, 1, "local", identity, record.InputsHash, record.ExecutionID, f.build, f.release); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='completed' WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.service.entities.BuildCapacity().Find(ctx, op.ID)
	if err != nil || !found {
		t.Fatalf("find claim: %v %v", found, err)
	}
	placement, found, err := f.service.entities.BuildPlacement(ctx, op.ID)
	if err != nil || !found {
		t.Fatalf("find placement: %v %v", found, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_build_capacity_claims SET claim_id='replacement' WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.releaseRecoveredRetryCapacity(ctx, op, record, placement, claim, true); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("changed claim accepted: %v", err)
	}
	assertClaimCount(t, ctx, f.pool, op.ID, 1)
}

func TestIntegrationBuildRetryLeavesUnresolvedIntentUnretired(t *testing.T) {
	ctx := context.Background()
	f := newRetryRecoveryFixture(t)
	op, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	recordCompletedRetryPack(t, f.service, f.pool, op.ID)
	record, _, err := f.service.stepRecord(ctx, op.ID, deploymentPackStep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET state='running', result=NULL WHERE operation_id=$1 AND step=$2`, op.ID, record.Step); err != nil {
		t.Fatal(err)
	}
	tr, identity, err := f.service.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.beginBuildDispatch(ctx, op.ID, 1, "local", identity, record.InputsHash, record.ExecutionID, f.build, f.release); err != nil {
		t.Fatal(err)
	}
	local, ok := tr.(*node.Local)
	if !ok {
		t.Fatalf("expected local node, got %T", tr)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: record.Attempt, Step: record.Step}
	unlock, err := engine.TryEffectLock(ctx, local.StateDir(), exec)
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.reconcilePlatformOperation(ctx, op)
	unlock()
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("live intent was settled: %v", err)
	}
	assertClaimCount(t, ctx, f.pool, op.ID, 1)
	current, err := f.service.Get(ctx, testActor, op.ID)
	if err != nil || current.State.Terminal() {
		t.Fatalf("live operation: %+v %v", current, err)
	}
	if err := f.service.reconcilePlatformOperation(ctx, op); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("absent effect was settled: %v", err)
	}
	current, err = f.service.Get(ctx, testActor, op.ID)
	if err != nil || current.State.Terminal() {
		t.Fatalf("unresolved operation: %+v %v", current, err)
	}
	assertClaimCount(t, ctx, f.pool, op.ID, 1)
	e, err := engine.New(local.StateDir(), "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := e.IsRetired(exec); err != nil || retired {
		t.Fatalf("successor was retired: %v %v", retired, err)
	}
}
