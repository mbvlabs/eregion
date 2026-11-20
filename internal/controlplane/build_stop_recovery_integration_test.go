package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

type stopRecoveryFixture struct {
	pointerBuildFixture
	op       contract.Operation
	exec     engine.Execution
	tr       node.Transport
	identity string
	hash     string
	payload  []byte
}

func newStopRecoveryFixture(t *testing.T) stopRecoveryFixture {
	t.Helper()
	f := stopRecoveryFixture{pointerBuildFixture: newPointerBuildFixture(t)}
	ctx := context.Background()
	f.op = platformSubmit(t, f.service, platform.KindBuildStop, "build/"+string(f.build), "stop-recovery", map[string]any{})
	op, _, claimed, err := f.service.claimPlatform(ctx, f.op.ID, 1)
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	f.op = op
	binding := *f.service.registryHostIdentity
	binding.NodeName = "local"
	f.service.registryHostIdentity = &binding
	f.tr, f.identity, err = f.service.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	f.exec = engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentBuildStopStep, RequestID: "lost-ack"}
	f.payload, err = json.Marshal(platform.BuildStopPayload{Step: f.exec.Step, OperationID: string(f.pointerBuildFixture.op.ID), PackStep: deploymentPackStep})
	if err != nil {
		t.Fatal(err)
	}
	step, err := node.DefaultRegistry().Build(f.exec, f.payload)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	f.hash = engine.InputsHash(step.ID(), inputs)
	return f
}

func (f stopRecoveryFixture) persistIntent(t *testing.T, rollback error) {
	t.Helper()
	ctx := context.Background()
	_, owned, err := f.service.mutateOwned(ctx, f.op.ID, 1, func(tx *entities.StoreTx, locked *contract.Operation) error {
		if err := tx.SetBuildStopPlacement(ctx, f.op.ID, entities.BuildStopPlacement{Node: "local", Identity: f.identity}); err != nil {
			return err
		}
		if _, err := beginStepTx(ctx, tx, locked, 1, f.exec.Step, f.hash, f.exec.ExecutionID()); err != nil {
			return err
		}
		return rollback
	})
	if rollback != nil {
		if !errors.Is(err, rollback) {
			t.Fatalf("intent rollback: %v", err)
		}
		return
	}
	if err != nil || !owned {
		t.Fatalf("persist intent: %v %v", owned, err)
	}
}

func (f stopRecoveryFixture) dispatchWithoutAck(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	response, err := f.tr.Call(ctx, node.Request{Method: node.MethodRun, RequestID: f.exec.RequestID, Execution: f.exec, Payload: f.payload}, nil)
	if err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("real stop: %+v %v", response, err)
	}
	status, err := f.tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: f.exec.RequestID, Execution: f.exec}, nil)
	if err != nil || status.Status != engine.StatusCompleted || status.InputsHash != f.hash || status.ExecutionID != f.exec.ExecutionID() {
		t.Fatalf("journal completion: %+v %v", status, err)
	}
}

