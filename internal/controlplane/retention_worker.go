package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/riverqueue/river"
)

// retentionWorker executes one durable retention operation. The periodic daily
// job submits the operation and returns; the operation's own River job runs the
// planner and every effect. The separation keeps a pruned release's plan and
// its completed effects durable across a retry.
type retentionWorker struct {
	river.WorkerDefaults[jobargs.Retention]
	service *Service
}

func (*retentionWorker) Timeout(*river.Job[jobargs.Retention]) time.Duration { return 30 * time.Minute }

func (*retentionWorker) NextRetry(job *river.Job[jobargs.Retention]) time.Time {
	delay := time.Duration(job.Attempt) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay)
}

func (w *retentionWorker) Work(ctx context.Context, job *river.Job[jobargs.Retention]) error {
	if job.Args.OperationID == "" {
		return w.service.submitDailyRetention(ctx)
	}
	attempt := uint32(job.Attempt) //nolint:gosec // G115: River attempts are non-negative.
	op, claimed, err := w.service.claimRetention(ctx, job.Args.OperationID, attempt)
	if err != nil {
		return err
	}
	if !claimed {
		return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
	}
	if op.State == contract.Cancelling {
		return w.service.cancelPlatform(ctx, op.ID, attempt, "Cancellation requested before retention execution started.")
	}
	workCtx, claim, err := w.service.acquireRetentionTargets(ctx, op)
	if err != nil {
		return err
	}
	runErr := w.service.runRetentionAttempt(workCtx, op, attempt)
	if runErr == nil {
		runErr = w.service.finishRetentionOperation(workCtx, op.ID, attempt)
	} else {
		runErr = w.service.settleRetentionFailure(workCtx, op, attempt, job, runErr)
	}
	return errors.Join(runErr, w.service.settleRetentionTargets(ctx, claim))
}

// Reserve the complete set in the same transaction that gates platform
// submissions. Platform/environment creation must honor this same gate and
// the committed platform claim so the environment set cannot grow afterward.
func (s *Service) acquireRetentionTargets(ctx context.Context, op contract.Operation) (context.Context, entities.TargetClaimID, error) {
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetNode {
		return ctx, entities.TargetClaimID{}, contract.NewFault(contract.PreconditionFailed, "retention target is not a node", "Submit prune as node/<server>.")
	}
	// Only a previously sealed executor may be reconciled by a retry.
	var previous entities.TargetClaim
	var found bool
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		previous, found, err = tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		return err
	})
	if err != nil {
		return ctx, entities.TargetClaimID{}, err
	}
	if found && previous.Sealed {
		if err := s.settleRetentionTargets(ctx, previous.ID); err != nil {
			return ctx, entities.TargetClaimID{}, err
		}
	}
	token, err := contract.NewOperationID()
	if err != nil {
		return ctx, entities.TargetClaimID{}, err
	}
	claim := entities.TargetClaimID{OperationID: op.ID, Token: fmt.Sprintf("%d/%s", op.Attempt, token)}
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if current.Kind != platform.KindRetentionPrune || current.Target != op.Target {
			return errNotOwned
		}
		return reserveRetentionTargetsTx(ctx, tx, claim, op.Target, target.Node)
	})
	if err != nil {
		return ctx, entities.TargetClaimID{}, err
	}
	if !owned {
		return ctx, entities.TargetClaimID{}, errNotOwned
	}
	return context.WithValue(ctx, buildTargetContextKey{}, buildTargetExecutor{claim: claim, target: op.Target}), claim, nil
}

func reserveRetentionTargetsTx(ctx context.Context, tx *entities.StoreTx, claim entities.TargetClaimID, target, nodeName string) error {
	_, found, err := tx.UnreleasedTargetClaimByOperation(ctx, claim.OperationID)
	if err != nil {
		return err
	}
	if found {
		return river.JobSnooze(deploymentOwnerDefer)
	}
	locked, err := tx.Platform().Locks().TryTargetXact(ctx, targetLockKey("platform"))
	if err != nil {
		return err
	}
	if !locked {
		return river.JobSnooze(deploymentOwnerDefer)
	}
	rows, err := tx.Platform().Environments().RetentionRows(ctx, nodeName)
	if err != nil {
		return err
	}
	targets := []string{"platform", target, "registry"}
	for _, row := range rows {
		targets = append(targets, "env/"+row.App+"/"+row.Name)
	}
	acquired, err := acquirePlatformTargetClaimTx(ctx, tx, claim, targets)
	if err != nil {
		return err
	}
	if !acquired {
		return river.JobSnooze(deploymentOwnerDefer)
	}
	return nil
}

// An incomplete dispatch is not proof that the node stopped. Leave its
// sealed reservation durable rather than allowing a conflicting mutation.
func (s *Service) settleRetentionTargets(ctx context.Context, claim entities.TargetClaimID) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, err := retentionSettlementClaim(ctx, tx, claim)
		if err != nil || current.Sealed {
			return err
		}
		_, err = tx.SealTargetClaim(ctx, claim)
		return err
	}); err != nil {
		return err
	}
	// Neither a failed journal entry nor a status from a freshly resolved node
	// proves the original Docker action is quiescent on its original placement.
	return s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, err := retentionSettlementClaim(ctx, tx, claim)
		if err != nil {
			return err
		}
		if !current.Sealed {
			return errNotOwned
		}
		steps, err := tx.Steps().List(ctx, claim.OperationID)
		if err != nil {
			return err
		}
		for _, step := range steps {
			if step.State != "completed" {
				return nil
			}
		}
		_, err = tx.ReleaseTargetClaim(ctx, claim)
		return err
	})
}

