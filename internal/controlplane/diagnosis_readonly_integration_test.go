package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
)

// The application service reads the exact recorded deployment and build; the
// removed action cannot submit a job through generic service admission.
func TestIntegrationDiagnosisReadOnlyAndExecutionUnsupported(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	actor := contract.Actor{Kind: "operator", Subject: "diagnosis-readonly"}
	app := api.AppService{Operations: f.service}
	var operationsBefore, jobsBefore int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations), (SELECT count(*) FROM river_job)`).Scan(&operationsBefore, &jobsBefore); err != nil {
		t.Fatal(err)
	}
	report, err := app.Diagnose(ctx, operations.OperatorAuthority(actor), f.op.ID, 0, 1000, "")
	if err != nil || report.Operation.ID != f.op.ID || report.Build == nil || report.Build.ID != f.build || report.Deployment == nil || report.Deployment.ID != f.dep || report.Deployment.RevisionID != f.env.RevisionID || report.Release == nil || report.Release.ID != f.release {
		t.Fatalf("diagnosis=%+v err=%v", report, err)
	}
	_, err = f.service.Submit(ctx, platformSubmission("platform.diagnostic.execute", "operation/"+string(f.op.ID), "removed-diagnostic", map[string]any{}))
	if err == nil {
		t.Fatal("removed execution action was accepted")
	}
	var operationsAfter, jobsAfter int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations), (SELECT count(*) FROM river_job)`).Scan(&operationsAfter, &jobsAfter); err != nil || operationsAfter != operationsBefore || jobsAfter != jobsBefore {
		t.Fatalf("diagnosis or rejected submission changed counts: operations %d->%d jobs %d->%d err=%v", operationsBefore, operationsAfter, jobsBefore, jobsAfter, err)
	}
}
