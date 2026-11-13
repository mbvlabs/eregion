package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

const maxDefinitionDNSProgressBytes = 4 << 20

type definitionApprovedRun struct {
	Snapshot definitionSnapshot `json:"snapshot"`
	Plan     api.DefinitionPlan `json:"plan"`
}

// definitionDNSChild is read from the journal, never from public action input.
type definitionDNSChild struct {
	ParentID      contract.OperationID
	Ordinal       int
	EnvironmentID platform.EnvironmentID
	DomainID      platform.DomainID
	AllocatedID   platform.DomainID
	Plan          *integrations.DefinitionDNSPlan
	Cleanup       *integrations.DomainDNSPlan
	Approved      definitionApprovedRun
	Input         platform.DomainInput
	Kind          string
	Target        string
	Actor         contract.Actor
	ParentState   contract.State
	InputHash     string
	Progress      []integrations.DefinitionDNSProgress
	progressJSON  json.RawMessage
	childID       contract.OperationID
}

func definitionDNSConflict() error {
	return contract.NewFault(contract.Conflict, "definition DNS authorization or approved state changed", "Stop this child and obtain a new definition plan.")
}

func definitionCleanupDefinitiveRejection(message string, plan integrations.DomainDNSPlan) bool {
	rest, found := strings.CutPrefix(message, "definitive cleanup rejection: ")
	if !found {
		return false
	}
	digest, category, found := strings.Cut(rest, " ")
	if !found || !definitionDefinitiveRejection(category) {
		return false
	}
	for _, change := range plan.Changes {
		if change.Action == "delete" && change.Before != nil && change.Before.ID != "" {
			hash := sha256.Sum256([]byte(change.Before.ID))
			if digest == hex.EncodeToString(hash[:]) {
				return true
			}
		}
	}
	return false
}

func lockDefinitionDNSCredentialTx(ctx context.Context, tx pgx.Tx, snapshot definitionSnapshot, zoneID string) error {
	var expected int64
	for _, credential := range snapshot.Credentials {
		if credential.ZoneID == zoneID {
			expected = credential.Revision
			break
		}
	}
	var current int64
	err := tx.QueryRow(ctx, `SELECT latest_revision FROM eregion_provider_credentials WHERE provider='cloudflare' AND zone_id=$1 FOR SHARE`, zoneID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (expected == 0 || current != expected)) {
		return definitionDNSConflict()
	}
	if err != nil {
		return unavailable()
	}
	return nil
}

// loadDefinitionDNSChildTx distinguishes an ordinary unlinked operation from
// a broken link. It locks journal and operation rows for the caller's short
// transaction. No parent attempt is pinned: a parent retry retains its child.
func loadDefinitionDNSChildTx(ctx context.Context, tx pgx.Tx, child contract.OperationID) (*definitionDNSChild, error) {
	return loadDefinitionDNSChildTxMode(ctx, tx, child, false)
}