func retentionSettlementClaim(ctx context.Context, tx *entities.StoreTx, claim entities.TargetClaimID) (entities.TargetClaim, error) {
	_, found, err := tx.Operations().Lock(ctx, claim.OperationID)
	if err != nil {
		return entities.TargetClaim{}, err
	}
	if !found {
		return entities.TargetClaim{}, errNotOwned
	}
	current, found, err := tx.UnreleasedTargetClaimByOperation(ctx, claim.OperationID)
	if err != nil {
		return entities.TargetClaim{}, err
	}
	if !found || current.ID != claim {
		return entities.TargetClaim{}, errNotOwned
	}
	return current, nil
}

// retentionDailyCheckInterval is how often the maintenance queue checks
// whether the UTC day still needs a retention run. It is not the retention
// policy: the submitter itself enforces one successful run per day.
const retentionDailyCheckInterval = time.Hour

// retentionDailyRetryDelay bounds a deterministic failure to one new daily
// attempt per hour. A failed or cancelled run younger than this is left alone.
const retentionDailyRetryDelay = time.Hour

// retentionDailyKeyPrefix is the stable prefix of every automatic daily
// operation's idempotency key. The first run of a day uses
// "retention-daily:<date>"; a same-day retry uses
// "retention-daily:<date>:<n>".
const retentionDailyKeyPrefix = "retention-daily:"

// submitDailyRetention submits at most one successful retention operation per
// UTC day. An in-progress or already succeeded operation for the day stops it;
// a failed or cancelled day gets a new numbered idempotency key, but only after
// retentionDailyRetryDelay. The check runs hourly and on worker start, so a
// failed run can be retried the same day without a tight loop.
func (s *Service) submitDailyRetention(ctx context.Context) error {
	nodeName, err := s.retentionNodeName("")
	if err != nil {
		return err
	}
	date := time.Now().UTC().Format("2006-01-02")
	existing, err := s.dailyRetentionOperations(ctx, nodeName, date)
	if err != nil {
		return err
	}
	key := retentionDailyKeyPrefix + date
	if len(existing) > 0 {
		latest := existing[len(existing)-1]
		for _, operation := range existing {
			if operation.State == contract.Succeeded || !operation.State.Terminal() {
				return nil
			}
		}
		if time.Since(latest.UpdatedAt) < retentionDailyRetryDelay {
			return nil
		}
		key = fmt.Sprintf("%s%s:%d", retentionDailyKeyPrefix, date, len(existing)+1)
	}
	input, err := json.Marshal(platform.RetentionPruneInput{Node: nodeName})
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode the retention input", "This is an internal error; do not resubmit accepted work.")
	}
	_, err = s.Submit(ctx, operations.Submission{
		Kind: platform.KindRetentionPrune, Target: "node/" + nodeName, Actor: maintenanceActor,
		IdempotencyKey: key, Input: input,
	})
	return err
}

// dailyRetentionOperations returns this UTC day's automatic retention
// operations in creation order. A same-day retry shares the date prefix, so
// the legacy first-run key and every numbered retry are both included.
func (s *Service) dailyRetentionOperations(ctx context.Context, nodeName, date string) ([]contract.Operation, error) {
	operations, err := s.entities.Operations().ListByIdentityPattern(ctx, entities.OperationIdentityPattern{
		Kind: platform.KindRetentionPrune, Actor: maintenanceActor,
		Target: "node/" + nodeName, IdempotencyPattern: retentionDailyKeyPrefix + date + "%",
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return operations, nil
}

func (s *Service) claimRetention(ctx context.Context, id contract.OperationID, attempt uint32) (contract.Operation, bool, error) {
	op, claimed, err := s.mutateOwnedOrClaim(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "", "Retention worker started durable execution."); err != nil {
				return err
			}
		}
		if _, found, err := tx.Operations().ClaimAttempt(ctx, id, op.Attempt); err != nil {
			return err
		} else if !found {
			return contract.NewFault(contract.NotFound, "operation disappeared while it was claimed", "Inspect operation history; do not resubmit accepted work.")
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: "retention", Message: "Retention execution attempt started under the environment locks."})
	})
	return op, claimed, err
}

func (s *Service) finishRetentionOperation(ctx context.Context, id contract.OperationID, attempt uint32) error {
	return s.finishPlatformOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		return appendEvent(ctx, tx, op, contract.Event{Kind: "log", Stage: "retention", Message: "Retention completed; removed artifacts and reclaimed bytes are recorded in the retention run."})
	})
}

// settleRetentionFailure records a retryable failure for River or fails the
// operation when no attempt remains or the failure is permanent. A permanent
// registry refusal (for example deletes still disabled) is not retried eight
// times: the next periodic run retries it after the host is fixed.
func (s *Service) settleRetentionFailure(ctx context.Context, op contract.Operation, attempt uint32, job *river.Job[jobargs.Retention], cause error) error {
	code := contract.Classify(cause).Code
	permanent := errors.Is(cause, engine.ErrPermanent) || errors.Is(cause, errNodeStepPermanentFailure) ||
		code == contract.ValidationFailed || code == contract.PreconditionFailed ||
		code == contract.NotFound || code == contract.Forbidden || code == contract.Conflict
	if !permanent && job.Attempt < job.MaxAttempts {
		message := "retention attempt did not complete"
		if text := s.cleanupCauseText(cause); text != "" {
			message += ": " + text
		}
		if err := s.recordRetryResult(context.WithoutCancel(ctx), op.ID, attempt, code, message); err != nil {
			return err
		}
		return cause
	}
	if code == contract.Internal || code == "" {
		code = contract.Unavailable
	}
	message := "retention did not complete"
	if text := s.cleanupCauseText(cause); text != "" {
		message += ": " + text
	}
	if err := s.failPlatformOperation(context.WithoutCancel(ctx), op.ID, attempt, code, message); err != nil {
		return err
	}
	return nil
}

