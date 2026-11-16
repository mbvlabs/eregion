package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

const definitionPoll = 2 * time.Second

var definitionProviderFailure = regexp.MustCompile(`^Cloudflare provider: (authorization|client-rejection|rate-limit|provider-unavailable|network/timeout|invalid-response)( \(HTTP [1-5][0-9][0-9]\))?$`)
var definitionDefinitiveProviderFailure = regexp.MustCompile(`^Cloudflare provider: (authorization \(HTTP (401|403)\)|client-rejection \(HTTP 4[0-9][0-9]\))$`)
var definitionCleanupRejectionPrefix = regexp.MustCompile(`^definitive cleanup rejection: [a-f0-9]{64} `)
var definitionProviderDetail = regexp.MustCompile(`^(.*) (\(code ([1-9][0-9]{0,8}): [^()]*\))$`)

func definitionDefinitiveRejection(message string) bool {
	if parts := definitionProviderDetail.FindStringSubmatch(message); parts != nil {
		code, _ := strconv.Atoi(parts[3])
		if integrations.CloudflareErrorDetail(code) != parts[2] {
			return false
		}
		message = parts[1]
	}
	return definitionDefinitiveProviderFailure.MatchString(message) &&
		!strings.HasSuffix(message, "(HTTP 408)") && !strings.HasSuffix(message, "(HTTP 429)") &&
		!strings.Contains(message, "client-rejection (HTTP 401)") && !strings.Contains(message, "client-rejection (HTTP 403)")
}

func definitionSafeProviderDiagnostic(message string) string {
	message = strings.TrimPrefix(message, "definitive write rejection: ")
	message = definitionCleanupRejectionPrefix.ReplaceAllString(message, "")
	base := message
	if parts := definitionProviderDetail.FindStringSubmatch(message); parts != nil {
		code, _ := strconv.Atoi(parts[3])
		if integrations.CloudflareErrorDetail(code) != parts[2] {
			return ""
		}
		base = parts[1]
	}
	if definitionProviderFailure.MatchString(base) {
		return message
	}
	return ""
}

// definitionFinalState never permits a parent to finish while a linked child
// remains active. Failure intent is private journal state, not operation.error_code.
func definitionFinalState(cancelling, active bool, failure contract.Code) contract.State {
	if active {
		return ""
	}
	if failure != "" {
		return contract.Failed
	}
	if cancelling {
		return contract.Canceled
	}
	return contract.Succeeded
}

func definitionFailureTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, code contract.Code, message string) error {
	if !contract.KnownCode(code) || code == "" {
		code = contract.Internal
	}
	if len(message) > 1024 {
		message = message[:1024]
	}
	failed, err := tx.DefinitionRuns().SetFailure(ctx, op.ID, code, message)
	if err != nil {
		return err
	}
	if failed {
		if err := appendEvent(ctx, tx, op, contract.Event{Kind: "warning", Stage: "definition", Message: message}); err != nil {
			return err
		}
	}
	if op.State != contract.Cancelling {
		return transition(ctx, tx, op, contract.Cancelling, "", "Definition execution stopped; waiting for linked work to settle.")
	}
	return nil
}

// definitionDrainTx requests cancellation of every linked active child. It is
// also used for malformed journals, where the cursor cannot be trusted.
func definitionDrainTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (bool, error) {
	ids, err := tx.DefinitionSteps().ListActiveChildren(ctx, op.ID)
	if err != nil {
		return false, err
	}
	active := false
	for _, id := range ids {
		child, found, err := tx.Operations().Lock(ctx, id)
		if err != nil {
			return false, err
		}
		if !found {
			return false, definitionDNSConflict()
		}
		if child.State.Terminal() {
			continue
		}
		active = true
		if child.State != contract.Cancelling {
			if err := transition(ctx, tx, &child, contract.Cancelling, "", "Parent definition cancellation requested; waiting for a safe boundary."); err != nil {
				return false, err
			}
		}
	}
	return active, nil
}

func definitionFinishTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, state contract.State, code contract.Code, message string) error {
	activeChildren, err := tx.DefinitionSteps().ListActiveChildren(ctx, op.ID)
	if err != nil {
		return err
	}
	if len(activeChildren) > 0 {
		return errors.New("definition parent cannot finish while a child is active")
	}
	if err := guardDefinitionChildrenTerminalTx(ctx, tx, op.ID); err != nil {
		return err
	}
	if _, err := tx.DefinitionRuns().Finish(ctx, op.ID); err != nil {
		return err
	}
	return transition(ctx, tx, op, state, code, message)
}

func guardDefinitionChildrenTerminalTx(ctx context.Context, tx *entities.StoreTx, parent contract.OperationID) error {
	broken, err := tx.Platform().Outcomes().DomainDNSDefinitionCleanupPending(ctx, parent)
	if err != nil {
		return unavailable(err)
	}
	if broken {
		return definitionDNSConflict()
	}
	rows, err := tx.DefinitionSteps().ListChildJournal(ctx, parent)
	if err != nil {
		return unavailable(err)
	}
	var children []contract.Operation
	for _, row := range rows {
		if row.ChildID == "" {
			continue
		}
		step, stepErr := platform.Default().Normalize(row.Kind, row.Target, row.Input)
		linked, linkedErr := platform.Default().Normalize(row.ChildKind, row.ChildTarget, row.ChildInput)
		if row.Kind != row.ChildKind || row.Target != row.ChildTarget || row.ParentActor != row.ChildActor ||
			row.IdempotencyKey != fmt.Sprintf("definition:%s:%d", parent, row.Ordinal) || stepErr != nil || linkedErr != nil ||
			!bytes.Equal(step.Canonical, linked.Canonical) {
			return definitionDNSConflict()
		}
		children = append(children, contract.Operation{
			ID: row.ChildID, Kind: row.ChildKind, Target: row.ChildTarget,
			Actor: row.ChildActor, State: row.ChildState,
		})
	}
	for _, child := range children {
		if err := guardDefinitionDNSTerminalTx(ctx, tx, &child); err != nil {
			return err
		}
	}
	return nil
}

func (w *platformWorker) runDefinitionApply(ctx context.Context, claimed contract.Operation, attempt uint32) error {
	return w.runDefinitionApplyTx(ctx, claimed, attempt)
}

type definitionApplyTxOutcome struct {
	parent   contract.Operation
	approved definitionApproved
	envID    string
	ordinal  int
	kind     string
	target   string
	input    json.RawMessage
	create   bool
	snooze   bool
}

func (w *platformWorker) runDefinitionApplyTx(ctx context.Context, claimed contract.Operation, attempt uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	outcome, err := w.definitionApplyTx(ctx, claimed, attempt)
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	w.service.observeCommittedActionResult(outcome.parent)
	if outcome.snooze {
		return river.JobSnooze(definitionPoll)
	}
	if !outcome.create {
		return nil
	}
	// Release operation/journal locks before acquiring the app then environment
	// locks in the creation transaction. No session lock spans child execution.
	return w.createDefinitionChild(ctx, outcome.parent, attempt, outcome.approved, outcome.envID, outcome.ordinal, outcome.kind, outcome.target, outcome.input)
}

func (w *platformWorker) definitionApplyTx(ctx context.Context, claimed contract.Operation, attempt uint32) (definitionApplyTxOutcome, error) {
	var outcome definitionApplyTxOutcome
	err := w.service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		return w.definitionApplyLockedTx(ctx, tx, claimed, attempt, &outcome)
	})
	return outcome, err
}

func (w *platformWorker) definitionApplyLockedTx(ctx context.Context, tx *entities.StoreTx, claimed contract.Operation, attempt uint32, outcome *definitionApplyTxOutcome) error {
	op, found, err := tx.Operations().Lock(ctx, claimed.ID)
	if err != nil || !found {
		return unavailable(err)
	}
	if op.State.Terminal() || op.Attempt != attempt {
		return nil
	}
	journal, found, err := tx.DefinitionRuns().LockJournal(ctx, op.ID)
	if err != nil || !found {
		return unavailable(err)
	}
	failure, failureMessage := journal.FailureCode, journal.FailureMessage
	approvedValue, valid := definitionJournalApproved(journal, op)
	if !valid && op.State != contract.Cancelling {
		failure, failureMessage = contract.Internal, "Definition journal validation failed; linked work is being drained without rollback."
		if err := definitionFailureTx(ctx, tx, &op, failure, failureMessage); err != nil {
			return err
		}
	}
	if op.State == contract.Cancelling {
		return definitionApplyCancellingTx(ctx, tx, op, failure, failureMessage, outcome)
	}
	return definitionApplyNextStepTx(ctx, tx, op, journal, approvedValue, outcome)
}

