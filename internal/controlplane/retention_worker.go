package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
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
	river.WorkerDefaults[retentionArgs]
	service *Service
}

func (*retentionWorker) Timeout(*river.Job[retentionArgs]) time.Duration { return 30 * time.Minute }

func (*retentionWorker) NextRetry(job *river.Job[retentionArgs]) time.Time {
	delay := time.Duration(job.Attempt) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay)
}

func (w *retentionWorker) Work(ctx context.Context, job *river.Job[retentionArgs]) error {
	if job.Args.OperationID == "" {
		return w.service.submitDailyRetention(ctx)
	}
	attempt := uint32(job.Attempt)
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
	runErr := w.service.runRetentionAttempt(ctx, op, attempt)
	if runErr == nil {
		return w.service.finishRetentionOperation(ctx, op.ID, attempt)
	}
	return w.service.settleRetentionFailure(ctx, op, attempt, job, runErr)
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
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM eregion_operations
        WHERE kind=$1 AND actor_kind=$2 AND actor_subject=$3 AND target=$4 AND idempotency_key LIKE $5
        ORDER BY created_at,id`,
		platform.KindRetentionPrune, maintenanceActor.Kind, maintenanceActor.Subject,
		"node/"+nodeName, retentionDailyKeyPrefix+date+"%")
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var operations []contract.Operation
	for rows.Next() {
		operation, err := scanOperation(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

func (s *Service) claimRetention(ctx context.Context, id contract.OperationID, attempt uint32) (contract.Operation, bool, error) {
	op, claimed, err := s.mutateOwnedOrClaim(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "", "Retention worker started durable execution."); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_operations SET attempt=$2 WHERE id=$1`, id, op.Attempt); err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: "retention", Message: "Retention execution attempt started under the environment locks."})
	})
	return op, claimed, err
}

func (s *Service) finishRetentionOperation(ctx context.Context, id contract.OperationID, attempt uint32) error {
	return s.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		return appendEvent(ctx, tx, op, contract.Event{Kind: "log", Stage: "retention", Message: "Retention completed; removed artifacts and reclaimed bytes are recorded in the retention run."})
	})
}

// settleRetentionFailure records a retryable failure for River or fails the
// operation when no attempt remains or the failure is permanent. A permanent
// registry refusal (for example deletes still disabled) is not retried eight
// times: the next periodic run retries it after the host is fixed.
func (s *Service) settleRetentionFailure(ctx context.Context, op contract.Operation, attempt uint32, job *river.Job[retentionArgs], cause error) error {
	code := contract.Classify(cause).Code
	permanent := errors.Is(cause, engine.ErrPermanent) || errors.Is(cause, errNodeStepPermanentFailure) ||
		code == contract.ValidationFailed || code == contract.PreconditionFailed ||
		code == contract.NotFound || code == contract.Forbidden || code == contract.Conflict
	if !permanent && job.Attempt < job.MaxAttempts {
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
	// Platform and node locks come first: an application or environment cannot
	// be created while they are held, so the environment lock set cannot grow
	// after it is read. Environment locks then serialize the planner with a
	// deploy, rollback, restart or delete for each served environment.
	baseLocks, err := s.lockTargets(ctx, []string{"platform", "node/" + nodeName})
	if err != nil {
		return err
	}
	defer baseLocks()
	environments, err := s.retentionEnvironmentRows(ctx, nodeName)
	if err != nil {
		return err
	}
	targets := make([]string, 0, len(environments))
	for _, environment := range environments {
		targets = append(targets, "env/"+environment.App+"/"+environment.Name)
	}
	environmentLocks, err := s.lockTargets(ctx, targets)
	if err != nil {
		return err
	}
	defer environmentLocks()

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
	if !exists {
		inventory, input, loadErr := s.loadRetentionInput(ctx, nodeName, now)
		if loadErr != nil {
			s.recordRetentionPlanFailure(ctx, op, attempt, loadErr)
			return loadErr
		}
		result := retention.PlanInventory(inventory, retention.PlatformState{
			Environments: input.Environments,
			Releases:     input.Releases,
			Deployments:  input.Deployments,
			InFlight:     input.InFlight,
			ServerPolicy: input.ServerPolicy,
		}, now)
		run = retention.Run{
			OperationID: string(op.ID), Status: retention.RunRunning,
			Planned: result.Items, PlannedBytes: result.PlannedBytes,
			StartedAt: now,
		}
		retentionApplyCacheMeasurement(&run, inventory)
		run.ID, err = retention.NewRunID()
		if err != nil {
			return contract.NewFault(contract.Internal, "could not allocate a retention run", "Report the binary version.")
		}
		if err := s.insertRetentionRun(ctx, run); err != nil {
			return err
		}
		s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
			fmt.Sprintf("Retention planned %d item(s), %d byte(s) on node %s.", len(retentionRemovalItems(run.Planned)), run.PlannedBytes, nodeName))
	} else {
		run.Status = retention.RunRunning
		run.FinishedAt = nil
		if err := s.updateRetentionRun(ctx, run); err != nil {
			return err
		}
	}

	return s.executeRetentionRun(ctx, op, attempt, &run, nodeName)
}

