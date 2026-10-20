package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

const diagnosticQueue = "diagnostic"
const platformQueue = "platform"

// jobTimeout is the deadline one River job gets. It is the effective bound
// for a wait on a detached node task: the transport's own wait bound stays
// below it, so a worker never waits longer than its job.
const jobTimeout = time.Minute

// Job arguments deliberately contain an operation reference only, not input or secrets.
type testArgs struct {
	OperationID contract.OperationID `json:"operation_id"`
}

func (testArgs) Kind() string { return "diagnostic_test" }

// platformArgs deliberately contains only durable identity. Input stays in
// PostgreSQL and may be read by the worker; no secret material enters River.
type platformArgs struct {
	OperationID contract.OperationID `json:"operation_id"`
	ActionKind  string               `json:"action_kind"`
}

func (platformArgs) Kind() string { return "platform_action" }

type reconcileArgs struct{}

func (reconcileArgs) Kind() string { return "operation_reconcile" }

type Runtime struct {
	Service *Service
	Jobs    *river.Client[pgx.Tx]
	// Nodes resolves the transport of any node. Extensions that dispatch node
	// work (telemetry, resources) take it instead of a single transport.
	Nodes NodeTransports
}

// Close releases the per-node tunnels. Call it after the workers stopped.
func (r *Runtime) Close() {
	if r != nil && r.Service != nil && r.Service.resolver != nil {
		r.Service.resolver.Close()
	}
}

type WorkerConfig struct {
	Concurrency int
	// TestOnly removes River startup jitter. Never enable it in service commands.
	TestOnly bool
	// Transport defaults to a local node dispatcher. SSH and userspace WireGuard
	// adapters will implement the same interface.
	Transport node.Transport
	// NodeStateDir is the local dispatcher's shared state directory. Tests inject
	// a temporary directory; services use EREGION_NODE_STATE_DIR or the default.
	NodeStateDir string
	// SecretResolver resolves environment secret references at deployment time.
	// Nil means referenced secrets fail honestly instead of being injected.
	SecretResolver SecretResolver
	// SourceRoots is the directory-build allowlist. Empty uses
	// EREGION_BUILD_SOURCE_ROOTS (or /srv/eregion/sources).
	SourceRoots []string
	// Nodes configures how enrolled nodes are reached (SSH identity, WireGuard
	// key directory). The zero value serves only the fallback Transport.
	Nodes ResolverConfig
	// LocalNodeName is the identity of the in-process local transport. Empty
	// uses EREGION_NODE_NAME, then "local" for compatibility. A fresh main
	// node bootstrap writes "main".
	LocalNodeName string
}