func (s *Service) runRetentionAttempt(ctx context.Context, op contract.Operation, attempt uint32) error {
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetNode {
		return contract.NewFault(contract.PreconditionFailed, "retention target is not a node", "Submit prune as node/<server>.")
	}
	nodeName := target.Node
	run, exists, err := s.retentionRunForOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if exists && retentionRunPlanFailed(run) {
		// A failed observation never became a plan. Re-observe rather than
		// treating an empty planned list as a completed prune.
		exists = false
	}
	now := time.Now().UTC()
	if exists {
		run, err = s.resumeRetentionRun(ctx, run)
	} else {
		run, err = s.planRetentionRun(ctx, op, attempt, nodeName, now)
	}
	if err != nil {
		return err
	}
	return s.executeRetentionRun(ctx, op, attempt, &run, nodeName)
}

func (s *Service) planRetentionRun(ctx context.Context, op contract.Operation, attempt uint32, nodeName string, now time.Time) (retention.Run, error) {
	inventory, input, err := s.loadRetentionInput(ctx, nodeName, now)
	if err != nil {
		s.recordRetentionPlanFailure(ctx, op, attempt, err)
		return retention.Run{}, err
	}
	result := retention.PlanInventory(inventory, retention.PlatformState{
		Environments: input.Environments,
		Releases:     input.Releases,
		Deployments:  input.Deployments,
		InFlight:     input.InFlight,
		ServerPolicy: input.ServerPolicy,
	}, now)
	run := retention.Run{
		OperationID: string(op.ID), Status: retention.RunRunning,
		Planned: result.Items, PlannedBytes: result.PlannedBytes,
		StartedAt: now,
	}
	retentionApplyCacheMeasurement(&run, inventory)
	run.ID, err = retention.NewRunID()
	if err != nil {
		return retention.Run{}, contract.NewFault(contract.Internal, "could not allocate a retention run", "Report the binary version.")
	}
	if err := s.insertRetentionRun(ctx, run); err != nil {
		return retention.Run{}, err
	}
	s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
		fmt.Sprintf("Retention planned %d item(s), %d byte(s) on node %s.", len(retentionRemovalItems(run.Planned)), run.PlannedBytes, nodeName))
	return run, nil
}

func (s *Service) resumeRetentionRun(ctx context.Context, run retention.Run) (retention.Run, error) {
	run.Status = retention.RunRunning
	run.FinishedAt = nil
	if err := s.updateRetentionRun(ctx, run); err != nil {
		return retention.Run{}, err
	}
	return run, nil
}

// retentionRunForOperation returns the newest run for one operation. A retry
// reuses its planned items, so an effect that failed after a release row was
// already marked pruned is still retried instead of disappearing from a fresh
// plan.
func (s *Service) retentionRunForOperation(ctx context.Context, id contract.OperationID) (retention.Run, bool, error) {
	record, found, err := s.entities.Retention().LatestRunForOperation(ctx, id)
	if err != nil {
		return retention.Run{}, false, unavailable(err)
	}
	if !found {
		return retention.Run{}, false, nil
	}
	run := retention.Run{
		ID: record.ID, OperationID: string(record.OperationID), Status: retention.RunStatus(record.Status),
		PlannedBytes: record.PlannedBytes, ReclaimedBytes: record.ReclaimedBytes,
		StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
		CacheBytes: record.CacheBytes, CacheMeasuredAt: record.CacheMeasuredAt,
		CacheError: record.CacheMeasurementError, GCGeneration: record.GCGeneration,
	}
	run.Planned = decodeRetentionItems(record.PlannedItems)
	run.Removed = decodeRetentionItems(record.RemovedItems)
	run.Errors = decodeRetentionErrors(record.Errors)
	return run, true, nil
}

func (s *Service) insertRetentionRun(ctx context.Context, run retention.Run) error {
	record, err := retentionRunRecord(run)
	if err != nil {
		return unavailable(err)
	}
	if err := s.retentionWrite(ctx, func(tx *entities.StoreTx) error {
		return tx.Retention().InsertRun(ctx, entities.RetentionRunInsert(record))
	}); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) updateRetentionRun(ctx context.Context, run retention.Run) error {
	record, err := retentionRunRecord(run)
	if err != nil {
		return unavailable(err)
	}
	if err := s.retentionWrite(ctx, func(tx *entities.StoreTx) error {
		return tx.Retention().UpdateRun(ctx, record)
	}); err != nil {
		return unavailable(err)
	}
	return nil
}

func retentionRunRecord(run retention.Run) (entities.RetentionRun, error) {
	planned, removed, runErrors, err := retentionRunJSON(run)
	if err != nil {
		return entities.RetentionRun{}, err
	}
	return entities.RetentionRun{
		ID: run.ID, OperationID: contract.OperationID(run.OperationID), Status: string(run.Status),
		PlannedItems: planned, RemovedItems: removed, PlannedBytes: run.PlannedBytes,
		ReclaimedBytes: run.ReclaimedBytes, Errors: runErrors, StartedAt: run.StartedAt,
		FinishedAt: run.FinishedAt, CacheBytes: run.CacheBytes, CacheMeasuredAt: run.CacheMeasuredAt,
		CacheMeasurementError: run.CacheError, GCGeneration: run.GCGeneration,
	}, nil
}

