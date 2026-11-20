package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/noded"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Uses the real local dispatcher and an existing image on an isolated daemon.
func TestIntegrationImageObservationAndAttemptFence(t *testing.T) {
	reference := os.Getenv("EREGION_TEST_IMAGE_REFERENCE")
	if reference == "" {
		t.Skip("set EREGION_TEST_IMAGE_REFERENCE to an existing canonical image on the isolated Docker daemon")
	}
	tr, err := node.NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	image := verifyImageObservation(t, tr, reference)
	t.Run("post-pack", func(t *testing.T) { verifyPostPackObservation(t, tr, image) })
	t.Run("image-source", func(t *testing.T) { verifyImageSourceObservation(t, tr, image) })
	t.Run("deployment-foreign-release", verifyDeploymentForeignRelease)
}

func verifyImageObservation(t *testing.T, tr node.Transport, reference string) platform.ImageObservation {
	t.Helper()
	ctx := t.Context()
	image, err := observeReleaseImage(ctx, tr, reference)
	if err != nil || !image.ValidFor(reference) {
		t.Fatalf("invalid observation: %+v %v", image, err)
	}
	request, err := json.Marshal(node.ObserveImagePayload{Reference: reference})
	if err != nil {
		t.Fatal(err)
	}
	response := noded.Handle(noded.Config{}, node.NewStepRegistry(), node.Request{Method: node.MethodObserveImage, Payload: request})
	var remote platform.ImageObservation
	if response.Status != engine.StatusCompleted || json.Unmarshal(response.Result, &remote) != nil || remote.ID != image.ID || !remote.ValidFor(reference) {
		t.Fatalf("node server observation: %+v %+v", response, remote)
	}
	for _, payload := range []string{`{"reference":"--help"}`, `{"reference":"` + reference + `","argv":["rm"]}`, `{} {}`} {
		response, err := tr.Call(ctx, node.Request{Method: node.MethodObserveImage, Payload: json.RawMessage(payload)}, nil)
		if err != nil || response.Status != engine.StatusFailed {
			t.Fatalf("invalid payload accepted: %s: %+v %v", payload, response, err)
		}
	}
	missing := "example.invalid/missing@sha256:" + strings.Repeat("a", 64)
	if _, err := observeReleaseImage(ctx, tr, missing); err == nil {
		t.Fatal("missing image was accepted")
	}
	return image
}

func claimImageAttempts(t *testing.T, f pointerBuildFixture) {
	t.Helper()
	for _, attempt := range []uint32{1, 2} {
		if _, _, claimed, err := f.service.claimPlatform(t.Context(), f.op.ID, attempt); err != nil || !claimed {
			t.Fatalf("claim %d: %v %v", attempt, claimed, err)
		}
	}
}

func verifyPostPackObservation(t *testing.T, tr node.Transport, image platform.ImageObservation) {
	t.Helper()
	ctx := t.Context()
	f := newPointerBuildFixture(t)
	claimImageAttempts(t, f)
	build, err := f.service.buildByID(ctx, f.build)
	if err != nil {
		t.Fatal(err)
	}
	release, err := f.service.releaseByID(ctx, f.release)
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]string{"commit": submittedGitCommit, "reference": image.Reference, "image_id": image.ID, "runtime_id": image.ID})
	if err != nil {
		t.Fatal(err)
	}
	w := &platformWorker{service: f.service}
	if _, err := w.finishBuildRelease(ctx, f.op.ID, 1, release, build, tr, result); !errors.Is(err, errNotOwned) {
		t.Fatalf("stale completion: %v", err)
	}
	assertBuildResultState(t, f, "pending", "pending")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.finishBuildRelease(cancelled, f.op.ID, 2, release, build, tr, result); err == nil {
		t.Fatal("interrupted inspection succeeded")
	}
	assertBuildResultState(t, f, "pending", "pending")
	if _, err := w.finishBuildRelease(ctx, f.op.ID, 2, release, build, tr, result); err != nil {
		t.Fatal(err)
	}
	assertBuildResultState(t, f, "succeeded", "ready")
	assertNoImageEffect(t, f)
}

func verifyImageSourceObservation(t *testing.T, tr node.Transport, image platform.ImageObservation) {
	t.Helper()
	ctx := t.Context()
	f := newPointerBuildFixture(t)
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET kind=$2 WHERE id=$1`, f.op.ID, platform.KindBuildRetry); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_releases SET image=$2,image_id=$3 WHERE id=$1`, f.release, image.Reference, image.ID); err != nil {
		t.Fatal(err)
	}
	claimImageAttempts(t, f)
	release, err := f.service.releaseByID(ctx, f.release)
	if err != nil {
		t.Fatal(err)
	}
	f.service.imageIdentityOverrides = map[string][2]string{image.Reference: {"", image.ID}}
	w := &platformWorker{service: f.service}
	if _, err := w.verifyReleaseImage(ctx, f.op.ID, 1, release, tr, false); !errors.Is(err, errNotOwned) {
		t.Fatalf("stale image completion: %v", err)
	}
	assertBuildResultState(t, f, "pending", "pending")
	if err := f.service.markOwnedImageReleaseReady(ctx, f.op.ID, 2, f.release, image.Reference+"-different", image.ID); err == nil {
		t.Fatal("changed image reference accepted")
	}
	if err := f.service.markOwnedImageReleaseReady(ctx, f.op.ID, 2, f.release, image.Reference, "sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("changed config digest accepted")
	}
	other := imageOtherOperation(t, f)
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_releases SET operation_id=$2 WHERE id=$1`, f.release, other); err != nil {
		t.Fatal(err)
	}
	if _, err := w.verifyReleaseImage(ctx, f.op.ID, 2, release, tr, false); err == nil {
		t.Fatal("foreign release accepted")
	}
	assertBuildResultState(t, f, "pending", "pending")
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_releases SET operation_id=$2 WHERE id=$1`, f.release, f.op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.verifyReleaseImage(ctx, f.op.ID, 2, release, tr, false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.verifyReleaseImage(ctx, f.op.ID, 2, release, tr, false); err != nil {
		t.Fatalf("resume after ready write: %v", err)
	}
	updated, err := f.service.releaseByID(ctx, f.release)
	if err != nil || updated.Status != platform.ReleaseReady || updated.ImageID != image.ID {
		t.Fatalf("ready release: %+v %v", updated, err)
	}
	assertNoImageEffect(t, f)
}

func imageOtherOperation(t *testing.T, f pointerBuildFixture) contract.OperationID {
	t.Helper()
	op, err := f.service.Submit(t.Context(), operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "other-image-owner", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return op.ID
}

func verifyDeploymentForeignRelease(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := t.Context()
	claimImageAttempts(t, f)
	other := imageOtherOperation(t, f)
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_releases SET operation_id=$2 WHERE id=$1`, f.release, other); err != nil {
		t.Fatal(err)
	}
	if err := f.service.markOwnedImageReleaseReady(ctx, f.op.ID, 2, f.release, "image", "digest"); err != nil {
		t.Fatalf("deployment rejected foreign release: %v", err)
	}
}

func assertNoImageEffect(t *testing.T, f pointerBuildFixture) {
	t.Helper()
	if _, found, err := f.service.stepRecord(t.Context(), f.op.ID, deploymentImageStep); err != nil || found {
		t.Fatalf("observation recorded as an effect: found=%v err=%v", found, err)
	}
}