func (f stopRecoveryFixture) moveEnvironment(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := f.service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Nodes().InsertRegistration(ctx, "other", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_environments SET build_node='other' WHERE id=$1`, f.env.ID); err != nil {
		t.Fatal(err)
	}
}

func (f stopRecoveryFixture) checkMismatch(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_build_stop_placements SET node_identity='wrong' WHERE operation_id=$1`, f.op.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcilePlatformOperation(ctx, f.op); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("identity mismatch: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_build_stop_placements SET node_identity=$2 WHERE operation_id=$1`, f.op.ID, f.identity); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET inputs_hash='wrong' WHERE operation_id=$1 AND step=$2`, f.op.ID, f.exec.Step); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcilePlatformOperation(ctx, f.op); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("hash mismatch: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operation_steps SET inputs_hash=$3 WHERE operation_id=$1 AND step=$2`, f.op.ID, f.exec.Step, f.hash); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationBuildStopRecoversLostAcknowledgement(t *testing.T) {
	for _, cancelling := range []bool{false, true} {
		name := "running"
		if cancelling {
			name = "cancelling"
		}
		t.Run(name, func(t *testing.T) { checkStopLostAcknowledgement(t, cancelling) })
	}
}

func checkStopLostAcknowledgement(t *testing.T, cancelling bool) {
	t.Helper()
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_builds SET state='cancelled' WHERE id=$1`, f.build); err != nil {
		t.Fatal(err)
	}
	f.persistIntent(t, nil)
	f.dispatchWithoutAck(t)
	f.moveEnvironment(t)
	op, _, claimed, err := f.service.claimPlatform(ctx, f.op.ID, 2)
	if err != nil || !claimed {
		t.Fatalf("new attempt: %v %v", claimed, err)
	}
	f.op = op
	if cancelling {
		if _, err := f.service.RequestCancel(ctx, testActor, f.op.ID); err != nil {
			t.Fatal(err)
		}
		f.op, err = f.service.Get(ctx, testActor, f.op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.checkMismatch(t)
	checkStopRecoveryResult(t, f, cancelling)
}

func checkStopRecoveryResult(t *testing.T, f stopRecoveryFixture, cancelling bool) {
	t.Helper()
	ctx := context.Background()
	want := contract.Succeeded
	if cancelling {
		worker := &platformWorker{service: f.service}
		handled, err := worker.handleCancellingPlatformWork(ctx, f.op, nil, 2)
		if err != nil || !handled {
			t.Fatalf("cancel recovery: %v %v", handled, err)
		}
		want = contract.Canceled
	} else if err := f.service.reconcilePlatformOperation(ctx, f.op); err != nil {
		t.Fatalf("reconciliation recovery: %v", err)
	}
	current, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil || current.State != want {
		t.Fatalf("state: %+v %v", current, err)
	}
	record, found, err := f.service.stepRecord(ctx, f.op.ID, f.exec.Step)
	if err != nil || !found || record.State != "completed" || record.ExecutionID != f.exec.ExecutionID() {
		t.Fatalf("recovered acknowledgement: %+v %v %v", record, found, err)
	}
}

func TestIntegrationBuildStopNoIntentSettlesFailure(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_builds SET state='cancelled' WHERE id=$1`, f.build); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcilePlatformOperation(ctx, f.op); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil || current.State != contract.Failed {
		t.Fatalf("no intent: %+v %v", current, err)
	}
}

func TestIntegrationBuildStopDoesNotRedispatchUnknownIntent(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	f.persistIntent(t, errors.New("force intent rollback"))
	if _, found, err := f.service.entities.BuildStopPlacement(ctx, f.op.ID); err != nil || found {
		t.Fatalf("rolled-back placement: %v %v", found, err)
	}
	if _, found, err := f.service.stepRecord(ctx, f.op.ID, f.exec.Step); err != nil || found {
		t.Fatalf("rolled-back step: %v %v", found, err)
	}
	f.persistIntent(t, nil)
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unknown intent: %v", err)
	}
	status, err := f.tr.Call(ctx, node.Request{Method: node.MethodStatus, Execution: f.exec}, nil)
	if err != nil || status.Status != engine.StatusAbsent {
		t.Fatalf("unexpected dispatch: %+v %v", status, err)
	}
	current, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil || current.State.Terminal() {
		t.Fatalf("unknown intent terminalized: %+v %v", current, err)
	}
}

func TestIntegrationBuildStopLegacyIntentHasNoPlacement(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	f.seedBuildClaim(t)
	if _, err := f.service.beginStep(ctx, f.op.ID, 1, f.exec.Step, f.hash, f.exec.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("legacy stop: %v", err)
	}
	if _, found, err := f.service.entities.BuildStopPlacement(ctx, f.op.ID); err != nil || found {
		t.Fatalf("legacy placement was guessed: %v %v", found, err)
	}
	status, err := f.tr.Call(ctx, node.Request{Method: node.MethodStatus, Execution: f.exec}, nil)
	if err != nil || status.Status != engine.StatusAbsent {
		t.Fatalf("legacy stop unexpectedly dispatched: %+v %v", status, err)
	}
}

func (f stopRecoveryFixture) seedBuildClaim(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	id := f.pointerBuildFixture.op.ID
	if _, _, claimed, err := f.service.claimPlatform(ctx, id, 1); err != nil || !claimed {
		t.Fatalf("original build attempt: %v %v", claimed, err)
	}
	if ok, err := f.service.claimLocalBuildCapacity(ctx, id, 1); err != nil || !ok {
		t.Fatalf("original build capacity: %v %v", ok, err)
	}
}

func TestIntegrationBuildStopUsesOriginalClaimAfterEnvironmentMove(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	f.seedBuildClaim(t)
	f.moveEnvironment(t)
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); err != nil {
		t.Fatalf("worker stop: %v", err)
	}
	current, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil || current.State != contract.Succeeded {
		t.Fatalf("stop result: %+v %v", current, err)
	}
	placement, found, err := f.service.entities.BuildStopPlacement(ctx, f.op.ID)
	if err != nil || !found || placement.Node != "local" || placement.Identity != f.identity {
		t.Fatalf("placement: %+v %v %v", placement, found, err)
	}
	record, found, err := f.service.stepRecord(ctx, f.op.ID, deploymentBuildStopStep)
	if err != nil || !found || record.State != "completed" {
		t.Fatalf("effect acknowledgement: %+v %v %v", record, found, err)
	}
}

func TestIntegrationBuildStopRejectsUnknownOriginalClaim(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		name := "missing"
		if mismatch {
			name = "mismatched"
		}
		t.Run(name, func(t *testing.T) { checkStopRejectsClaim(t, mismatch) })
	}
}

func checkStopRejectsClaim(t *testing.T, mismatch bool) {
	t.Helper()
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	if mismatch {
		f.seedBuildClaim(t)
		if _, err := f.pool.Exec(ctx, `UPDATE eregion_build_capacity_claims SET builder_identity='different:ssh' WHERE operation_id=$1`, f.pointerBuildFixture.op.ID); err != nil {
			t.Fatal(err)
		}
	} else if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_builds SET state='running' WHERE id=$1`, f.build); err != nil {
		t.Fatal(err)
	}
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unknown builder: %v", err)
	}
	if _, found, err := f.service.entities.BuildStopPlacement(ctx, f.op.ID); err != nil || found {
		t.Fatalf("unexpected stop placement: %v %v", found, err)
	}
	if _, found, err := f.service.stepRecord(ctx, f.op.ID, f.exec.Step); err != nil || found {
		t.Fatalf("unexpected stop dispatch: %v %v", found, err)
	}
}

func TestIntegrationBuildStopPendingBeforePackDispatch(t *testing.T) {
	for _, started := range []bool{false, true} {
		name := "queued"
		if started {
			name = "worker-started"
		}
		t.Run(name, func(t *testing.T) { checkPendingBuildStop(t, started) })
	}
}

func checkPendingBuildStop(t *testing.T, started bool) {
	t.Helper()
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	original := f.pointerBuildFixture.op.ID
	if started {
		if _, _, claimed, err := f.service.claimPlatform(ctx, original, 1); err != nil || !claimed {
			t.Fatalf("original attempt: %v %v", claimed, err)
		}
	}
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); err != nil {
		t.Fatalf("stop pending build: %v", err)
	}
	stop, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil || stop.State != contract.Succeeded {
		t.Fatalf("stop: %+v %v", stop, err)
	}
	owner, err := f.service.Get(ctx, testActor, original)
	if err != nil || owner.State != contract.Cancelling {
		t.Fatalf("original cancellation: %+v %v", owner, err)
	}
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil || build.State != platform.BuildCancelled {
		t.Fatalf("build: %+v %v", build, err)
	}
	if !started {
		if _, _, claimed, err := f.service.claimPlatform(ctx, original, 1); err != nil || !claimed {
			t.Fatalf("late worker claim: %v %v", claimed, err)
		}
	}
	exec := engine.Execution{OperationID: original, Attempt: 1, Step: deploymentPackStep}
	if _, err := f.service.beginBuildDispatch(ctx, original, 1, "local", f.identity, "late", exec.ExecutionID(), f.build, f.release); !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("late Pack admission: %v", err)
	}
	checkNoPendingStopEffects(t, f)
}

