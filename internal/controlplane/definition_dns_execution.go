package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
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

func lockDefinitionDNSCredentialTx(ctx context.Context, tx *entities.StoreTx, snapshot definitionSnapshot, zoneID string) error {
	var expected int64
	for _, credential := range snapshot.Credentials {
		if credential.ZoneID == zoneID {
			expected = credential.Revision
			break
		}
	}
	current, found, err := tx.Providers().LockRevisionForZoneShare(ctx, "cloudflare", zoneID)
	if err != nil {
		return unavailable(err)
	}
	if !found || expected <= 0 || current > math.MaxInt64 || int64(current) != expected {
		return definitionDNSConflict()
	}
	return nil
}

// loadDefinitionDNSChildTx distinguishes an ordinary unlinked operation from
// a broken link. It locks journal and operation rows for the caller's short
// transaction. No parent attempt is pinned: a parent retry retains its child.
func loadDefinitionDNSChildTx(ctx context.Context, tx *entities.StoreTx, child contract.OperationID) (*definitionDNSChild, error) {
	return loadDefinitionDNSChildTxMode(ctx, tx, child, false)
}

func loadDefinitionDNSChildTxMode(ctx context.Context, tx *entities.StoreTx, child contract.OperationID, recovery bool) (*definitionDNSChild, error) {
	journal, target, found, err := lockDefinitionDNSChildJournalTx(ctx, tx, child)
	if err != nil || !found {
		return nil, err
	}
	d, err := definitionDNSChildJournal(journal, target, recovery)
	if err != nil {
		return nil, err
	}
	if d.Plan, err = definitionDNSChildPlan(journal.DNSPlan, d, journal.StepKind); err != nil {
		return nil, err
	}
	if d.Cleanup, err = definitionDNSChildCleanup(journal.DNSCleanup, d, journal.StepKind); err != nil {
		return nil, err
	}
	if d.Progress, d.progressJSON, err = definitionDNSChildProgress(journal.DNSProgress); err != nil {
		return nil, err
	}
	d.childID = child
	return &d, nil
}

// lockDefinitionDNSChildJournalTx locks the child journal in the established
// target, parent, run, step, child order and returns its joined snapshot.
func lockDefinitionDNSChildJournalTx(ctx context.Context, tx *entities.StoreTx, child contract.OperationID) (entities.DefinitionDNSJournal, entities.DefinitionDNSChildTarget, bool, error) {
	// The unlocked lookup only chooses which target to lock; the locked journal
	// and approved input below revalidate it. Never lock a child before its
	// app/environment: the parent sequencer and admission take those first.
	targetInfo, found, err := tx.DefinitionDNS().ChildTarget(ctx, child)
	if err != nil {
		return entities.DefinitionDNSJournal{}, targetInfo, false, unavailable(err)
	}
	if !found {
		exists, err := tx.DefinitionSteps().ChildExists(ctx, child)
		if err != nil {
			return entities.DefinitionDNSJournal{}, targetInfo, false, unavailable(err)
		}
		if exists {
			return entities.DefinitionDNSJournal{}, targetInfo, false, definitionDNSConflict()
		}
		return entities.DefinitionDNSJournal{}, targetInfo, false, nil
	}
	if err := lockDefinitionTargetTx(ctx, tx, targetInfo.App, targetInfo.Environment); err != nil {
		return entities.DefinitionDNSJournal{}, targetInfo, false, err
	}
	// Parent polling takes parent, run, then step. Explicit single-row locks
	// preserve that order regardless of the planner's join order; the child is
	// last. Re-read all relationships below after the locks are held.
	if _, found, err := tx.Operations().Lock(ctx, targetInfo.ParentID); err != nil {
		return entities.DefinitionDNSJournal{}, targetInfo, false, unavailable(err)
	} else if !found {
		return entities.DefinitionDNSJournal{}, targetInfo, false, definitionDNSConflict()
	}
	if _, found, err := tx.DefinitionRuns().Lock(ctx, targetInfo.ParentID); err != nil {
		return entities.DefinitionDNSJournal{}, targetInfo, false, unavailable(err)
	} else if !found {
		return entities.DefinitionDNSJournal{}, targetInfo, false, definitionDNSConflict()
	}
	if _, found, err := tx.DefinitionSteps().Lock(ctx, targetInfo.ParentID, targetInfo.Ordinal); err != nil {
		return entities.DefinitionDNSJournal{}, targetInfo, false, unavailable(err)
	} else if !found {
		return entities.DefinitionDNSJournal{}, targetInfo, false, definitionDNSConflict()
	}
	if _, found, err := tx.Operations().Lock(ctx, child); err != nil {
		return entities.DefinitionDNSJournal{}, targetInfo, false, unavailable(err)
	} else if !found {
		return entities.DefinitionDNSJournal{}, targetInfo, false, definitionDNSConflict()
	}
	journal, found, err := tx.DefinitionDNS().LoadJournal(ctx, targetInfo.ParentID, targetInfo.Ordinal, child)
	if err != nil {
		return entities.DefinitionDNSJournal{}, targetInfo, false, unavailable(err)
	}
	if !found {
		return entities.DefinitionDNSJournal{}, targetInfo, false, definitionDNSConflict()
	}
	return journal, targetInfo, true, nil
}