func New(pool *pgxpool.Pool, logger *slog.Logger, config WorkerConfig) (*Runtime, error) {
	if config.Concurrency < 0 || config.Concurrency > 16 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid worker concurrency", "Use 0 for insert-only, or 1–16 workers.")
	}
	service := &Service{pool: pool, secrets: config.SecretResolver, sourceRoots: config.SourceRoots}
	service.transport = config.Transport
	if service.transport == nil && (config.Concurrency > 0 || config.NodeStateDir != "") {
		stateDir := config.NodeStateDir
		if stateDir == "" {
			stateDir = node.DefaultStateDir()
		}
		localName := config.LocalNodeName
		if localName == "" {
			localName = strings.TrimSpace(os.Getenv("EREGION_NODE_NAME"))
		}
		if localName == "" {
			localName = "local"
		}
		local, err := node.NewLocal(stateDir, localName, &secrets.Registry{})
		if err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "could not initialise the local node transport", "Check EREGION_NODE_STATE_DIR permissions and that every worker on this host uses the same directory.")
		}
		service.transport = local
		service.localNode = localName
	} else if local, ok := service.transport.(*node.Local); ok {
		// Tests and embedders inject the local transport directly. An SSH
		// transport is never mistaken for a local node merely because
		// EREGION_NODE_NAME is also set.
		service.localNode = local.NodeIdentity()
	}
	service.resolver = NewNodeResolver(pool, config.Nodes, func() node.Transport { return service.transport })
	workers := river.NewWorkers()
	river.AddWorker(workers, &testWorker{service: service})
	river.AddWorker(workers, &platformWorker{service: service})
	river.AddWorker(workers, &reconcileWorker{service: service})
	river.AddWorker(workers, &backupScheduleWorker{service: service, logger: logger})
	riverConfig := &river.Config{
		Workers: workers, Logger: logger, MaxAttempts: operations.TestMaxAttempts,
		JobTimeout: jobTimeout, RescueStuckJobsAfter: 2 * time.Minute,
		SoftStopTimeout: 5 * time.Second, TestOnly: config.TestOnly,
	}
	if config.Concurrency > 0 {
		riverConfig.Queues = map[string]river.QueueConfig{
			diagnosticQueue: {MaxWorkers: config.Concurrency},
			platformQueue:   {MaxWorkers: config.Concurrency},
			"maintenance":   {MaxWorkers: 1},
		}
		riverConfig.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(5*time.Second), func() (river.JobArgs, *river.InsertOpts) {
				return reconcileArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 3}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			// Backup policies are evaluated by every worker; the deterministic
			// idempotency key makes concurrent evaluation submit each slot once.
			river.NewPeriodicJob(river.PeriodicInterval(backupScheduleInterval), func() (river.JobArgs, *river.InsertOpts) {
				return backupScheduleArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: backupScheduleInterval / 2}}
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
	service.enqueuePlatform = func(ctx context.Context, tx pgx.Tx, op contract.Operation) (int64, error) {
		inserted, err := client.InsertTx(ctx, tx, platformArgs{OperationID: op.ID, ActionKind: op.Kind}, &river.InsertOpts{Queue: platformQueue, MaxAttempts: platformMaxAttempts})
		if err != nil {
			return 0, err
		}
		return inserted.Job.ID, nil
	}
	return &Runtime{Service: service, Jobs: client, Nodes: service.resolver}, nil
}

type testWorker struct {
	river.WorkerDefaults[testArgs]
	service *Service
}

func (*testWorker) NextRetry(job *river.Job[testArgs]) time.Time {
	return time.Now().Add(time.Duration(job.Attempt) * time.Second)
}

var (
	errOperationTerminal = errors.New("operation is terminal")
	errNotOwned          = errors.New("operation is owned by a newer attempt")
)

func (w *testWorker) Work(ctx context.Context, job *river.Job[testArgs]) error {
	attempt := uint32(job.Attempt)
	op, raw, claimed, err := w.service.claimAttempt(ctx, job.Args.OperationID, attempt, "diagnostic")
	if err != nil {
		return err
	}
	if !claimed {
		return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
	}
	var input operations.TestInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	if op.State == contract.Cancelling {
		return w.settleAttempt(ctx, op.ID, attempt, w.reconcileCancellation(ctx, op.ID, attempt, input))
	}
	if job.Attempt <= input.FailUntilAttempt {
		return w.settleAttempt(ctx, op.ID, attempt, w.injectedFailure(ctx, op.ID, attempt, job))
	}
	for index := 1; index <= input.Steps; index++ {
		state, currentAttempt, err := w.service.operationState(ctx, op.ID)
		if err != nil {
			return err
		}
		if currentAttempt != attempt {
			return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
		}
		if state.Terminal() {
			return nil
		}
		if state == contract.Cancelling {
			return w.settleAttempt(ctx, op.ID, attempt, w.reconcileCancellation(ctx, op.ID, attempt, input))
		}
		if err := w.runStep(ctx, op.ID, attempt, index, input); err != nil {
			return w.settleAttempt(ctx, op.ID, attempt, err)
		}
	}
	_, err = w.service.finishSucceeded(ctx, op.ID, attempt)
	return w.settleAttempt(ctx, op.ID, attempt, err)
}

// settleAttempt completes terminal work and makes a lost ownership race wait for
// the newer attempt. River's completion query is not attempt-fenced.
func (w *testWorker) settleAttempt(ctx context.Context, id contract.OperationID, attempt uint32, err error) error {
	if errors.Is(err, errOperationTerminal) {
		return nil
	}
	if errors.Is(err, errNotOwned) {
		return w.service.waitForNewerAttempt(ctx, id, attempt)
	}
	return err
}