func checkNoPendingStopEffects(t *testing.T, f stopRecoveryFixture) {
	t.Helper()
	ctx := context.Background()
	if _, found, err := f.service.entities.BuildCapacity().Find(ctx, f.pointerBuildFixture.op.ID); err != nil || found {
		t.Fatalf("unexpected capacity: %v %v", found, err)
	}
	if _, found, err := f.service.stepRecord(ctx, f.pointerBuildFixture.op.ID, deploymentPackStep); err != nil || found {
		t.Fatalf("unexpected Pack intent: %v %v", found, err)
	}
	if _, found, err := f.service.stepRecord(ctx, f.op.ID, deploymentBuildStopStep); err != nil || found {
		t.Fatalf("unexpected stop dispatch: %v %v", found, err)
	}
	if _, found, err := f.service.entities.BuildStopPlacement(ctx, f.op.ID); err != nil || found {
		t.Fatalf("guessed stop placement: %v %v", found, err)
	}
}

func TestIntegrationBuildStopPendingRejectsHistoricalPackIntent(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	original := f.pointerBuildFixture.op.ID
	if _, _, claimed, err := f.service.claimPlatform(ctx, original, 1); err != nil || !claimed {
		t.Fatalf("original attempt: %v %v", claimed, err)
	}
	exec := engine.Execution{OperationID: original, Attempt: 1, Step: "platform-pack-01-v1"}
	if _, err := f.service.beginStep(ctx, original, 1, exec.Step, "legacy", exec.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("legacy Pack without capacity: %v", err)
	}
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil || build.State != platform.BuildPending {
		t.Fatalf("ambiguous build changed: %+v %v", build, err)
	}
	owner, err := f.service.Get(ctx, testActor, original)
	if err != nil || owner.State != contract.Running {
		t.Fatalf("ambiguous operation changed: %+v %v", owner, err)
	}
}