func definitionDNSChildJournal(journal entities.DefinitionDNSJournal, target entities.DefinitionDNSChildTarget, recovery bool) (definitionDNSChild, error) {
	d := definitionDNSChild{
		ParentID: journal.ParentID, Ordinal: journal.Ordinal,
		EnvironmentID: journal.EnvironmentID, DomainID: journal.DomainID,
		AllocatedID: journal.AllocatedDomainID, Progress: nil, progressJSON: journal.DNSProgress,
	}
	if err := definitionDNSChildJournalIdentity(&d, journal, recovery); err != nil {
		return d, err
	}
	if err := definitionDNSChildJournalInput(&d, journal, target); err != nil {
		return d, err
	}
	return d, nil
}

func definitionDNSChildJournalIdentity(d *definitionDNSChild, journal entities.DefinitionDNSJournal, recovery bool) error {
	d.ParentState = journal.ParentState
	d.Kind, d.Target, d.Actor, d.InputHash = journal.StepKind, journal.StepTarget, journal.ChildActor, journal.ChildInputHash
	d.Approved = definitionApprovedRun{}
	if json.Unmarshal(journal.Approved, &d.Approved) != nil {
		return definitionDNSConflict()
	}
	parentAllowed := journal.ParentState == contract.Running || journal.ParentState == contract.Queued || (recovery && journal.ParentState == contract.Cancelling)
	childAllowed := journal.ChildState == contract.Running || journal.ChildState == contract.Queued || (recovery && journal.ChildState == contract.Cancelling)
	if journal.ParentKind != "platform.definition.apply" || journal.ParentTarget != journal.StepTarget || journal.StepKind != journal.ChildKind || journal.StepTarget != journal.ChildTarget || journal.ParentActor != journal.ChildActor || journal.Finished || !parentAllowed || !childAllowed {
		return definitionDNSConflict()
	}
	if err := requireCloudflareOperator(journal.ChildActor); err != nil {
		return err
	}
	if journal.StepKind != platform.KindDomainAdd && journal.StepKind != platform.KindDomainRemove {
		return definitionDNSConflict()
	}
	return nil
}

func definitionDNSChildJournalInput(d *definitionDNSChild, journal entities.DefinitionDNSJournal, target entities.DefinitionDNSChildTarget) error {
	n, err := platform.Default().Normalize(journal.StepKind, journal.StepTarget, journal.StepInput)
	if err != nil || hashInput(n.Canonical) != journal.ChildInputHash {
		return definitionDNSConflict()
	}
	on, err := platform.Default().Normalize(journal.ChildKind, journal.ChildTarget, journal.ChildInput)
	if err != nil || hashInput(on.Canonical) != journal.ChildInputHash || !reflect.DeepEqual(n.Canonical, on.Canonical) {
		return definitionDNSConflict()
	}
	in, ok := n.Input.(*platform.DomainInput)
	if !ok || d.DomainID == "" || (d.AllocatedID != "" && d.AllocatedID != d.DomainID) {
		return definitionDNSConflict()
	}
	d.Input = *in
	d.Kind, d.Target, d.Actor, d.InputHash = journal.StepKind, journal.StepTarget, journal.ChildActor, journal.ChildInputHash
	d.ParentState = journal.ParentState
	if d.ParentID != target.ParentID || d.Ordinal != target.Ordinal || d.EnvironmentID != target.EnvironmentID || in.App != target.App || in.Environment != target.Environment || d.Approved.Snapshot.EnvironmentID != d.EnvironmentID || d.Approved.Plan.App != in.App || d.Approved.Plan.Environment != in.Environment {
		return definitionDNSConflict()
	}
	return nil
}

func definitionDNSChildPlan(raw json.RawMessage, d definitionDNSChild, kind string) (*integrations.DefinitionDNSPlan, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var p integrations.DefinitionDNSPlan
	if json.Unmarshal(raw, &p) != nil || p.DomainID != string(d.DomainID) || p.Host != d.Input.Domain || kind != platform.KindDomainAdd {
		return nil, definitionDNSConflict()
	}
	for _, approvedPlan := range d.Approved.Plan.DNS {
		if reflect.DeepEqual(approvedPlan, p) {
			return &p, nil
		}
	}
	return nil, definitionDNSConflict()
}

func definitionDNSChildCleanup(raw json.RawMessage, d definitionDNSChild, kind string) (*integrations.DomainDNSPlan, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var p integrations.DomainDNSPlan
	if json.Unmarshal(raw, &p) != nil || p.DomainID != string(d.DomainID) || p.Host != d.Input.Domain || kind != platform.KindDomainRemove {
		return nil, definitionDNSConflict()
	}
	for _, approvedPlan := range d.Approved.Plan.DNSCleanup {
		if reflect.DeepEqual(approvedPlan, p) {
			return &p, nil
		}
	}
	return nil, definitionDNSConflict()
}

func definitionDNSChildProgress(raw json.RawMessage) ([]integrations.DefinitionDNSProgress, json.RawMessage, error) {
	var progress []integrations.DefinitionDNSProgress
	if len(raw) > maxDefinitionDNSProgressBytes || json.Unmarshal(raw, &progress) != nil || progress == nil {
		return nil, nil, definitionDNSConflict()
	}
	return progress, raw, nil
}

