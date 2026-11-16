package entities

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// StepRecord is one durable node-effect acknowledgement. It is a row mapping,
// not a process outcome: the caller decides whether a completed step is already
// satisfied or a failed step may be retried.
type StepRecord struct {
	OperationID contract.OperationID
	Step        string
	InputsHash  string
	State       string
	Attempt     uint32
	ExecutionID string
	StartedAt   time.Time
	CompletedAt *time.Time
	Error       string
	ErrorCode   contract.Code
	Result      json.RawMessage
}

// StepStore is the read view over operation steps. It borrows the Store's pool
// and exposes no query, transaction or driver handle.
type StepStore struct{ q *query.Queries }

// Steps returns the step read view of this Store.
func (s *Store) Steps() *StepStore { return &StepStore{q: query.New(s.pool)} }

// Get returns one step and whether it exists.
func (s *StepStore) Get(ctx context.Context, id contract.OperationID, step string) (StepRecord, bool, error) {
	row, err := s.q.GetOperationStep(ctx, query.GetOperationStepParams{OperationID: string(id), Step: step})
	if errors.Is(err, pgx.ErrNoRows) {
		return StepRecord{}, false, nil
	}
	if err != nil {
		return StepRecord{}, false, err
	}
	record, err := stepRecord(row.OperationID, row.Step, row.InputsHash, row.State,
		row.Attempt, row.ExecutionID, row.StartedAt, row.CompletedAt, row.Error,
		row.ErrorCode, row.Result)
	if err != nil {
		return StepRecord{}, false, err
	}
	return record, true, nil
}

// List returns every step of one operation ordered by step name.
func (s *StepStore) List(ctx context.Context, id contract.OperationID) ([]StepRecord, error) {
	rows, err := s.q.ListOperationSteps(ctx, string(id))
	if err != nil {
		return nil, err
	}
	return stepRecordsFromList(rows)
}

// ListByPattern returns the steps whose name matches any LIKE pattern plus the exact
// names in exact. It is the neutral form of the action-payload projections that
// read one family of steps and a few named boundaries.
func (s *StepStore) ListByPattern(ctx context.Context, id contract.OperationID, patterns, exact []string) ([]StepRecord, error) {
	rows, err := s.q.ListOperationStepsByPattern(ctx, query.ListOperationStepsByPatternParams{
		OperationID: string(id), Patterns: patterns, Exact: exact,
	})
	if err != nil {
		return nil, err
	}
	return stepRecordsFromPattern(rows)
}

// StepTx is the step view of one StoreTx. It exposes persistence primitives
// only: the caller keeps the completed-never-reopened and retry decisions.
type StepTx struct{ owner *StoreTx }

// Steps returns the step view of this transaction.
func (tx *StoreTx) Steps() *StepTx { return &StepTx{owner: tx} }

// Get returns one step and whether it exists without locking it.
func (s *StepTx) Get(ctx context.Context, id contract.OperationID, step string) (StepRecord, bool, error) {
	row, err := s.owner.q.GetOperationStep(ctx, query.GetOperationStepParams{OperationID: string(id), Step: step})
	if errors.Is(err, pgx.ErrNoRows) {
		return StepRecord{}, false, nil
	}
	if err != nil {
		return StepRecord{}, false, err
	}
	record, err := stepRecord(row.OperationID, row.Step, row.InputsHash, row.State,
		row.Attempt, row.ExecutionID, row.StartedAt, row.CompletedAt, row.Error,
		row.ErrorCode, row.Result)
	if err != nil {
		return StepRecord{}, false, err
	}
	return record, true, nil
}

// GetForUpdate locks one step row until the caller's transaction ends.
func (s *StepTx) GetForUpdate(ctx context.Context, id contract.OperationID, step string) (StepRecord, bool, error) {
	row, err := s.owner.q.LockOperationStep(ctx, query.LockOperationStepParams{OperationID: string(id), Step: step})
	if errors.Is(err, pgx.ErrNoRows) {
		return StepRecord{}, false, nil
	}
	if err != nil {
		return StepRecord{}, false, err
	}
	record, err := stepRecord(row.OperationID, row.Step, row.InputsHash, row.State,
		row.Attempt, row.ExecutionID, row.StartedAt, row.CompletedAt, row.Error,
		row.ErrorCode, row.Result)
	if err != nil {
		return StepRecord{}, false, err
	}
	return record, true, nil
}

// List returns every step of one operation ordered by step name.
func (s *StepTx) List(ctx context.Context, id contract.OperationID) ([]StepRecord, error) {
	rows, err := s.owner.q.ListOperationSteps(ctx, string(id))
	if err != nil {
		return nil, err
	}
	return stepRecordsFromList(rows)
}

// ListByPattern returns the steps whose name matches any LIKE pattern plus the exact
// names in exact.
func (s *StepTx) ListByPattern(ctx context.Context, id contract.OperationID, patterns, exact []string) ([]StepRecord, error) {
	rows, err := s.owner.q.ListOperationStepsByPattern(ctx, query.ListOperationStepsByPatternParams{
		OperationID: string(id), Patterns: patterns, Exact: exact,
	})
	if err != nil {
		return nil, err
	}
	return stepRecordsFromPattern(rows)
}

