package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

// A pending cleanup retries indefinitely at a bounded rate, not as a claim of
// success. River snoozes do not consume an attempt; the operation stays open.
const diagnosticCleanupRetry = 30 * time.Second

func diagnosticExecutionPayload(id contract.OperationID, in platform.DiagnosticExecutionInput, step string) platform.DiagnosticExecutionPayload {
	return platform.DiagnosticExecutionPayload{Step: step, OperationID: id, ImageID: in.ImageID, Argv: in.Argv}
}

// markDiagnosticCleanupTx is the durable cleanup-only fence. It uses the same
// step identity and hash as node dispatch, so retries and rescue cannot start
// execution again after cleanup has been requested.
func markDiagnosticCleanupTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, raw json.RawMessage) error {
	in, _, err := platform.NormalizeDiagnosticExecutionInput(raw)
	if err != nil || op.Target != "operation/"+string(in.OriginalOperationID) {
		return contract.NewFault(contract.PreconditionFailed, "diagnostic cleanup input is invalid", "Retain the operation and inspect its durable input before recovery.")
	}
	payload := diagnosticExecutionPayload(op.ID, in, platform.DiagnosticCleanupStep)
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: op.Attempt, Step: platform.DiagnosticCleanupStep}
	step, err := node.DefaultRegistry().Build(exec, data)
	if err != nil {
		return err
	}
	inputs, err := step.Inputs()
	if err != nil {
		return err
	}
	hash := engine.InputsHash(step.ID(), inputs)
	tag, err := tx.Exec(ctx, `INSERT INTO eregion_operation_steps
        (operation_id,step,inputs_hash,state,attempt,execution_id)
        VALUES ($1,$2,$3,'running',$4,'') ON CONFLICT (operation_id,step) DO NOTHING`,
		op.ID, step.ID(), hash, op.Attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var existing string
		if err := tx.QueryRow(ctx, `SELECT inputs_hash FROM eregion_operation_steps WHERE operation_id=$1 AND step=$2`, op.ID, step.ID()).Scan(&existing); err != nil {
			return err
		}
		if existing != hash {
			return contract.NewFault(contract.PreconditionFailed, "diagnostic cleanup identity disagrees", "Retain the operation and inspect its node and control-plane journals.")
		}
		return nil
	}
	return appendEvent(ctx, tx, op, contract.Event{Kind: "warning", Stage: platform.DiagnosticCleanupStep, Message: "Diagnostic cleanup is pending; no further execution will be started. Output is withheld."})
}

func (s *Service) markDiagnosticCleanup(ctx context.Context, id contract.OperationID, attempt uint32, raw json.RawMessage) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		return markDiagnosticCleanupTx(ctx, tx, op, raw)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func diagnosticResult(raw json.RawMessage) (platform.DiagnosticExecutionResult, bool) {
	var result platform.DiagnosticExecutionResult
	if json.Unmarshal(raw, &result) != nil || result.Cleanup != "complete" || !result.OutputWithheld {
		return result, false
	}
	switch result.Outcome {
	case "exited":
		return result, result.ExitCode != nil && !result.TimedOut
	case "timeout":
		return result, result.TimedOut
	case "unknown", "cancelled":
		return result, !result.TimedOut
	default:
		return result, false
	}
}

// runDiagnosticExecution dispatches only to the worker's exact local transport.
// A cleanup fence, cancellation, or uncertain execution error makes every
// subsequent attempt cleanup-only. Node status is checked by runStepOn before
// dispatch, and the node's start intent prevents a duplicate executable start.
func (w *platformWorker) runDiagnosticExecution(ctx context.Context, op contract.Operation, raw json.RawMessage, attempt uint32) error {
	in, _, err := platform.NormalizeDiagnosticExecutionInput(raw)
	if err != nil || op.Target != "operation/"+string(in.OriginalOperationID) {
		// Without the exact pinned payload, neither an effect nor its cleanup can
		// safely be attempted. Never terminalize an unproven cleanup.
		return river.JobSnooze(diagnosticCleanupRetry)
	}
	if w.service.localNode != in.Node || w.service.transport == nil {
		return river.JobSnooze(diagnosticCleanupRetry)
	}
	_, cleanupOnly, err := w.service.stepRecord(ctx, op.ID, platform.DiagnosticCleanupStep)
	if err != nil {
		return err
	}
	state, _, err := w.service.operationState(ctx, op.ID)
	if err != nil {
		return err
	}
	if state.Terminal() {
		return nil
	}
	cleanupOnly = cleanupOnly || state == contract.Cancelling
	if !cleanupOnly {
		resultRaw, runErr := w.runStepOn(ctx, w.service.transport, op.ID, attempt, platform.DiagnosticExecutionStep, diagnosticExecutionPayload(op.ID, in, platform.DiagnosticExecutionStep), nil)
		if runErr == nil {
			if result, ok := diagnosticResult(resultRaw); ok {
				return w.finishDiagnosticExecution(ctx, op.ID, attempt, result, false)
			}
		} else if errors.Is(runErr, errNotOwned) {
			return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
		} else if fault := contract.Classify(runErr); fault.Code == contract.Unavailable && fault.Message == "node task is still running" {
			// The detached task is still active; cancellation takes the cleanup
			// tail on the next pass. This is not an execution failure.
			return river.JobSnooze(diagnosticCleanupRetry)
		}
	}
	// Cancellation and failure must survive the worker's own cancellation. A
	// bounded independent context commits cleanup intent before asking the node.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	if err := w.service.markDiagnosticCleanup(cleanupCtx, op.ID, attempt, raw); err != nil {
		if errors.Is(err, errNotOwned) {
			return w.service.waitForNewerAttempt(cleanupCtx, op.ID, attempt)
		}
		return err
	}
	resultRaw, err := w.runStepOn(cleanupCtx, w.service.transport, op.ID, attempt, platform.DiagnosticCleanupStep, diagnosticExecutionPayload(op.ID, in, platform.DiagnosticCleanupStep), nil)
	if err != nil {
		if errors.Is(err, errNotOwned) {
			return w.service.waitForNewerAttempt(cleanupCtx, op.ID, attempt)
		}
		return river.JobSnooze(diagnosticCleanupRetry)
	}
	result, ok := diagnosticResult(resultRaw)
	if !ok {
		return river.JobSnooze(diagnosticCleanupRetry)
	}
	// If the execution effect was acknowledged before cancellation or rescue,
	// preserve its real result; otherwise cleanup establishes only its own
	// cancellation/unknown observation, not proof of a successful execution.
	executionProven := false
	if record, found, err := w.service.stepRecord(cleanupCtx, op.ID, platform.DiagnosticExecutionStep); err != nil {
		return err
	} else if found && record.State == "completed" {
		if execution, valid := diagnosticResult(record.Result); valid {
			result = execution
			executionProven = true
		}
	}
	if !executionProven && result.Outcome == "exited" {
		// Cleanup can observe an exit, but it cannot replace acknowledgement of
		// the execution effect as proof that the requested command succeeded.
		result.Outcome = "unknown"
	}
	return w.finishDiagnosticExecution(cleanupCtx, op.ID, attempt, result, true)
}

