package entities

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

var (
	// ErrJobRuntimeInitialized reports a second JobRuntime construction on one
	// Store. One Store owns at most one River client, so there is no second
	// insertion path.
	ErrJobRuntimeInitialized = errors.New("entities: the River job runtime is already initialized")
	// ErrJobRuntimeUninitialized reports a typed enqueue before the Store's
	// River client was constructed. There is no fallback client or raw insert.
	ErrJobRuntimeUninitialized = errors.New("entities: the River job runtime is not initialized")
)

// jobRuntime is the Store's single River insertion client. Construction does
// not start it: worker registration, queues and scheduling stay with the
// control-plane composition.
type jobRuntime struct {
	client *river.Client[pgx.Tx]
}

// JobRuntime is the typed lifecycle view of the Store's River client. It never
// exposes the client, driver or transaction. Start/Stop/StopAndCancel delegate
// to the one client owned by the Store.
type JobRuntime struct{ runtime *jobRuntime }

// NewJobRuntime constructs the Store's one River client over the same borrowed
// pool and returns its typed lifecycle view. It does not start the client,
// register workers or schedule jobs, and a second call is rejected. The client
// is never exported, so callers can insert only through the typed transaction
// methods and can control only the public lifecycle.
func (s *Store) NewJobRuntime(config *river.Config) (*JobRuntime, error) {
	if s == nil {
		return nil, errors.New("entities: a store is required for the job runtime")
	}
	if config == nil {
		return nil, errors.New("entities: a River configuration is required")
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.jobs != nil {
		return nil, ErrJobRuntimeInitialized
	}
	client, err := river.NewClient(riverpgxv5.New(s.pool), config)
	if err != nil {
		return nil, fmt.Errorf("entities: River client construction failed: %w", err)
	}
	s.jobs = &jobRuntime{client: client}
	return &JobRuntime{runtime: s.jobs}, nil
}

// Start starts the Store's River client. It is a no-op when the client is
// already running and reports ErrJobRuntimeUninitialized for a nil view.
func (j *JobRuntime) Start(ctx context.Context) error {
	if j == nil || j.runtime == nil || j.runtime.client == nil {
		return ErrJobRuntimeUninitialized
	}
	return j.runtime.client.Start(ctx)
}

// Stop gracefully stops the Store's River client. Stopping an unstarted client
// is a no-op.
func (j *JobRuntime) Stop(ctx context.Context) error {
	if j == nil || j.runtime == nil || j.runtime.client == nil {
		return ErrJobRuntimeUninitialized
	}
	return j.runtime.client.Stop(ctx)
}

// StopAndCancel hard-stops the Store's River client and cancels in-progress
// work.
func (j *JobRuntime) StopAndCancel(ctx context.Context) error {
	if j == nil || j.runtime == nil || j.runtime.client == nil {
		return ErrJobRuntimeUninitialized
	}
	return j.runtime.client.StopAndCancel(ctx)
}

func (s *Store) jobRuntime() (*jobRuntime, error) {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()
	if s.jobs == nil || s.jobs.client == nil {
		return nil, ErrJobRuntimeUninitialized
	}
	return s.jobs, nil
}

// JobTx is the typed River insertion view of one StoreTx. Every job is
// inserted on the caller's transaction, so a job and its domain rows commit or
// roll back together. No River client, driver or pgx transaction is exported.
type JobTx struct{ owner *StoreTx }

// Jobs returns the job insertion view of this transaction. The view is always
// constructed; an enqueue reports ErrJobRuntimeUninitialized when the Store has
// no runtime.
func (tx *StoreTx) Jobs() *JobTx { return &JobTx{owner: tx} }

// EnqueueDiagnostic inserts the diagnostic.test job for one operation.
func (j *JobTx) EnqueueDiagnostic(ctx context.Context, id contract.OperationID) (int64, error) {
	return j.insert(ctx, jobargs.DiagnosticTest{OperationID: id},
		&river.InsertOpts{Queue: jobargs.DiagnosticQueue, MaxAttempts: operations.TestMaxAttempts})
}

// EnqueuePlatform inserts the platform action job for one operation. Routing by
// operation kind remains the control plane's decision; this method always
// inserts a platform action.
func (j *JobTx) EnqueuePlatform(ctx context.Context, op contract.Operation) (int64, error) {
	return j.insert(ctx, jobargs.PlatformAction{OperationID: op.ID, ActionKind: op.Kind},
		&river.InsertOpts{Queue: jobargs.PlatformQueue, MaxAttempts: jobargs.PlatformMaxAttempts})
}

// EnqueueRetention inserts a retention operation job on the maintenance queue.
func (j *JobTx) EnqueueRetention(ctx context.Context, id contract.OperationID) (int64, error) {
	return j.insert(ctx, jobargs.Retention{OperationID: id},
		&river.InsertOpts{Queue: jobargs.MaintenanceQueue, MaxAttempts: jobargs.PlatformMaxAttempts})
}

// EnqueueWebhook inserts the durable webhook admission job for one delivery.
func (j *JobTx) EnqueueWebhook(ctx context.Context, environmentID, deliveryIDHash string) (int64, error) {
	return j.insert(ctx, jobargs.WebhookAdmission{EnvironmentID: environmentID, DeliveryIDHash: deliveryIDHash},
		&river.InsertOpts{Queue: jobargs.WebhookQueue, MaxAttempts: jobargs.PlatformMaxAttempts})
}

func (j *JobTx) insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (int64, error) {
	if j == nil || j.owner == nil || j.owner.tx == nil {
		return 0, errors.New("entities: a job transaction is required")
	}
	runtime, err := j.owner.store.jobRuntime()
	if err != nil {
		return 0, err
	}
	inserted, err := runtime.client.InsertTx(ctx, j.owner.tx, args, opts)
	if err != nil {
		return 0, err
	}
	return inserted.Job.ID, nil
}

// ID reports the one client's stable identity. It is metadata used for host
// ownership checks, not a handle to the client or its driver.
func (j *JobRuntime) ID() string {
	if j == nil || j.runtime == nil || j.runtime.client == nil {
		return ""
	}
	return j.runtime.client.ID()
}