// Insert records a running step before dispatch.
func (s *StepTx) Insert(ctx context.Context, id contract.OperationID, step, inputsHash string, attempt uint32, executionID string) error {
	value, err := operationAttempt(attempt)
	if err != nil {
		return err
	}
	return s.owner.q.InsertOperationStep(ctx, query.InsertOperationStepParams{
		OperationID: string(id), Step: step, InputsHash: inputsHash,
		Attempt: value, ExecutionID: executionID,
	})
}

// Restart marks an existing step running again for this attempt.
func (s *StepTx) Restart(ctx context.Context, id contract.OperationID, step string, attempt uint32, executionID string) (bool, error) {
	value, err := operationAttempt(attempt)
	if err != nil {
		return false, err
	}
	affected, err := s.owner.q.RestartOperationStep(ctx, query.RestartOperationStepParams{
		OperationID: string(id), Step: step, Attempt: value, ExecutionID: executionID,
	})
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// Complete marks one step completed. The caller has already decided that a
// completed step must not be rewritten and that the inputs hash matched.
func (s *StepTx) Complete(ctx context.Context, id contract.OperationID, step, executionID string, result json.RawMessage) (bool, error) {
	affected, err := s.owner.q.CompleteOperationStep(ctx, query.CompleteOperationStepParams{
		OperationID: string(id), Step: step, ExecutionID: executionID,
		Result: nullableStepResult(result),
	})
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// Fail marks one not-yet-completed step failed.
func (s *StepTx) Fail(ctx context.Context, id contract.OperationID, step string, code contract.Code, message string) (bool, error) {
	affected, err := s.owner.q.FailOperationStep(ctx, query.FailOperationStepParams{
		OperationID: string(id), Step: step, Message: message, ErrorCode: string(code),
	})
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// FailIncomplete marks every not-yet-completed step failed.
func (s *StepTx) FailIncomplete(ctx context.Context, id contract.OperationID, code contract.Code, message string) (int64, error) {
	return s.owner.q.FailIncompleteOperationSteps(ctx, query.FailIncompleteOperationStepsParams{
		OperationID: string(id), Message: message, ErrorCode: string(code),
	})
}

// CountCompleted counts the durably acknowledged steps of one operation.
func (s *StepTx) CountCompleted(ctx context.Context, id contract.OperationID) (uint32, error) {
	count, err := s.owner.q.CountCompletedOperationSteps(ctx, string(id))
	if err != nil {
		return 0, err
	}
	return operationAttemptValue(count)
}

func nullableStepResult(result json.RawMessage) *json.RawMessage {
	if len(result) == 0 || string(result) == "null" {
		return nil
	}
	value := result
	return &value
}

func stepRecordsFromList(rows []query.ListOperationStepsRow) ([]StepRecord, error) {
	result := make([]StepRecord, 0, len(rows))
	for _, row := range rows {
		record, err := stepRecord(row.OperationID, row.Step, row.InputsHash, row.State,
			row.Attempt, row.ExecutionID, row.StartedAt, row.CompletedAt, row.Error,
			row.ErrorCode, row.Result)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func stepRecordsFromPattern(rows []query.ListOperationStepsByPatternRow) ([]StepRecord, error) {
	result := make([]StepRecord, 0, len(rows))
	for _, row := range rows {
		record, err := stepRecord(row.OperationID, row.Step, row.InputsHash, row.State,
			row.Attempt, row.ExecutionID, row.StartedAt, row.CompletedAt, row.Error,
			row.ErrorCode, row.Result)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func stepRecord(operationID, step, inputsHash, state string, attempt int64, executionID string,
	startedAt, completedAt pgtype.Timestamptz, message, errorCode string, result *json.RawMessage,
) (StepRecord, error) {
	current, err := operationAttemptValue(attempt)
	if err != nil {
		return StepRecord{}, err
	}
	started, err := operationTimestamp(startedAt)
	if err != nil {
		return StepRecord{}, err
	}
	record := StepRecord{
		OperationID: contract.OperationID(operationID),
		Step:        step,
		InputsHash:  inputsHash,
		State:       state,
		Attempt:     current,
		ExecutionID: executionID,
		StartedAt:   started,
		Error:       message,
		ErrorCode:   contract.Code(errorCode),
	}
	if completedAt.Valid {
		completed := completedAt.Time.UTC()
		record.CompletedAt = &completed
	}
	if result != nil && len(*result) > 0 && string(*result) != "null" {
		record.Result = *result
	}
	return record, nil
}

// CountCompleted reports the number of completed steps of one operation.
func (s *StepStore) CountCompleted(ctx context.Context, id contract.OperationID) (int, error) {
	count, err := s.q.CountCompletedOperationSteps(ctx, string(id))
	if err != nil {
		return 0, err
	}
	return int(count), nil
}