func diagnosticStepID(index int) string { return node.DiagnosticStepID(index) }

func (w *testWorker) injectedFailure(ctx context.Context, id contract.OperationID, attempt uint32, job *river.Job[testArgs]) error {
	op, owned, err := w.service.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if current.State == contract.Cancelling {
			return transition(ctx, tx, current, contract.Canceled, "", "Diagnostic cancellation acknowledged.")
		}
		if job.Attempt >= job.MaxAttempts {
			if err := markIncompleteSteps(ctx, tx, id, contract.Unavailable, "retry limit reached before the step completed"); err != nil {
				return err
			}
			return transition(ctx, tx, current, contract.Failed, contract.Unavailable, "Diagnostic retry limit reached.")
		}
		return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Message: "Diagnostic requested a retry; work remains durably queued."})
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	if op.State.Terminal() {
		return nil
	}
	if job.Attempt >= job.MaxAttempts {
		return river.JobCancel(errors.New("diagnostic retry limit reached"))
	}
	return errors.New("diagnostic retry requested")
}

// runStep asks the database, then the node, whether an effect already exists.
// Only a node status of absent/stale/failed leads to a dispatch.
func (w *testWorker) runStep(ctx context.Context, id contract.OperationID, attempt uint32, index int, input operations.TestInput) error {
	if w.service.transport == nil {
		return contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	stepID := diagnosticStepID(index)
	exec := engine.Execution{OperationID: id, Attempt: attempt, Step: stepID, RequestID: newRequestID()}
	payload := diagnosticPayload(index, input)
	// The step is built through the same registry the node uses, so both sides
	// derive the same canonical inputs for the effect.
	step, err := node.DefaultRegistry().Build(exec, payload)
	if err != nil {
		return err
	}
	inputs, err := step.Inputs()
	if err != nil {
		return err
	}
	inputsHash := engine.InputsHash(step.ID(), inputs)

	record, hasRecord, err := w.service.stepRecord(ctx, id, stepID)
	if err != nil {
		return err
	}
	if hasRecord {
		if record.InputsHash != inputsHash {
			return w.failInputsMismatch(ctx, id, attempt, "step inputs changed for an existing effect")
		}
		if record.State == "completed" {
			return nil
		}
	}

	status, err := w.service.transport.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil {
		if errors.Is(err, engine.ErrJournalCorrupt) {
			if failErr := w.service.failOperation(ctx, id, attempt, contract.PreconditionFailed, "Node journal is corrupt; inspect it before retrying."); failErr != nil {
				return failErr
			}
			return errOperationTerminal
		}
		if node.IsPermanentTransport(err) {
			return w.failTransport(ctx, id, attempt, err)
		}
		return fmt.Errorf("node status %s: %w", stepID, err)
	}
	if status.InputsHash != "" && status.InputsHash != inputsHash {
		return w.failInputsMismatch(ctx, id, attempt, "node effect has different inputs")
	}
	if status.Status == engine.StatusCompleted {
		if !hasRecord {
			if _, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID); err != nil {
				return err
			}
		}
		if err := w.service.recordRecoveredStep(ctx, id, attempt, stepID, inputsHash, status); err != nil {
			return err
		}
		_, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID, status.Result, input.Steps)
		return err
	}

	already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, exec.ExecutionID())
	if err != nil {
		return err
	}
	if already {
		return nil
	}

	var nodeEvents []node.Event
	onEvent := func(event node.Event) { nodeEvents = append(nodeEvents, event) }
	response, err := w.service.transport.Call(ctx, node.Request{
		Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec,
		Payload: payload,
	}, onEvent)
	if appendErr := w.service.appendNodeEvents(ctx, id, attempt, nodeEvents); appendErr != nil && err == nil {
		err = appendErr
	}
	if err != nil {
		if node.IsPermanentTransport(err) {
			return w.failTransport(ctx, id, attempt, err)
		}
		// A lost stream is an unknown outcome, never proof that the node
		// stopped. The next attempt asks the node for its status first.
		return fmt.Errorf("node run %s: %w", stepID, err)
	}
	if w.service.afterNode != nil {
		if err := w.service.afterNode(id, stepID); err != nil {
			return err
		}
	}
	if response.InputsHash != "" && response.InputsHash != inputsHash {
		return w.failInputsMismatch(ctx, id, attempt, "node effect has different inputs")
	}
	switch response.Status {
	case engine.StatusCompleted:
		_, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, response.ExecutionID, response.Result, input.Steps)
		return err
	case engine.StatusRunning:
		// A live node owner holds the effect beyond our lock timeout. Do not
		// start a second Apply; let River retry and reconcile.
		return contract.NewFault(contract.Unavailable, "node task is still running", "The operation remains running; reconnecting or retrying will attach to the existing effect, not start another.")
	case engine.StatusFailed:
		code := response.ErrorCode
		if code == "" {
			code = string(contract.Internal)
		}
		if failErr := w.service.failStep(ctx, id, attempt, stepID, contract.Code(code), response.Detail); failErr != nil {
			return failErr
		}
		if response.Retryable {
			return fmt.Errorf("node step %s failed: %s", stepID, response.Detail)
		}
		if response.InputsMismatch {
			return w.failInputsMismatch(ctx, id, attempt, "node step failed with different inputs")
		}
		if err := w.service.failOperation(ctx, id, attempt, contract.Code(code), "Node step failed without a retryable cause: "+response.Detail); err != nil {
			return err
		}
		return errOperationTerminal
	default:
		return fmt.Errorf("node returned unexpected status %q for %s", response.Status, stepID)
	}
}

