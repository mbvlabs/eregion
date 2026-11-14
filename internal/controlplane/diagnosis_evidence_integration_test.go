package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// Uses the existing isolated PostgreSQL fixture; no Docker or host mutation.
func TestIntegrationDiagnosisFindsFailureAfterLaterCaptureAndBeforePage(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", f.op.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_operations SET state='failed', attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	op.State, op.Attempt = contract.Failed, 1
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Failed, Stage: "container", Message: "original failure"}); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "warning", Stage: platform.CandidateDiagnosticStage, Message: "capture interrupted", CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: string(f.dep), Status: "unavailable"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	report, err := f.service.Diagnose(ctx, operations.OperatorAuthority(testActor), op.ID, 0, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Failure == nil || report.Failure.Message != "original failure" || report.Failure.Stage != "container" {
		t.Fatalf("failure summary must be independent of page and later capture: %+v", report.Failure)
	}
	if !report.HasMore || report.HistoryUnavailable {
		t.Fatalf("ordinary pagination is not lost history: %+v", report)
	}
}
