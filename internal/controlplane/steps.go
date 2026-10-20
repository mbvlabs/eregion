package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
)

// StepRecord is the control-plane acknowledgement for one node effect.
type StepRecord struct {
	OperationID contract.OperationID `json:"operation_id"`
	Step        string               `json:"step"`
	InputsHash  string               `json:"inputs_hash"`
	State       string               `json:"state"`
	Attempt     uint32               `json:"attempt"`
	ExecutionID string               `json:"execution_id"`
	StartedAt   time.Time            `json:"started_at"`
	CompletedAt *time.Time           `json:"completed_at,omitempty"`
	Error       string               `json:"error,omitempty"`
	Result      json.RawMessage      `json:"result,omitempty"`
}

const stepColumns = `operation_id,step,inputs_hash,state,attempt,execution_id,started_at,completed_at,error,result`

func scanStep(row pgx.Row) (StepRecord, error) {
	var step StepRecord
	var result []byte
	err := row.Scan(&step.OperationID, &step.Step, &step.InputsHash, &step.State, &step.Attempt, &step.ExecutionID, &step.StartedAt, &step.CompletedAt, &step.Error, &result)
	if err != nil {
		return step, err
	}
	if len(result) > 0 {
		step.Result = result
	}
	return step, nil
}

func (s *Service) stepRecord(ctx context.Context, id contract.OperationID, step string) (StepRecord, bool, error) {
	record, err := scanStep(s.pool.QueryRow(ctx, "SELECT "+stepColumns+" FROM eregion_operation_steps WHERE operation_id=$1 AND step=$2", id, step))
	if errors.Is(err, pgx.ErrNoRows) {
		return StepRecord{}, false, nil
	}
	if err != nil {
		return StepRecord{}, false, unavailable()
	}
	return record, true, nil
}

func (s *Service) stepRecords(ctx context.Context, id contract.OperationID) ([]StepRecord, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+stepColumns+" FROM eregion_operation_steps WHERE operation_id=$1 ORDER BY step", id)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	var records []StepRecord
	for rows.Next() {
		record, err := scanStep(rows)
		if err != nil {
			return nil, unavailable()
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable()
	}
	return records, nil
}

// claimAttempt makes this River attempt the operation's owner. A newer attempt
// already in the database is not displaced.
func (s *Service) claimAttempt(ctx context.Context, id contract.OperationID, attempt uint32, stage string) (contract.Operation, json.RawMessage, bool, error) {
	var input json.RawMessage
	op, claimed, err := s.mutateOwnedOrClaim(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "Diagnostic worker started through node dispatch."); err != nil {
				return err
			}
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "UPDATE eregion_operations SET attempt=$2 WHERE id=$1 RETURNING input", op.ID, op.Attempt).Scan(&raw); err != nil {
			return err
		}
		input = raw
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: stage, Message: "Node dispatch attempt started."})
	})
	return op, input, claimed, err
}

// mutateOwnedOrClaim is mutateOwned, but it lets a strictly newer River attempt
// take ownership and refuses an older one.
func (s *Service) mutateOwnedOrClaim(ctx context.Context, id contract.OperationID, attempt uint32, change func(pgx.Tx, *contract.Operation) error) (contract.Operation, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contract.Operation{}, false, unavailable()
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return op, false, unavailable()
	}
	if op.State.Terminal() || op.Attempt > attempt {
		if err := tx.Commit(ctx); err != nil {
			return op, false, unavailable()
		}
		return op, false, nil
	}
	if op.Attempt < attempt {
		op.Attempt = attempt
	}
	if err := change(tx, &op); err != nil {
		return op, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return op, false, unavailable()
	}
	return op, true, nil
}

