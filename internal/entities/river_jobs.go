package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/riverquery"
)

// RunningRiverJob is the app-owned observed state of one running River job.
type RunningRiverJob struct {
	ID          int64
	AttemptedBy []string
	OperationID contract.OperationID
	Attempt     uint32
	MaxAttempts uint32
}

// RiverRescue identifies the observed running job owner used by the
// conditional discard/requeue updates.
type RiverRescue struct {
	ID         int64
	Queue      string
	Attempt    uint32
	LastClient string
}

// RiverJobStore is the read/reconcile view of River metadata.
type RiverJobStore struct{ q *riverquery.Queries }

// RiverJobs returns the River metadata view of this Store. It has no insert or
// delete methods; enqueue remains StoreTx.Jobs() on the same client.
func (s *Store) RiverJobs() *RiverJobStore { return &RiverJobStore{q: riverquery.New(s.pool)} }

// RiverJobTx is the transaction-scoped River metadata view.
type RiverJobTx struct{ q *riverquery.Queries }

// RiverJobs returns the River metadata view of this transaction.
func (tx *StoreTx) RiverJobs() *RiverJobTx { return &RiverJobTx{q: riverquery.New(tx.tx)} }

// ListRunningForQueue returns running jobs for the rescue scan.
func (s *RiverJobStore) ListRunningForQueue(ctx context.Context, queue string) ([]RunningRiverJob, error) {
	rows, err := s.q.ListRunningRiverJobsForQueue(ctx, queue)
	if err != nil {
		return nil, err
	}
	result := make([]RunningRiverJob, 0, len(rows))
	for _, row := range rows {
		if row.ID < 0 {
			return nil, errors.New("entities: river job id is negative")
		}
		attempt, err := platformUint32(row.Attempt)
		if err != nil {
			return nil, err
		}
		maxAttempts, err := platformUint32(row.MaxAttempts)
		if err != nil {
			return nil, err
		}
		result = append(result, RunningRiverJob{
			ID: row.ID, AttemptedBy: row.AttemptedBy,
			OperationID: contract.OperationID(row.Column3),
			Attempt:     attempt, MaxAttempts: maxAttempts,
		})
	}
	return result, nil
}

// GetState reads a River state without a lock.
func (s *RiverJobStore) GetState(ctx context.Context, id int64) (string, bool, error) {
	return riverJobState(ctx, s.q, id)
}

// GetState reads a River state inside the caller's transaction.
func (tx *RiverJobTx) GetState(ctx context.Context, id int64) (string, bool, error) {
	return riverJobState(ctx, tx.q, id)
}

func riverJobState(ctx context.Context, q *riverquery.Queries, id int64) (string, bool, error) {
	state, err := q.GetRiverJobState(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(state), true, nil
}

// GetForUpdate row-locks one River job and returns its state.
func (tx *RiverJobTx) GetForUpdate(ctx context.Context, id int64) (string, bool, error) {
	state, err := tx.q.GetRiverJobForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(state), true, nil
}

// DiscardRunning applies the observed-owner conditional discard.
func (tx *RiverJobTx) DiscardRunning(ctx context.Context, rescue RiverRescue, reason string) (bool, error) {
	rows, err := tx.q.DiscardRunningRiverJob(ctx, riverquery.DiscardRunningRiverJobParams{
		ID: rescue.ID, Queue: rescue.Queue, Attempt: int64(rescue.Attempt),
		LastClient: rescue.LastClient, Reason: reason,
	})
	return rows == 1, err
}

// RequeueRunning applies the observed-owner conditional available update.
func (tx *RiverJobTx) RequeueRunning(ctx context.Context, rescue RiverRescue) (bool, error) {
	rows, err := tx.q.RequeueRunningRiverJob(ctx, riverquery.RequeueRunningRiverJobParams{
		ID: rescue.ID, Queue: rescue.Queue, Attempt: int64(rescue.Attempt), LastClient: rescue.LastClient,
	})
	return rows == 1, err
}

// Snooze schedules a job and raises attempt/max_attempts to the observed
// definition attempt.
func (tx *RiverJobTx) Snooze(ctx context.Context, id int64, attempt uint32, delaySeconds int) (bool, error) {
	if id < 0 || delaySeconds < 0 {
		return false, errors.New("entities: invalid River snooze parameters")
	}
	rows, err := tx.q.SnoozeRiverJobWithAttempt(ctx, riverquery.SnoozeRiverJobWithAttemptParams{
		ID: id, Attempt: int64(attempt), DelaySeconds: int64(delaySeconds),
	})
	return rows == 1, err
}

// Reschedule schedules a job without changing its attempt, used by rescue.
func (tx *RiverJobTx) Reschedule(ctx context.Context, id int64, delaySeconds int) (bool, error) {
	if id < 0 || delaySeconds < 0 {
		return false, errors.New("entities: invalid River reschedule parameters")
	}
	rows, err := tx.q.RescheduleRiverJob(ctx, riverquery.RescheduleRiverJobParams{
		ID: id, DelaySeconds: int64(delaySeconds),
	})
	return rows == 1, err
}

// ListReconcileCandidateOperations returns active operations whose River job is
// missing or terminal.
func (s *RiverJobStore) ListReconcileCandidateOperations(ctx context.Context, limit int) ([]contract.OperationID, error) {
	if limit < 1 {
		return nil, errors.New("entities: River reconcile limit must be positive")
	}
	ids, err := s.q.ListReconcileCandidateOperations(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	result := make([]contract.OperationID, 0, len(ids))
	for _, id := range ids {
		result = append(result, contract.OperationID(id))
	}
	return result, nil
}
