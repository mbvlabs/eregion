package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func TestIntegrationBuildTargetClaimBlocksWorkerUntilReleased(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := t.Context()
	holder := entities.TargetClaimID{OperationID: "fixture-holder", Token: "fixture-token"}
	if err := f.service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		acquired, err := acquirePlatformTargetClaimTx(ctx, tx, holder, []string{f.op.Target})
		if err == nil && !acquired {
			t.Fatal("fixture could not acquire build target")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var snooze *river.JobSnoozeError
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, int(f.exec.Attempt)); !errors.As(err, &snooze) {
		t.Fatalf("contended stop: %v", err)
	}
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil || build.State != platform.BuildPending {
		t.Fatalf("contended stop changed build: %+v %v", build, err)
	}
	for _, change := range []func(*entities.StoreTx, context.Context, entities.TargetClaimID) (bool, error){
		(*entities.StoreTx).SealTargetClaim, (*entities.StoreTx).ReleaseTargetClaim,
	} {
		if err := f.service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
			ok, err := change(tx, ctx, holder)
			if err == nil && !ok {
				t.Fatal("fixture could not settle build target")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runPlatformAttempt(ctx, f.service, f.op.ID, int(f.exec.Attempt)); err != nil {
		t.Fatal(err)
	}
	stop, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil || stop.State != contract.Succeeded {
		t.Fatalf("stop after release: %+v %v", stop, err)
	}
}

func TestIntegrationBuildTargetSealBlocksResendingAdmittedStop(t *testing.T) {
	f := newStopRecoveryFixture(t)
	ctx := t.Context()
	f.persistIntent(t, nil)
	op, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	work, executor, err := f.service.acquireBuildTarget(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.checkClaimedNodeDispatch(work, op.ID, op.Attempt, f.exec, f.hash); err != nil {
		t.Fatalf("live claim cannot use its intent: %v", err)
	}
	if err := f.service.settleBuildTarget(ctx, executor); err != nil {
		t.Fatal(err)
	}
	if err := f.service.checkClaimedNodeDispatch(work, op.ID, op.Attempt, f.exec, f.hash); !errors.Is(err, errNotOwned) {
		t.Fatalf("sealed executor can still send Run: %v", err)
	}
	claims := claimFixture{t: t, ctx: ctx, a: f.service.entities, b: f.service.entities}
	if claims.acquire(claims.b, claimID("late-competitor", "late-competitor"), op.Target) {
		t.Fatal("sealing released an intent that could still arrive at the node")
	}
}