// beginStep durably records a running step before dispatch. A completed step is
// never reopened; a failed or stale running step may be retried.
func (s *Service) beginStep(ctx context.Context, id contract.OperationID, attempt uint32, step, inputsHash, executionID string) (bool, error) {
	already := false
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		record, err := scanStep(tx.QueryRow(ctx, "SELECT "+stepColumns+" FROM eregion_operation_steps WHERE operation_id=$1 AND step=$2 FOR UPDATE", id, step))
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `INSERT INTO eregion_operation_steps
                (operation_id,step,inputs_hash,state,attempt,execution_id) VALUES ($1,$2,$3,'running',$4,$5)`,
				id, step, inputsHash, attempt, executionID); err != nil {
				return err
			}
			return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: step, ExecutionID: executionID, Message: "Node step started."})
		}
		if err != nil {
			return err
		}
		if record.InputsHash != inputsHash {
			return contract.NewFault(contract.PreconditionFailed, "step inputs changed for an existing effect", "Never reuse an operation ID with different inputs; inspect the node journal.")
		}
		if record.State == "completed" {
			already = true
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_operation_steps SET state='running',attempt=$3,execution_id=$4,started_at=clock_timestamp(),error='' WHERE operation_id=$1 AND step=$2`,
			id, step, attempt, executionID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: step, ExecutionID: executionID, Message: "Node step retried."})
	})
	if err != nil {
		return false, err
	}
	if !owned {
		return false, errNotOwned
	}
	return already, nil
}

// ackStep records a completed effect at most once. Only the transaction that
// changes the step row from running/failed to completed appends progress.
func (s *Service) ackStep(ctx context.Context, id contract.OperationID, attempt uint32, step, inputsHash, executionID string, result json.RawMessage, total int) (bool, error) {
	acked := false
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		record, err := scanStep(tx.QueryRow(ctx, "SELECT "+stepColumns+" FROM eregion_operation_steps WHERE operation_id=$1 AND step=$2 FOR UPDATE", id, step))
		if errors.Is(err, pgx.ErrNoRows) {
			return contract.NewFault(contract.PreconditionFailed, "node step was not started in the database", "Begin the step before acknowledging its effect.")
		}
		if err != nil {
			return err
		}
		if record.InputsHash != inputsHash {
			return contract.NewFault(contract.PreconditionFailed, "step inputs changed for an existing effect", "Do not acknowledge an effect recorded with different inputs.")
		}
		if record.State == "completed" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_operation_steps SET state='completed',completed_at=clock_timestamp(),error='',result=$3,execution_id=$4 WHERE operation_id=$1 AND step=$2`,
			id, step, nullableJSON(result), executionID); err != nil {
			return err
		}
		var completed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_steps WHERE operation_id=$1 AND state='completed'`, id).Scan(&completed); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_operations SET completed_steps=$2 WHERE id=$1`, id, completed); err != nil {
			return err
		}
		acked = true
		return appendEvent(ctx, tx, op, contract.Event{
			Kind:     "progress",
			Progress: &contract.Progress{Current: uint64(completed), Total: uint64(total), Unit: "steps"},
			Message:  "Node effect acknowledged durably.",
		})
	})
	if err != nil {
		return false, err
	}
	if !owned {
		return false, errNotOwned
	}
	return acked, nil
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// failStep records a failed node step without touching a completed one.
func (s *Service) failStep(ctx context.Context, id contract.OperationID, attempt uint32, step, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		result, err := tx.Exec(ctx, `UPDATE eregion_operation_steps SET state='failed',completed_at=clock_timestamp(),error=$3 WHERE operation_id=$1 AND step=$2 AND state<>'completed'`,
			id, step, message)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return nil
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "warning", Stage: step, Message: "Node step failed: " + message})
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// finishSucceeded records terminal success without rewriting an existing terminal
// state. All step effects are already acknowledged when this is called.
func (s *Service) finishSucceeded(ctx context.Context, id contract.OperationID, attempt uint32) (contract.Operation, error) {
	op, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State == contract.Succeeded {
			return nil
		}
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "Reconciled node work from a recovered execution."); err != nil {
				return err
			}
		}
		return transition(ctx, tx, op, contract.Succeeded, "Diagnostic operation succeeded; node-local marker effects were acknowledged.")
	})
	if err != nil {
		return op, err
	}
	if !owned {
		return op, errNotOwned
	}
	return op, nil
}