// definitionDNSChild opens and releases a short transaction before provider IO.
func (s *Service) definitionDNSChild(ctx context.Context, child contract.OperationID) (*definitionDNSChild, error) {
	return s.definitionDNSChildMode(ctx, child, false)
}

func (s *Service) definitionDNSChildMode(ctx context.Context, child contract.OperationID, recovery bool) (*definitionDNSChild, error) {
	var step *definitionDNSChild
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		step, err = loadDefinitionDNSChildTxMode(ctx, tx, child, recovery)
		return err
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return nil, unavailable(err)
		}
		return nil, err
	}
	return step, nil
}

// stopDefinitionDNSChild is called with the target reserved. It
// never repeats provider writes. An unresolved intent keeps both operations
// nonterminal and is retried with bounded read-only probes.
func (w *platformWorker) stopDefinitionDNSChild(ctx context.Context, op contract.Operation, attempt uint32) (bool, error) {
	step, err := w.service.definitionDNSChildMode(ctx, op.ID, true)
	if err != nil {
		return w.definitionDNSChildWait(ctx, op, attempt)
	}
	if step == nil {
		return false, nil
	}
	if step.Plan == nil && step.Cleanup == nil {
		return w.definitionDNSStopUnresolved(ctx, op, attempt, step)
	}
	ledger, err := w.service.loadDomainDNSLedger(ctx, step.Input.App, step.Input.Environment, step.Input.Domain)
	if err != nil {
		return w.definitionDNSChildWait(ctx, op, attempt)
	}
	if ledger == nil && step.Cleanup != nil {
		return w.definitionDNSChildFinish(ctx, op, attempt) // The terminal guard requires durable cleanup proof.
	}
	if ledger == nil || ledger.LastOperationID != op.ID {
		return w.definitionDNSChildWait(ctx, op, attempt)
	}
	if step.Plan != nil {
		if err := w.definitionDNSRecoverPlan(ctx, op, attempt, ledger, step); err != nil {
			return w.definitionDNSChildWait(ctx, op, attempt)
		}
	} else if definitionDNSCleanupNeedsReadback(step, *ledger) {
		if err := w.definitionDNSRecoverCleanup(ctx, op, attempt, *ledger, step); err != nil {
			return w.definitionDNSChildWait(ctx, op, attempt)
		}
	}
	return w.definitionDNSChildFinish(ctx, op, attempt)
}

func (w *platformWorker) definitionDNSChildWait(ctx context.Context, op contract.Operation, attempt uint32) (bool, error) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _, _ = w.service.mutateOwned(recordCtx, op.ID, attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		recorded, err := tx.Operations().EventStageExists(recordCtx, op.ID, "dns_outcome_unknown")
		if err != nil {
			return err
		}
		if !recorded && !current.State.Terminal() {
			return appendEvent(recordCtx, tx, current, contract.Event{Kind: "warning", Stage: "dns_outcome_unknown", Message: "DNS outcome remains unknown; read-only recovery retains the child and parent reservation."})
		}
		return nil
	})
	return true, river.JobSnooze(30 * time.Second)
}

func (w *platformWorker) definitionDNSChildFinish(ctx context.Context, op contract.Operation, attempt uint32) (bool, error) {
	exhausted, err := w.service.entities.Operations().EventStageExists(ctx, op.ID, "dns_recovery_exhausted")
	if err != nil {
		return w.definitionDNSChildWait(ctx, op, attempt)
	}
	if exhausted {
		err = w.service.failPlatformOperation(ctx, op.ID, attempt, contract.Unavailable, "Definition DNS worker stopped; read-only recovery resolved its pending provider effect.")
	} else {
		err = w.service.cancelPlatform(ctx, op.ID, attempt, "Definition DNS child stopped after read-only recovery; completed effects were retained.")
	}
	if err != nil {
		return w.definitionDNSChildWait(ctx, op, attempt)
	}
	return true, nil
}

func (w *platformWorker) definitionDNSStopUnresolved(ctx context.Context, op contract.Operation, attempt uint32, step *definitionDNSChild) (bool, error) {
	if step.Input.DNSMode == integrations.DomainDNSModeCloudflare {
		return w.definitionDNSChildWait(ctx, op, attempt)
	}
	if step.Kind == platform.KindDomainRemove {
		ledger, err := w.service.loadDomainDNSLedger(ctx, step.Input.App, step.Input.Environment, step.Input.Domain)
		if err != nil || ledger != nil {
			return w.definitionDNSChildWait(ctx, op, attempt)
		}
	}
	return false, nil
}

func (w *platformWorker) definitionDNSRecoverPlan(ctx context.Context, op contract.Operation, attempt uint32, ledger *domainDNSLedger, step *definitionDNSChild) error {
	store := newDefinitionDNSProgressStore(w.service, op, attempt, *ledger, step)
	store.recovery = true
	entries, err := store.Load(ctx)
	if err != nil {
		return err
	}
	knownRejection := strings.TrimPrefix(ledger.LastError, "definitive write rejection: ")
	definitive := knownRejection != ledger.LastError && definitionDefinitiveRejection(knownRejection)
	if len(entries) == 0 || entries[len(entries)-1].Result != nil || definitive {
		return nil
	}
	request := integrations.DomainDNSRequest{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID), Ownership: ledger.Ownership, Host: ledger.Domain, Addresses: ledger.DesiredOrigins, Proxied: ledger.DesiredProxied}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return w.service.domainDNS.ResolveApprovedDefinitionDNS(probeCtx, op.Actor, op.ID, request, *step.Plan, store)
}