// Retention rows and release marks must be fenced in the same transaction as
// the write, not by a check before an external observation or effect.
func (s *Service) retentionWrite(ctx context.Context, write func(*entities.StoreTx) error) error {
	return s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if executor, ok := ctx.Value(buildTargetContextKey{}).(buildTargetExecutor); ok {
			generation, _, _ := strings.Cut(executor.claim.Token, "/")
			attempt, err := strconv.ParseUint(generation, 10, 32)
			if err != nil {
				return errNotOwned
			}
			op, found, err := tx.Operations().Lock(ctx, executor.claim.OperationID)
			if err != nil {
				return err
			}
			if !found || op.State.Terminal() || op.Attempt != uint32(attempt) {
				return errNotOwned
			}
			if err := guardBuildTargetExecutor(ctx, tx, op); err != nil {
				return err
			}
		}
		return write(tx)
	})
}

func retentionRunJSON(run retention.Run) (planned, removed, runErrors []byte, err error) {
	if run.Planned == nil {
		run.Planned = []retention.Item{}
	}
	if run.Removed == nil {
		run.Removed = []retention.Item{}
	}
	if run.Errors == nil {
		run.Errors = []retention.RunError{}
	}
	if planned, err = json.Marshal(run.Planned); err != nil {
		return nil, nil, nil, err
	}
	if removed, err = json.Marshal(run.Removed); err != nil {
		return nil, nil, nil, err
	}
	if runErrors, err = json.Marshal(run.Errors); err != nil {
		return nil, nil, nil, err
	}
	return planned, removed, runErrors, nil
}

func decodeRetentionItems(raw []byte) []retention.Item {
	items := []retention.Item{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &items)
	}
	return items
}

func decodeRetentionErrors(raw []byte) []retention.RunError {
	items := []retention.RunError{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &items)
	}
	return items
}

func retentionRunPlanFailed(run retention.Run) bool {
	for _, item := range run.Errors {
		if item.Stage == "plan" {
			return true
		}
	}
	return false
}

func (s *Service) recordRetentionPlanFailure(ctx context.Context, op contract.Operation, attempt uint32, cause error) {
	runID, err := retention.NewRunID()
	if err != nil {
		return
	}
	run := retention.Run{
		ID: runID, OperationID: string(op.ID), Status: retention.RunFailed,
		Planned: []retention.Item{}, Removed: []retention.Item{}, StartedAt: time.Now().UTC(),
	}
	finished := time.Now().UTC()
	run.FinishedAt = &finished
	if text := s.cleanupCauseText(cause); text != "" {
		run.Errors = append(run.Errors, retention.RunError{Stage: "plan", Message: text})
	}
	if err := s.insertRetentionRun(ctx, run); err != nil {
		s.appendCleanupWarning(ctx, op.ID, attempt, "Retention could not record its failed plan: "+err.Error())
	}
}

// executeRetentionRun applies one planned run: mark release rows, remove the
// artifacts, and garbage-collect the registry if any manifest was deleted.
// Every effect records its own success or failure, so a retry resumes rather
// than repeating completed removals. Before an artifact is removed the run is
// re-evaluated against the current protection state; anything no longer
// eligible is dropped with a recorded reason.
func (s *Service) executeRetentionRun(ctx context.Context, op contract.Operation, attempt uint32, run *retention.Run, nodeName string) error {
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetNode {
		return contract.NewFault(contract.PreconditionFailed, "retention target is not a node", "Submit prune as node/<server>.")
	}
	if s.resolver == nil {
		return contract.NewFault(contract.Unavailable, "the worker has no node resolver", "Run retention in a worker configured with node transports.")
	}
	transport, err := s.resolver.TransportFor(ctx, nodeName)
	if err != nil {
		return err
	}
	execution := newRetentionExecution(s, ctx, op, attempt, run, nodeName, transport)
	if s.beforeRetentionMark != nil {
		s.beforeRetentionMark(ctx, op.ID)
	}
	// A persisted plan can be older than the policy or the serving state. Drop
	// what is no longer eligible before any release row is marked.
	if err := s.revalidateRetentionRun(ctx, nodeName, attempt, run, execution.removed); err != nil {
		return err
	}
	if err := execution.markReleases(); err != nil {
		return err
	}
	execution.prepareRegistryGC()
	if err := execution.removeItems(); err != nil {
		return err
	}
	execution.dropSkipped()
	if err := execution.finalizeRegistryGC(); err != nil {
		return err
	}
	return execution.finish()
}

type retentionExecution struct {
	service   *Service
	ctx       context.Context
	op        contract.Operation
	attempt   uint32
	run       *retention.Run
	nodeName  string
	transport node.Transport
	removed   map[string]bool

	failures                   []error
	protected                  map[string]bool
	markFailed                 map[string]bool
	protectedScopes            map[string]bool
	skippedItems               map[string]bool
	registryRemovedThisAttempt bool
	registryFailedThisAttempt  bool
	registryGCStateBlocked     bool
	gcPending                  bool
}

func newRetentionExecution(service *Service, ctx context.Context, op contract.Operation, attempt uint32, run *retention.Run, nodeName string, transport node.Transport) *retentionExecution {
	removed := map[string]bool{}
	for _, item := range run.Removed {
		removed[retentionItemKey(item)] = true
	}
	return &retentionExecution{
		service: service, ctx: ctx, op: op, attempt: attempt, run: run, nodeName: nodeName, transport: transport,
		removed: removed, protected: map[string]bool{}, markFailed: map[string]bool{},
		protectedScopes: map[string]bool{}, skippedItems: map[string]bool{},
	}
}

