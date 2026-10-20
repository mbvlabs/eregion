package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

const diagnosticQueue = "diagnostic"

// Job arguments deliberately contain an operation reference only, not input or secrets.
type testArgs struct {
	OperationID contract.OperationID `json:"operation_id"`
}

func (testArgs) Kind() string { return "diagnostic_test" }

type reconcileArgs struct{}

func (reconcileArgs) Kind() string { return "operation_reconcile" }

type Runtime struct {
	Service *Service
	Jobs    *river.Client[pgx.Tx]
}

type WorkerConfig struct {
	Concurrency int
	// TestOnly removes River startup jitter. Never enable it in service commands.
	TestOnly bool
}

func New(pool *pgxpool.Pool, logger *slog.Logger, config WorkerConfig) (*Runtime, error) {
	if config.Concurrency < 0 || config.Concurrency > 16 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid worker concurrency", "Use 0 for insert-only, or 1–16 workers.")
	}
	service := &Service{pool: pool}
	workers := river.NewWorkers()
	river.AddWorker(workers, &testWorker{service: service})
	river.AddWorker(workers, &reconcileWorker{service: service})
	riverConfig := &river.Config{
		Workers: workers, Logger: logger, MaxAttempts: operations.TestMaxAttempts,
		JobTimeout: time.Minute, RescueStuckJobsAfter: 2 * time.Minute,
		SoftStopTimeout: 5 * time.Second, TestOnly: config.TestOnly,
	}
	if config.Concurrency > 0 {
		riverConfig.Queues = map[string]river.QueueConfig{
			diagnosticQueue: {MaxWorkers: config.Concurrency},
			"maintenance":   {MaxWorkers: 1},
		}
		riverConfig.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(5*time.Second), func() (river.JobArgs, *river.InsertOpts) {
				return reconcileArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 3}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		}
	}
	client, err := river.NewClient(riverpgxv5.New(pool), riverConfig)
	if err != nil {
		return nil, err
	}
	service.enqueue = func(ctx context.Context, tx pgx.Tx, id contract.OperationID) (int64, error) {
		inserted, err := client.InsertTx(ctx, tx, testArgs{OperationID: id}, &river.InsertOpts{Queue: diagnosticQueue, MaxAttempts: operations.TestMaxAttempts})
		if err != nil {
			return 0, err
		}
		return inserted.Job.ID, nil
	}
	return &Runtime{Service: service, Jobs: client}, nil
}

type testWorker struct {
	river.WorkerDefaults[testArgs]
	service *Service
}

func (*testWorker) NextRetry(job *river.Job[testArgs]) time.Time {
	return time.Now().Add(time.Duration(job.Attempt) * time.Second)
}

func (w *testWorker) Work(ctx context.Context, job *river.Job[testArgs]) error {
	var input operations.TestInput
	op, err := w.service.mutate(ctx, job.Args.OperationID, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if op.State == contract.Cancelling {
			return transition(ctx, tx, op, contract.Canceled, "Diagnostic cancellation acknowledged at a safe boundary.")
		}
		if op.Attempt > uint32(job.Attempt) {
			return nil // A rescued newer attempt owns database-only progress.
		}
		op.Attempt = uint32(job.Attempt)
		var raw []byte
		if err := tx.QueryRow(ctx, "UPDATE eregion_operations SET attempt=$2 WHERE id=$1 RETURNING input", op.ID, op.Attempt).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return err
		}
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "Diagnostic worker started."); err != nil {
				return err
			}
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: "diagnostic", Message: "Database-only diagnostic attempt started."})
	})
	if err != nil || op.State.Terminal() || op.Attempt > uint32(job.Attempt) {
		return err
	}
	if job.Attempt <= input.FailUntilAttempt {
		_, err := w.service.mutate(ctx, op.ID, func(tx pgx.Tx, current *contract.Operation) error {
			if current.State.Terminal() || current.Attempt != uint32(job.Attempt) {
				return nil
			}
			if current.State == contract.Cancelling {
				return transition(ctx, tx, current, contract.Canceled, "Diagnostic cancellation acknowledged.")
			}
			if job.Attempt >= job.MaxAttempts {
				return transition(ctx, tx, current, contract.Failed, "Diagnostic retry limit reached.")
			}
			return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Message: "Diagnostic requested a retry; work remains durably queued."})
		})
		if err != nil {
			return err
		}
		if job.Attempt >= job.MaxAttempts {
			return river.JobCancel(errors.New("diagnostic retry limit reached"))
		}
		return errors.New("diagnostic retry requested")
	}
	for {
		timer := time.NewTimer(time.Duration(input.StepDelayMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		op, err = w.service.mutate(ctx, op.ID, func(tx pgx.Tx, current *contract.Operation) error {
			if current.State.Terminal() || current.Attempt > uint32(job.Attempt) {
				return nil
			}
			if current.State == contract.Cancelling {
				return transition(ctx, tx, current, contract.Canceled, "Diagnostic cancellation acknowledged at a safe boundary.")
			}
			var completed int
			if err := tx.QueryRow(ctx, "UPDATE eregion_operations SET completed_steps=LEAST(completed_steps+1,$2) WHERE id=$1 RETURNING completed_steps", current.ID, input.Steps).Scan(&completed); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, current, contract.Event{Kind: "progress", Progress: &contract.Progress{Current: uint64(completed), Total: uint64(input.Steps), Unit: "steps"}, Message: "Diagnostic step committed."}); err != nil {
				return err
			}
			if completed == input.Steps {
				return transition(ctx, tx, current, contract.Succeeded, "Diagnostic operation succeeded; no host changes were made.")
			}
			return nil
		})
		if err != nil || op.State.Terminal() || op.Attempt > uint32(job.Attempt) {
			return err
		}
	}
}

type reconcileWorker struct {
	river.WorkerDefaults[reconcileArgs]
	service *Service
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[reconcileArgs]) error {
	return w.service.Reconcile(ctx)
}

// Reconcile covers exhausted attempts lost to a process crash, not just errors
// returned by Work. Domain terminal state cannot depend on an in-process callback.
func (s *Service) Reconcile(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT o.id FROM eregion_operations o LEFT JOIN river_job j ON j.id=o.job_id
        WHERE o.state IN ('queued','running','cancelling') AND (j.id IS NULL OR j.state IN ('discarded','cancelled','completed')) LIMIT 100`)
	if err != nil {
		return unavailable()
	}
	var ids []contract.OperationID
	for rows.Next() {
		var id contract.OperationID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.mutate(ctx, id, func(tx pgx.Tx, op *contract.Operation) error {
			if op.State.Terminal() {
				return nil
			}
			var ended bool
			if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM river_job WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1) AND state NOT IN ('discarded','cancelled','completed'))`, id).Scan(&ended); err != nil {
				return err
			}
			if !ended {
				return nil
			}
			return transition(ctx, tx, op, contract.Failed, "Execution ended without an acknowledged domain result; inspect operation history before retrying.")
		}); err != nil {
			return err
		}
	}
	return nil
}