func (w *platformWorker) finishDiagnosticExecution(ctx context.Context, id contract.OperationID, attempt uint32, result platform.DiagnosticExecutionResult, cleanupOnly bool) error {
	_, owned, err := w.service.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if cleanupOnly && op.State != contract.Cancelling && result.Outcome == "cancelled" {
			result.Outcome = "unknown"
		}
		detail := fmt.Sprintf("Diagnostic outcome: %s; OOM killed: %t; timeout: %t; output withheld; cleanup verified complete.", result.Outcome, result.OOMKilled, result.TimedOut)
		if result.ExitCode != nil {
			detail = fmt.Sprintf("Diagnostic outcome: %s; exit code: %d; OOM killed: %t; timeout: %t; output withheld; cleanup verified complete.", result.Outcome, *result.ExitCode, result.OOMKilled, result.TimedOut)
		}
		if err := appendEvent(ctx, tx, op, contract.Event{Kind: "log", Stage: platform.DiagnosticExecutionStep, Message: detail}); err != nil {
			return err
		}
		if op.State == contract.Cancelling {
			if err := markIncompleteSteps(ctx, tx, id, contract.Cancelled, "Diagnostic cancellation completed after cleanup."); err != nil {
				return err
			}
			return transition(ctx, tx, op, contract.Canceled, "", "Diagnostic cancellation completed after owned-container cleanup was verified; output was withheld.")
		}
		if result.Outcome == "exited" && result.ExitCode != nil && *result.ExitCode == 0 && !result.OOMKilled && !result.TimedOut {
			return transition(ctx, tx, op, contract.Succeeded, "", "Diagnostic exited successfully and cleanup was verified; output was withheld.")
		}
		if err := markIncompleteSteps(ctx, tx, id, contract.PreconditionFailed, detail); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Failed, contract.PreconditionFailed, detail)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// requeueDiagnosticCleanup handles a disappeared, discarded, cancelled or
// completed River job. Cleanup intent and the replacement/revived job commit
// together; an active job is left alone. Attempt numbering never moves back.
func (s *Service) requeueDiagnosticCleanup(ctx context.Context, id contract.OperationID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return unavailable(err)
	}
	if op.State.Terminal() || op.Kind != platform.KindDiagnosticExecution {
		return nil
	}
	var raw json.RawMessage
	var jobID *int64
	if err := tx.QueryRow(ctx, "SELECT input,job_id FROM eregion_operations WHERE id=$1", id).Scan(&raw, &jobID); err != nil {
		return unavailable(err)
	}
	var state string
	if jobID != nil {
		err = tx.QueryRow(ctx, "SELECT state FROM river_job WHERE id=$1 FOR UPDATE", *jobID).Scan(&state)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return unavailable(err)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			jobID = nil
		} else if state != "discarded" && state != "cancelled" && state != "completed" {
			return nil
		}
	}
	if err := markDiagnosticCleanupTx(ctx, tx, &op, raw); err != nil {
		return err
	}
	if jobID == nil {
		if s.enqueuePlatform == nil {
			return unavailable()
		}
		created, err := s.enqueuePlatform(ctx, tx, op)
		if err != nil {
			return unavailable(err)
		}
		jobID = &created
		if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET job_id=$2 WHERE id=$1", id, created); err != nil {
			return unavailable(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE river_job SET state='scheduled',finalized_at=NULL,
        attempt=GREATEST(attempt,$2),max_attempts=GREATEST(max_attempts,attempt+1,$2+1),
        scheduled_at=clock_timestamp()+interval '30 seconds' WHERE id=$1`, *jobID, op.Attempt); err != nil {
		return unavailable(err)
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "warning", Stage: "worker", Message: "Diagnostic cleanup job was recovered; it will retry at a bounded interval until cleanup can be proven."}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}
