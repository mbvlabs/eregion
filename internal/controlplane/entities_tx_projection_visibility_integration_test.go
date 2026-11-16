package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/retention"
)

var errProjectionRollback = errors.New("projection rollback")

// TestIntegrationEntitiesTxProjectionVisibility proves StoreTx.Results and
// StoreTx.Retention read the caller's exact transaction (including a nested
// savepoint) instead of re-querying the pool.
func TestIntegrationEntitiesTxProjectionVisibility(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	op := insertDefinitionOperation(t, store, "platform.deploy", "env/tx-projection/staging", "tx-visibility", json.RawMessage(`{}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		locked, found, err := tx.Operations().Lock(ctx, op)
		if err != nil || !found {
			t.Fatalf("lock operation found=%v err=%v", found, err)
		}
		if err := tx.Operations().AppendEvent(ctx, &locked, contract.Event{
			Kind: "warning", Stage: "tx-health", Message: "Node step failed: tx-health",
			CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "dep-outer", Status: "failed", RedactionContext: "logs"},
		}); err != nil {
			return err
		}
		if err := tx.Steps().Insert(ctx, op, "tx-step-outer", "hash-outer", 0, "exec-outer"); err != nil {
			return err
		}
		if _, err := tx.Steps().Fail(ctx, op, "tx-step-outer", contract.Unavailable, "outer failure"); err != nil {
			return err
		}
		if err := tx.Retention().InsertRun(ctx, entities.RetentionRunInsert{
			ID: "run_tx_outer", OperationID: op, Status: string(retention.RunRunning),
			PlannedItems: json.RawMessage(`[]`), RemovedItems: json.RawMessage(`[]`),
			Errors: json.RawMessage(`[]`), StartedAt: time.Now().UTC(), GCGeneration: 1,
		}); err != nil {
			return err
		}
		diagnostics, err := tx.Results().CandidateDiagnostics(ctx, op, 0, 100)
		if err != nil || len(diagnostics) != 1 || diagnostics[0].DeploymentID != "dep-outer" {
			t.Fatalf("uncommitted diagnostics=%+v err=%v", diagnostics, err)
		}
		if stage, err := tx.Results().FailedStage(ctx, op, 0); err != nil || stage != "tx-health" {
			t.Fatalf("uncommitted stage=%q err=%v", stage, err)
		}
		run, found, err := tx.Retention().LatestRunForOperation(ctx, op)
		if err != nil || !found || run.ID != "run_tx_outer" {
			t.Fatalf("uncommitted run=%+v found=%v err=%v", run, found, err)
		}

		savepointErr := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
			locked, found, err := nested.Operations().Lock(ctx, op)
			if err != nil || !found {
				t.Fatalf("nested lock found=%v err=%v", found, err)
			}
			if err := nested.Operations().AppendEvent(ctx, &locked, contract.Event{
				Kind: "warning", Stage: "tx-health-nested", Message: "Node step failed: nested",
				CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "dep-nested", Status: "failed"},
			}); err != nil {
				return err
			}
			if err := nested.Steps().Insert(ctx, op, "tx-step-nested", "hash-nested", 0, "exec-nested"); err != nil {
				return err
			}
			if _, err := nested.Steps().Fail(ctx, op, "tx-step-nested", contract.Unavailable, "nested failure"); err != nil {
				return err
			}
			if err := nested.Retention().InsertRun(ctx, entities.RetentionRunInsert{
				ID: "run_tx_nested", OperationID: op, Status: string(retention.RunRunning),
				PlannedItems: json.RawMessage(`[]`), RemovedItems: json.RawMessage(`[]`),
				Errors: json.RawMessage(`[]`), StartedAt: time.Now().UTC(), GCGeneration: 1,
			}); err != nil {
				return err
			}
			diagnostics, err := nested.Results().CandidateDiagnostics(ctx, op, 0, 100)
			if err != nil || len(diagnostics) != 2 {
				t.Fatalf("nested diagnostics=%+v err=%v", diagnostics, err)
			}
			nestedRun, found, err := nested.Retention().LatestRunForOperation(ctx, op)
			if err != nil || !found || nestedRun.ID != "run_tx_nested" {
				t.Fatalf("nested run=%+v found=%v err=%v", nestedRun, found, err)
			}
			if stage, err := nested.Results().FailedStage(ctx, op, 0); err != nil || stage != "tx-health" {
				t.Fatalf("nested stage=%q err=%v", stage, err)
			}
			return errProjectionRollback
		})
		if savepointErr != errProjectionRollback {
			t.Fatalf("savepoint error=%v, want exact sentinel", savepointErr)
		}
		diagnostics, err = tx.Results().CandidateDiagnostics(ctx, op, 0, 100)
		if err != nil || len(diagnostics) != 1 {
			t.Fatalf("post-savepoint diagnostics=%+v err=%v", diagnostics, err)
		}
		run, found, err = tx.Retention().LatestRunForOperation(ctx, op)
		if err != nil || !found || run.ID != "run_tx_outer" {
			t.Fatalf("post-savepoint run=%+v found=%v err=%v", run, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The committed root writes are observable through the pool view; the
	// discarded nested scope is not.
	diagnostics, err := store.Results().CandidateDiagnostics(ctx, op, 0, 100)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].DeploymentID != "dep-outer" {
		t.Fatalf("committed diagnostics=%+v err=%v", diagnostics, err)
	}
	run, found, err := store.Retention().LatestRunForOperation(ctx, op)
	if err != nil || !found || run.ID != "run_tx_outer" {
		t.Fatalf("committed run=%+v found=%v err=%v", run, found, err)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_retention_runs WHERE operation_id=$1`, op).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("retention runs=%d err=%v", runs, err)
	}

	// A wholly discarded StoreTx is invisible to the pool view.
	discardOp := insertDefinitionOperation(t, store, "platform.deploy", "env/tx-projection/staging", "tx-discard", json.RawMessage(`{}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		locked, found, err := tx.Operations().Lock(ctx, discardOp)
		if err != nil || !found {
			t.Fatalf("discard lock found=%v err=%v", found, err)
		}
		if err := tx.Operations().AppendEvent(ctx, &locked, contract.Event{
			Kind: "warning", Stage: "discard", Message: "Node step failed: discard",
			CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "dep-discard", Status: "failed"},
		}); err != nil {
			return err
		}
		if err := tx.Retention().InsertRun(ctx, entities.RetentionRunInsert{
			ID: "run_tx_discard", OperationID: discardOp, Status: string(retention.RunRunning),
			PlannedItems: json.RawMessage(`[]`), RemovedItems: json.RawMessage(`[]`),
			Errors: json.RawMessage(`[]`), StartedAt: time.Now().UTC(), GCGeneration: 1,
		}); err != nil {
			return err
		}
		if diagnostics, err := tx.Results().CandidateDiagnostics(ctx, discardOp, 0, 10); err != nil || len(diagnostics) != 1 {
			t.Fatalf("discarded diagnostics=%+v err=%v", diagnostics, err)
		}
		if _, found, err := tx.Retention().LatestRunForOperation(ctx, discardOp); err != nil || !found {
			t.Fatalf("discarded run found=%v err=%v", found, err)
		}
		return errProjectionRollback
	}); err != errProjectionRollback {
		t.Fatalf("discard error=%v", err)
	}
	if diagnostics, err := store.Results().CandidateDiagnostics(ctx, discardOp, 0, 10); err != nil || len(diagnostics) != 0 {
		t.Fatalf("visible discarded diagnostics=%+v err=%v", diagnostics, err)
	}
	if _, found, err := store.Retention().LatestRunForOperation(ctx, discardOp); err != nil || found {
		t.Fatalf("visible discarded run found=%v err=%v", found, err)
	}
}