// retentionRunForOperation returns the newest run for one operation. A retry
// reuses its planned items, so an effect that failed after a release row was
// already marked pruned is still retried instead of disappearing from a fresh
// plan.
func (s *Service) retentionRunForOperation(ctx context.Context, id contract.OperationID) (retention.Run, bool, error) {
	var run retention.Run
	var operationID string
	var planned, removed, runErrors []byte
	var finished *time.Time
	err := s.pool.QueryRow(ctx, `SELECT id,coalesce(operation_id,''),status,planned_items,removed_items,
        planned_bytes,reclaimed_bytes,errors,started_at,finished_at,
        cache_bytes,cache_measured_at,cache_measurement_error,gc_generation
        FROM eregion_retention_runs WHERE operation_id=$1 ORDER BY started_at DESC,id DESC LIMIT 1`, id).
		Scan(&run.ID, &operationID, &run.Status, &planned, &removed,
			&run.PlannedBytes, &run.ReclaimedBytes, &runErrors, &run.StartedAt, &finished,
			&run.CacheBytes, &run.CacheMeasuredAt, &run.CacheError, &run.GCGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return retention.Run{}, false, nil
	}
	if err != nil {
		return retention.Run{}, false, unavailable(err)
	}
	run.OperationID = operationID
	run.FinishedAt = finished
	run.Planned = decodeRetentionItems(planned)
	run.Removed = decodeRetentionItems(removed)
	run.Errors = decodeRetentionErrors(runErrors)
	return run, true, nil
}