// failTransport ends an operation whose transport refused the request for a
// reason a retry cannot fix, such as a host-key mismatch.
func (w *testWorker) failTransport(ctx context.Context, id contract.OperationID, attempt uint32, cause error) error {
	message := "The node transport refused the request: " + cause.Error()
	if err := w.service.failOperation(ctx, id, attempt, contract.PreconditionFailed, message); err != nil {
		return err
	}
	return errOperationTerminal
}

// failInputsMismatch ends the operation once. A recorded effect under other
// inputs can never converge, so returning a retryable error would only loop.
func (w *testWorker) failInputsMismatch(ctx context.Context, id contract.OperationID, attempt uint32, message string) error {
	fault := contract.NewFault(contract.PreconditionFailed, message, "Do not re-apply an effect recorded under other inputs; inspect the node journal.")
	if err := w.service.failOperation(ctx, id, attempt, contract.PreconditionFailed, fault.Message); err != nil {
		return err
	}
	return errOperationTerminal
}

func diagnosticPayload(index int, input operations.TestInput) json.RawMessage {
	raw, _ := json.Marshal(node.DiagnosticPayload{Index: index, Total: input.Steps, StepDelayMS: input.StepDelayMS})
	return raw
}

func newRequestID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "request"
	}
	return hex.EncodeToString(value)
}