func definitionJournalApproved(journal entities.DefinitionRunJournal, op contract.Operation) (definitionApproved, bool) {
	var approved definitionApproved
	valid := json.Unmarshal(journal.Approved, &approved) == nil && journal.SameInput && !journal.Finished &&
		!approved.Input.Plan.Deploy && op.Kind == platform.KindDefinitionApply &&
		op.Actor == approved.Actor && journal.EnvironmentID == string(approved.Snapshot.EnvironmentID) &&
		op.Target == "env/"+approved.Plan.App+"/"+approved.Plan.Environment
	return approved, valid
}

func definitionApplyCancellingTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, failure contract.Code, failureMessage string, outcome *definitionApplyTxOutcome) error {
	active, err := definitionDrainTx(ctx, tx, &op)
	if err != nil {
		return err
	}
	state := definitionFinalState(true, active, failure)
	if state != "" {
		message := "Definition cancelled after linked work settled; completed changes were not rolled back."
		if state == contract.Failed {
			message = failureMessage
		}
		if err := definitionFinishTx(ctx, tx, &op, state, failure, message); err != nil {
			return err
		}
	}
	outcome.snooze = active
	outcome.parent = op
	return nil
}

func definitionApplyNextStepTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, journal entities.DefinitionRunJournal, approvedValue definitionApproved, outcome *definitionApplyTxOutcome) error {
	step, found, err := tx.DefinitionSteps().Lock(ctx, op.ID, journal.NextStep)
	if err != nil {
		return err
	}
	if !found {
		return definitionApplyCursorGapTx(ctx, tx, op, journal, outcome)
	}
	otherActive, err := tx.DefinitionSteps().ActiveOutsideOrdinal(ctx, op.ID, journal.NextStep)
	if err != nil {
		return err
	}
	if otherActive {
		return definitionApplyStepFailureTx(ctx, tx, op, "Definition journal has an active child outside its cursor; linked work is being drained.", outcome)
	}
	normalized, normErr := platform.Default().Normalize(step.Kind, step.Target, step.Input)
	allowed := step.Kind == platform.KindResourceApply || step.Kind == platform.KindEnvironmentUpdate ||
		step.Kind == platform.KindDomainAdd || step.Kind == platform.KindDomainRemove
	if normErr != nil || !allowed || normalized.Target.App != approvedValue.Plan.App || normalized.Target.Environment != approvedValue.Plan.Environment {
		return definitionApplyStepFailureTx(ctx, tx, op, "Definition step does not match the approved environment or action.", outcome)
	}
	if step.ChildOperationID != "" {
		return definitionApplyLinkedChildTx(ctx, tx, op, step, journal, normalized, outcome)
	}
	outcome.parent, outcome.approved = op, approvedValue
	outcome.envID, outcome.ordinal = journal.EnvironmentID, journal.NextStep
	outcome.kind, outcome.target, outcome.input, outcome.create = step.Kind, step.Target, step.Input, true
	return nil
}

func definitionApplyCursorGapTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, journal entities.DefinitionRunJournal, outcome *definitionApplyTxOutcome) error {
	// A gap cannot be mistaken for completion.
	bounds, err := tx.DefinitionSteps().CursorBounds(ctx, op.ID, journal.NextStep)
	if err != nil {
		return err
	}
	if bounds.Later || bounds.Active {
		if err := definitionFailureTx(ctx, tx, &op, contract.Internal, "Definition journal cursor is inconsistent; linked work is being drained."); err != nil {
			return err
		}
	} else if err := definitionFinishTx(ctx, tx, &op, contract.Succeeded, "", "Definition changes completed; no deployment was queued."); err != nil {
		return err
	}
	outcome.snooze = bounds.Later || bounds.Active
	outcome.parent = op
	return nil
}

func definitionApplyStepFailureTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, message string, outcome *definitionApplyTxOutcome) error {
	if err := definitionFailureTx(ctx, tx, &op, contract.Internal, message); err != nil {
		return err
	}
	outcome.snooze = true
	outcome.parent = op
	return nil
}

func definitionApplyLinkedChildTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, step entities.DefinitionStep, journal entities.DefinitionRunJournal, normalized platform.Normalized, outcome *definitionApplyTxOutcome) error {
	linked, found, err := tx.DefinitionSteps().LinkedChild(ctx, step.ChildOperationID, step.Kind, step.Target, normalized.Canonical, op.Actor)
	if err != nil {
		return err
	}
	if !found || !linked.Matches {
		return definitionApplyStepFailureTx(ctx, tx, op, "Definition linked child does not match its frozen journal; linked work is being drained.", outcome)
	}
	if !linked.State.Terminal() {
		outcome.snooze = true
		outcome.parent = op
		return nil
	}
	switch linked.State {
	case contract.Succeeded:
		if _, err := tx.DefinitionRuns().Advance(ctx, op.ID, journal.NextStep); err != nil {
			return err
		}
	case contract.Canceled:
		if err := transition(ctx, tx, &op, contract.Cancelling, "", "Definition child "+string(step.ChildOperationID)+" was cancelled; completed changes were not rolled back."); err != nil {
			return err
		}
	case contract.Queued, contract.Running, contract.Cancelling, contract.Failed:
		fallthrough
	default:
		message, err := definitionLinkedChildFailureMessage(ctx, tx, op, step, journal.NextStep)
		if err != nil {
			return err
		}
		if err := definitionFailureTx(ctx, tx, &op, linked.ErrorCode, message); err != nil {
			return err
		}
	}
	outcome.snooze = true
	outcome.parent = op
	return nil
}

func definitionLinkedChildFailureMessage(ctx context.Context, tx *entities.StoreTx, op contract.Operation, step entities.DefinitionStep, nextStep int) (string, error) {
	message := "Definition child " + string(step.ChildOperationID) + " failed; completed changes were not rolled back."
	if step.Kind != platform.KindDomainAdd && step.Kind != platform.KindDomainRemove {
		return message, nil
	}
	counts, err := tx.Platform().Outcomes().DefinitionCleanupCounts(ctx, op.ID, nextStep,
		platform.KindResourceApply, platform.KindEnvironmentUpdate, platform.KindDomainRemove, step.ChildOperationID)
	if err != nil {
		return "", err
	}
	provider := counts.Error
	prior := false
	if provider == "" && step.Kind == platform.KindDomainRemove {
		provider, err = tx.Operations().LatestEventMessageForStage(ctx, step.ChildOperationID, "dns_prior_provider_failure")
		if err != nil {
			return "", err
		}
		prior = provider != ""
	}
	message = fmt.Sprintf("Definition DNS child %s failed; completed resource steps=%d, revision steps=%d were not rolled back. Inspect serving, route, TLS and public status separately.", step.ChildOperationID, counts.Successes, counts.Authorized)
	if safeProvider := definitionSafeProviderDiagnostic(provider); safeProvider != "" {
		if prior {
			message += " Prior provider diagnostic: " + safeProvider + "; cleanup was subsequently verified."
		} else {
			message += " " + safeProvider + "; inspect the child and provider configuration before replanning."
		}
	}
	return message, nil
}

type definitionChildResult struct {
	failureCode    contract.Code
	failureMessage string
	snooze         bool
}

type definitionChildPreflight struct {
	op             contract.Operation
	ready          bool
	failureCode    contract.Code
	failureMessage string
}