func (e *retentionExecution) markReleases() error {
	changed := false
	for _, item := range e.run.Planned {
		if item.Kind != retention.ItemRelease || e.removed[retentionItemKey(item)] {
			continue
		}
		safe, err := e.service.markRetentionReleaseForAttempt(e.ctx, item.Reference)
		if err != nil {
			e.markFailed[item.Reference] = true
			e.protectedScopes[item.App+"\x00"+item.Environment] = true
			e.service.recordRetentionRunError(e.run, "mark_release", item.Reference, err)
			e.failures = append(e.failures, err)
			continue
		}
		if !safe {
			e.protected[item.Reference] = true
			e.protectedScopes[item.App+"\x00"+item.Environment] = true
			e.service.appendWorkerEvent(e.ctx, e.op.ID, e.attempt, "retention",
				"Release "+item.Reference+" became serving, a rollback target or in flight after planning; it is not pruned.")
			continue
		}
		e.run.Removed = append(e.run.Removed, item)
		e.removed[retentionItemKey(item)] = true
		changed = true
		e.service.appendWorkerEvent(e.ctx, e.op.ID, e.attempt, "retention", "Marked release "+item.Reference+" pruned; its row remains for history.")
	}
	filtered := make([]retention.Item, 0, len(e.run.Planned))
	for _, item := range e.run.Planned {
		if e.retainPlannedItem(item) {
			filtered = append(filtered, item)
		} else {
			changed = true
		}
	}
	e.run.Planned = filtered
	if !changed {
		return nil
	}
	return e.service.updateRetentionRun(e.ctx, *e.run)
}

func (e *retentionExecution) retainPlannedItem(item retention.Item) bool {
	if e.removed[retentionItemKey(item)] {
		return false
	}
	switch item.Kind {
	case retention.ItemRelease:
		// A failed mark stays in the plan so the next River attempt re-attempts
		// it. Only a release that became protected/stale is dropped.
		return !e.protected[item.Reference]
	case retention.ItemImage, retention.ItemImageSkipped:
		return !e.protected[item.Release]
	case retention.ItemRegistryArtifact, retention.ItemDanglingImage:
		return len(e.protected) == 0 && len(e.markFailed) == 0
	case retention.ItemContainer:
		return !e.protectedScopes[item.App+"\x00"+item.Environment]
	case retention.ItemRegistrySkipped, retention.ItemRegistryGC, retention.ItemCacheSkipped, retention.ItemBuildpackCache:
		// Diagnostics and cache removals carry no additional release guard.
		return true
	default:
		return true
	}
}

func (e *retentionExecution) prepareRegistryGC() {
	gcPending, err := e.service.retentionGCPending(e.ctx)
	if err != nil {
		e.service.recordRetentionRunError(e.run, "registry_gc_state", "registry-gc", err)
		e.failures = append(e.failures, err)
	}
	e.gcPending = gcPending
	for _, item := range e.run.Planned {
		if item.Kind != retention.ItemRegistryArtifact || e.removed[retentionItemKey(item)] {
			continue
		}
		// Make the follow-up durable before any manifest delete. A crash after
		// the delete can then never leave an un-GCed registry.
		if pendingErr := e.service.setRetentionGCPending(e.ctx, true); pendingErr != nil {
			e.service.recordRetentionRunError(e.run, "registry_gc_state", "registry-gc", pendingErr)
			e.failures = append(e.failures, pendingErr)
			e.registryGCStateBlocked = true
		} else {
			e.gcPending = true
		}
		break
	}
}

func (e *retentionExecution) removeItems() error {
	attempted := map[string]bool{}
	for {
		item, ok := nextRetentionRemoval(e.run, e.removed, attempted)
		if !ok {
			return nil
		}
		stop, err := e.removeItem(item, attempted)
		if err != nil || stop {
			return err
		}
	}
}

func (e *retentionExecution) removeItem(item retention.Item, attempted map[string]bool) (bool, error) {
	// Re-run protection and resolution before every effect. A retry or a
	// manually changed release/container must not delete something that became
	// protected after the original plan.
	if err := e.service.revalidateRetentionRun(e.ctx, e.nodeName, e.attempt, e.run, e.removed); err != nil {
		return true, err
	}
	if !retentionPlanContains(e.run, item) {
		return false, nil
	}
	attempted[retentionItemKey(item)] = true
	if skip := e.removalPrecondition(item); skip {
		return false, nil
	}
	stepID, payload := retentionRemovalPayload(item)
	result, err := (&platformWorker{service: e.service}).runStepOn(e.ctx, e.transport, e.op.ID, e.attempt, stepID, payload, nil)
	if err != nil {
		e.handleRemovalError(item, err)
		return false, nil
	}
	if skipped, reason := retentionStepSkipped(result); skipped {
		if reason == "" {
			reason = "the node reported a deterministic skip"
		}
		e.recordSkip("skipped", item, reason, "")
		return false, nil
	}
	e.run.Removed = append(e.run.Removed, item)
	e.removed[retentionItemKey(item)] = true
	if item.Bytes > 0 {
		e.run.ReclaimedBytes += item.Bytes
	}
	if item.Kind == retention.ItemRegistryArtifact {
		e.registryRemovedThisAttempt = true
	}
	if err := e.service.updateRetentionRun(e.ctx, *e.run); err != nil {
		return true, err
	}
	e.service.appendWorkerEvent(e.ctx, e.op.ID, e.attempt, "retention",
		fmt.Sprintf("Removed %s %s (%d bytes).", item.Kind, item.Reference, item.Bytes))
	return false, nil
}