func definitionDNSCleanupNeedsReadback(step *definitionDNSChild, ledger domainDNSLedger) bool {
	if len(step.Cleanup.Changes) == 0 {
		return false
	}
	return ledger.State != domainDNSStateCleanupFailed || !definitionCleanupDefinitiveRejection(ledger.CleanupError, *step.Cleanup)
}

func (w *platformWorker) definitionDNSRecoverCleanup(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, step *definitionDNSChild) error {
	// Cleanup has no per-record intent. Only an exact read proving every
	// approved owned record absent can resolve an interrupted cleanup.
	if w.service.domainDNS == nil || w.service.domainDNS.AuthorizeWrite(ctx, op.Actor) != nil {
		return definitionDNSConflict()
	}
	request := integrations.DomainDNSRequest{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID), Ownership: ledger.Ownership, Host: ledger.Domain}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := w.service.checkDefinitionDNSCleanupAuthority(probeCtx, op, attempt, ledger, step, true)
	var current integrations.DomainDNSPlan
	if err == nil {
		current, err = w.service.domainDNS.PlanRemove(probeCtx, request)
	}
	if err == nil {
		_, err = w.service.checkDefinitionDNSCleanupAuthority(probeCtx, op, attempt, ledger, step, true)
	}
	if err != nil || validateDefinitionDNSCleanup(*step.Cleanup, current) != nil || len(current.Changes) != 0 {
		return definitionDNSConflict()
	}
	if err := w.service.domainDNS.AuthorizeWrite(ctx, op.Actor); err != nil {
		return err
	}
	return w.service.recordDefinitionDNSCleanupResolution(ctx, op, attempt, ledger, step, current)
}

func (s *Service) recordDefinitionDNSCleanupResolution(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild, observed integrations.DomainDNSPlan) error {
	return s.recordDefinitionDNSCleanupResolutionTx(ctx, op, attempt, ledger, expected, observed)
}
func (s *Service) recordDefinitionDNSCleanupResolutionTx(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild, observed integrations.DomainDNSPlan) error {
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := definitionDNSCleanupResolutionTx(ctx, tx, op, attempt, ledger, expected, observed); err != nil {
			return err
		}
		rows, err := tx.Platform().DomainDNS().MarkCleanupVerifiedForGeneration(ctx, ledger.DomainID, op.ID, ledger.Generation)
		if err != nil {
			return unavailable(err)
		}
		if rows != 1 {
			return definitionDNSConflict()
		}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	return nil
}

func definitionDNSCleanupResolutionTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild, observed integrations.DomainDNSPlan) error {
	step, err := loadDefinitionDNSChildTxMode(ctx, tx, op.ID, true)
	if err != nil {
		return err
	}
	if step == nil || step.ParentID != expected.ParentID || step.Ordinal != expected.Ordinal || step.Actor != op.Actor || step.Cleanup == nil || !reflect.DeepEqual(step.Cleanup, expected.Cleanup) || len(observed.Changes) != 0 || validateDefinitionDNSCleanup(*step.Cleanup, observed) != nil {
		return definitionDNSConflict()
	}
	currentOp, found, err := tx.Operations().Get(ctx, op.ID)
	if err != nil || !found {
		return unavailable(err)
	}
	if currentOp.State != contract.Cancelling || currentOp.Attempt != attempt {
		return definitionDNSConflict()
	}
	current, err := lockDefinitionDNSLedgerTx(ctx, tx, ledger.DomainID, step.EnvironmentID)
	if err != nil {
		return err
	}
	if current.LastOperationID != op.ID || current.Generation != ledger.Generation || current.Domain != ledger.Domain || current.ZoneID != ledger.ZoneID || current.ZoneName != ledger.ZoneName || current.Ownership != ledger.Ownership || current.Provider != "cloudflare" {
		return definitionDNSConflict()
	}
	policy, err := lockedDefinitionDNSPolicy(ctx, tx)
	if err != nil {
		return err
	}
	if policy.Revision != step.Approved.Snapshot.Policy.Revision {
		return definitionDNSConflict()
	}
	if err := lockDefinitionDNSCredentialTx(ctx, tx, step.Approved.Snapshot, ledger.ZoneID); err != nil {
		return err
	}
	// Diagnostics must use the row locked in this transaction: cleanup_error
	// can change without changing the generation checked above.
	if err := recordDefinitionDNSPriorDiagnosticTx(ctx, tx, currentOp, current); err != nil {
		return err
	}
	return nil
}

func recordDefinitionDNSPriorDiagnosticTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, ledger domainDNSLedger) error {
	if diagnostic := definitionSafeProviderDiagnostic(ledger.CleanupError); diagnostic != "" {
		recorded, err := tx.Operations().EventStageExists(ctx, op.ID, "dns_prior_provider_failure")
		if err != nil {
			return unavailable(err)
		}
		if !recorded {
			if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "warning", Stage: "dns_prior_provider_failure", Message: diagnostic}); err != nil {
				return err
			}
		}
	}
	return nil
}