func (w *platformWorker) createDefinitionChild(ctx context.Context, parent contract.Operation, attempt uint32, approved definitionApproved, envID string, ordinal int, kind, target string, input []byte) error {
	switch kind {
	case platform.KindResourceApply, platform.KindEnvironmentUpdate, platform.KindDomainAdd, platform.KindDomainRemove:
	default:
		return w.failDefinition(ctx, parent.ID, attempt, contract.Internal, "Definition contains an unsupported child action.")
	}
	n, err := platform.Default().Normalize(kind, target, input)
	if err != nil {
		return w.failDefinition(ctx, parent.ID, attempt, contract.Internal, "Definition child input is invalid.")
	}
	if n.Target.App != approved.Plan.App || n.Target.Environment != approved.Plan.Environment {
		return w.failDefinition(ctx, parent.ID, attempt, contract.Internal, "Definition child target does not match the approved environment.")
	}
	var result definitionChildResult
	err = w.service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		preflight, err := definitionChildPreflightTx(ctx, tx, parent, attempt, approved, envID, ordinal, kind, target, n)
		result.failureCode, result.failureMessage = preflight.failureCode, preflight.failureMessage
		if err != nil || !preflight.ready {
			return err
		}
		return w.definitionChildCreateTx(ctx, tx, preflight.op, ordinal, n, &result)
	})
	if err != nil {
		if errors.Is(err, errDefinitionChildFailed) {
			return w.failDefinition(ctx, parent.ID, attempt, result.failureCode, result.failureMessage)
		}
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	if result.snooze {
		return river.JobSnooze(definitionPoll)
	}
	return nil
}

func definitionChildPreflightTx(ctx context.Context, tx *entities.StoreTx, parent contract.Operation, attempt uint32, approved definitionApproved, envID string, ordinal int, kind, target string, n platform.Normalized) (definitionChildPreflight, error) {
	var preflight definitionChildPreflight
	if err := lockDefinitionTargetTx(ctx, tx, approved.Plan.App, approved.Plan.Environment); err != nil {
		return preflight, err
	}
	env, err := environmentByNameTx(ctx, tx, approved.Plan.App, approved.Plan.Environment)
	if err != nil {
		return preflight, err
	}
	if string(env.ID) != envID {
		preflight.failureCode, preflight.failureMessage = contract.Internal, "Definition target environment was replaced."
		return preflight, errDefinitionChildFailed
	}
	op, found, err := tx.Operations().Lock(ctx, parent.ID)
	if err != nil || !found {
		return preflight, unavailable(err)
	}
	preflight.op = op
	if op.Attempt != attempt || op.State != contract.Running {
		return preflight, nil
	}
	if op.Kind != platform.KindDefinitionApply || op.Actor != approved.Actor || op.Target != "env/"+approved.Plan.App+"/"+approved.Plan.Environment {
		preflight.failureCode, preflight.failureMessage = contract.Internal, "Definition parent identity changed."
		return preflight, errDefinitionChildFailed
	}
	matches, err := tx.DefinitionSteps().JournalReady(ctx, op.ID, ordinal, envID, kind, target, n.Canonical)
	if err != nil {
		return preflight, unavailable(err)
	}
	if !matches {
		preflight.failureCode, preflight.failureMessage = contract.Internal, "Definition child journal changed or is not canonical."
		return preflight, errDefinitionChildFailed
	}
	preflight.ready = true
	return preflight, nil
}

func (w *platformWorker) definitionChildCreateTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, ordinal int, n platform.Normalized, result *definitionChildResult) error {
	id, err := contract.NewOperationID()
	if err != nil {
		return err
	}
	if linked, err := tx.DefinitionSteps().SetChild(ctx, op.ID, ordinal, id); err != nil || !linked {
		return unavailable(err)
	}
	submitted, err := w.service.recordPlatformSubmissionTx(ctx, tx, id, n, op.Actor, fmt.Sprintf("definition:%s:%d", op.ID, ordinal), nil, nil, "", nil)
	if err != nil {
		fault := contract.Classify(err)
		switch fault.Code {
		case contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound, contract.Forbidden, contract.Conflict:
			// Roll back the uncommitted child link before recording the parent
			// failure. A hostname claimed after approval is a durable conflict,
			// not a reason to retry admission until the job is exhausted.
			result.failureCode = fault.Code
			result.failureMessage = "Definition child admission failed: " + fault.Message + "; completed changes were not rolled back."
			return errDefinitionChildFailed
		case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.MissingTool,
			contract.Unauthenticated, contract.DoctorFailed, contract.ConfirmationRequired,
			contract.NodeVersionMismatch, contract.RemoteUnreachable, contract.Unavailable,
			contract.Cancelled, contract.Internal:
			// Retryable or transient: leave the parent for the next attempt.
		}
		return err
	}
	if submitted.Operation.ID != id {
		if linked, err := tx.DefinitionSteps().SetChild(ctx, op.ID, ordinal, submitted.Operation.ID); err != nil || !linked {
			return unavailable(err)
		}
	}
	result.snooze = true
	return nil
}