func (e *retentionExecution) removalPrecondition(item retention.Item) bool {
	if item.Kind == retention.ItemRegistryArtifact && e.registryGCStateBlocked {
		// The durable GC follow-up could not be recorded; never delete a
		// manifest without it. The item stays planned for the retry.
		return true
	}
	if item.Kind == retention.ItemImage && e.markFailed[item.Release] {
		// The row could not be marked this attempt, so its image must not be
		// removed yet. Keep the item for the retry.
		return true
	}
	if item.Kind != retention.ItemImage {
		return false
	}
	shared, reason, err := e.service.retentionImageStillShared(e.ctx, item.Reference, item.Release, e.transport)
	if err != nil {
		e.recordFailure("image_shared_check", item.Reference, err)
		return true
	}
	if !shared {
		return false
	}
	e.recordSkip("image_skipped", item, reason,
		"Skipped local image "+item.Reference+" for release "+item.Release+": "+reason)
	return true
}

func (e *retentionExecution) handleRemovalError(item retention.Item, err error) {
	if item.Kind == retention.ItemRegistryArtifact {
		e.registryFailedThisAttempt = true
	}
	e.recordFailure(string(item.Kind), item.Reference, err)
}

func retentionRemovalPayload(item retention.Item) (string, platform.RetentionRemovePayload) {
	stepID := platform.RetentionRemoveStepID(item.Kind, item.Reference)
	payload := platform.RetentionRemovePayload{
		Step: stepID, Kind: string(item.Kind), Reference: item.Reference,
	}
	if item.Kind == retention.ItemContainer {
		payload.App = item.App
		payload.Environment = item.Environment
	}
	if item.Kind == retention.ItemRegistryArtifact {
		payload.Registry = platform.DefaultRegistryAddr
	}
	return stepID, payload
}

func (e *retentionExecution) recordFailure(stage, reference string, err error) {
	e.service.recordRetentionRunError(e.run, stage, reference, err)
	e.failures = append(e.failures, err)
}

func (e *retentionExecution) recordSkip(stage string, item retention.Item, reason, event string) {
	e.service.recordRetentionSkip(e.run, stage, item.Reference, reason)
	if event == "" {
		event = "Skipped " + string(item.Kind) + " " + item.Reference + ": " + reason
	}
	e.service.appendWorkerEvent(e.ctx, e.op.ID, e.attempt, "retention", event)
	e.skippedItems[retentionItemKey(item)] = true
}

func (e *retentionExecution) dropSkipped() {
	if len(e.skippedItems) == 0 {
		return
	}
	kept := make([]retention.Item, 0, len(e.run.Planned))
	for _, item := range e.run.Planned {
		if e.skippedItems[retentionItemKey(item)] {
			continue
		}
		kept = append(kept, item)
	}
	e.run.Planned = kept
}

func (e *retentionExecution) registryUnsettled() bool {
	for _, item := range e.run.Planned {
		if item.Kind == retention.ItemRegistryArtifact && !e.removed[retentionItemKey(item)] {
			return true
		}
	}
	return false
}

func (e *retentionExecution) finalizeRegistryGC() error {
	if e.registryUnsettled() || e.registryFailedThisAttempt || (!e.registryRemovedThisAttempt && !e.gcPending) {
		return nil
	}
	if err := e.service.setRetentionGCPending(e.ctx, true); err != nil {
		e.recordFailure("registry_gc_state", "registry-gc", err)
		return nil
	}
	allowed, reason, err := e.service.retentionGCEligible(e.ctx, e.nodeName)
	if err != nil {
		e.recordFailure("registry_gc_check", "registry-gc", err)
		return nil
	}
	if !allowed {
		e.service.recordRetentionSkip(e.run, "registry_gc_skipped", "registry-gc", reason)
		e.service.appendWorkerEvent(e.ctx, e.op.ID, e.attempt, "retention", "Skipped registry garbage collection: "+reason)
		return nil
	}
	return e.runRegistryGC()
}

func (e *retentionExecution) runRegistryGC() error {
	generation := e.run.GCGeneration
	switch {
	case generation == 0:
		generation = 1
	case e.registryRemovedThisAttempt:
		// A completed earlier GC pass must not satisfy this one.
		generation++
	}
	if generation != e.run.GCGeneration {
		e.run.GCGeneration = generation
		if err := e.service.updateRetentionRun(e.ctx, *e.run); err != nil {
			return err
		}
	}
	stepID := platform.RetentionGCStepID(generation)
	payload := platform.RetentionGCPayload{Step: stepID, Generation: generation}
	if _, err := (&platformWorker{service: e.service}).runStepOn(e.ctx, e.transport, e.op.ID, e.attempt, stepID, payload, nil); err != nil {
		e.recordFailure("registry_gc", "registry-gc", err)
		return nil
	}
	if err := e.service.setRetentionGCPending(e.ctx, false); err != nil {
		e.recordFailure("registry_gc_state", "registry-gc", err)
		return nil
	}
	if !e.removed[retentionGCMarker] {
		marker := retention.Item{Kind: retention.ItemRegistryGC, Reference: "registry-gc"}
		e.run.Removed = append(e.run.Removed, marker)
		e.removed[retentionGCMarker] = true
	}
	if err := e.service.updateRetentionRun(e.ctx, *e.run); err != nil {
		return err
	}
	e.service.appendWorkerEvent(e.ctx, e.op.ID, e.attempt, "retention", "Registry garbage collection completed.")
	return nil
}

