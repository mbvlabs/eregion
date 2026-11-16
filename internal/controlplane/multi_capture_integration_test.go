package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
)

func TestIntegrationCandidateCaptureClaimsStayIndependent(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	for _, completedFirst := range []bool{false, true} {
		name := "interrupted-first"
		if completedFirst {
			name = "completed-first"
		}
		t.Run(name, func(t *testing.T) {
			runMultiCaptureCase(t, ctx, run, name, completedFirst)
		})
	}
}

func runMultiCaptureCase(t *testing.T, ctx context.Context, run *Runtime, name string, completedFirst bool) {
	t.Helper()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "multi-capture-" + name, Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, claimed, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	_, owned, err := run.Service.mutateOwned(ctx, op.ID, 1, func(tx *entities.StoreTx, current *contract.Operation) error {
		if completedFirst {
			if err := appendCandidateCaptureComplete(ctx, tx, current, "capture-b-v1"); err != nil {
				return err
			}
			return appendCandidateCaptureClaim(ctx, tx, current, "capture-a-v1")
		}
		if err := appendCandidateCaptureClaim(ctx, tx, current, "capture-a-v1"); err != nil {
			return err
		}
		return appendCandidateCaptureComplete(ctx, tx, current, "capture-b-v1")
	})
	if err != nil || !owned {
		t.Fatalf("events: %v %v", owned, err)
	}
	if err := run.Service.failPlatformOperation(ctx, op.ID, 1, contract.Unavailable, "original failure"); err != nil {
		t.Fatal(err)
	}
	assertCandidateCaptureSettlement(t, ctx, run, op.ID, "settlement")
	// A later history event forces a fresh projection without changing
	// the immutable settled result or completing the first claim.
	_, err = run.Service.mutate(ctx, op.ID, func(tx *entities.StoreTx, current *contract.Operation) error {
		return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Message: "Later read-only evidence observation."})
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateCaptureSettlement(t, ctx, run, op.ID, "late view")
}

func appendCandidateCaptureClaim(ctx context.Context, tx *entities.StoreTx, current *contract.Operation, step string) error {
	return appendEvent(ctx, tx, current, contract.Event{
		Kind: "warning", Stage: "candidate", Step: step,
		Message:             "Candidate diagnostic capture started; completion is unconfirmed until a summary follows.",
		CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "fixture", Status: "unconfirmed"},
	})
}

func appendCandidateCaptureComplete(ctx context.Context, tx *entities.StoreTx, current *contract.Operation, step string) error {
	if err := appendCandidateCaptureClaim(ctx, tx, current, step); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, current, contract.Event{
		Kind: "log", Stage: "candidate", Step: step, Message: "safe output from completed capture",
		CandidateDiagnostic: &contract.CandidateDiagnostic{
			DeploymentID: "fixture", Status: "captured", RedactionContext: "original",
		},
	}); err != nil {
		return err
	}
	return appendEvent(ctx, tx, current, contract.Event{
		Kind: "warning", Stage: "candidate", Step: step,
		Message:             "Candidate diagnostic capture: complete.",
		CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "fixture", Status: "complete"},
	})
}

func assertCandidateCaptureSettlement(t *testing.T, ctx context.Context, run *Runtime, opID contract.OperationID, label string) {
	t.Helper()
	got, err := run.Service.Get(ctx, testActor, opID)
	if err != nil || got.State != contract.Failed || got.LatestResult == nil ||
		got.LatestResult.Evidence != "interrupted" || got.LatestResult.Excerpt != "safe output from completed capture" ||
		got.LatestResult.ExcerptCursor == nil || got.Evidence == nil ||
		got.Evidence.Availability != "interrupted" || got.Evidence.Excerpt != "safe output from completed capture" ||
		got.Evidence.ExcerptCursor == nil {
		t.Fatalf("%s: %+v %v", label, got, err)
	}
	page, err := run.Service.Events(ctx, testActor, *got.Evidence.ExcerptCursor, 1)
	if err != nil || len(page.Events) != 1 || page.Events[0].Step != "capture-b-v1" ||
		page.Events[0].Message != "safe output from completed capture" {
		t.Fatalf("%s excerpt cursor: %+v %v", label, page, err)
	}
}
