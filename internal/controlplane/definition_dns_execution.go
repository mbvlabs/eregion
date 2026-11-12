package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
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
	InputHash     string
	Progress      []integrations.DefinitionDNSProgress
	progressJSON  json.RawMessage
	childID       contract.OperationID
}

func definitionDNSConflict() error {
	return contract.NewFault(contract.Conflict, "definition DNS authorization or approved state changed", "Stop this child and obtain a new definition plan.")
}

// loadDefinitionDNSChildTx distinguishes an ordinary unlinked operation from
// a broken link. It locks journal and operation rows for the caller's short
// transaction. No parent attempt is pinned: a parent retry retains its child.
func loadDefinitionDNSChildTx(ctx context.Context, tx pgx.Tx, child contract.OperationID) (*definitionDNSChild, error) {
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
	if parentKind != "platform.definition.apply" || parentTarget != target || kind != opKind || target != opTarget || parentActor != childActor || finished || (parentState != contract.Running && parentState != contract.Queued) || (childState != contract.Running && childState != contract.Queued) {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable()
	}
	defer rollback(tx)
	step, err := loadDefinitionDNSChildTx(ctx, tx, child)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable()
	}
	return step, nil
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
	d, err := loadDefinitionDNSChildTx(ctx, tx, p.child.ID)
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
	if actor != p.child.Actor || state != contract.Running || attempt != p.attempt {
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
		step, err := s.checkDefinitionDNSCleanupAuthority(ctx, op, attempt, ledger, expected)
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

func (s *Service) checkDefinitionDNSCleanupAuthority(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, expected *definitionDNSChild) (*definitionDNSChild, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable()
	}
	defer rollback(tx)
	step, err := loadDefinitionDNSChildTx(ctx, tx, op.ID)
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
	if state != contract.Running || currentAttempt != attempt {
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
