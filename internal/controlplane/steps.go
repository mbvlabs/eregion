package controlplane

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
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
	ErrorCode   contract.Code        `json:"error_code,omitempty"`
	Result      json.RawMessage      `json:"result,omitempty"`
}

func stepRecordFromEntities(record entities.StepRecord) StepRecord {
	return StepRecord{
		OperationID: record.OperationID, Step: record.Step, InputsHash: record.InputsHash,
		State: record.State, Attempt: record.Attempt, ExecutionID: record.ExecutionID,
		StartedAt: record.StartedAt, CompletedAt: record.CompletedAt, Error: record.Error,
		ErrorCode: record.ErrorCode, Result: record.Result,
	}
}

func (s *Service) stepRecord(ctx context.Context, id contract.OperationID, step string) (StepRecord, bool, error) {
	record, found, err := s.entities.Steps().Get(ctx, id, step)
	if err != nil {
		return StepRecord{}, false, unavailable(err)
	}
	if !found {
		return StepRecord{}, false, nil
	}
	return stepRecordFromEntities(record), true, nil
}

func (s *Service) stepRecords(ctx context.Context, id contract.OperationID) ([]StepRecord, error) {
	records, err := s.entities.Steps().List(ctx, id)
	if err != nil {
		return nil, unavailable(err)
	}
	result := make([]StepRecord, 0, len(records))
	for _, record := range records {
		result = append(result, stepRecordFromEntities(record))
	}
	return result, nil
}

// claimAttempt makes this River attempt the operation's owner. A newer attempt
// already in the database is not displaced.
func (s *Service) claimAttempt(ctx context.Context, id contract.OperationID, attempt uint32, stage string) (contract.Operation, json.RawMessage, bool, error) {
	var input json.RawMessage
	op, claimed, err := s.mutateOwnedOrClaim(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "", "Diagnostic worker started through node dispatch."); err != nil {
				return err
			}
		}
		raw, found, err := tx.Operations().ClaimAttempt(ctx, op.ID, op.Attempt)
		if err != nil {
			return err
		}
		if !found {
			return contract.NewFault(contract.NotFound, "operation disappeared while it was claimed", "Inspect operation history; do not resubmit accepted work.")
		}
		input = raw
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: stage, Message: "Node dispatch attempt started."})
	})
	return op, input, claimed, err
}

// mutateOwnedOrClaim is mutateOwned, but it lets a strictly newer River attempt
// take ownership and refuses an older one.
func (s *Service) mutateOwnedOrClaim(ctx context.Context, id contract.OperationID, attempt uint32, change func(*entities.StoreTx, *contract.Operation) error) (contract.Operation, bool, error) {
	var op contract.Operation
	claimed := false
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, found, err := tx.Operations().Lock(ctx, id)
		if err != nil || !found {
			return unavailable(err)
		}
		op = current
		if op.State.Terminal() || op.Attempt > attempt {
			return nil
		}
		if op.Attempt < attempt {
			op.Attempt = attempt
		}
		if err := change(tx, &op); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return op, false, unavailable(err)
		}
		return op, false, err
	}
	return op, claimed, nil
}

