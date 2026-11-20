package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func TestIntegrationBuildStopWhileCapacityQueued(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	original := f.pointerBuildFixture.op.ID
	if _, _, claimed, err := f.service.claimPlatform(ctx, original, 1); err != nil || !claimed {
		t.Fatalf("original attempt: %v %v", claimed, err)
	}
	holder, err := f.service.Submit(ctx, submission("queue-stop-holder", `{"steps":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, claimed, err := f.service.claimAttempt(ctx, holder.ID, 1, "test"); err != nil || !claimed {
		t.Fatalf("holder attempt: %v %v", claimed, err)
	}
	if ok, err := f.service.claimLocalBuildCapacity(ctx, holder.ID, 1); err != nil || !ok {
		t.Fatalf("hold capacity: %v %v", ok, err)
	}
	release, err := f.service.releaseByID(ctx, f.release)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&platformWorker{service: f.service}).buildRelease(ctx, original, 1, f.env, release, f.tr, nil)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("busy capacity must snooze: %v", err)
	}
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil || build.State != platform.BuildPending || build.StartedAt != nil {
		t.Fatalf("queued build must remain pending: %+v %v", build, err)
	}
	checkNoPendingStopEffects(t, f)
	assertBuildSlotEvent(t, ctx, f.service, original, "build_queued")
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, 2); err != nil {
		t.Fatalf("stop queued build: %v", err)
	}
	if err := f.service.releaseBuildCapacity(ctx, holder.ID, 1, "local"); err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: original, Attempt: 1, Step: deploymentPackStep}
	if _, err := f.service.beginBuildDispatch(ctx, original, 1, "local", f.identity, "late", exec.ExecutionID(), f.build, f.release); !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("late Pack admission: %v", err)
	}
	build, err = f.service.buildByID(ctx, f.build)
	if err != nil || build.State != platform.BuildCancelled {
		t.Fatalf("stopped build: %+v %v", build, err)
	}
	stop, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil || stop.State != contract.Succeeded {
		t.Fatalf("stop result: %+v %v", stop, err)
	}
	checkNoPendingStopEffects(t, f)
	local, ok := f.tr.(*node.Local)
	if !ok {
		t.Fatalf("expected local transport, got %T", f.tr)
	}
	marker := filepath.Join(local.StateDir(), "builds", string(original)+"-"+deploymentPackStep+".cancel")
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected node cancellation marker: %v", err)
	}
}

func TestIntegrationBuildPreparationRejectsForeignFallback(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	release, err := f.service.releaseByID(ctx, f.release)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&platformWorker{service: f.service}).prepareBuildRecord(ctx, f.op.ID, release)
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("foreign fallback: %v", err)
	}
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil || build.State != platform.BuildPending || build.StartedAt != nil {
		t.Fatalf("foreign build changed: %+v %v", build, err)
	}
	checkNoPendingStopEffects(t, f)
}

func TestIntegrationBuildDispatchMarksOnlyOwnedBuild(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := context.Background()
	id := f.pointerBuildFixture.op.ID
	if _, _, claimed, err := f.service.claimPlatform(ctx, id, 1); err != nil || !claimed {
		t.Fatalf("original attempt: %v %v", claimed, err)
	}
	exec := engine.Execution{OperationID: id, Attempt: 1, Step: deploymentPackStep}
	if _, err := f.service.beginBuildDispatch(ctx, id, 1, "local", f.identity, "hash", exec.ExecutionID(), f.build, "rel_ffffffffffffffffffffffffffffffff"); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("wrong release: %v", err)
	}
	foreign := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: deploymentPackStep}
	if _, err := f.service.beginBuildDispatch(ctx, f.op.ID, 1, "local", f.identity, "hash", foreign.ExecutionID(), f.build, f.release); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("wrong operation: %v", err)
	}
	if _, found, err := f.service.entities.BuildCapacity().Find(ctx, f.op.ID); err != nil || found {
		t.Fatalf("foreign capacity was not rolled back: %v %v", found, err)
	}
	checkNoPendingStopEffects(t, f)
	already, err := f.service.beginBuildDispatch(ctx, id, 1, "local", f.identity, "hash", exec.ExecutionID(), f.build, f.release)
	if err != nil || already {
		t.Fatalf("owned dispatch: already=%v err=%v", already, err)
	}
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil || build.State != platform.BuildRunning || build.StartedAt == nil {
		t.Fatalf("admitted build: %+v %v", build, err)
	}
	claim, found, err := f.service.entities.BuildCapacity().Find(ctx, id)
	if err != nil || !found || claim.Node != "local" {
		t.Fatalf("capacity: %+v %v %v", claim, found, err)
	}
	record, found, err := f.service.stepRecord(ctx, id, deploymentPackStep)
	if err != nil || !found || record.State != "running" || record.ExecutionID != exec.ExecutionID() {
		t.Fatalf("Pack intent: %+v %v %v", record, found, err)
	}
}