// finishCancelled acknowledges cancellation at a step boundary. If every effect
// was already acknowledged, success is the honest result.
func (s *Service) finishCancelled(ctx context.Context, id contract.OperationID, attempt uint32, total int) (contract.Operation, error) {
	op, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		var completed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_steps WHERE operation_id=$1 AND state='completed'`, id).Scan(&completed); err != nil {
			return err
		}
		if completed == total {
			return transition(ctx, tx, op, contract.Succeeded, "Cancellation arrived after the final safe boundary; all node effects were already complete.")
		}
		if op.State != contract.Cancelling {
			return nil
		}
		if err := markIncompleteSteps(ctx, tx, id, "cancelled before the step completed"); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Canceled, "Cancellation acknowledged at a node step boundary; completed effects were not rolled back.")
	})
	if err != nil {
		return op, err
	}
	if !owned {
		return op, errNotOwned
	}
	return op, nil
}

func markIncompleteSteps(ctx context.Context, tx pgx.Tx, id contract.OperationID, message string) error {
	_, err := tx.Exec(ctx, `UPDATE eregion_operation_steps SET state='failed',completed_at=clock_timestamp(),error=$2 WHERE operation_id=$1 AND state<>'completed'`, id, message)
	return err
}

// failOperation makes a non-retryable node failure visible and closes any
// unfinished step rows, without rewriting a terminal operation.
func (s *Service) failOperation(ctx context.Context, id contract.OperationID, attempt uint32, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if err := markIncompleteSteps(ctx, tx, id, message); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) operationState(ctx context.Context, id contract.OperationID) (contract.State, uint32, error) {
	var state contract.State
	var attempt uint32
	if err := s.pool.QueryRow(ctx, "SELECT state,attempt FROM eregion_operations WHERE id=$1", id).Scan(&state, &attempt); err != nil {
		return "", 0, unavailable()
	}
	return state, attempt, nil
}

// recordRecoveredStep makes a node-side completed effect visible in operation
// history without claiming that this attempt re-ran it.
func (s *Service) recordRecoveredStep(ctx context.Context, id contract.OperationID, attempt uint32, step, inputsHash string, status node.Response) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		return appendEvent(ctx, tx, op, contract.Event{
			Kind:        "step",
			Stage:       step,
			ExecutionID: status.ExecutionID,
			Message:     "Reconciled a completed node effect without re-running it.",
		})
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// appendNodeEvents persists one redacted batch of node observations.
func (s *Service) appendNodeEvents(ctx context.Context, id contract.OperationID, attempt uint32, events []node.Event) error {
	if len(events) == 0 {
		return nil
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		for _, event := range events {
			kind := "log"
			if event.Kind == "warning" {
				kind = "warning"
			}
			message := event.Message
			if len(message) > 4096 {
				message = message[:4096]
			}
			if event.Kind == "step" {
				kind = "step"
			}
			contractEvent := contract.Event{Kind: kind, Message: message}
			if kind == "step" {
				contractEvent.Stage = event.Step
				contractEvent.ExecutionID = event.ExecutionID
			}
			if err := appendEvent(ctx, tx, op, contractEvent); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// reconcileCompletedSteps acknowledges node effects that completed but whose
// database acknowledgement was interrupted. It never starts new work.
func (s *Service) reconcileCompletedSteps(ctx context.Context, id contract.OperationID, attempt uint32, input operations.TestInput) (bool, error) {
	if s.transport == nil {
		return false, contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return false, err
	}
	live := false
	for _, record := range records {
		if record.State == "completed" {
			continue
		}
		exec := engine.Execution{OperationID: id, Attempt: attempt, Step: record.Step, RequestID: newRequestID()}
		status, err := s.transport.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
		if err != nil {
			return false, unavailable(err)
		}
		if status.InputsHash != "" && status.InputsHash != record.InputsHash {
			return false, contract.NewFault(contract.PreconditionFailed, "node effect has different inputs", "Do not acknowledge an effect recorded under other inputs.")
		}
		if status.Status == engine.StatusRunning {
			live = true
			continue
		}
		if status.Status != engine.StatusCompleted {
			continue
		}
		if _, err := s.ackStep(ctx, id, attempt, record.Step, record.InputsHash, status.ExecutionID, status.Result, input.Steps); err != nil {
			return false, err
		}
	}
	return live, nil
}