// prepareDefinitionDNSRecoveryTx is called with the child operation locked.
// It records exhaustion once and leaves the child nonterminal.
func prepareDefinitionDNSRecoveryTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (bool, error) {
	linked, err := tx.DefinitionDNS().LinkedCleanupPending(ctx, op.ID)
	if err != nil {
		return false, unavailable(err)
	}
	if !linked || op.State.Terminal() {
		return false, nil
	}
	recorded, err := tx.Operations().EventStageExists(ctx, op.ID, "dns_recovery_exhausted")
	if err != nil {
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
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		op, found, err := tx.Operations().Lock(ctx, id)
		if err != nil || !found {
			return unavailable(err)
		}
		handled, err := prepareDefinitionDNSRecoveryTx(ctx, tx, &op)
		if err != nil {
			return err
		}
		if !handled {
			return definitionDNSConflict()
		}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	return nil
}

func (s *Service) requeueDefinitionDNSRecovery(ctx context.Context, id contract.OperationID) (bool, error) {
	handled := false
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		jobID, hasJob, jobHandled, err := definitionDNSRecoveryJobTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if jobHandled {
			handled = true
			return nil
		}
		op, found, err := tx.Operations().Lock(ctx, id)
		if err != nil || !found {
			return unavailable(err)
		}
		handled, err = prepareDefinitionDNSRecoveryTx(ctx, tx, &op)
		if err != nil || !handled {
			return err
		}
		return ensureDefinitionDrainJobTx(ctx, tx, id, op, jobID, hasJob, 30)
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return false, unavailable(err)
		}
		return handled, err
	}
	return handled, nil
}

func definitionDNSRecoveryJobTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID) (int64, bool, bool, error) {
	jobID, hasJob, err := tx.Operations().JobID(ctx, id)
	if err != nil {
		return jobID, false, false, unavailable(err)
	}
	if !hasJob {
		return jobID, false, false, nil
	}
	state, found, err := tx.RiverJobs().GetForUpdate(ctx, jobID)
	if err != nil {
		return jobID, false, false, unavailable(err)
	}
	if !found {
		return jobID, false, false, nil
	}
	if state == "discarded" || state == "completed" || state == "cancelled" {
		return jobID, true, false, nil
	}
	linked, err := tx.DefinitionDNS().LinkedCleanupPending(ctx, id)
	if err != nil {
		return jobID, true, false, unavailable(err)
	}
	return jobID, true, linked, nil
}

// guardDefinitionDNSTerminalTx runs while the operation row is locked. An
// intent writer locks that same row before persisting intent, so terminal
// settlement cannot race a new request past this check.
func guardDefinitionDNSTerminalTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) error {
	if op.Kind != platform.KindDomainAdd && op.Kind != platform.KindDomainRemove {
		return nil
	}
	cleanupState, found, err := tx.Platform().Outcomes().DomainDNSDefinitionCleanupState(ctx, op.ID)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		marker, found, err := tx.DefinitionDNS().TerminalMarker(ctx, op.ID)
		if err != nil {
			return unavailable(err)
		}
		if found && marker.DefinitionKey && marker.DNSEvidence {
			return definitionDNSConflict()
		}
		return nil
	}
	return guardDefinitionDNSLinkedTerminalTx(ctx, tx, op, cleanupState)
}

func guardDefinitionDNSLinkedTerminalTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, cleanupState entities.DefinitionCleanupState) error {
	progress, plan, cleanup, input, approvedRun := cleanupState.DNSProgress, cleanupState.DNSPlan, cleanupState.DNSCleanup, cleanupState.Input, cleanupState.Approved
	domainID, ledgerState := string(cleanupState.DomainID), cleanupState.State
	ledgerError, cleanupError := cleanupState.LastError, cleanupState.CleanupError
	ledgerOwner := string(cleanupState.LastOperationID)
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
		return guardDefinitionDNSNoPlanTerminalTx(progress, input, op, ledgerOwner, ledgerState)
	}
	if hasCleanup && !hasPlan && ledgerOwner == "" && op.Kind == platform.KindDomainRemove {
		verified, err := guardDefinitionDNSUnlinkedCleanupTx(ctx, tx, op, progress, cleanup, domainID, run)
		if err != nil {
			return err
		}
		if verified {
			return nil
		}
		return definitionDNSUnresolvedFault()
	}
	return guardDefinitionDNSOwnedTerminalTx(progress, plan, cleanup, op, hasPlan, hasCleanup, ledgerOwner, domainID, ledgerState, ledgerError, cleanupError, run)
}

func guardDefinitionDNSNoPlanTerminalTx(progress, input []byte, op *contract.Operation, ledgerOwner, ledgerState string) error {
	if !definitionDNSNoPlanCleanupValid(progress, input, op.Kind, ledgerOwner, op.ID, ledgerState) {
		return definitionDNSConflict()
	}
	return nil
}