var errDefinitionChildFailed = errors.New("definition child admission failed")

func (w *platformWorker) failDefinition(ctx context.Context, id contract.OperationID, attempt uint32, code contract.Code, message string) error {
	_, owned, err := w.service.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State == contract.Cancelling {
			return nil // A previously accepted manual cancellation wins.
		}
		return definitionFailureTx(ctx, tx, op, code, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return river.JobSnooze(definitionPoll)
}

// requeueDefinitionDrain preserves the durable cursor and linked children when
// River has discarded, completed, cancelled or lost the parent job.
func (s *Service) requeueDefinitionDrain(ctx context.Context, id contract.OperationID) error {
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		op, found, err := tx.Operations().Lock(ctx, id)
		if err != nil || !found {
			return unavailable(err)
		}
		if op.Kind != platform.KindDefinitionApply || op.State.Terminal() {
			return nil
		}
		jobID, hasJob, live, err := definitionDrainJobTx(ctx, tx, id)
		if err != nil || live {
			return err
		}
		if op.State != contract.Cancelling {
			if err := definitionFailureTx(ctx, tx, &op, contract.Unavailable, "Definition worker stopped; linked work is being drained."); err != nil {
				return err
			}
		}
		if _, err := definitionDrainTx(ctx, tx, &op); err != nil {
			return err
		}
		return ensureDefinitionDrainJobTx(ctx, tx, id, op, jobID, hasJob, 2)
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	return nil
}

func ensureDefinitionDrainJobTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, op contract.Operation, jobID int64, hasJob bool, delay int) error {
	if !hasJob {
		created, err := tx.Jobs().EnqueuePlatform(ctx, op)
		if err != nil {
			return unavailable(err)
		}
		jobID, hasJob = created, true
		if err := tx.Operations().SetJob(ctx, id, created); err != nil {
			return unavailable(err)
		}
	}
	if hasJob {
		if _, err := tx.RiverJobs().Snooze(ctx, jobID, op.Attempt, delay); err != nil {
			return unavailable(err)
		}
	}
	return nil
}

func definitionDrainJobTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID) (int64, bool, bool, error) {
	jobID, hasJob, err := tx.Operations().JobID(ctx, id)
	if err != nil {
		return jobID, false, false, unavailable(err)
	}
	if !hasJob {
		return jobID, false, false, nil
	}
	state, found, err := tx.RiverJobs().GetForUpdate(ctx, jobID)
	if err != nil {
		return jobID, hasJob, false, unavailable(err)
	}
	if !found {
		return jobID, false, false, nil
	}
	if state != "discarded" && state != "completed" && state != "cancelled" {
		return jobID, hasJob, true, nil
	}
	return jobID, hasJob, false, nil
}

// Repair pre-existing inconsistent terminal journals only after all linked work
// settles. An active child is cancelled, never hidden by clearing reservation.
func (s *Service) reconcileTerminalDefinitionRuns(ctx context.Context) error {
	ids, err := s.entities.DefinitionRuns().ListUnfinishedTerminal(ctx, 100)
	if err != nil {
		return unavailable(err)
	}
	for _, id := range ids {
		err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
			return s.reconcileTerminalDefinitionRunTx(ctx, tx, id)
		})
		if err != nil {
			if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
				return unavailable(err)
			}
			code := contract.Classify(err).Code
			if code == contract.Conflict || code == contract.PreconditionFailed {
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Service) reconcileTerminalDefinitionRunTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID) error {
	op, found, err := tx.Operations().Lock(ctx, id)
	if err != nil || !found {
		return unavailable(err)
	}
	active, err := definitionDrainTx(ctx, tx, &op)
	if err != nil {
		return err
	}
	if active {
		return nil
	}
	if err := guardDefinitionChildrenTerminalTx(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.DefinitionRuns().Finish(ctx, id); err != nil {
		return err
	}
	return nil
}