func (e *retentionExecution) finish() error {
	finished := time.Now().UTC()
	e.run.FinishedAt = &finished
	if len(e.failures) > 0 {
		e.run.Status = retention.RunFailed
	} else if retentionRunHasSkips(e.run.Errors) {
		e.run.Status = retention.RunSucceededWithSkips
	} else {
		e.run.Status = retention.RunSucceeded
	}
	if err := e.service.updateRetentionRun(e.ctx, *e.run); err != nil {
		return err
	}
	if len(e.failures) == 0 {
		return nil
	}
	e.service.appendWorkerEvent(e.ctx, e.op.ID, e.attempt, "retention",
		fmt.Sprintf("Retention run %s recorded %d failure(s); unfinished effects remain for retry.", e.run.ID, len(e.run.Errors)))
	return errors.Join(e.failures...)
}

const retentionGCMarker = "registry_gc\x00registry-gc"

func retentionItemKey(item retention.Item) string {
	return string(item.Kind) + "\x00" + item.Reference
}

// retentionStepSkipped reads the per-item skip result a node effect records when
// it deterministically refuses a removal. A skip is a completed effect, not an
// error: the item leaves the plan and its reason stays in the run.
func retentionStepSkipped(result json.RawMessage) (bool, string) {
	if len(result) == 0 {
		return false, ""
	}
	var value struct {
		Skipped bool   `json:"skipped"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(result, &value) != nil {
		return false, ""
	}
	return value.Skipped, value.Reason
}

// retentionRunHasSkips reports whether the run recorded any per-item skip
// diagnostic. Only skip stages count; a real failure keeps the failed status.
func retentionRunHasSkips(runErrors []retention.RunError) bool {
	for _, item := range runErrors {
		if item.Stage == "skipped" || item.Stage == "stale_plan" || strings.HasSuffix(item.Stage, "_skipped") {
			return true
		}
	}
	return false
}

// retentionApplyCacheMeasurement records the node's latest cache observation.
// A nil CacheBytes means unknown; doctor must never render that as a healthy
// zero.
func retentionApplyCacheMeasurement(run *retention.Run, inventory retention.Inventory) {
	measured := inventory.CollectedAt.UTC()
	if measured.IsZero() {
		measured = time.Now().UTC()
	}
	run.CacheMeasuredAt = &measured
	unknown := inventory.CacheSizeUnknown
	for _, volume := range inventory.CacheVolumes {
		if volume.SizeUnknown {
			unknown = true
		}
	}
	if unknown {
		run.CacheBytes = nil
		run.CacheError = inventory.CacheError
		if run.CacheError == "" {
			run.CacheError = "one or more Buildpacks cache volumes could not be measured"
		}
		return
	}
	var total int64
	for _, volume := range inventory.CacheVolumes {
		total += volume.SizeBytes
	}
	run.CacheBytes = &total
	run.CacheError = ""
}

func nextRetentionRemoval(run *retention.Run, removed, attempted map[string]bool) (retention.Item, bool) {
	for _, item := range run.Planned {
		if removed[retentionItemKey(item)] || attempted[retentionItemKey(item)] {
			continue
		}
		switch item.Kind {
		case retention.ItemRelease, retention.ItemRegistrySkipped, retention.ItemImageSkipped, retention.ItemCacheSkipped, retention.ItemRegistryGC:
			continue
		case retention.ItemRegistryArtifact, retention.ItemImage, retention.ItemBuildpackCache, retention.ItemContainer, retention.ItemDanglingImage:
			// Removal candidates are returned below.
		}
		return item, true
	}
	return retention.Item{}, false
}

func retentionPlanContains(run *retention.Run, item retention.Item) bool {
	key := retentionItemKey(item)
	for _, candidate := range run.Planned {
		if retentionItemKey(candidate) == key {
			return true
		}
	}
	return false
}

// revalidateRetentionRun intersects the persisted remaining items with a fresh
// plan under the attempt's locks. A retry after a release became serving, or
// after a new release/container started using an artifact, must not delete the
// now-protected object just because it was eligible in the original plan.
func (s *Service) revalidateRetentionRun(ctx context.Context, nodeName string, attempt uint32, run *retention.Run, removed map[string]bool) error {
	inventory, input, err := s.loadRetentionInput(ctx, nodeName, time.Now().UTC())
	if err != nil {
		return err
	}
	retentionApplyCacheMeasurement(run, inventory)
	eligible := map[string]bool{}
	for _, item := range retention.Plan(input) {
		eligible[retentionItemKey(item)] = true
	}
	kept := make([]retention.Item, 0, len(run.Planned))
	dropped := 0
	for _, item := range run.Planned {
		if removed[retentionItemKey(item)] {
			continue
		}
		if item.Kind == retention.ItemRegistrySkipped || item.Kind == retention.ItemImageSkipped || item.Kind == retention.ItemCacheSkipped || item.Kind == retention.ItemRegistryGC {
			kept = append(kept, item)
			continue
		}
		if eligible[retentionItemKey(item)] {
			kept = append(kept, item)
			continue
		}
		dropped++
		s.recordRetentionSkip(run, "stale_plan", item.Reference,
			"item is no longer eligible under the current protection state; it was dropped before removal")
	}
	run.Planned = kept
	if err := s.updateRetentionRun(ctx, *run); err != nil {
		return err
	}
	if dropped > 0 {
		s.appendWorkerEvent(context.WithoutCancel(ctx), runOperationID(run), attempt, "retention",
			fmt.Sprintf("Retention dropped %d stale planned item(s) before removal.", dropped))
	}
	return nil
}

func runOperationID(run *retention.Run) contract.OperationID {
	return contract.OperationID(run.OperationID)
}

// retentionImageStillShared re-checks the immutable local image against the
// current release rows and the node's current container list. A retained
// release, an in-flight release, or any stopped/running container that uses the
// image blocks its removal.
func (s *Service) retentionImageStillShared(ctx context.Context, imageID, releaseID string, transport node.Transport) (bool, string, error) {
	imageID = strings.TrimSpace(imageID)
	owner, found, err := s.entities.Platform().Releases().ImageStillShared(ctx, imageID, platform.ReleaseID(releaseID))
	if err != nil {
		return false, "", unavailable(err)
	}
	if found {
		return true, "image is still used by retained release " + string(owner), nil
	}
	inventory, err := observeRetentionInventoryTransport(ctx, transport)
	if err != nil {
		return false, "", err
	}
	for _, container := range inventory.Containers {
		if strings.EqualFold(strings.TrimSpace(container.Image), imageID) {
			return true, "image is still used by container " + container.Name, nil
		}
	}
	return false, "", nil
}

// retentionGCEligible re-runs the planner immediately before garbage
// collection. If registry protection is unresolved for any retained release,
// GC is deferred rather than deleting blobs from an uncertain storage state.
func (s *Service) retentionGCEligible(ctx context.Context, nodeName string) (bool, string, error) {
	_, input, err := s.loadRetentionInput(ctx, nodeName, time.Now().UTC())
	if err != nil {
		return false, "", err
	}
	for _, item := range retention.Plan(input) {
		if item.Kind == retention.ItemRegistrySkipped {
			reason := item.Reason
			if reason == "" {
				reason = "registry protection is unresolved"
			}
			return false, reason, nil
		}
	}
	return true, "", nil
}

func (s *Service) recordRetentionRunError(run *retention.Run, stage, reference string, cause error) {
	message := s.cleanupCauseText(cause)
	if message == "" {
		message = "retention effect failed"
	}
	run.Errors = append(run.Errors, retention.RunError{Stage: stage, Reference: reference, Message: message})
}

func (s *Service) recordRetentionSkip(run *retention.Run, stage, reference, reason string) {
	run.Errors = append(run.Errors, retention.RunError{Stage: stage, Reference: reference, Message: reason})
}

// retentionGCPending reads the durable registry GC follow-up. It is node-wide,
// not owned by one operation, so a later retention run can drain a GC that an
// earlier run deferred or never reached.
func (s *Service) retentionGCPending(ctx context.Context) (bool, error) {
	pending, err := s.entities.Retention().GCPending(ctx)
	if err != nil {
		return false, unavailable(err)
	}
	return pending, nil
}

func (s *Service) setRetentionGCPending(ctx context.Context, pending bool) error {
	if err := s.retentionWrite(ctx, func(tx *entities.StoreTx) error {
		return tx.Retention().SetGCPending(ctx, pending)
	}); err != nil {
		return unavailable(err)
	}
	return nil
}

// markRetentionReleaseForAttempt applies the test-only mark seam.
func (s *Service) markRetentionReleaseForAttempt(ctx context.Context, releaseID string) (bool, error) {
	if s.retentionMark != nil {
		return s.retentionMark(ctx, releaseID)
	}
	return s.markRetentionRelease(ctx, releaseID)
}

// markRetentionRelease re-checks the release under the environment row lock
// immediately before setting pruned_at. A release that became serving, the
// current rollback target, or part of an in-flight deployment or operation is
// never marked. The returned bool is false for that protection, not an error.
func (s *Service) markRetentionRelease(ctx context.Context, releaseID string) (bool, error) {
	var safe bool
	err := s.retentionWrite(ctx, func(tx *entities.StoreTx) error {
		releases := tx.Platform().Releases()
		environment, found, err := releases.LockServingEnvironment(ctx, platform.ReleaseID(releaseID))
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return contract.NewFault(contract.NotFound, "release not found while pruning", "Inspect the release row and retry the operation.")
		}
		protected, err := releaseIsProtectedTx(ctx, tx, environment, platform.ReleaseID(releaseID))
		if err != nil {
			return err
		}
		if protected {
			return nil
		}
		if _, err := releases.MarkPruned(ctx, platform.ReleaseID(releaseID)); err != nil {
			return unavailable(err)
		}
		safe = true
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return false, unavailable(err)
		}
		return false, err
	}
	return safe, nil
}

func releaseIsProtectedTx(ctx context.Context, tx *entities.StoreTx, environment entities.ReleaseServingEnvironment, releaseID platform.ReleaseID) (bool, error) {
	if environment.Serving == releaseID ||
		environment.Desired == releaseID ||
		environment.Applied == releaseID {
		return true, nil
	}
	releases := tx.Platform().Releases()
	active, err := releases.CountActiveDeployments(ctx, releaseID)
	if err != nil {
		return false, unavailable(err)
	}
	if active > 0 {
		return true, nil
	}
	active, err = releases.CountActiveOperations(ctx, releaseID)
	if err != nil {
		return false, unavailable(err)
	}
	if active > 0 {
		return true, nil
	}
	if environment.Serving != "" {
		previous, found, err := tx.Platform().Deployments().PreviousReleaseOfServing(ctx, environment.EnvironmentID, environment.Serving)
		if err != nil {
			return false, unavailable(err)
		}
		if found && previous == releaseID {
			return true, nil
		}
	}
	previous, found, err := tx.Platform().Deployments().LatestSucceededReleaseExcluding(ctx, environment.EnvironmentID, environment.Serving)
	if err != nil {
		return false, unavailable(err)
	}
	if found && previous == releaseID {
		return true, nil
	}
	return false, nil
}