func loadDefinitionDNSChildTxMode(ctx context.Context, tx pgx.Tx, child contract.OperationID, recovery bool) (*definitionDNSChild, error) {
	// The unlocked lookup only chooses which target to lock; the locked journal
	// and approved input below revalidate it. Never lock a child before its
	// app/environment: the parent sequencer and admission take those first.
	var app, environment string
	var environmentID platform.EnvironmentID
	var parentID contract.OperationID
	var ordinal int
	err := tx.QueryRow(ctx, `SELECT a.name,e.name,e.id,s.parent_operation_id,s.ordinal FROM eregion_definition_steps s
        JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE s.child_operation_id=$1`, child).Scan(&app, &environment, &environmentID, &parentID, &ordinal)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_definition_steps WHERE child_operation_id=$1)`, child).Scan(&exists); err != nil {
			return nil, unavailable()
		}
		if exists {
			return nil, definitionDNSConflict()
		}
		return nil, nil
	}
	if err != nil {
		return nil, unavailable()
	}
	if err := lockDefinitionTargetTx(ctx, tx, app, environment); err != nil {
		return nil, err
	}
	// Parent polling takes parent, run, then step. Explicit single-row locks
	// preserve that order regardless of the planner's join order; the child is
	// last. Re-read all relationships below after the locks are held.
	lock := func(row pgx.Row) error {
		var id string
		if err := row.Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return definitionDNSConflict()
		} else if err != nil {
			return unavailable()
		}
		return nil
	}
	if err := lock(tx.QueryRow(ctx, `SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, parentID)); err != nil {
		return nil, err
	}
	if err := lock(tx.QueryRow(ctx, `SELECT operation_id FROM eregion_definition_runs WHERE operation_id=$1 FOR UPDATE`, parentID)); err != nil {
		return nil, err
	}
	if err := lock(tx.QueryRow(ctx, `SELECT parent_operation_id FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=$2 FOR UPDATE`, parentID, ordinal)); err != nil {
		return nil, err
	}
	if err := lock(tx.QueryRow(ctx, `SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, child)); err != nil {
		return nil, err
	}
	var d definitionDNSChild
	var approved, input, opInput, plan, cleanup, progress []byte
	var kind, target, opKind, opTarget, inputHash, parentKind, parentTarget string
	var parentState, childState contract.State
	var parentActor, childActor contract.Actor
	var attempt uint32
	var finished bool
	err = tx.QueryRow(ctx, `SELECT r.operation_id,s.ordinal,r.environment_id,
        coalesce(s.domain_id,''),coalesce(s.allocated_domain_id,''),r.approved,s.kind,s.target,s.input,
        s.dns_plan,s.dns_cleanup,s.dns_progress,p.kind,p.target,p.actor_kind,p.actor_subject,p.state,
        c.kind,c.target,c.actor_kind,c.actor_subject,c.state,c.attempt,c.input_hash,c.input,r.finished_at IS NOT NULL
        FROM eregion_definition_steps s
        JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id
        JOIN eregion_operations p ON p.id=r.operation_id
        JOIN eregion_operations c ON c.id=s.child_operation_id
        WHERE s.parent_operation_id=$1 AND s.ordinal=$2 AND s.child_operation_id=$3`, parentID, ordinal, child).Scan(
		&d.ParentID, &d.Ordinal, &d.EnvironmentID, &d.DomainID, &d.AllocatedID, &approved, &kind, &target, &input,
		&plan, &cleanup, &progress, &parentKind, &parentTarget, &parentActor.Kind, &parentActor.Subject, &parentState,
		&opKind, &opTarget, &childActor.Kind, &childActor.Subject, &childState, &attempt, &inputHash, &opInput, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, definitionDNSConflict()
	}
	if err != nil {
		return nil, unavailable()
	}
	parentAllowed := parentState == contract.Running || parentState == contract.Queued || (recovery && parentState == contract.Cancelling)
	childAllowed := childState == contract.Running || childState == contract.Queued || (recovery && childState == contract.Cancelling)
	if parentKind != "platform.definition.apply" || parentTarget != target || kind != opKind || target != opTarget || parentActor != childActor || finished || !parentAllowed || !childAllowed {
		return nil, definitionDNSConflict()
	}
	if err := requireCloudflareOperator(childActor); err != nil {
		return nil, err
	}
	if kind != platform.KindDomainAdd && kind != platform.KindDomainRemove {
		return nil, definitionDNSConflict()
	}
	n, err := platform.Default().Normalize(kind, target, input)
	if err != nil || hashInput(n.Canonical) != inputHash {
		return nil, definitionDNSConflict()
	}
	on, err := platform.Default().Normalize(opKind, opTarget, opInput)
	if err != nil || hashInput(on.Canonical) != inputHash || !reflect.DeepEqual(n.Canonical, on.Canonical) {
		return nil, definitionDNSConflict()
	}
	in, ok := n.Input.(*platform.DomainInput)
	if !ok || d.DomainID == "" || (d.AllocatedID != "" && d.AllocatedID != d.DomainID) {
		return nil, definitionDNSConflict()
	}
	d.Input = *in
	d.Kind, d.Target, d.Actor, d.InputHash = kind, target, childActor, inputHash
	d.ParentState = parentState
	if d.ParentID != parentID || d.Ordinal != ordinal || d.EnvironmentID != environmentID || in.App != app || in.Environment != environment || json.Unmarshal(approved, &d.Approved) != nil || d.Approved.Snapshot.EnvironmentID != d.EnvironmentID || d.Approved.Plan.App != in.App || d.Approved.Plan.Environment != in.Environment {
		return nil, definitionDNSConflict()
	}
	if len(plan) != 0 && string(plan) != "null" {
		var p integrations.DefinitionDNSPlan
		if json.Unmarshal(plan, &p) != nil || p.DomainID != string(d.DomainID) || p.Host != in.Domain || kind != platform.KindDomainAdd {
			return nil, definitionDNSConflict()
		}
		found := false
		for _, approvedPlan := range d.Approved.Plan.DNS {
			if reflect.DeepEqual(approvedPlan, p) {
				found = true
				break
			}
		}
		if !found {
			return nil, definitionDNSConflict()
		}
		d.Plan = &p
	}
	if len(cleanup) != 0 && string(cleanup) != "null" {
		var p integrations.DomainDNSPlan
		if json.Unmarshal(cleanup, &p) != nil || p.DomainID != string(d.DomainID) || p.Host != in.Domain || kind != platform.KindDomainRemove {
			return nil, definitionDNSConflict()
		}
		found := false
		for _, approvedPlan := range d.Approved.Plan.DNSCleanup {
			if reflect.DeepEqual(approvedPlan, p) {
				found = true
				break
			}
		}
		if !found {
			return nil, definitionDNSConflict()
		}
		d.Cleanup = &p
	}
	if len(progress) > maxDefinitionDNSProgressBytes || json.Unmarshal(progress, &d.Progress) != nil || d.Progress == nil {
		return nil, definitionDNSConflict()
	}
	d.progressJSON = progress
	d.childID = child
	return &d, nil
}

// definitionDNSChild opens and releases a short transaction before provider IO.
func (s *Service) definitionDNSChild(ctx context.Context, child contract.OperationID) (*definitionDNSChild, error) {
	return s.definitionDNSChildMode(ctx, child, false)
}

func (s *Service) definitionDNSChildMode(ctx context.Context, child contract.OperationID, recovery bool) (*definitionDNSChild, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable()
	}
	defer rollback(tx)
	step, err := loadDefinitionDNSChildTxMode(ctx, tx, child, recovery)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable()
	}
	return step, nil
}

// stopDefinitionDNSChild is called with the target's session lock held. It
// never repeats provider writes. An unresolved intent keeps both operations
// nonterminal and is retried with bounded read-only probes.
func (w *platformWorker) stopDefinitionDNSChild(ctx context.Context, op contract.Operation, attempt uint32) (bool, error) {
	wait := func() (bool, error) {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _, _ = w.service.mutateOwned(recordCtx, op.ID, attempt, func(tx pgx.Tx, current *contract.Operation) error {
			var recorded bool
			if err := tx.QueryRow(recordCtx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='dns_outcome_unknown')`, op.ID).Scan(&recorded); err != nil {
				return err
			}
			if !recorded && !current.State.Terminal() {
				return appendEvent(recordCtx, tx, current, contract.Event{Kind: "warning", Stage: "dns_outcome_unknown", Message: "DNS outcome remains unknown; read-only recovery retains the child and parent reservation."})
			}
			return nil
		})
		return true, river.JobSnooze(30 * time.Second)
	}
	step, err := w.service.definitionDNSChildMode(ctx, op.ID, true)
	if err != nil {
		return wait()
	}
	if step == nil {
		return false, nil
	}
	if step.Plan == nil && step.Cleanup == nil {
		if step.Input.DNSMode == integrations.DomainDNSModeCloudflare {
			return wait()
		}
		if step.Kind == platform.KindDomainRemove {
			ledger, err := w.service.loadDomainDNSLedger(ctx, step.Input.App, step.Input.Environment, step.Input.Domain)
			if err != nil || ledger != nil {
				return wait()
			}
		}
		return false, nil
	}
	finish := func() (bool, error) {
		var exhausted bool
		if err := w.service.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='dns_recovery_exhausted')`, op.ID).Scan(&exhausted); err != nil {
			return wait()
		}
		var err error
		if exhausted {
			err = w.service.failPlatformOperation(ctx, op.ID, attempt, contract.Unavailable, "Definition DNS worker stopped; read-only recovery resolved its pending provider effect.")
		} else {
			err = w.service.cancelPlatform(ctx, op.ID, attempt, "Definition DNS child stopped after read-only recovery; completed effects were retained.")
		}
		if err != nil {
			return wait()
		}
		return true, nil
	}
	ledger, err := w.service.loadDomainDNSLedger(ctx, step.Input.App, step.Input.Environment, step.Input.Domain)
	if err != nil {
		return wait()
	}
	if ledger == nil && step.Cleanup != nil {
		return finish() // The terminal guard requires durable cleanup proof.
	}
	if ledger == nil || ledger.LastOperationID != op.ID {
		return wait()
	}
	if step.Plan != nil {
		store := newDefinitionDNSProgressStore(w.service, op, attempt, *ledger, step)
		store.recovery = true
		entries, err := store.Load(ctx)
		if err != nil {
			return wait()
		}
		knownRejection := strings.TrimPrefix(ledger.LastError, "definitive write rejection: ")
		definitive := knownRejection != ledger.LastError && definitionDefinitiveRejection(knownRejection)
		if len(entries) > 0 && entries[len(entries)-1].Result == nil && !definitive {
			request := integrations.DomainDNSRequest{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID), Ownership: ledger.Ownership, Host: ledger.Domain, Addresses: ledger.DesiredOrigins, Proxied: ledger.DesiredProxied}
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := w.service.domainDNS.ResolveApprovedDefinitionDNS(probeCtx, op.Actor, op.ID, request, *step.Plan, store)
			cancel()
			if err != nil {
				return wait()
			}
		}
	} else if len(step.Cleanup.Changes) != 0 && !(ledger.State == domainDNSStateCleanupFailed && definitionCleanupDefinitiveRejection(ledger.CleanupError, *step.Cleanup)) {
		// Cleanup has no per-record intent. Only an exact read proving every
		// approved owned record absent can resolve an interrupted cleanup.
		if w.service.domainDNS == nil || w.service.domainDNS.AuthorizeWrite(ctx, op.Actor) != nil {
			return wait()
		}
		request := integrations.DomainDNSRequest{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID), Ownership: ledger.Ownership, Host: ledger.Domain}
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := w.service.checkDefinitionDNSCleanupAuthority(probeCtx, op, attempt, *ledger, step, true)
		var current integrations.DomainDNSPlan
		if err == nil {
			current, err = w.service.domainDNS.PlanRemove(probeCtx, request)
		}
		if err == nil {
			_, err = w.service.checkDefinitionDNSCleanupAuthority(probeCtx, op, attempt, *ledger, step, true)
		}
		cancel()
		if err != nil || validateDefinitionDNSCleanup(*step.Cleanup, current) != nil || len(current.Changes) != 0 {
			return wait()
		}
		if err := w.service.domainDNS.AuthorizeWrite(ctx, op.Actor); err != nil {
			return wait()
		}
		if err := w.service.recordDefinitionDNSCleanupResolution(ctx, op, attempt, *ledger, step, current); err != nil {
			return wait()
		}
	}
	return finish()
}

func (s *Service) recordDefinitionDNSCleanupResolution(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild, observed integrations.DomainDNSPlan) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	step, err := loadDefinitionDNSChildTxMode(ctx, tx, op.ID, true)
	if err != nil {
		return err
	}
	if step == nil || step.ParentID != expected.ParentID || step.Ordinal != expected.Ordinal || step.Actor != op.Actor || step.Cleanup == nil || !reflect.DeepEqual(step.Cleanup, expected.Cleanup) || len(observed.Changes) != 0 || validateDefinitionDNSCleanup(*step.Cleanup, observed) != nil {
		return definitionDNSConflict()
	}
	var state contract.State
	var currentAttempt uint32
	if err := tx.QueryRow(ctx, `SELECT state,attempt FROM eregion_operations WHERE id=$1`, op.ID).Scan(&state, &currentAttempt); err != nil {
		return unavailable(err)
	}
	if state != contract.Cancelling || currentAttempt != attempt {
		return definitionDNSConflict()
	}
	current, err := lockDefinitionDNSLedgerTx(ctx, tx, ledger.DomainID, step.EnvironmentID)
	if err != nil {
		return err
	}
	if current.LastOperationID != op.ID || current.Generation != ledger.Generation || current.Domain != ledger.Domain || current.ZoneID != ledger.ZoneID || current.ZoneName != ledger.ZoneName || current.Ownership != ledger.Ownership || current.Provider != "cloudflare" {
		return definitionDNSConflict()
	}
	policy, err := scanDefinitionDNSPolicy(tx.QueryRow(ctx, definitionDNSPolicySelect+` FOR SHARE OF p`))
	if err != nil {
		return err
	}
	if policy.Revision != step.Approved.Snapshot.Policy.Revision {
		return definitionDNSConflict()
	}
	if err := lockDefinitionDNSCredentialTx(ctx, tx, step.Approved.Snapshot, ledger.ZoneID); err != nil {
		return err
	}
	if diagnostic := definitionSafeProviderDiagnostic(current.CleanupError); diagnostic != "" {
		var recorded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='dns_prior_provider_failure')`, op.ID).Scan(&recorded); err != nil {
			return unavailable(err)
		}
		if !recorded {
			child, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", op.ID))
			if err != nil {
				return unavailable(err)
			}
			if err := appendEvent(ctx, tx, &child, contract.Event{Kind: "warning", Stage: "dns_prior_provider_failure", Message: diagnostic}); err != nil {
				return err
			}
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns SET state='cleanup_verified',cleanup_verified_at=clock_timestamp(),cleanup_error='',updated_at=clock_timestamp()
		WHERE domain_id=$1 AND last_operation_id=$2 AND generation=$3 AND state IN ('cleanup_pending','cleanup_failed','cleanup_verified')`, ledger.DomainID, op.ID, ledger.Generation)
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() != 1 {
		return definitionDNSConflict()
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

// prepareDefinitionDNSRecoveryTx is called with the child operation locked.
// It records exhaustion once and leaves the child nonterminal.
func prepareDefinitionDNSRecoveryTx(ctx context.Context, tx pgx.Tx, op *contract.Operation) (bool, error) {
	var linked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_definition_steps s JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id WHERE s.child_operation_id=$1 AND r.finished_at IS NULL AND (s.dns_plan IS NOT NULL OR s.dns_cleanup IS NOT NULL))`, op.ID).Scan(&linked); err != nil {
		return false, unavailable(err)
	}
	if !linked || op.State.Terminal() {
		return false, nil
	}
	var recorded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='dns_recovery_exhausted')`, op.ID).Scan(&recorded); err != nil {
		return false, unavailable(err)
	}
	if !recorded {
		if err := appendEvent(ctx, tx, op, contract.Event{Kind: "warning", Stage: "dns_recovery_exhausted", Message: "DNS attempts ended; read-only recovery retains the reservation while the provider outcome is unknown."}); err != nil {
			return false, err
		}
	}
	if op.State != contract.Cancelling {
		if err := transition(ctx, tx, op, contract.Cancelling, "", "DNS execution stopped; read-only recovery will not repeat provider writes."); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *Service) latchDefinitionDNSRecovery(ctx context.Context, id contract.OperationID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return unavailable(err)
	}
	handled, err := prepareDefinitionDNSRecoveryTx(ctx, tx, &op)
	if err != nil {
		return err
	}
	if !handled {
		return definitionDNSConflict()
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) requeueDefinitionDNSRecovery(ctx context.Context, id contract.OperationID) (bool, error) {
	var jobID *int64
	if err := s.pool.QueryRow(ctx, `SELECT job_id FROM eregion_operations WHERE id=$1`, id).Scan(&jobID); err != nil {
		return false, unavailable(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, unavailable(err)
	}
	defer rollback(tx)
	if jobID != nil {
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM river_job WHERE id=$1 FOR UPDATE`, *jobID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			jobID = nil
		} else if err != nil {
			return false, unavailable(err)
		} else if state != "discarded" && state != "completed" && state != "cancelled" {
			var linked bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_definition_steps s JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id WHERE s.child_operation_id=$1 AND r.finished_at IS NULL AND (s.dns_plan IS NOT NULL OR s.dns_cleanup IS NOT NULL))`, id).Scan(&linked); err != nil {
				return false, unavailable(err)
			}
			return linked, nil
		}
	}
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return false, unavailable(err)
	}
	handled, err := prepareDefinitionDNSRecoveryTx(ctx, tx, &op)
	if err != nil || !handled {
		return handled, err
	}
	if jobID == nil {
		if s.enqueuePlatform == nil {
			return true, unavailable()
		}
		created, err := s.enqueuePlatform(ctx, tx, op)
		if err != nil {
			return true, unavailable(err)
		}
		jobID = &created
		if _, err := tx.Exec(ctx, `UPDATE eregion_operations SET job_id=$2 WHERE id=$1`, id, created); err != nil {
			return true, unavailable(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE river_job SET state='scheduled',finalized_at=NULL,attempt=GREATEST(attempt,$2),max_attempts=GREATEST(max_attempts,attempt+1,$2+1),scheduled_at=clock_timestamp()+interval '30 seconds' WHERE id=$1`, *jobID, op.Attempt); err != nil {
		return true, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return true, unavailable(err)
	}
	return true, nil
}