// reconcileCancellation acknowledges completed effects that were not yet
// acknowledged, then decides between success (all effects complete) and
// cancellation. It never starts a new node step.
func (w *testWorker) reconcileCancellation(ctx context.Context, id contract.OperationID, attempt uint32, input operations.TestInput) error {
	live, err := w.service.reconcileCompletedSteps(ctx, id, attempt, input)
	if err != nil {
		return err
	}
	if live {
		return contract.NewFault(contract.Unavailable, "node task is still running", "Cancellation waits for the in-flight step boundary; retry to reconcile the completed effect.")
	}
	if _, err := w.service.finishCancelled(ctx, id, attempt, input.Steps); err != nil {
		return err
	}
	return nil
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
	var failures []error
	for _, id := range ids {
		if err := s.reconcileOperation(ctx, id); err != nil {
			if errors.Is(err, errNotOwned) || errors.Is(err, errOperationTerminal) {
				continue
			}
			failures = append(failures, fmt.Errorf("reconcile %s: %w", id, err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func (s *Service) reconcileOperation(ctx context.Context, id contract.OperationID) error {
	if s.transport == nil {
		return contract.NewFault(contract.Unavailable, "node transport is not configured", "Reconciliation requires a worker node transport.")
	}
	op, err := scanOperation(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return unavailable()
	}
	if op.State.Terminal() {
		return nil
	}
	if strings.HasPrefix(op.Kind, "platform.") {
		return s.reconcilePlatformOperation(ctx, op)
	}
	var input operations.TestInput
	var rawInput []byte
	if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", id).Scan(&rawInput); err != nil {
		return unavailable()
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return err
	}
	live := false
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.State == "completed" {
			continue
		}
		exec := engine.Execution{OperationID: id, Attempt: op.Attempt, Step: record.Step, RequestID: newRequestID()}
		status, statusErr := s.transport.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
		if statusErr != nil {
			if errors.Is(statusErr, engine.ErrJournalCorrupt) {
				err := s.failOperation(ctx, id, op.Attempt, contract.PreconditionFailed, "Node journal is corrupt; inspect it before retrying.")
				if errors.Is(err, errNotOwned) {
					return nil
				}
				return err
			}
			if node.IsPermanentTransport(statusErr) {
				err := s.failOperation(ctx, id, op.Attempt, contract.PreconditionFailed, "The node transport refused the request: "+statusErr.Error())
				if errors.Is(err, errNotOwned) {
					return nil
				}
				return err
			}
			return unavailable(statusErr)
		}
		if status.InputsHash != "" && status.InputsHash != record.InputsHash {
			// The write is attempt-fenced like every other worker write: a
			// newer attempt owns the operation and must not be overwritten.
			_, owned, err := s.mutateOwned(ctx, id, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
				if current.State.Terminal() {
					return nil
				}
				return transition(ctx, tx, current, contract.Failed, contract.PreconditionFailed, "Node effect inputs changed during reconciliation; inspect the node journal.")
			})
			if !owned || errors.Is(err, errNotOwned) {
				return nil
			}
			return err
		}
		if status.Status == engine.StatusCompleted {
			if _, err := s.ackStep(ctx, id, op.Attempt, record.Step, record.InputsHash, status.ExecutionID, status.Result, input.Steps); err != nil {
				if errors.Is(err, errNotOwned) {
					return nil
				}
				return err
			}
			continue
		}
		if status.Status == engine.StatusRunning {
			live = true
		}
	}
	var completed int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_steps WHERE operation_id=$1 AND state='completed'`, id).Scan(&completed); err != nil {
		return unavailable()
	}
	if completed == input.Steps {
		_, err := s.finishSucceeded(ctx, id, op.Attempt)
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	if live {
		_, owned, err := s.mutateOwned(ctx, id, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
			if current.State.Terminal() {
				return nil
			}
			if current.State == contract.Queued {
				if err := transition(ctx, tx, current, contract.Running, "", "Reconciled a live node task after its job ended."); err != nil {
					return err
				}
			}
			return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Message: "Execution ended without a domain result, but the node task is still running; cancellation and failure wait for the in-flight step boundary."})
		})
		if !owned || errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	if op.State == contract.Cancelling {
		_, err := s.finishCancelled(ctx, id, op.Attempt, input.Steps)
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	_, owned, err := s.mutateOwned(ctx, id, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if err := markIncompleteSteps(ctx, tx, id, contract.Unavailable, "execution ended before the step was acknowledged"); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, contract.Unavailable, "Execution ended without an acknowledged domain result; inspect operation history before retrying.")
	})
	if !owned || errors.Is(err, errNotOwned) {
		return nil
	}
	return err
}

// waitForNewerAttempt does not write domain state while a newer River attempt
// owns the operation. River's completion query is not attempt-fenced, so
// returning early could complete the shared job row underneath newer work.
func (s *Service) waitForNewerAttempt(ctx context.Context, id contract.OperationID, attempt uint32) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, current, err := s.operationState(ctx, id)
		if err != nil {
			return err
		}
		if state.Terminal() || current <= attempt {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
