package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// These are the database transitions after Pack (and after image inspection).
// No simulated node or Pack process is involved.
func TestIntegrationBuildResultAttemptOwnership(t *testing.T) { //nolint:gocognit // Compare each stale result with its current-attempt transition.
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			f := newPointerBuildFixture(t)
			ctx := context.Background()
			for _, attempt := range []uint32{1, 2} {
				if _, _, claimed, err := f.service.claimPlatform(ctx, f.op.ID, attempt); err != nil || !claimed {
					t.Fatalf("claim %d: %v %v", attempt, claimed, err)
				}
			}
			build, err := f.service.buildByID(ctx, f.build)
			if err != nil {
				t.Fatal(err)
			}
			release, err := f.service.releaseByID(ctx, f.release)
			if err != nil {
				t.Fatal(err)
			}
			worker := &platformWorker{service: f.service}
			failure := contract.NewFault(contract.PreconditionFailed, "pack failed", "")
			apply := func(attempt uint32) error {
				switch outcome {
				case "success":
					return f.service.markOwnedBuildSucceeded(ctx, f.op.ID, attempt, f.build, f.release,
						"registry.example/shop@sha256:"+strings.Repeat("a", 64), submittedGitCommit, "sha256:"+strings.Repeat("b", 64))
				case "cancel":
					return worker.handleBuildPackFailure(ctx, f.op.ID, attempt, release, build, errPlatformCancelled)
				default:
					return worker.handleBuildPackFailure(ctx, f.op.ID, attempt, release, build, failure)
				}
			}
			if err := apply(1); !errors.Is(err, errNotOwned) {
				t.Fatalf("stale result: %v", err)
			}
			assertBuildResultState(t, f, "pending", "pending")
			err = apply(2)
			switch outcome {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
				assertBuildResultState(t, f, "succeeded", "ready")
			case "failure":
				if !errors.Is(err, failure) {
					t.Fatalf("failure: %v", err)
				}
				assertBuildResultState(t, f, "failed", "failed")
			case "cancel":
				if !errors.Is(err, errPlatformCancelled) {
					t.Fatalf("cancellation: %v", err)
				}
				assertBuildResultState(t, f, "cancelled", "pending")
			}
		})
	}
}

func TestIntegrationBuildResultClaimRace(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, _, claimed, err := f.service.claimPlatform(ctx, f.op.ID, 1); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	locked, proceed := make(chan struct{}), make(chan struct{})
	claimDone := make(chan error, 1)
	go func() {
		_, _, err := f.service.mutateOwnedOrClaim(ctx, f.op.ID, 2, func(tx *entities.StoreTx, op *contract.Operation) error {
			_, _, err := tx.Operations().ClaimAttempt(ctx, op.ID, op.Attempt)
			close(locked)
			<-proceed
			return err
		})
		claimDone <- err
	}()
	<-locked
	resultDone := make(chan error, 1)
	go func() {
		resultDone <- f.service.markOwnedBuildSucceeded(ctx, f.op.ID, 1, f.build, f.release, "image", "commit", "digest")
	}()
	waitPointerLockWaiter(t, ctx, f.pool, "SELECT id, kind, target, actor_kind")
	close(proceed)
	if err := <-claimDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resultDone; !errors.Is(err, errNotOwned) {
		t.Fatalf("old inspect result after successor claim: %v", err)
	}
	assertBuildResultState(t, f, "pending", "pending")
}

func assertBuildResultState(t *testing.T, f pointerBuildFixture, wantBuild, wantRelease string) {
	t.Helper()
	var build, release string
	if err := f.pool.QueryRow(context.Background(), `SELECT b.state,r.status FROM eregion_platform_builds b JOIN eregion_platform_releases r ON r.id=b.release_id WHERE b.id=$1`, f.build).Scan(&build, &release); err != nil {
		t.Fatal(err)
	}
	if build != wantBuild || release != wantRelease {
		t.Fatalf("build=%s release=%s, want %s %s", build, release, wantBuild, wantRelease)
	}
}

func TestIntegrationBuildResultWhileCancelling(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if _, _, claimed, err := f.service.claimPlatform(ctx, f.op.ID, 1); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := f.service.requestCancelOperation(ctx, f.op.ID, "test cancellation"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.markOwnedBuildSucceeded(ctx, f.op.ID, 1, f.build, f.release, "image", "commit", "digest"); !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("success while cancelling: %v", err)
	}
	assertBuildResultState(t, f, "pending", "pending")
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil {
		t.Fatal(err)
	}
	release, err := f.service.releaseByID(ctx, f.release)
	if err != nil {
		t.Fatal(err)
	}
	err = (&platformWorker{service: f.service}).handleBuildPackFailure(ctx, f.op.ID, 1, release, build, errors.New("pack failed"))
	if !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("failure while cancelling: %v", err)
	}
	assertBuildResultState(t, f, "cancelled", "pending")
}

// Keep platform referenced here so fixture ownership is explicitly checked
// against a different release, not just against a different attempt.
func TestIntegrationBuildResultRejectsWrongRelease(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if _, _, claimed, err := f.service.claimPlatform(ctx, f.op.ID, 1); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := f.service.markOwnedBuildSucceeded(ctx, f.op.ID, 1, f.build, platform.ReleaseID("wrong"), "image", "commit", "digest"); err == nil {
		t.Fatal("wrong release accepted")
	}
	assertBuildResultState(t, f, "pending", "pending")
}