// guardDefinitionDNSTerminalTx runs while the operation row is locked. An
// intent writer locks that same row before persisting intent, so terminal
// settlement cannot race a new request past this check.
func guardDefinitionDNSTerminalTx(ctx context.Context, tx pgx.Tx, op *contract.Operation) error {
	if op.Kind != platform.KindDomainAdd && op.Kind != platform.KindDomainRemove {
		return nil
	}
	var progress, plan, cleanup, input, approvedRun []byte
	var domainID, ledgerState, ledgerError, cleanupError, ledgerOwner string
	err := tx.QueryRow(ctx, `SELECT s.dns_progress,s.dns_plan,s.dns_cleanup,s.input,r.approved,coalesce(s.domain_id,''),coalesce(x.state,''),coalesce(x.last_error,''),coalesce(x.cleanup_error,''),coalesce(x.last_operation_id,'')
		FROM eregion_definition_steps s JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=s.domain_id
		WHERE s.child_operation_id=$1`, op.ID).Scan(&progress, &plan, &cleanup, &input, &approvedRun, &domainID, &ledgerState, &ledgerError, &cleanupError, &ledgerOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		var definitionKey, dnsEvidence bool
		if err := tx.QueryRow(ctx, `SELECT left(idempotency_key,11)='definition:', coalesce(input->>'dns_mode','')='cloudflare' OR EXISTS(SELECT 1 FROM eregion_platform_domain_dns WHERE last_operation_id=$1) FROM eregion_operations WHERE id=$1`, op.ID).Scan(&definitionKey, &dnsEvidence); err != nil {
			return unavailable(err)
		}
		if definitionKey && dnsEvidence {
			return definitionDNSConflict()
		}
		return nil
	}
	if err != nil {
		return unavailable(err)
	}
	hasPlan := len(plan) > 0 && string(plan) != "null"
	hasCleanup := len(cleanup) > 0 && string(cleanup) != "null"
	if (hasPlan && hasCleanup) || (hasPlan && op.Kind != platform.KindDomainAdd) || (hasCleanup && op.Kind != platform.KindDomainRemove) {
		return definitionDNSConflict()
	}
	var run definitionApprovedRun
	if json.Unmarshal(approvedRun, &run) != nil {
		return definitionDNSConflict()
	}
	if !hasPlan && !hasCleanup {
		var entries []integrations.DefinitionDNSProgress
		var domain platform.DomainInput
		if json.Unmarshal(progress, &entries) != nil || entries == nil || len(entries) != 0 || json.Unmarshal(input, &domain) != nil || domain.DNSMode == integrations.DomainDNSModeCloudflare ||
			(op.Kind == platform.KindDomainRemove && ledgerOwner == string(op.ID)) ||
			ledgerState == domainDNSStatePending || ledgerState == domainDNSStateFailed || ledgerState == domainDNSStateCleanupPending || ledgerState == domainDNSStateCleanupFailed {
			return definitionDNSConflict()
		}
		return nil
	}
	if hasCleanup && !hasPlan && ledgerOwner == "" && op.Kind == platform.KindDomainRemove {
		var entries []integrations.DefinitionDNSProgress
		var approved integrations.DomainDNSPlan
		if json.Unmarshal(progress, &entries) != nil || entries == nil || len(entries) != 0 || json.Unmarshal(cleanup, &approved) != nil || approved.DomainID != domainID {
			return definitionDNSConflict()
		}
		found := false
		for _, candidate := range run.Plan.DNSCleanup {
			if reflect.DeepEqual(candidate, approved) {
				found = true
				break
			}
		}
		if !found {
			return definitionDNSConflict()
		}
		var verified bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='dns' AND event->>'message' LIKE 'Verified Cloudflare cleanup for %') AND NOT EXISTS(SELECT 1 FROM eregion_definition_steps s JOIN eregion_platform_domains d ON d.id=s.domain_id WHERE s.child_operation_id=$1)`, op.ID).Scan(&verified); err != nil {
			return unavailable(err)
		}
		if verified {
			return nil
		}
	}
	if ledgerOwner != string(op.ID) {
		return contract.NewFault(contract.Unavailable, "definition DNS recovery is unresolved", "Retain the child and parent reservation.")
	}
	var entries []integrations.DefinitionDNSProgress
	if json.Unmarshal(progress, &entries) != nil || entries == nil {
		return contract.NewFault(contract.Unavailable, "definition DNS recovery is unresolved", "Retain the child and parent reservation.")
	}
	if hasPlan {
		var approved integrations.DefinitionDNSPlan
		if json.Unmarshal(plan, &approved) != nil || validateDefinitionDNSProgress(approved, entries) != nil {
			return definitionDNSConflict()
		}
		found := false
		for _, candidate := range run.Plan.DNS {
			if reflect.DeepEqual(candidate, approved) && approved.DomainID == domainID {
				found = true
				break
			}
		}
		if !found {
			return definitionDNSConflict()
		}
		for _, entry := range entries {
			if entry.Result != nil && !integrations.DefinitionDNSObserved(entry, approved.Ownership) {
				return definitionDNSConflict()
			}
		}
		if len(entries) > 0 && entries[len(entries)-1].Result == nil {
			rejection := strings.TrimPrefix(ledgerError, "definitive write rejection: ")
			if rejection == ledgerError || !definitionDefinitiveRejection(rejection) {
				return contract.NewFault(contract.Unavailable, "approved DNS write outcome remains unknown", "Retain the child and parent reservation.")
			}
		}
	}
	if hasCleanup {
		var approved integrations.DomainDNSPlan
		if json.Unmarshal(cleanup, &approved) != nil || (len(approved.Changes) > 0 && ledgerState != domainDNSStateCleanupVerified &&
			!(ledgerState == domainDNSStateCleanupFailed && definitionCleanupDefinitiveRejection(cleanupError, approved))) {
			return contract.NewFault(contract.Unavailable, "approved DNS cleanup outcome remains unknown", "Retain the child and parent reservation.")
		}
		found := false
		for _, candidate := range run.Plan.DNSCleanup {
			if reflect.DeepEqual(candidate, approved) && approved.DomainID == domainID {
				found = true
				break
			}
		}
		if !found {
			return definitionDNSConflict()
		}
	}
	return nil
}

// definitionDNSProgressStore fences every access to the captured child attempt
// and ledger generation. Callers must not hold a transaction over provider IO.
type definitionDNSProgressStore struct {
	service  *Service
	child    contract.Operation
	attempt  uint32
	ledger   domainDNSLedger
	parentID contract.OperationID
	ordinal  int
	recovery bool
}

func newDefinitionDNSProgressStore(s *Service, child contract.Operation, attempt uint32, ledger domainDNSLedger, step *definitionDNSChild) *definitionDNSProgressStore {
	return &definitionDNSProgressStore{service: s, child: child, attempt: attempt, ledger: ledger, parentID: step.ParentID, ordinal: step.Ordinal}
}

func validateDefinitionDNSProgress(plan integrations.DefinitionDNSPlan, entries []integrations.DefinitionDNSProgress) error {
	if len(entries) > len(plan.Changes) {
		return definitionDNSConflict()
	}
	before := plan.Before
	for i, e := range entries {
		if e.Index != i || !reflect.DeepEqual(e.Before, before) || !reflect.DeepEqual(e.Expected, plan.Changes[i]) || (e.Result == nil && i != len(entries)-1) {
			return definitionDNSConflict()
		}
		before = e.Result
	}
	return nil
}

func definitionDNSPlanMatchesLedger(plan integrations.DefinitionDNSPlan, ledger domainDNSLedger) bool {
	want := make([]integrations.DNSRecord, 0, len(ledger.DesiredOrigins))
	seen := map[string]bool{}
	for _, origin := range ledger.DesiredOrigins {
		ip := net.ParseIP(origin)
		if ip == nil {
			return false
		}
		kind := "AAAA"
		if ip.To4() != nil {
			kind = "A"
		}
		content := ip.String()
		if seen[kind+"\x00"+content] {
			continue
		}
		seen[kind+"\x00"+content] = true
		want = append(want, integrations.DNSRecord{Name: ledger.Domain, Type: kind, Content: content, TTL: 1, Proxied: ledger.DesiredProxied, Comment: ledger.Ownership})
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].Type != want[j].Type {
			return want[i].Type < want[j].Type
		}
		return want[i].Content < want[j].Content
	})
	return len(want) > 0 && reflect.DeepEqual(want, plan.Desired)
}

// Lock the ledger before the binding, matching domain cleanup. The
// app/environment and parent/run/step/child locks are already held.
func lockDefinitionDNSLedgerTx(ctx context.Context, tx pgx.Tx, domainID platform.DomainID, environmentID platform.EnvironmentID) (domainDNSLedger, error) {
	var id platform.DomainID
	err := tx.QueryRow(ctx, `SELECT domain_id FROM eregion_platform_domain_dns WHERE domain_id=$1 FOR UPDATE`, domainID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainDNSLedger{}, definitionDNSConflict()
	}
	if err != nil {
		return domainDNSLedger{}, unavailable()
	}
	err = tx.QueryRow(ctx, `SELECT id FROM eregion_platform_domains WHERE id=$1 AND environment_id=$2 FOR UPDATE`, domainID, environmentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainDNSLedger{}, definitionDNSConflict()
	}
	if err != nil {
		return domainDNSLedger{}, unavailable()
	}
	ledger, err := scanDomainDNSLedger(tx.QueryRow(ctx, domainDNSLedgerSelect+` WHERE x.domain_id=$1`, domainID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domainDNSLedger{}, definitionDNSConflict()
	}
	if err != nil {
		return domainDNSLedger{}, unavailable()
	}
	return ledger, nil
}

// withLocked rechecks authority and the approved ledger before any progress
// access. The policy row is locked only until this transaction commits.
func (p *definitionDNSProgressStore) withLocked(ctx context.Context, fn func(pgx.Tx, *definitionDNSChild) error) error {
	if p == nil || p.service == nil || p.service.pool == nil || p.service.domainDNS == nil {
		return unavailable()
	}
	if err := p.service.domainDNS.AuthorizeWrite(ctx, p.child.Actor); err != nil {
		return err
	}
	tx, err := p.service.pool.Begin(ctx)
	if err != nil {
		return unavailable()
	}
	defer rollback(tx)
	d, err := loadDefinitionDNSChildTxMode(ctx, tx, p.child.ID, p.recovery)
	if err != nil {
		return err
	}
	if d == nil || d.ParentID != p.parentID || d.Ordinal != p.ordinal || d.Plan == nil || d.DomainID != p.ledger.DomainID {
		return definitionDNSConflict()
	}
	var state contract.State
	var attempt uint32
	var actor contract.Actor
	if err := tx.QueryRow(ctx, `SELECT state,attempt,actor_kind,actor_subject FROM eregion_operations WHERE id=$1`, p.child.ID).Scan(&state, &attempt, &actor.Kind, &actor.Subject); err != nil {
		return unavailable()
	}
	if actor != p.child.Actor || (state != contract.Running && !(p.recovery && state == contract.Cancelling)) || (!p.recovery && d.ParentState == contract.Cancelling) || attempt != p.attempt {
		return definitionDNSConflict()
	}
	var ledger domainDNSLedger
	ledger, err = lockDefinitionDNSLedgerTx(ctx, tx, d.DomainID, d.EnvironmentID)
	if err != nil {
		return err
	}
	var environmentID platform.EnvironmentID
	if err := tx.QueryRow(ctx, `SELECT environment_id FROM eregion_platform_domains WHERE id=$1`, d.DomainID).Scan(&environmentID); err != nil {
		return unavailable()
	}
	if environmentID != d.EnvironmentID {
		return definitionDNSConflict()
	}
	if ledger.Generation != p.ledger.Generation || ledger.LastOperationID != p.child.ID || ledger.App != d.Input.App || ledger.Environment != d.Input.Environment || ledger.Domain != d.Input.Domain || ledger.ZoneID != d.Plan.ZoneID || ledger.ZoneName != d.Plan.Zone || ledger.Ownership != d.Plan.Ownership || ledger.Ownership != integrations.DomainDNSOwnership(string(d.DomainID), ledger.ZoneID) || ledger.Provider != "cloudflare" || !definitionDNSPlanMatchesLedger(*d.Plan, ledger) {
		return definitionDNSConflict()
	}
	policy, err := scanDefinitionDNSPolicy(tx.QueryRow(ctx, definitionDNSPolicySelect+` FOR SHARE OF p`))
	if err != nil {
		return err
	}
	approvedPolicy := d.Approved.Snapshot.Policy
	if !reflect.DeepEqual(policy, approvedPolicy) || !policy.Enabled || (policy.ProxyMode != "proxied" && policy.ProxyMode != "dns-only") || policy.ZoneID != ledger.ZoneID || policy.ZoneName != ledger.ZoneName || !reflect.DeepEqual(policy.Origins, ledger.DesiredOrigins) || (policy.ProxyMode == "proxied") != ledger.DesiredProxied || d.Input.DNSMode != "cloudflare" || d.Input.DNSZone != ledger.ZoneID || d.Input.DNSProxied == nil || *d.Input.DNSProxied != ledger.DesiredProxied || !reflect.DeepEqual(d.Input.DNSOrigins, ledger.DesiredOrigins) {
		return definitionDNSConflict()
	}
	if err := lockDefinitionDNSCredentialTx(ctx, tx, d.Approved.Snapshot, ledger.ZoneID); err != nil {
		return err
	}
	if err := validateDefinitionDNSProgress(*d.Plan, d.Progress); err != nil {
		return err
	}
	if err := fn(tx, d); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable()
	}
	return nil
}

func (p *definitionDNSProgressStore) Load(ctx context.Context) ([]integrations.DefinitionDNSProgress, error) {
	var entries []integrations.DefinitionDNSProgress
	err := p.withLocked(ctx, func(_ pgx.Tx, d *definitionDNSChild) error { entries = d.Progress; return nil })
	return entries, err
}

func (p *definitionDNSProgressStore) Guard(ctx context.Context) error {
	return p.withLocked(ctx, func(pgx.Tx, *definitionDNSChild) error { return nil })
}

// validateDefinitionDNSCleanup permits missing approved records on retry, but
// never authorizes a new or modified owned record. Foreign records are ignored.
func validateDefinitionDNSCleanup(approved, current integrations.DomainDNSPlan) error {
	if approved.ZoneID != current.ZoneID || approved.Zone != current.Zone || approved.DomainID != current.DomainID || approved.Host != current.Host || approved.Ownership != current.Ownership {
		return definitionDNSConflict()
	}
	allowed := make(map[string]integrations.DomainDNSRecord, len(approved.Changes))
	for _, change := range approved.Changes {
		if change.Action != "delete" || change.Before == nil || change.Before.ID == "" || change.Before.Ownership != "owned" || change.Before.Comment != approved.Ownership || change.After != nil || change.Type != change.Before.Type {
			return definitionDNSConflict()
		}
		if _, exists := allowed[change.Before.ID]; exists {
			return definitionDNSConflict()
		}
		allowed[change.Before.ID] = *change.Before
	}
	seen := make(map[string]bool, len(current.Changes))
	for _, change := range current.Changes {
		if change.Action != "delete" || change.Before == nil || change.After != nil || change.Type != change.Before.Type || seen[change.Before.ID] {
			return definitionDNSConflict()
		}
		record, ok := allowed[change.Before.ID]
		if !ok || !reflect.DeepEqual(record, *change.Before) {
			return definitionDNSConflict()
		}
		seen[change.Before.ID] = true
	}
	return nil
}

// definitionDNSCleanupBoundary brackets the provider read with authority checks.
func definitionDNSCleanupBoundary(check, snapshot func() error) error {
	if err := check(); err != nil {
		return err
	}
	if err := snapshot(); err != nil {
		return err
	}
	return check()
}

// guardDefinitionDNSCleanup releases all SQL locks before reading the provider,
// then rechecks authority immediately before returning deletion permission.
// A provider-side concurrent writer can still race the final deletion.
func (s *Service) guardDefinitionDNSCleanup(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild, request integrations.DomainDNSRequest) error {
	var approved *integrations.DomainDNSPlan
	check := func() error {
		if err := s.domainDNS.AuthorizeWrite(ctx, op.Actor); err != nil {
			return err
		}
		step, err := s.checkDefinitionDNSCleanupAuthority(ctx, op, attempt, ledger, expected, false)
		if err != nil {
			return err
		}
		if approved != nil && !reflect.DeepEqual(approved, step.Cleanup) {
			return definitionDNSConflict()
		}
		approved = step.Cleanup
		return nil
	}
	return definitionDNSCleanupBoundary(check, func() error {
		currentPlan, err := s.domainDNS.PlanRemove(ctx, request)
		if err != nil {
			return err
		}
		return validateDefinitionDNSCleanup(*approved, currentPlan)
	})
}

func (s *Service) checkDefinitionDNSCleanupAuthority(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild, recovery bool) (*definitionDNSChild, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable()
	}
	defer rollback(tx)
	step, err := loadDefinitionDNSChildTxMode(ctx, tx, op.ID, recovery)
	if err != nil {
		return nil, err
	}
	if step == nil || step.ParentID != expected.ParentID || step.Ordinal != expected.Ordinal || step.Actor != op.Actor || step.Cleanup == nil || step.DomainID != ledger.DomainID || step.Cleanup.ZoneID != ledger.ZoneID || step.Cleanup.Zone != ledger.ZoneName || step.Cleanup.Host != ledger.Domain || step.Cleanup.Ownership != ledger.Ownership || ledger.Ownership != integrations.DomainDNSOwnership(string(ledger.DomainID), ledger.ZoneID) {
		return nil, definitionDNSConflict()
	}
	var state contract.State
	var currentAttempt uint32
	if err := tx.QueryRow(ctx, `SELECT state,attempt FROM eregion_operations WHERE id=$1`, op.ID).Scan(&state, &currentAttempt); err != nil {
		return nil, unavailable()
	}
	if (state != contract.Running && !(recovery && state == contract.Cancelling)) || currentAttempt != attempt {
		return nil, definitionDNSConflict()
	}
	if state == contract.Running && step.ParentState == contract.Cancelling {
		return nil, definitionDNSConflict()
	}
	current, err := lockDefinitionDNSLedgerTx(ctx, tx, ledger.DomainID, step.EnvironmentID)
	if err != nil {
		return nil, err
	}
	var environmentID platform.EnvironmentID
	if err := tx.QueryRow(ctx, `SELECT environment_id FROM eregion_platform_domains WHERE id=$1`, ledger.DomainID).Scan(&environmentID); err != nil {
		return nil, unavailable()
	}
	if environmentID != step.EnvironmentID || current.LastOperationID != op.ID || current.Generation != ledger.Generation || current.App != step.Input.App || current.Environment != step.Input.Environment || current.Domain != ledger.Domain || current.ZoneID != ledger.ZoneID || current.ZoneName != ledger.ZoneName || current.Ownership != ledger.Ownership || current.Provider != "cloudflare" {
		return nil, definitionDNSConflict()
	}
	policy, err := scanDefinitionDNSPolicy(tx.QueryRow(ctx, definitionDNSPolicySelect+` FOR SHARE OF p`))
	if err != nil {
		return nil, err
	}
	if policy.Revision != step.Approved.Snapshot.Policy.Revision {
		return nil, definitionDNSConflict()
	}
	if err := lockDefinitionDNSCredentialTx(ctx, tx, step.Approved.Snapshot, ledger.ZoneID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable()
	}
	return step, nil
}

func (p *definitionDNSProgressStore) Intent(ctx context.Context, entry integrations.DefinitionDNSProgress) error {
	return p.withLocked(ctx, func(tx pgx.Tx, d *definitionDNSChild) error {
		if entry.Result != nil || entry.Index < 0 || entry.Index >= len(d.Plan.Changes) {
			return definitionDNSConflict()
		}
		if entry.Index < len(d.Progress) {
			if entry.Index == len(d.Progress)-1 && d.Progress[entry.Index].Result == nil && reflect.DeepEqual(d.Progress[entry.Index], entry) {
				return nil
			}
			return definitionDNSConflict()
		}
		if entry.Index != len(d.Progress) {
			return definitionDNSConflict()
		}
		d.Progress = append(d.Progress, entry)
		if err := validateDefinitionDNSProgress(*d.Plan, d.Progress); err != nil {
			return err
		}
		return saveDefinitionDNSProgress(ctx, tx, d)
	})
}

func (p *definitionDNSProgressStore) Observed(ctx context.Context, entry integrations.DefinitionDNSProgress) error {
	return p.withLocked(ctx, func(tx pgx.Tx, d *definitionDNSChild) error {
		if entry.Result == nil || entry.Index < 0 || entry.Index != len(d.Progress)-1 {
			return definitionDNSConflict()
		}
		old := d.Progress[entry.Index]
		if old.Result != nil {
			if reflect.DeepEqual(old, entry) {
				return nil
			}
			return definitionDNSConflict()
		}
		if !reflect.DeepEqual(old.Before, entry.Before) || !reflect.DeepEqual(old.Expected, entry.Expected) {
			return definitionDNSConflict()
		}
		d.Progress[entry.Index] = entry
		return saveDefinitionDNSProgress(ctx, tx, d)
	})
}

func saveDefinitionDNSProgress(ctx context.Context, tx pgx.Tx, d *definitionDNSChild) error {
	raw, err := json.Marshal(d.Progress)
	if err != nil || len(raw) > maxDefinitionDNSProgressBytes {
		return definitionDNSConflict()
	}
	tag, err := tx.Exec(ctx, `UPDATE eregion_definition_steps SET dns_progress=$3 WHERE parent_operation_id=$1 AND ordinal=$2 AND dns_progress=$4::jsonb AND child_operation_id=$5`, d.ParentID, d.Ordinal, raw, d.progressJSON, d.childID)
	if err != nil {
		return unavailable()
	}
	if tag.RowsAffected() != 1 {
		return definitionDNSConflict()
	}
	return nil
}