func TestIntegrationBuildStopPendingFencesConcurrentPackAdmission(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	original := f.pointerBuildFixture.op.ID
	if _, _, claimed, err := f.service.claimPlatform(ctx, original, 1); err != nil || !claimed {
		t.Fatalf("original attempt: %v %v", claimed, err)
	}
	// The stop first owns the original operation and then waits on this parent.
	// The actual Pack admission must wait on that same operation row.
	parent, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Rollback(context.Background()) }()
	var env string
	if err := parent.QueryRow(ctx, `SELECT id FROM eregion_platform_environments WHERE id=$1 FOR UPDATE`, f.env.ID).Scan(&env); err != nil {
		t.Fatal(err)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- runPlatformAttempt(ctx, f.service, f.op.ID, 2) }()
	waitPointerLockWaiter(t, ctx, f.pool, "current_revision_id,desired_release_id,applied_release_id")
	packDone := make(chan error, 1)
	exec := engine.Execution{OperationID: original, Attempt: 1, Step: deploymentPackStep}
	go func() {
		_, err := f.service.beginBuildDispatch(ctx, original, 1, "local", f.identity, "late", exec.ExecutionID(), f.build, f.release)
		packDone <- err
	}()
	waitPointerLockWaiter(t, ctx, f.pool, "-- name: LockOperation :one")
	if err := parent.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	stopErr, packErr := <-stopDone, <-packDone
	assertNoDeadlock(t, stopErr)
	assertNoDeadlock(t, packErr)
	if stopErr != nil || !errors.Is(packErr, errPlatformCancelled) {
		t.Fatalf("stop=%v late Pack=%v", stopErr, packErr)
	}
	checkNoPendingStopEffects(t, f)
}

func TestIntegrationBuildStopDoesNotRedispatchAfterRealFailure(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	f.seedBuildClaim(t)
	local, ok := f.tr.(*node.Local)
	if !ok {
		t.Fatalf("expected local transport, got %T", f.tr)
	}
	blocked := filepath.Join(local.StateDir(), "builds")
	if err := os.WriteFile(blocked, []byte("block marker directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("failed stop: %v", err)
	}
	exec := f.exec
	exec.Attempt = 2
	before, err := f.tr.Call(ctx, node.Request{Method: node.MethodStatus, Execution: exec}, nil)
	if err != nil || before.Status != engine.StatusFailed {
		t.Fatalf("expected real node failure: %+v %v", before, err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 3); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("retry of failed stop: %v", err)
	}
	after, err := f.tr.Call(ctx, node.Request{Method: node.MethodStatus, Execution: exec}, nil)
	if err != nil || after.Status != engine.StatusFailed || after.ExecutionID != before.ExecutionID {
		t.Fatalf("stop unexpectedly redispatched: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestIntegrationBuildStopRechecksClaimBeforeIntent(t *testing.T) {
	for _, change := range []string{"removed", "identity", "replaced"} {
		t.Run(change, func(t *testing.T) { checkStopClaimChangedBeforeIntent(t, change) })
	}
}

func checkStopClaimChangedBeforeIntent(t *testing.T, change string) {
	t.Helper()
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	f.seedBuildClaim(t)
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil {
		t.Fatal(err)
	}
	_, dispatch, err := f.service.prepareBuildStopDispatch(ctx, f.op.ID, build)
	if err != nil {
		t.Fatal(err)
	}
	query := `DELETE FROM eregion_build_capacity_claims WHERE operation_id=$1`
	switch change {
	case "identity":
		query = `UPDATE eregion_build_capacity_claims SET builder_identity='different:ssh' WHERE operation_id=$1`
	case "replaced":
		query = `UPDATE eregion_build_capacity_claims SET claim_id=gen_random_uuid()::text WHERE operation_id=$1`
	}
	if _, err := f.pool.Exec(ctx, query, f.pointerBuildFixture.op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.beginBuildStopDispatch(ctx, f.exec, f.hash, dispatch); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("changed claim: %v", err)
	}
	if _, found, err := f.service.entities.BuildStopPlacement(ctx, f.op.ID); err != nil || found {
		t.Fatalf("unexpected placement: %v %v", found, err)
	}
	if _, found, err := f.service.stepRecord(ctx, f.op.ID, f.exec.Step); err != nil || found {
		t.Fatalf("unexpected intent: %v %v", found, err)
	}
}
