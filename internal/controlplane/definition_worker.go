package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

const definitionPoll = 2 * time.Second

var definitionProviderFailure = regexp.MustCompile(`^Cloudflare provider: (authorization|client-rejection|rate-limit|provider-unavailable|network/timeout|invalid-response)( \(HTTP [1-5][0-9][0-9]\))?$`)
var definitionDefinitiveProviderFailure = regexp.MustCompile(`^Cloudflare provider: (authorization \(HTTP (401|403)\)|client-rejection \(HTTP 4[0-9][0-9]\))$`)
var definitionCleanupRejectionPrefix = regexp.MustCompile(`^definitive cleanup rejection: [a-f0-9]{64} `)

func definitionDefinitiveRejection(message string) bool {
	return definitionDefinitiveProviderFailure.MatchString(message) &&
		!strings.HasSuffix(message, "(HTTP 408)") && !strings.HasSuffix(message, "(HTTP 429)") &&
		!strings.Contains(message, "client-rejection (HTTP 401)") && !strings.Contains(message, "client-rejection (HTTP 403)")
}

func definitionSafeProviderDiagnostic(message string) string {
	message = strings.TrimPrefix(message, "definitive write rejection: ")
	message = definitionCleanupRejectionPrefix.ReplaceAllString(message, "")
	if definitionProviderFailure.MatchString(message) {
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

func definitionFailureTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, code contract.Code, message string) error {
	if !contract.KnownCode(code) || code == "" {
		code = contract.Internal
	}
	if len(message) > 1024 {
		message = message[:1024]
	}
	tag, err := tx.Exec(ctx, `UPDATE eregion_definition_runs SET failure_code=$2,failure_message=$3 WHERE operation_id=$1 AND failure_code='' AND finished_at IS NULL`, op.ID, code, message)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 0 {
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
func definitionDrainTx(ctx context.Context, tx pgx.Tx, op *contract.Operation) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT c.id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state IN ('queued','running','cancelling') ORDER BY s.ordinal`, op.ID)
	if err != nil {
		return false, err
	}
	var ids []contract.OperationID
	for rows.Next() {
		var id contract.OperationID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	active := false
	for _, id := range ids {
		child, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
		if err != nil {
			return false, err
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

func definitionFinishTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, state contract.State, code contract.Code, message string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state IN ('queued','running','cancelling'))`, op.ID).Scan(&active); err != nil {
		return err
	}
	if active {
		return errors.New("definition parent cannot finish while a child is active")
	}
	if err := guardDefinitionChildrenTerminalTx(ctx, tx, op.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_definition_runs SET finished_at=clock_timestamp() WHERE operation_id=$1 AND finished_at IS NULL`, op.ID); err != nil {
		return err
	}
	return transition(ctx, tx, op, state, code, message)
}

func guardDefinitionChildrenTerminalTx(ctx context.Context, tx pgx.Tx, parent contract.OperationID) error {
	var broken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM eregion_definition_steps s
		WHERE s.parent_operation_id=$1 AND s.child_operation_id IS NULL AND (
			s.dns_progress <> '[]'::jsonb OR EXISTS (
				SELECT 1 FROM eregion_operations c JOIN eregion_platform_domain_dns x ON x.last_operation_id=c.id
				WHERE c.idempotency_key='definition:'||s.parent_operation_id||':'||s.ordinal::text
			) OR EXISTS (SELECT 1 FROM eregion_platform_domain_dns x WHERE x.domain_id=s.domain_id AND x.state IN ('pending','failed','cleanup_pending','cleanup_failed'))
		))`, parent).Scan(&broken); err != nil {
		return unavailable(err)
	}
	if broken {
		return definitionDNSConflict()
	}
	rows, err := tx.Query(ctx, `SELECT s.ordinal,s.kind,s.target,s.input,coalesce(c.id,''),coalesce(c.kind,''),coalesce(c.target,''),coalesce(c.input,'null'::jsonb),coalesce(c.state,''),coalesce(c.actor_kind,''),coalesce(c.actor_subject,''),coalesce(c.idempotency_key,''),p.actor_kind,p.actor_subject
		FROM eregion_definition_steps s
		JOIN eregion_operations p ON p.id=s.parent_operation_id
		LEFT JOIN eregion_operations c ON c.id=s.child_operation_id
		WHERE s.parent_operation_id=$1 AND s.child_operation_id IS NOT NULL ORDER BY s.ordinal`, parent)
	if err != nil {
		return unavailable(err)
	}
	var children []contract.Operation
	for rows.Next() {
		var child contract.Operation
		var ordinal int
		var kind, target, childTarget, actorKind, actorSubject, parentKind, parentSubject, key string
		var input, childInput []byte
		if err := rows.Scan(&ordinal, &kind, &target, &input, &child.ID, &child.Kind, &childTarget, &childInput, &child.State, &actorKind, &actorSubject, &key, &parentKind, &parentSubject); err != nil {
			rows.Close()
			return unavailable(err)
		}
		step, stepErr := platform.Default().Normalize(kind, target, input)
		linked, linkedErr := platform.Default().Normalize(child.Kind, childTarget, childInput)
		if child.ID == "" || kind != child.Kind || target != childTarget || actorKind != parentKind || actorSubject != parentSubject ||
			key != fmt.Sprintf("definition:%s:%d", parent, ordinal) || stepErr != nil || linkedErr != nil || !bytes.Equal(step.Canonical, linked.Canonical) {
			rows.Close()
			return definitionDNSConflict()
		}
		children = append(children, child)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return unavailable(err)
	}
	for _, child := range children {
		if err := guardDefinitionDNSTerminalTx(ctx, tx, &child); err != nil {
			return err
		}
	}
	return nil
}

func (w *platformWorker) runDefinitionApply(ctx context.Context, claimed contract.Operation, attempt uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := w.service.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", claimed.ID))
	if err != nil {
		return unavailable(err)
	}
	if op.State.Terminal() || op.Attempt != attempt {
		return nil
	}
	var approvedJSON []byte
	var envID string
	var next int
	var failure contract.Code
	var failureMessage string
	var sameInput, finished bool
	err = tx.QueryRow(ctx, `SELECT r.approved,r.environment_id,r.next_step,r.failure_code,r.failure_message,coalesce(o.input=r.approved->'input',false),r.finished_at IS NOT NULL FROM eregion_definition_runs r JOIN eregion_operations o ON o.id=r.operation_id WHERE r.operation_id=$1 FOR UPDATE OF r`, op.ID).Scan(&approvedJSON, &envID, &next, &failure, &failureMessage, &sameInput, &finished)
	if err != nil {
		return unavailable(err)
	}
	var approved definitionApproved
	valid := json.Unmarshal(approvedJSON, &approved) == nil && sameInput && !finished && !approved.Input.Plan.Deploy && op.Kind == platform.KindDefinitionApply && op.Actor == approved.Actor && envID == string(approved.Snapshot.EnvironmentID) && op.Target == "env/"+approved.Plan.App+"/"+approved.Plan.Environment
	if !valid && op.State != contract.Cancelling {
		failure, failureMessage = contract.Internal, "Definition journal validation failed; linked work is being drained without rollback."
		if err := definitionFailureTx(ctx, tx, &op, failure, failureMessage); err != nil {
			return err
		}
	}
	if op.State == contract.Cancelling {
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
		if err := tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
		if active {
			return river.JobSnooze(definitionPoll)
		}
		return nil
	}
	var childID *string
	var kind, target string
	var input []byte
	err = tx.QueryRow(ctx, `SELECT kind,target,input,child_operation_id FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=$2 FOR UPDATE`, op.ID, next).Scan(&kind, &target, &input, &childID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A gap cannot be mistaken for completion.
		var later, active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal >= $2), EXISTS(SELECT 1 FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state IN ('queued','running','cancelling'))`, op.ID, next).Scan(&later, &active); err != nil {
			return err
		}
		if later || active {
			if err := definitionFailureTx(ctx, tx, &op, contract.Internal, "Definition journal cursor is inconsistent; linked work is being drained."); err != nil {
				return err
			}
		} else if err := definitionFinishTx(ctx, tx, &op, contract.Succeeded, "", "Definition changes completed; no deployment was queued."); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
		if later || active {
			return river.JobSnooze(definitionPoll)
		}
		return nil
	}
	if err != nil {
		return unavailable(err)
	}
	var otherActive bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND s.ordinal<>$2 AND c.state IN ('queued','running','cancelling'))`, op.ID, next).Scan(&otherActive); err != nil {
		return unavailable(err)
	}
	if otherActive {
		if err := definitionFailureTx(ctx, tx, &op, contract.Internal, "Definition journal has an active child outside its cursor; linked work is being drained."); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
		return river.JobSnooze(definitionPoll)
	}
	n, normErr := platform.Default().Normalize(kind, target, input)
	allowed := kind == platform.KindResourceApply || kind == platform.KindEnvironmentUpdate || kind == platform.KindDomainAdd || kind == platform.KindDomainRemove
	if normErr != nil || !allowed || n.Target.App != approved.Plan.App || n.Target.Environment != approved.Plan.Environment {
		if err := definitionFailureTx(ctx, tx, &op, contract.Internal, "Definition step does not match the approved environment or action."); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
		return river.JobSnooze(definitionPoll)
	}
	if childID != nil {
		var childState contract.State
		var childCode contract.Code
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT state,error_code,kind=$2 AND target=$3 AND input=$4::jsonb AND actor_kind=$5 AND actor_subject=$6 FROM eregion_operations WHERE id=$1`, *childID, kind, target, n.Canonical, op.Actor.Kind, op.Actor.Subject).Scan(&childState, &childCode, &matches); err != nil {
			return unavailable(err)
		}
		if !matches {
			if err := definitionFailureTx(ctx, tx, &op, contract.Internal, "Definition linked child does not match its frozen journal; linked work is being drained."); err != nil {
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return unavailable(err)
			}
			return river.JobSnooze(definitionPoll)
		}
		if !childState.Terminal() {
			if err := tx.Commit(ctx); err != nil {
				return unavailable(err)
			}
			return river.JobSnooze(definitionPoll)
		}
		switch childState {
		case contract.Succeeded:
			if _, err := tx.Exec(ctx, `UPDATE eregion_definition_runs SET next_step=next_step+1 WHERE operation_id=$1 AND next_step=$2`, op.ID, next); err != nil {
				return err
			}
		case contract.Canceled:
			if err := transition(ctx, tx, &op, contract.Cancelling, "", "Definition child "+*childID+" was cancelled; completed changes were not rolled back."); err != nil {
				return err
			}
		default:
			message := "Definition child " + *childID + " failed; completed changes were not rolled back."
			if kind == platform.KindDomainAdd || kind == platform.KindDomainRemove {
				var resources, revisions int
				var provider string
				err := tx.QueryRow(ctx, `SELECT
					count(*) FILTER (WHERE s.kind=$2 AND c.state='succeeded'),
					count(*) FILTER (WHERE s.kind=$3 AND c.state='succeeded'),
					coalesce((SELECT CASE WHEN failed.kind=$6 THEN x.cleanup_error ELSE x.last_error END FROM eregion_definition_steps failed
						JOIN eregion_platform_domain_dns x ON x.domain_id=failed.domain_id
						WHERE failed.parent_operation_id=$1 AND failed.ordinal=$4 AND x.last_operation_id=$5),'')
					FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id
					WHERE s.parent_operation_id=$1 AND s.ordinal<$4`, op.ID, platform.KindResourceApply, platform.KindEnvironmentUpdate, next, *childID, platform.KindDomainRemove).Scan(&resources, &revisions, &provider)
				if err != nil {
					return unavailable(err)
				}
				prior := false
				if provider == "" && kind == platform.KindDomainRemove {
					if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT event->>'message' FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='dns_prior_provider_failure' ORDER BY sequence DESC LIMIT 1),'')`, *childID).Scan(&provider); err != nil {
						return unavailable(err)
					}
					prior = provider != ""
				}
				message = fmt.Sprintf("Definition DNS child %s failed; completed resource steps=%d, revision steps=%d were not rolled back. Inspect serving, route, TLS and public status separately.", *childID, resources, revisions)
				if safeProvider := definitionSafeProviderDiagnostic(provider); safeProvider != "" {
					if prior {
						message += " Prior provider diagnostic: " + safeProvider + "; cleanup was subsequently verified."
					} else {
						message += " " + safeProvider + "; inspect the child and provider configuration before replanning."
					}
				}
			}
			if err := definitionFailureTx(ctx, tx, &op, childCode, message); err != nil {
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
		return river.JobSnooze(definitionPoll)
	}
	// Release operation/journal locks before acquiring the app then environment
	// locks in the creation transaction. No session lock spans child execution.
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return w.createDefinitionChild(ctx, op, attempt, approved, envID, next, kind, target, input)
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
	tx, err := w.service.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	if err := lockDefinitionTargetTx(ctx, tx, approved.Plan.App, approved.Plan.Environment); err != nil {
		return err
	}
	env, err := environmentByNameTx(ctx, tx, approved.Plan.App, approved.Plan.Environment)
	if err != nil {
		return err
	}
	if string(env.ID) != envID {
		return w.failDefinitionAfterRollback(ctx, tx, parent.ID, attempt, "Definition target environment was replaced.")
	}
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", parent.ID))
	if err != nil {
		return unavailable(err)
	}
	if op.Attempt != attempt || op.State != contract.Running {
		return nil
	}
	if op.Kind != platform.KindDefinitionApply || op.Actor != approved.Actor || op.Target != "env/"+approved.Plan.App+"/"+approved.Plan.Environment {
		return w.failDefinitionAfterRollback(ctx, tx, parent.ID, attempt, "Definition parent identity changed.")
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_definition_runs r JOIN eregion_definition_steps s ON s.parent_operation_id=r.operation_id WHERE r.operation_id=$1 AND r.finished_at IS NULL AND r.next_step=$2 AND r.environment_id=$3 AND s.ordinal=$2 AND s.kind=$4 AND s.target=$5 AND s.input=$6::jsonb AND s.child_operation_id IS NULL)`, op.ID, ordinal, envID, kind, target, n.Canonical).Scan(&matches); err != nil {
		return unavailable(err)
	}
	if !matches {
		return w.failDefinitionAfterRollback(ctx, tx, parent.ID, attempt, "Definition child journal changed or is not canonical.")
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_definition_steps SET child_operation_id=$3 WHERE parent_operation_id=$1 AND ordinal=$2`, op.ID, ordinal, id); err != nil {
		return err
	}
	result, err := w.service.recordPlatformSubmissionTx(ctx, tx, id, n, op.Actor, fmt.Sprintf("definition:%s:%d", op.ID, ordinal), nil, nil, "", nil)
	if err != nil {
		fault := contract.Classify(err)
		switch fault.Code {
		case contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound, contract.Forbidden, contract.Conflict:
			// Roll back the uncommitted child link before recording the parent
			// failure. A hostname claimed after approval is a durable conflict,
			// not a reason to retry admission until the job is exhausted.
			rollback(tx)
			return w.failDefinition(ctx, parent.ID, attempt, fault.Code, "Definition child admission failed: "+fault.Message+"; completed changes were not rolled back.")
		}
		return err
	}
	if result.Operation.ID != id {
		if _, err := tx.Exec(ctx, `UPDATE eregion_definition_steps SET child_operation_id=$3 WHERE parent_operation_id=$1 AND ordinal=$2`, op.ID, ordinal, result.Operation.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return river.JobSnooze(definitionPoll)
}

func (w *platformWorker) failDefinitionAfterRollback(ctx context.Context, tx pgx.Tx, id contract.OperationID, attempt uint32, message string) error {
	rollback(tx)
	return w.failDefinition(ctx, id, attempt, contract.Internal, message)
}

func (w *platformWorker) failDefinition(ctx context.Context, id contract.OperationID, attempt uint32, code contract.Code, message string) error {
	_, owned, err := w.service.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return unavailable(err)
	}
	if op.Kind != platform.KindDefinitionApply || op.State.Terminal() {
		return nil
	}
	var jobID *int64
	if err := tx.QueryRow(ctx, `SELECT job_id FROM eregion_operations WHERE id=$1`, id).Scan(&jobID); err != nil {
		return unavailable(err)
	}
	if jobID != nil {
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM river_job WHERE id=$1 FOR UPDATE`, *jobID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			jobID = nil
		} else if err != nil {
			return unavailable(err)
		} else if state != "discarded" && state != "completed" && state != "cancelled" {
			return nil
		}
	}
	if op.State != contract.Cancelling {
		if err := definitionFailureTx(ctx, tx, &op, contract.Unavailable, "Definition worker stopped; linked work is being drained."); err != nil {
			return err
		}
	}
	if _, err := definitionDrainTx(ctx, tx, &op); err != nil {
		return err
	}
	if jobID == nil {
		if s.enqueuePlatform == nil {
			return unavailable()
		}
		created, err := s.enqueuePlatform(ctx, tx, op)
		if err != nil {
			return unavailable(err)
		}
		jobID = &created
		if _, err := tx.Exec(ctx, `UPDATE eregion_operations SET job_id=$2 WHERE id=$1`, id, created); err != nil {
			return unavailable(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE river_job SET state='scheduled',finalized_at=NULL,attempt=GREATEST(attempt,$2),max_attempts=GREATEST(max_attempts,attempt+1,$2+1),scheduled_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, *jobID, op.Attempt); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

// Repair pre-existing inconsistent terminal journals only after all linked work
// settles. An active child is cancelled, never hidden by clearing reservation.
func (s *Service) reconcileTerminalDefinitionRuns(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT r.operation_id FROM eregion_definition_runs r JOIN eregion_operations o ON o.id=r.operation_id WHERE r.finished_at IS NULL AND o.state IN ('succeeded','failed','cancelled') ORDER BY o.updated_at LIMIT 100`)
	if err != nil {
		return unavailable(err)
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
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return unavailable(err)
		}
		op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
		if err != nil {
			rollback(tx)
			return unavailable(err)
		}
		active, err := definitionDrainTx(ctx, tx, &op)
		if err != nil {
			rollback(tx)
			return err
		}
		if !active {
			if err := guardDefinitionChildrenTerminalTx(ctx, tx, id); err != nil {
				rollback(tx)
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE eregion_definition_runs SET finished_at=clock_timestamp() WHERE operation_id=$1 AND finished_at IS NULL`, id); err != nil {
				rollback(tx)
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
	}
	return nil
}