func guardDefinitionDNSOwnedTerminalTx(progress, plan, cleanup []byte, op *contract.Operation, hasPlan, hasCleanup bool, ledgerOwner, domainID, ledgerState, ledgerError, cleanupError string, run definitionApprovedRun) error {
	if ledgerOwner != string(op.ID) {
		return definitionDNSUnresolvedFault()
	}
	entries, err := definitionDNSTerminalEntries(progress)
	if err != nil {
		return err
	}
	if hasPlan {
		if err := guardDefinitionDNSPlanTerminalTx(plan, entries, ledgerError, domainID, run); err != nil {
			return err
		}
	}
	if hasCleanup {
		if err := guardDefinitionDNSCleanupTerminalTx(cleanup, ledgerState, cleanupError, domainID, run); err != nil {
			return err
		}
	}
	return nil
}

func definitionDNSUnresolvedFault() *contract.FaultError {
	return contract.NewFault(contract.Unavailable, "definition DNS recovery is unresolved", "Retain the child and parent reservation.")
}

func definitionDNSNoPlanCleanupValid(progress, input []byte, kind, ledgerOwner string, opID contract.OperationID, ledgerState string) bool {
	if kind == platform.KindDomainRemove && ledgerOwner == string(opID) {
		return false
	}
	var entries []integrations.DefinitionDNSProgress
	if json.Unmarshal(progress, &entries) != nil || entries == nil || len(entries) != 0 {
		return false
	}
	var domain platform.DomainInput
	if json.Unmarshal(input, &domain) != nil || domain.DNSMode == integrations.DomainDNSModeCloudflare {
		return false
	}
	switch ledgerState {
	case domainDNSStatePending, domainDNSStateFailed, domainDNSStateCleanupPending, domainDNSStateCleanupFailed:
		return false
	}
	return true
}

func guardDefinitionDNSUnlinkedCleanupTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, progress, cleanup []byte, domainID string, run definitionApprovedRun) (bool, error) {
	var entries []integrations.DefinitionDNSProgress
	var approved integrations.DomainDNSPlan
	if json.Unmarshal(progress, &entries) != nil || entries == nil || len(entries) != 0 || json.Unmarshal(cleanup, &approved) != nil || approved.DomainID != domainID {
		return false, definitionDNSConflict()
	}
	if !definitionRunHasCleanup(run.Plan.DNSCleanup, approved) {
		return false, definitionDNSConflict()
	}
	verified, err := tx.DefinitionDNS().CleanupVerifiedWithoutLink(ctx, op.ID)
	if err != nil {
		return false, unavailable(err)
	}
	return verified, nil
}

func definitionDNSTerminalEntries(progress []byte) ([]integrations.DefinitionDNSProgress, error) {
	var entries []integrations.DefinitionDNSProgress
	if json.Unmarshal(progress, &entries) != nil || entries == nil {
		return nil, contract.NewFault(contract.Unavailable, "definition DNS recovery is unresolved", "Retain the child and parent reservation.")
	}
	return entries, nil
}

