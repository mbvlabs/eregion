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

// submitDailyRetention submits the one retention operation per UTC day. The
// idempotency key makes a worker restart or a second periodic trigger return
// the accepted operation instead of creating duplicate work.
func (s *Service) submitDailyRetention(ctx context.Context) error {
	nodeName, err := s.retentionNodeName("")
	if err != nil {
		return err
	}
	input, err := json.Marshal(platform.RetentionPruneInput{Node: nodeName})
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode the retention input", "This is an internal error; do not resubmit accepted work.")
	}
	_, err = s.Submit(ctx, operations.Submission{
		Kind: platform.KindRetentionPrune, Target: "node/" + nodeName, Actor: maintenanceActor,
		IdempotencyKey: "retention-daily:" + time.Now().UTC().Format("2006-01-02"), Input: input,
	})
	return err
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
	if environments, err = s.retentionEnvironmentRows(ctx, nodeName); err != nil {
		return err
	}

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

	server, err := s.ServerRetentionPolicy(ctx)
	if err != nil {
		return err
	}
	state, err := s.retentionPlatformState(ctx, nodeName, environments, server)
	if err != nil {
		return err
	}
	input := retention.InputFromInventory(retention.Inventory{}, state, now)
	return s.executeRetentionRun(ctx, op, attempt, &run, input, nodeName)
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
        planned_bytes,reclaimed_bytes,errors,started_at,finished_at
        FROM eregion_retention_runs WHERE operation_id=$1 ORDER BY started_at DESC,id DESC LIMIT 1`, id).
		Scan(&run.ID, &operationID, &run.Status, &planned, &removed,
			&run.PlannedBytes, &run.ReclaimedBytes, &runErrors, &run.StartedAt, &finished)
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
        (id,operation_id,status,planned_items,removed_items,planned_bytes,reclaimed_bytes,errors,started_at,finished_at)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		run.ID, nullableOperationID(contract.OperationID(run.OperationID)), run.Status, planned, removed,
		run.PlannedBytes, run.ReclaimedBytes, runErrors, run.StartedAt, run.FinishedAt)
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
        SET status=$2,planned_items=$3,removed_items=$4,planned_bytes=$5,reclaimed_bytes=$6,errors=$7,finished_at=$8
        WHERE id=$1`, run.ID, run.Status, planned, removed, run.PlannedBytes, run.ReclaimedBytes, runErrors, run.FinishedAt)
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
// than repeating completed removals.
func (s *Service) executeRetentionRun(ctx context.Context, op contract.Operation, attempt uint32, run *retention.Run, input retention.Input, nodeName string) error {
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
	releaseByID := map[string]retention.Release{}
	for _, release := range input.Releases {
		releaseByID[release.ID] = release
	}
	if s.beforeRetentionMark != nil {
		s.beforeRetentionMark(ctx, op.ID)
	}
	removed := map[string]bool{}
	for _, item := range run.Removed {
		removed[retentionItemKey(item)] = true
	}

	var failures []error
	protected := map[string]bool{}
	markFailed := map[string]bool{}
	protectedScopes := map[string]bool{}
	planned := make([]retention.Item, 0, len(run.Planned))
	for _, item := range run.Planned {
		if item.Kind != retention.ItemRelease || removed[retentionItemKey(item)] {
			planned = append(planned, item)
			continue
		}
		safe, err := s.markRetentionRelease(ctx, item.Reference)
		if err != nil {
			markFailed[item.Reference] = true
			if release, ok := releaseByID[item.Reference]; ok {
				protectedScopes[release.App+"\x00"+release.Environment] = true
			}
			s.recordRetentionRunError(run, "mark_release", item.Reference, err)
			failures = append(failures, err)
			planned = append(planned, item)
			continue
		}
		if !safe {
			protected[item.Reference] = true
			if release, ok := releaseByID[item.Reference]; ok {
				protectedScopes[release.App+"\x00"+release.Environment] = true
			}
			s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
				"Release "+item.Reference+" became serving, a rollback target or in flight after planning; it is not pruned.")
			continue
		}
		planned = append(planned, item)
	}
	if len(protected) > 0 || len(markFailed) > 0 {
		filtered := make([]retention.Item, 0, len(planned))
		for _, item := range planned {
			switch item.Kind {
			case retention.ItemRegistryArtifact, retention.ItemDanglingImage:
				// A protected or unmarked release may own an untagged
				// manifest or image; removing it could destroy a now-serving
				// artifact. The next run re-plans after the release settles.
				continue
			case retention.ItemContainer:
				if protectedScopes[item.App+"\x00"+item.Environment] {
					continue
				}
			}
			filtered = append(filtered, item)
		}
		run.Planned = filtered
		run.PlannedBytes = retention.TotalBytes(run.Planned)
		if err := s.updateRetentionRun(ctx, *run); err != nil {
			return err
		}
	}

	registryRemoved := false
	for _, item := range run.Removed {
		if item.Kind == retention.ItemRegistryArtifact {
			registryRemoved = true
		}
	}

	skipRegistryForProtected := len(protected) > 0 || len(markFailed) > 0
	if skipRegistryForProtected {
		s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
			"A release became protected or could not be marked after planning; registry and dangling-image deletions are skipped so its artifact cannot be removed. The next run re-plans them.")
	}
	for _, item := range run.Planned {
		if item.Kind == retention.ItemRegistrySkipped || item.Kind == retention.ItemRegistryGC {
			continue
		}
		if removed[retentionItemKey(item)] {
			continue
		}
		if item.Kind == retention.ItemRelease && (protected[item.Reference] || markFailed[item.Reference]) {
			continue
		}
		if skipRegistryForProtected {
			switch item.Kind {
			case retention.ItemRegistryArtifact, retention.ItemDanglingImage:
				continue
			case retention.ItemContainer:
				if protectedScopes[item.App+"\x00"+item.Environment] {
					continue
				}
			}
		}
		reference, err := retentionRemovalReference(item, releaseByID)
		if err != nil {
			s.recordRetentionRunError(run, string(item.Kind), item.Reference, err)
			failures = append(failures, err)
			continue
		}
		stepID := platform.RetentionRemoveStepID(item.Kind, reference)
		payload := platform.RetentionRemovePayload{
			Step: stepID, Kind: string(item.Kind), Reference: reference,
		}
		if item.Kind == retention.ItemContainer {
			payload.App = item.App
			payload.Environment = item.Environment
		}
		if item.Kind == retention.ItemRegistryArtifact {
			payload.Registry = platform.DefaultRegistryAddr
		}
		if _, err := (&platformWorker{service: s}).runStepOn(ctx, transport, op.ID, attempt, stepID, payload, nil); err != nil {
			s.recordRetentionRunError(run, string(item.Kind), item.Reference, err)
			failures = append(failures, err)
			continue
		}
		run.Removed = append(run.Removed, item)
		removed[retentionItemKey(item)] = true
		if item.Bytes > 0 {
			run.ReclaimedBytes += item.Bytes
		}
		if item.Kind == retention.ItemRegistryArtifact {
			registryRemoved = true
		}
		if err := s.updateRetentionRun(ctx, *run); err != nil {
			return err
		}
		s.appendWorkerEvent(ctx, op.ID, attempt, "retention",
			fmt.Sprintf("Removed %s %s (%d bytes).", item.Kind, item.Reference, item.Bytes))
	}

	if registryRemoved && !removed[retentionGCMarker] {
		stepID := "platform-retention-gc-01-v1"
		payload := platform.RetentionGCPayload{Step: stepID}
		if _, err := (&platformWorker{service: s}).runStepOn(ctx, transport, op.ID, attempt, stepID, payload, nil); err != nil {
			s.recordRetentionRunError(run, "registry_gc", "registry-gc", err)
			failures = append(failures, err)
		} else {
			marker := retention.Item{Kind: retention.ItemRegistryGC, Reference: "registry-gc"}
			run.Removed = append(run.Removed, marker)
			removed[retentionGCMarker] = true
			if err := s.updateRetentionRun(ctx, *run); err != nil {
				return err
			}
			s.appendWorkerEvent(ctx, op.ID, attempt, "retention", "Registry garbage collection completed.")
		}
	}

	finished := time.Now().UTC()
	run.FinishedAt = &finished
	if len(failures) > 0 {
		run.Status = retention.RunFailed
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

func retentionRemovalReference(item retention.Item, releases map[string]retention.Release) (string, error) {
	if item.Kind != retention.ItemRelease {
		return item.Reference, nil
	}
	release, ok := releases[item.Reference]
	if !ok {
		return "", contract.NewFault(contract.NotFound, "release not found while pruning", "Inspect the release row and retry the operation.")
	}
	if release.ImageID != "" {
		return release.ImageID, nil
	}
	if release.Image != "" {
		return release.Image, nil
	}
	return "", contract.NewFault(contract.PreconditionFailed, "release has no image identity to remove", "Inspect the release row; the next retention run retries it.")
}

func (s *Service) recordRetentionRunError(run *retention.Run, stage, reference string, cause error) {
	message := s.cleanupCauseText(cause)
	if message == "" {
		message = "retention effect failed"
	}
	run.Errors = append(run.Errors, retention.RunError{Stage: stage, Reference: reference, Message: message})
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

	var environmentID, serving string
	if err := tx.QueryRow(ctx, `SELECT e.id,coalesce(e.serving_release_id,'')
        FROM eregion_platform_releases r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE r.id=$1 FOR UPDATE OF e`, releaseID).Scan(&environmentID, &serving); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, contract.NewFault(contract.NotFound, "release not found while pruning", "Inspect the release row and retry the operation.")
		}
		return false, unavailable(err)
	}
	if serving == releaseID {
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