func (s *Service) insertRetentionRun(ctx context.Context, run retention.Run) error {
	planned, removed, runErrors, err := retentionRunJSON(run)
	if err != nil {
		return unavailable(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO eregion_retention_runs
        (id,operation_id,status,planned_items,removed_items,planned_bytes,reclaimed_bytes,errors,started_at,finished_at,
         cache_bytes,cache_measured_at,cache_measurement_error,gc_generation)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		run.ID, nullableOperationID(contract.OperationID(run.OperationID)), run.Status, planned, removed,
		run.PlannedBytes, run.ReclaimedBytes, runErrors, run.StartedAt, run.FinishedAt,
		run.CacheBytes, run.CacheMeasuredAt, run.CacheError, run.GCGeneration)
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) updateRetentionRun(ctx context.Context, run retention.Run) error {
	planned, removed, runErrors, err := retentionRunJSON(run)
	if err != nil {
		return unavailable(err)
	}
	_, err = s.pool.Exec(ctx, `UPDATE eregion_retention_runs
        SET status=$2,planned_items=$3,removed_items=$4,planned_bytes=$5,reclaimed_bytes=$6,errors=$7,finished_at=$8,
            cache_bytes=$9,cache_measured_at=$10,cache_measurement_error=$11,gc_generation=$12
        WHERE id=$1`, run.ID, run.Status, planned, removed, run.PlannedBytes, run.ReclaimedBytes, runErrors, run.FinishedAt,
		run.CacheBytes, run.CacheMeasuredAt, run.CacheError, run.GCGeneration)
	if err != nil {
		return unavailable(err)
	}
	return nil
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
	if s.beforeRetentionMark != nil {
		s.beforeRetentionMark(ctx, op.ID)
	}
	removed := map[string]bool{}
	for _, item := range run.Removed {
		removed[retentionItemKey(item)] = true
	}
	// A persisted plan can be older than the policy or the serving state. Drop
	// what is no longer eligible before any release row is marked.
	if _, err := s.revalidateRetentionRun(ctx, nodeName, attempt, run, removed); err != nil {
		return err
	}

	var failures []error
	protected := map[string]bool{}
	markFailed := map[string]bool{}
	protectedScopes := map[string]bool{}
	changed := false
	for _, item := range run.Planned {
		if item.Kind != retention.ItemRelease || removed[retentionItemKey(item)] {
			continue
		}
		safe, err := s.markRetentionReleaseForAttempt(ctx, item.Reference)
		if err != nil {
			markFailed[item.Reference] = true
			protectedScopes[item.App+"\x00"+item.Environment] = true
			s.recordRetentionRunError(run, "mark_release", item.Reference, err)
			failures = append(failures, err)
			continue
		}
		if !safe {
			protected[item.Reference] = true
			protectedScopes[item.App+"\x00"+item.Environment] = true
			s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
				"Release "+item.Reference+" became serving, a rollback target or in flight after planning; it is not pruned.")
			continue
		}
		run.Removed = append(run.Removed, item)
		removed[retentionItemKey(item)] = true
		changed = true
		s.appendWorkerEvent(ctx, op.ID, attempt, "retention", "Marked release "+item.Reference+" pruned; its row remains for history.")
	}

	filtered := make([]retention.Item, 0, len(run.Planned))
	for _, item := range run.Planned {
		if removed[retentionItemKey(item)] {
			changed = true
			continue
		}
		switch item.Kind {
		case retention.ItemRelease:
			// A failed mark stays in the plan so the next River attempt
			// re-attempts it. Only a release that became protected/stale is
			// dropped.
			if protected[item.Reference] {
				changed = true
				continue
			}
		case retention.ItemImage:
			if protected[item.Release] {
				changed = true
				continue
			}
		case retention.ItemImageSkipped:
			if protected[item.Release] {
				changed = true
				continue
			}
		case retention.ItemRegistryArtifact, retention.ItemDanglingImage:
			if len(protected) > 0 || len(markFailed) > 0 {
				changed = true
				continue
			}
		case retention.ItemContainer:
			if protectedScopes[item.App+"\x00"+item.Environment] {
				changed = true
				continue
			}
		}
		filtered = append(filtered, item)
	}
	run.Planned = filtered
	if changed {
		if err := s.updateRetentionRun(ctx, *run); err != nil {
			return err
		}
	}

	registryRemovedThisAttempt := false
	registryFailedThisAttempt := false
	gcPending, err := s.retentionGCPending(ctx)
	if err != nil {
		s.recordRetentionRunError(run, "registry_gc_state", "registry-gc", err)
		failures = append(failures, err)
	}
	registryGCStateBlocked := false
	for _, item := range run.Planned {
		if item.Kind == retention.ItemRegistryArtifact && !removed[retentionItemKey(item)] {
			// Make the follow-up durable before any manifest delete. A crash
			// after the delete can then never leave an un-GCed registry.
			if pendingErr := s.setRetentionGCPending(ctx, true); pendingErr != nil {
				s.recordRetentionRunError(run, "registry_gc_state", "registry-gc", pendingErr)
				failures = append(failures, pendingErr)
				registryGCStateBlocked = true
			} else {
				gcPending = true
			}
			break
		}
	}

	attempted := map[string]bool{}
	skippedItems := map[string]bool{}
	for {
		item, ok := nextRetentionRemoval(run, removed, attempted)
		if !ok {
			break
		}
		// Re-run protection and resolution before every effect. A retry or a
		// manually changed release/container must not delete something that
		// became protected after the original plan.
		if _, err := s.revalidateRetentionRun(ctx, nodeName, attempt, run, removed); err != nil {
			return err
		}
		if !retentionPlanContains(run, item) {
			continue
		}
		attempted[retentionItemKey(item)] = true
		if item.Kind == retention.ItemRegistryArtifact && registryGCStateBlocked {
			// The durable GC follow-up could not be recorded; never delete a
			// manifest without it. The item stays planned for the retry.
			continue
		}
		if item.Kind == retention.ItemImage && markFailed[item.Release] {
			// The row could not be marked this attempt, so its image must not
			// be removed yet. Keep the item for the retry.
			continue
		}
		if item.Kind == retention.ItemImage {
			shared, reason, err := s.retentionImageStillShared(ctx, item.Reference, item.Release, transport)
			if err != nil {
				s.recordRetentionRunError(run, "image_shared_check", item.Reference, err)
				failures = append(failures, err)
				continue
			}
			if shared {
				s.recordRetentionSkip(run, "image_skipped", item.Reference, reason)
				s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
					"Skipped local image "+item.Reference+" for release "+item.Release+": "+reason)
				skippedItems[retentionItemKey(item)] = true
				continue
			}
		}
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
		result, err := (&platformWorker{service: s}).runStepOn(ctx, transport, op.ID, attempt, stepID, payload, nil)
		if err != nil {
			// A deterministic conflict is a per-item skip, not a failed run:
			// retrying cannot make the node accept the same removal.
			if contract.Classify(err).Code == contract.Conflict {
				reason := s.cleanupCauseText(err)
				if reason == "" {
					reason = "the node refused the removal with a deterministic conflict"
				}
				s.recordRetentionSkip(run, "skipped", item.Reference, reason)
				s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
					"Skipped "+string(item.Kind)+" "+item.Reference+": "+reason)
				skippedItems[retentionItemKey(item)] = true
				continue
			}
			if item.Kind == retention.ItemRegistryArtifact {
				registryFailedThisAttempt = true
			}
			s.recordRetentionRunError(run, string(item.Kind), item.Reference, err)
			failures = append(failures, err)
			continue
		}
		if skipped, reason := retentionStepSkipped(result); skipped {
			if reason == "" {
				reason = "the node reported a deterministic skip"
			}
			s.recordRetentionSkip(run, "skipped", item.Reference, reason)
			s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
				"Skipped "+string(item.Kind)+" "+item.Reference+": "+reason)
			skippedItems[retentionItemKey(item)] = true
			continue
		}
		run.Removed = append(run.Removed, item)
		removed[retentionItemKey(item)] = true
		if item.Bytes > 0 {
			run.ReclaimedBytes += item.Bytes
		}
		if item.Kind == retention.ItemRegistryArtifact {
			registryRemovedThisAttempt = true
		}
		if err := s.updateRetentionRun(ctx, *run); err != nil {
			return err
		}
		s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
			fmt.Sprintf("Removed %s %s (%d bytes).", item.Kind, item.Reference, item.Bytes))
	}

	if len(skippedItems) > 0 {
		kept := make([]retention.Item, 0, len(run.Planned))
		for _, item := range run.Planned {
			if skippedItems[retentionItemKey(item)] {
				continue
			}
			kept = append(kept, item)
		}
		run.Planned = kept
	}

	registryUnsettled := false
	for _, item := range run.Planned {
		if item.Kind == retention.ItemRegistryArtifact && !removed[retentionItemKey(item)] {
			registryUnsettled = true
			break
		}
	}
	if !registryUnsettled && !registryFailedThisAttempt && (registryRemovedThisAttempt || gcPending) {
		if err := s.setRetentionGCPending(ctx, true); err != nil {
			s.recordRetentionRunError(run, "registry_gc_state", "registry-gc", err)
			failures = append(failures, err)
		} else if allowed, reason, err := s.retentionGCEligible(ctx, nodeName); err != nil {
			s.recordRetentionRunError(run, "registry_gc_check", "registry-gc", err)
			failures = append(failures, err)
		} else if !allowed {
			s.recordRetentionSkip(run, "registry_gc_skipped", "registry-gc", reason)
			s.appendWorkerEvent(ctx, op.ID, attempt, "retention", "Skipped registry garbage collection: "+reason)
		} else {
			generation := run.GCGeneration
			switch {
			case generation == 0:
				generation = 1
			case registryRemovedThisAttempt:
				// A completed earlier GC pass must not satisfy this one.
				generation++
			}
			if generation != run.GCGeneration {
				run.GCGeneration = generation
				if err := s.updateRetentionRun(ctx, *run); err != nil {
					return err
				}
			}
			stepID := platform.RetentionGCStepID(generation)
			payload := platform.RetentionGCPayload{Step: stepID, Generation: generation}
			if _, err := (&platformWorker{service: s}).runStepOn(ctx, transport, op.ID, attempt, stepID, payload, nil); err != nil {
				s.recordRetentionRunError(run, "registry_gc", "registry-gc", err)
				failures = append(failures, err)
			} else if err := s.setRetentionGCPending(ctx, false); err != nil {
				s.recordRetentionRunError(run, "registry_gc_state", "registry-gc", err)
				failures = append(failures, err)
			} else {
				if !removed[retentionGCMarker] {
					marker := retention.Item{Kind: retention.ItemRegistryGC, Reference: "registry-gc"}
					run.Removed = append(run.Removed, marker)
					removed[retentionGCMarker] = true
				}
				if err := s.updateRetentionRun(ctx, *run); err != nil {
					return err
				}
				s.appendWorkerEvent(ctx, op.ID, attempt, "retention", "Registry garbage collection completed.")
			}
		}
	}

	finished := time.Now().UTC()
	run.FinishedAt = &finished
	if len(failures) > 0 {
		run.Status = retention.RunFailed
	} else if retentionRunHasSkips(run.Errors) {
		run.Status = retention.RunSucceededWithSkips
	} else {
		run.Status = retention.RunSucceeded
	}
	if err := s.updateRetentionRun(ctx, *run); err != nil {
		return err
	}
	if len(failures) > 0 {
		s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
			fmt.Sprintf("Retention run %s recorded %d failure(s); unfinished effects remain for retry.", run.ID, len(run.Errors)))
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
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
func (s *Service) revalidateRetentionRun(ctx context.Context, nodeName string, attempt uint32, run *retention.Run, removed map[string]bool) (retention.Input, error) {
	inventory, input, err := s.loadRetentionInput(ctx, nodeName, time.Now().UTC())
	if err != nil {
		return retention.Input{}, err
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
		return retention.Input{}, err
	}
	if dropped > 0 {
		s.appendWorkerEvent(context.WithoutCancel(ctx), runOperationID(run), attempt, "retention",
			fmt.Sprintf("Retention dropped %d stale planned item(s) before removal.", dropped))
	}
	return input, nil
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
	var owner string
	err := s.pool.QueryRow(ctx, `SELECT r.id FROM eregion_platform_releases r
        WHERE r.image_id=$1 AND r.id<>$2 AND (
            r.pruned_at IS NULL
            OR EXISTS (SELECT 1 FROM eregion_platform_deployments d
                       WHERE d.state IN ('pending','running') AND (d.release_id=r.id OR d.previous_release_id=r.id))
            OR EXISTS (SELECT 1 FROM eregion_operations o
                       WHERE o.id=r.operation_id AND o.state IN ('queued','running','cancelling'))
        )
        ORDER BY r.created_at DESC,r.id LIMIT 1`, imageID, releaseID).Scan(&owner)
	switch {
	case err == nil:
		return true, "image is still used by retained release " + owner, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, "", unavailable(err)
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
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT coalesce(
        (SELECT retention_gc_pending FROM eregion_server_settings WHERE singleton),false)`).Scan(&pending)
	if err != nil {
		return false, unavailable(err)
	}
	return pending, nil
}

func (s *Service) setRetentionGCPending(ctx context.Context, pending bool) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO eregion_server_settings (singleton,retention_gc_pending,retention,updated_at)
        VALUES (true,$1,'{}'::jsonb,clock_timestamp())
        ON CONFLICT (singleton) DO UPDATE SET retention_gc_pending=EXCLUDED.retention_gc_pending, updated_at=clock_timestamp()`, pending); err != nil {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, unavailable(err)
	}
	defer rollback(tx)

	var environmentID, serving, desired, applied string
	if err := tx.QueryRow(ctx, `SELECT e.id,coalesce(e.serving_release_id,''),coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,'')
        FROM eregion_platform_releases r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE r.id=$1 FOR UPDATE OF e`, releaseID).Scan(&environmentID, &serving, &desired, &applied); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, contract.NewFault(contract.NotFound, "release not found while pruning", "Inspect the release row and retry the operation.")
		}
		return false, unavailable(err)
	}
	if serving == releaseID || desired == releaseID || applied == releaseID {
		return false, nil
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_deployments
        WHERE state IN ('pending','running') AND (release_id=$1 OR previous_release_id=$1)`, releaseID).Scan(&active); err != nil {
		return false, unavailable(err)
	}
	if active > 0 {
		return false, nil
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_operations o
        JOIN eregion_platform_releases r ON r.operation_id=o.id
        WHERE r.id=$1 AND o.state IN ('queued','running','cancelling')`, releaseID).Scan(&active); err != nil {
		return false, unavailable(err)
	}
	if active > 0 {
		return false, nil
	}

	var previous string
	if serving != "" {
		err := tx.QueryRow(ctx, `SELECT coalesce(previous_release_id,'') FROM eregion_platform_deployments
            WHERE environment_id=$1 AND state='succeeded' AND release_id=$2 AND previous_release_id IS NOT NULL
            ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, environmentID, serving).Scan(&previous)
		if err == nil && previous == releaseID {
			return false, nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, unavailable(err)
		}
	}
	err = tx.QueryRow(ctx, `SELECT release_id FROM eregion_platform_deployments
        WHERE environment_id=$1 AND state='succeeded' AND release_id IS DISTINCT FROM $2
        ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, environmentID, nullableString(serving)).Scan(&previous)
	if err == nil && previous == releaseID {
		return false, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, unavailable(err)
	}

	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET pruned_at=clock_timestamp() WHERE id=$1 AND pruned_at IS NULL`, releaseID); err != nil {
		return false, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, unavailable(err)
	}
	return true, nil
}