func guardDefinitionDNSPlanTerminalTx(plan []byte, entries []integrations.DefinitionDNSProgress, ledgerError, domainID string, run definitionApprovedRun) error {
	var approved integrations.DefinitionDNSPlan
	if json.Unmarshal(plan, &approved) != nil || validateDefinitionDNSProgress(approved, entries) != nil {
		return definitionDNSConflict()
	}
	if approved.DomainID != domainID || !definitionRunHasDNS(run.Plan.DNS, approved) {
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
	return nil
}

func guardDefinitionDNSCleanupTerminalTx(cleanup []byte, ledgerState, cleanupError, domainID string, run definitionApprovedRun) error {
	var approved integrations.DomainDNSPlan
	if json.Unmarshal(cleanup, &approved) != nil {
		return contract.NewFault(contract.Unavailable, "approved DNS cleanup outcome remains unknown", "Retain the child and parent reservation.")
	}
	if !definitionDNSCleanupResolved(approved, ledgerState, cleanupError) {
		return contract.NewFault(contract.Unavailable, "approved DNS cleanup outcome remains unknown", "Retain the child and parent reservation.")
	}
	if approved.DomainID != domainID || !definitionRunHasCleanup(run.Plan.DNSCleanup, approved) {
		return definitionDNSConflict()
	}
	return nil
}

func definitionDNSCleanupResolved(approved integrations.DomainDNSPlan, ledgerState, cleanupError string) bool {
	if len(approved.Changes) == 0 || ledgerState == domainDNSStateCleanupVerified {
		return true
	}
	return ledgerState == domainDNSStateCleanupFailed && definitionCleanupDefinitiveRejection(cleanupError, approved)
}

func definitionRunHasDNS(plans []integrations.DefinitionDNSPlan, approved integrations.DefinitionDNSPlan) bool {
	for _, candidate := range plans {
		if reflect.DeepEqual(candidate, approved) {
			return true
		}
	}
	return false
}

func definitionRunHasCleanup(plans []integrations.DomainDNSPlan, approved integrations.DomainDNSPlan) bool {
	for _, candidate := range plans {
		if reflect.DeepEqual(candidate, approved) {
			return true
		}
	}
	return false
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
func lockDefinitionDNSLedgerTx(ctx context.Context, tx *entities.StoreTx, domainID platform.DomainID, environmentID platform.EnvironmentID) (domainDNSLedger, error) {
	if _, found, err := tx.Platform().DomainDNS().LockLedger(ctx, domainID); err != nil {
		return domainDNSLedger{}, unavailable(err)
	} else if !found {
		return domainDNSLedger{}, definitionDNSConflict()
	}
	if locked, err := tx.Platform().Domains().LockByIDAndEnvironment(ctx, domainID, environmentID); err != nil {
		return domainDNSLedger{}, unavailable(err)
	} else if !locked {
		return domainDNSLedger{}, definitionDNSConflict()
	}
	ledger, found, err := tx.Platform().DomainDNS().GetByDomainID(ctx, domainID)
	if err != nil {
		return domainDNSLedger{}, unavailable(err)
	}
	if !found {
		return domainDNSLedger{}, definitionDNSConflict()
	}
	return domainDNSLedgerFromEntities(ledger), nil
}

// withLocked rechecks authority and the approved ledger before any progress
// access. The policy row is locked only until this transaction commits.
func (p *definitionDNSProgressStore) withLocked(ctx context.Context, fn func(*entities.StoreTx, *definitionDNSChild) error) error {
	if p == nil || p.service == nil || p.service.entities == nil || p.service.domainDNS == nil {
		return unavailable()
	}
	if err := p.service.domainDNS.AuthorizeWrite(ctx, p.child.Actor); err != nil {
		return err
	}
	err := p.service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		d, err := p.lockProgressChildTx(ctx, tx)
		if err != nil {
			return err
		}
		ledger, err := p.lockProgressLedgerTx(ctx, tx, d)
		if err != nil {
			return err
		}
		if err := p.validateProgressLedgerTx(ctx, tx, d, ledger); err != nil {
			return err
		}
		return fn(tx, d)
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	return nil
}

func (p *definitionDNSProgressStore) lockProgressChildTx(ctx context.Context, tx *entities.StoreTx) (*definitionDNSChild, error) {
	d, err := loadDefinitionDNSChildTxMode(ctx, tx, p.child.ID, p.recovery)
	if err != nil {
		return nil, err
	}
	if d == nil || d.ParentID != p.parentID || d.Ordinal != p.ordinal || d.Plan == nil || d.DomainID != p.ledger.DomainID {
		return nil, definitionDNSConflict()
	}
	currentOp, found, err := tx.Operations().Get(ctx, p.child.ID)
	if err != nil || !found {
		return nil, unavailable(err)
	}
	state, attempt, actor := currentOp.State, currentOp.Attempt, currentOp.Actor
	if actor != p.child.Actor || !definitionDNSChildRunnable(state, p.recovery) || (!p.recovery && d.ParentState == contract.Cancelling) || attempt != p.attempt {
		return nil, definitionDNSConflict()
	}
	return d, nil
}

func definitionDNSChildRunnable(state contract.State, recovery bool) bool {
	if recovery {
		return state == contract.Running || state == contract.Cancelling
	}
	return state == contract.Running
}

func (p *definitionDNSProgressStore) lockProgressLedgerTx(ctx context.Context, tx *entities.StoreTx, d *definitionDNSChild) (domainDNSLedger, error) {
	ledger, err := lockDefinitionDNSLedgerTx(ctx, tx, d.DomainID, d.EnvironmentID)
	if err != nil {
		return domainDNSLedger{}, err
	}
	environmentID, found, err := tx.Platform().Domains().EnvironmentIDByID(ctx, d.DomainID)
	if err != nil || !found {
		return domainDNSLedger{}, unavailable(err)
	}
	if environmentID != d.EnvironmentID {
		return domainDNSLedger{}, definitionDNSConflict()
	}
	return ledger, nil
}

func (p *definitionDNSProgressStore) validateProgressLedgerTx(ctx context.Context, tx *entities.StoreTx, d *definitionDNSChild, ledger domainDNSLedger) error {
	if ledger.Generation != p.ledger.Generation || ledger.LastOperationID != p.child.ID || !definitionDNSProgressLedgerMatches(d, ledger) {
		return definitionDNSConflict()
	}
	policy, err := lockedDefinitionDNSPolicy(ctx, tx)
	if err != nil {
		return err
	}
	if !definitionDNSProgressPolicyMatches(d, policy, ledger) {
		return definitionDNSConflict()
	}
	if err := lockDefinitionDNSCredentialTx(ctx, tx, d.Approved.Snapshot, ledger.ZoneID); err != nil {
		return err
	}
	return validateDefinitionDNSProgress(*d.Plan, d.Progress)
}

func definitionDNSProgressLedgerMatches(d *definitionDNSChild, ledger domainDNSLedger) bool {
	if ledger.App != d.Input.App || ledger.Environment != d.Input.Environment || ledger.Domain != d.Input.Domain || ledger.ZoneID != d.Plan.ZoneID || ledger.ZoneName != d.Plan.Zone || ledger.Ownership != d.Plan.Ownership || ledger.Provider != "cloudflare" {
		return false
	}
	return ledger.Ownership == integrations.DomainDNSOwnership(string(d.DomainID), ledger.ZoneID) && definitionDNSPlanMatchesLedger(*d.Plan, ledger)
}

func definitionDNSProgressPolicyMatches(d *definitionDNSChild, policy api.DefinitionDNSPolicy, ledger domainDNSLedger) bool {
	approvedPolicy := d.Approved.Snapshot.Policy
	return reflect.DeepEqual(policy, approvedPolicy) && policy.Enabled &&
		(policy.ProxyMode == "proxied" || policy.ProxyMode == "dns-only") &&
		policy.ZoneID == ledger.ZoneID && policy.ZoneName == ledger.ZoneName &&
		reflect.DeepEqual(policy.Origins, ledger.DesiredOrigins) &&
		(policy.ProxyMode == "proxied") == ledger.DesiredProxied &&
		d.Input.DNSMode == "cloudflare" && d.Input.DNSZone == ledger.ZoneID &&
		d.Input.DNSProxied != nil && *d.Input.DNSProxied == ledger.DesiredProxied &&
		reflect.DeepEqual(d.Input.DNSOrigins, ledger.DesiredOrigins)
}

func (p *definitionDNSProgressStore) Load(ctx context.Context) ([]integrations.DefinitionDNSProgress, error) {
	var entries []integrations.DefinitionDNSProgress
	err := p.withLocked(ctx, func(_ *entities.StoreTx, d *definitionDNSChild) error { entries = d.Progress; return nil })
	return entries, err
}

func (p *definitionDNSProgressStore) Guard(ctx context.Context) error {
	return p.withLocked(ctx, func(*entities.StoreTx, *definitionDNSChild) error { return nil })
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
	var step *definitionDNSChild
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		step, err = checkDefinitionDNSCleanupAuthorityTx(ctx, tx, op, attempt, ledger, expected, recovery)
		return err
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return nil, unavailable(err)
		}
		return nil, err
	}
	return step, nil
}

func checkDefinitionDNSCleanupAuthorityTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild, recovery bool) (*definitionDNSChild, error) {
	step, err := loadDefinitionDNSChildTxMode(ctx, tx, op.ID, recovery)
	if err != nil {
		return nil, err
	}
	if !definitionDNSCleanupChildMatches(step, expected, op, ledger) {
		return nil, definitionDNSConflict()
	}
	currentOp, found, err := tx.Operations().Get(ctx, op.ID)
	if err != nil || !found {
		return nil, unavailable(err)
	}
	state, currentAttempt := currentOp.State, currentOp.Attempt
	if !definitionDNSChildRunnable(state, recovery) || currentAttempt != attempt {
		return nil, definitionDNSConflict()
	}
	if state == contract.Running && step.ParentState == contract.Cancelling {
		return nil, definitionDNSConflict()
	}
	current, err := lockDefinitionDNSLedgerTx(ctx, tx, ledger.DomainID, step.EnvironmentID)
	if err != nil {
		return nil, err
	}
	environmentID, found, err := tx.Platform().Domains().EnvironmentIDByID(ctx, ledger.DomainID)
	if err != nil || !found {
		return nil, unavailable(err)
	}
	if environmentID != step.EnvironmentID || !definitionDNSCleanupLedgerMatches(step, ledger, current, op.ID) {
		return nil, definitionDNSConflict()
	}
	policy, err := lockedDefinitionDNSPolicy(ctx, tx)
	if err != nil {
		return nil, err
	}
	if policy.Revision != step.Approved.Snapshot.Policy.Revision {
		return nil, definitionDNSConflict()
	}
	if err := lockDefinitionDNSCredentialTx(ctx, tx, step.Approved.Snapshot, ledger.ZoneID); err != nil {
		return nil, err
	}
	return step, nil
}

func definitionDNSCleanupChildMatches(step, expected *definitionDNSChild, op contract.Operation, ledger domainDNSLedger) bool {
	if step == nil || step.ParentID != expected.ParentID || step.Ordinal != expected.Ordinal || step.Actor != op.Actor || step.Cleanup == nil || step.DomainID != ledger.DomainID {
		return false
	}
	return step.Cleanup.ZoneID == ledger.ZoneID && step.Cleanup.Zone == ledger.ZoneName &&
		step.Cleanup.Host == ledger.Domain && step.Cleanup.Ownership == ledger.Ownership &&
		ledger.Ownership == integrations.DomainDNSOwnership(string(ledger.DomainID), ledger.ZoneID)
}

func definitionDNSCleanupLedgerMatches(step *definitionDNSChild, ledger, current domainDNSLedger, opID contract.OperationID) bool {
	return current.LastOperationID == opID && current.Generation == ledger.Generation &&
		current.App == step.Input.App && current.Environment == step.Input.Environment &&
		current.Domain == ledger.Domain && current.ZoneID == ledger.ZoneID &&
		current.ZoneName == ledger.ZoneName && current.Ownership == ledger.Ownership &&
		current.Provider == "cloudflare"
}

func (p *definitionDNSProgressStore) Intent(ctx context.Context, entry integrations.DefinitionDNSProgress) error {
	return p.withLocked(ctx, func(tx *entities.StoreTx, d *definitionDNSChild) error {
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
	return p.withLocked(ctx, func(tx *entities.StoreTx, d *definitionDNSChild) error {
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

func saveDefinitionDNSProgress(ctx context.Context, tx *entities.StoreTx, d *definitionDNSChild) error {
	raw, err := json.Marshal(d.Progress)
	if err != nil || len(raw) > maxDefinitionDNSProgressBytes {
		return definitionDNSConflict()
	}
	saved, err := tx.DefinitionSteps().SaveDNSProgress(ctx, d.ParentID, d.Ordinal, raw, d.progressJSON, d.childID)
	if err != nil {
		return unavailable(err)
	}
	if !saved {
		return definitionDNSConflict()
	}
	return nil
}