// beginStep durably records a running step before dispatch. A completed step is
// never reopened; a failed or stale running step may be retried.
func (s *Service) beginStep(ctx context.Context, id contract.OperationID, attempt uint32, step, inputsHash, executionID string) (bool, error) {
	already := false
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		record, found, err := tx.Steps().GetForUpdate(ctx, id, step)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			if err := tx.Steps().Insert(ctx, id, step, inputsHash, attempt, executionID); err != nil {
				return err
			}
			return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: step, ExecutionID: executionID, Message: "Node step started."})
		}
		if record.InputsHash != inputsHash {
			return contract.NewFault(contract.PreconditionFailed, "step inputs changed for an existing effect", "Never reuse an operation ID with different inputs; inspect the node journal.")
		}
		if record.State == "completed" {
			already = true
			return nil
		}
		if _, err := tx.Steps().Restart(ctx, id, step, attempt, executionID); err != nil {
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
func (s *Service) ackStep(ctx context.Context, id contract.OperationID, attempt uint32, step, inputsHash, executionID string, result json.RawMessage, total int) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		record, found, err := tx.Steps().GetForUpdate(ctx, id, step)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return contract.NewFault(contract.PreconditionFailed, "node step was not started in the database", "Begin the step before acknowledging its effect.")
		}
		if record.InputsHash != inputsHash {
			return contract.NewFault(contract.PreconditionFailed, "step inputs changed for an existing effect", "Do not acknowledge an effect recorded with different inputs.")
		}
		if record.State == "completed" {
			return nil
		}
		if _, err := tx.Steps().Complete(ctx, id, step, executionID, result); err != nil {
			return err
		}
		completed, err := tx.Steps().CountCompleted(ctx, id)
		if err != nil {
			return err
		}
		if err := tx.Operations().SetCompletedSteps(ctx, id, completed); err != nil {
			return err
		}
		// Cleanup and setup steps can run beyond the planned count; the
		// measured total never falls below what has completed.
		if int(completed) > total {
			total = int(completed)
		}
		return appendEvent(ctx, tx, op, contract.Event{
			Kind:     "progress",
			Progress: &contract.Progress{Current: uint64(completed), Total: uint64(total), Unit: "steps"}, //nolint:gosec // G115: step counts are non-negative database counters.
			Message:  "Node effect acknowledged durably.",
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

// failStep records a failed node step without touching a completed one. The
// error code is machine-readable: a caller does not have to read the message.
func (s *Service) failStep(ctx context.Context, id contract.OperationID, attempt uint32, step string, code contract.Code, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		failed, err := tx.Steps().Fail(ctx, id, step, code, message)
		if err != nil {
			return err
		}
		if !failed {
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
func (s *Service) finishSucceeded(ctx context.Context, id contract.OperationID, attempt uint32) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State == contract.Succeeded {
			return nil
		}
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "", "Reconciled node work from a recovered execution."); err != nil {
				return err
			}
		}
		return transition(ctx, tx, op, contract.Succeeded, "", "Diagnostic operation succeeded; node-local marker effects were acknowledged.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// finishCancelled acknowledges cancellation at a step boundary. If every effect
// was already acknowledged, success is the honest result.
func (s *Service) finishCancelled(ctx context.Context, id contract.OperationID, attempt uint32, total int) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		completed, err := tx.Steps().CountCompleted(ctx, id)
		if err != nil {
			return err
		}
		if int(completed) == total {
			return transition(ctx, tx, op, contract.Succeeded, "", "Cancellation arrived after the final safe boundary; all node effects were already complete.")
		}
		if op.State != contract.Cancelling {
			return nil
		}
		if err := markIncompleteSteps(ctx, tx, id, contract.Unavailable, "cancelled before the step completed"); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Canceled, "", "Cancellation acknowledged at a node step boundary; completed effects were not rolled back.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func markIncompleteSteps(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, code contract.Code, message string) error {
	_, err := tx.Steps().FailIncomplete(ctx, id, code, message)
	return err
}

// failOperation makes a non-retryable node failure visible and closes any
// unfinished step rows, without rewriting a terminal operation. The error code
// is machine-readable.
func (s *Service) failOperation(ctx context.Context, id contract.OperationID, attempt uint32, code contract.Code, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if err := markIncompleteSteps(ctx, tx, id, code, message); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, code, message)
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
	state, attempt, found, err := s.entities.Operations().StateAttempt(ctx, id)
	if err != nil || !found {
		return "", 0, unavailable(err)
	}
	return state, attempt, nil
}

// recordRecoveredStep makes a node-side completed effect visible in operation
// history without claiming that this attempt re-ran it.
func (s *Service) recordRecoveredStep(ctx context.Context, id contract.OperationID, attempt uint32, step string, status node.Response) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
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
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		for _, event := range events {
			if err := appendEvent(ctx, tx, op, s.nodeContractEvent(event)); err != nil {
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

func (s *Service) nodeContractEvent(event node.Event) contract.Event {
	kind := "log"
	if event.Kind == "warning" {
		kind = "warning"
	}
	message := s.redaction.RedactPreservingLength(event.Message)
	if len(message) > 4096 {
		const marker = " [clipped after redaction]"
		end := 4096 - len(marker)
		for end > 0 && !utf8.RuneStart(message[end]) {
			end--
		}
		message = message[:end] + marker
	}
	if event.Kind == "step" {
		kind = "step"
	}
	contractEvent := contract.Event{Kind: kind, Stage: event.Step, Step: event.Step, Message: message}
	if kind == "step" {
		contractEvent.ExecutionID = event.ExecutionID
	}
	return contractEvent
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
		if err := s.ackStep(ctx, id, attempt, record.Step, record.InputsHash, status.ExecutionID, status.Result, input.Steps); err != nil {
			return false, err
		}
	}
	return live, nil
}
