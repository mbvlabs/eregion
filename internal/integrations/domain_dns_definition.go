package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// DefinitionDNSPlan is the complete, timestamp-free approval envelope for one
// hostname. The caller must bind its canonical digest to durable approval.
type DefinitionDNSPlan struct {
	Zone                 string            `json:"zone"`
	ZoneID               string            `json:"zone_id"`
	DomainID             string            `json:"domain_id"`
	Host                 string            `json:"host"`
	Ownership            string            `json:"ownership"`
	Before               []DomainDNSRecord `json:"before"`
	Desired              []DNSRecord       `json:"desired"`
	Changes              []DomainDNSChange `json:"changes"`
	ExternalSatisfaction []DomainDNSRecord `json:"external_satisfaction"`
	RequiresApproval     bool              `json:"requires_approval"`
	Warnings             []string          `json:"warnings"`
}

// DefinitionDNSProgress is durable evidence for one ordered change. Intent is
// saved before the provider request; Result is the exact observed hostname
// recordset after it. A nil Result means the request outcome is unknown.
type DefinitionDNSProgress struct {
	Index  int               `json:"index"`
	Before []DomainDNSRecord `json:"before"`
	// Expected is the approved delta to Before; a create has an unknown ID
	// until Result is observed. Together they define the expected post-state.
	Expected DomainDNSChange   `json:"expected"`
	Result   []DomainDNSRecord `json:"result"`
}

// DefinitionDNSProgressStore must durably and atomically persist each call.
// Load returns a contiguous ordered prefix, with at most its last entry pending.
// Implementations fence the operation/attempt and bind it to the approved plan.
// Guard checks cancellation and newest intent immediately before each request.
type DefinitionDNSProgressStore interface {
	Load(context.Context) ([]DefinitionDNSProgress, error)
	Intent(context.Context, DefinitionDNSProgress) error
	Observed(context.Context, DefinitionDNSProgress) error
	Guard(context.Context) error
}

func definitionRecords(ctx context.Context, client *Cloudflare, host, marker string) ([]DomainDNSRecord, error) {
	out := []DomainDNSRecord{}
	seen := map[string]bool{}
	complete := false
	for page := 1; page <= 100; page++ {
		endpoint := fmt.Sprintf("%s%s/dns_records?name=%s&per_page=100&page=%d", client.base, client.zonePath(), url.QueryEscape(host), page)
		var records []DNSRecord
		var info struct {
			TotalPages int `json:"total_pages"`
		}
		if err := client.requestWithInfo(ctx, "GET", endpoint, nil, &records, &info); err != nil {
			return nil, err
		}
		if records == nil {
			return nil, cloudflareFailure("invalid-response", 0, "Retry the read before planning DNS changes.")
		}
		if info.TotalPages > 100 {
			return nil, cloudflareFailure("invalid-response", 0, "The hostname recordset exceeds the read limit; review provider state before planning.")
		}
		pageRecords, err := definitionPageRecords(records, host, marker, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, pageRecords...)
		if (info.TotalPages > 0 && page >= info.TotalPages) || (info.TotalPages == 0 && len(records) < 100) {
			complete = true
			break
		}
	}
	if !complete {
		return nil, cloudflareFailure("invalid-response", 0, "The hostname recordset exceeds the read limit; review provider state before planning.")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func definitionPageRecords(records []DNSRecord, host, marker string, seen map[string]bool) ([]DomainDNSRecord, error) {
	var out []DomainDNSRecord
	for _, r := range records {
		if !hostnameEqual(r.Name, host) {
			continue
		}
		t := strings.ToUpper(r.Type)
		if t != "A" && t != "AAAA" && t != "CNAME" {
			continue
		}
		if r.ID == "" || seen[r.ID] {
			return nil, cloudflareFailure("invalid-response", 0, "Retry the read before planning DNS changes.")
		}
		seen[r.ID] = true
		owner := "foreign"
		if t != "CNAME" && r.Comment == marker {
			owner = "owned"
		}
		out = append(out, DomainDNSRecord{ID: r.ID, Name: r.Name, Type: t, Content: r.Content, TTL: r.TTL, Proxied: r.Proxied, Comment: r.Comment, Ownership: owner})
	}
	return out, nil
}

func definitionPlan(zone DNSZone, req DomainDNSRequest, before []DomainDNSRecord) (DefinitionDNSPlan, error) {
	host, desired, marker, err := domainDNSDesired(zone, req)
	if err != nil {
		return DefinitionDNSPlan{}, err
	}
	plan := DefinitionDNSPlan{Zone: zone.ZoneName, ZoneID: zone.ZoneID, DomainID: req.DomainID, Host: host, Ownership: marker, Before: before, Desired: desired, Changes: []DomainDNSChange{}, ExternalSatisfaction: []DomainDNSRecord{}, Warnings: []string{
		"Matching foreign records remain unowned and will not be removed by managed cleanup.",
		"Cloudflare has no conditional-write API; an external writer can race the final request.",
		"DNS provider state is not route, TLS or public readiness.",
	}}
	if err := definitionProxyConflicts(desired, before); err != nil {
		return DefinitionDNSPlan{}, err
	}
	planner := definitionPlanner{plan: &plan, before: before, desired: desired, used: make([]bool, len(before))}
	planner.markExternalSatisfaction()
	for _, want := range desired {
		if err := planner.planDesired(want); err != nil {
			return DefinitionDNSPlan{}, err
		}
	}
	planner.planObsolete()
	sortDefinitionChanges(plan.Changes)
	return plan, nil
}

func definitionProxyConflicts(desired []DNSRecord, before []DomainDNSRecord) error {
	for _, want := range desired {
		count, conflict := 0, false
		for _, r := range before {
			if r.Type == want.Type && domainDNSContentEqual(r.Type, r.Content, want.Content) {
				count++
				conflict = conflict || (r.Ownership == "foreign" && r.Proxied != want.Proxied)
			}
		}
		if conflict && count > 1 {
			return contract.NewFault(contract.PreconditionFailed, "ambiguous DNS proxy conflict", "Resolve duplicate records before obtaining a new definition plan.")
		}
	}
	return nil
}

type definitionPlanner struct {
	plan    *DefinitionDNSPlan
	before  []DomainDNSRecord
	desired []DNSRecord
	used    []bool
}

// markExternalSatisfaction records every matching foreign record as external,
// even when an owned record or another foreign duplicate already satisfies the
// same address.
func (p *definitionPlanner) markExternalSatisfaction() {
	for i, r := range p.before {
		if r.Ownership != "foreign" || (r.Type != "A" && r.Type != "AAAA") {
			continue
		}
		for _, want := range p.desired {
			if r.Type == want.Type && r.Proxied == want.Proxied && domainDNSContentEqual(r.Type, r.Content, want.Content) {
				p.used[i] = true
				p.plan.ExternalSatisfaction = append(p.plan.ExternalSatisfaction, r)
				break
			}
		}
	}
}

func (p *definitionPlanner) planDesired(want DNSRecord) error {
	found := p.ownedMatch(want)
	if found < 0 {
		if p.externallySatisfied(want) {
			return nil
		}
		return p.planUnmatchedDesired(want)
	}
	p.used[found] = true
	r := p.before[found]
	if r.TTL != want.TTL || r.Proxied != want.Proxied || r.Comment != want.Comment {
		a := want
		a.ID = r.ID
		b := r
		p.plan.Changes = append(p.plan.Changes, DomainDNSChange{Action: "update", Type: want.Type, Before: &b, After: &a})
		p.plan.RequiresApproval = true
	}
	return nil
}

func (p *definitionPlanner) ownedMatch(want DNSRecord) int {
	for i, r := range p.before {
		if !p.used[i] && r.Ownership == "owned" && r.Type == want.Type && domainDNSContentEqual(r.Type, r.Content, want.Content) {
			return i
		}
	}
	return -1
}

func (p *definitionPlanner) externallySatisfied(want DNSRecord) bool {
	for _, r := range p.plan.ExternalSatisfaction {
		if r.Type == want.Type && domainDNSContentEqual(r.Type, r.Content, want.Content) {
			return true
		}
	}
	return false
}

func (p *definitionPlanner) planUnmatchedDesired(want DNSRecord) error {
	// A proxy-mode conflict for the same address cannot be created alongside
	// the old Cloudflare record. Only a unique candidate may be transferred,
	// and the concrete before/after requires approval.
	candidate, count := p.proxyConflictCandidate(want)
	if count > 1 {
		return contract.NewFault(contract.PreconditionFailed, "ambiguous DNS proxy conflict", "Resolve duplicate records before obtaining a new definition plan.")
	}
	if count == 1 && p.before[candidate].Ownership == "foreign" && p.before[candidate].Proxied != want.Proxied {
		r := p.before[candidate]
		a := want
		a.ID, a.Name, a.Content, a.TTL = r.ID, r.Name, r.Content, r.TTL
		if a.Proxied {
			// Cloudflare requires automatic TTL for proxied records.
			a.TTL = 1
		}
		p.plan.Changes = append(p.plan.Changes, DomainDNSChange{Action: "update", Type: want.Type, Before: &r, After: &a})
		p.plan.RequiresApproval = true
		p.plan.Warnings = append(p.plan.Warnings, "The proxy-conflicting foreign record will be updated in place and transferred to this binding; managed cleanup may later delete it.")
		if r.TTL != a.TTL {
			p.plan.Warnings = append(p.plan.Warnings, "Proxying changes the record TTL to Cloudflare automatic TTL.")
		}
		p.used[candidate] = true
		return nil
	}
	a := want
	p.plan.Changes = append(p.plan.Changes, DomainDNSChange{Action: "create", Type: want.Type, After: &a})
	return nil
}

func (p *definitionPlanner) proxyConflictCandidate(want DNSRecord) (int, int) {
	candidate, count := -1, 0
	for i, r := range p.before {
		if r.Type == want.Type && domainDNSContentEqual(r.Type, r.Content, want.Content) {
			candidate, count = i, count+1
		}
	}
	return candidate, count
}

func (p *definitionPlanner) planObsolete() {
	for i, r := range p.before {
		if p.used[i] {
			continue
		}
		b := r
		p.plan.Changes = append(p.plan.Changes, DomainDNSChange{Action: "delete", Type: r.Type, Before: &b})
		p.plan.RequiresApproval = true
		if r.Type == "CNAME" {
			p.plan.Warnings = append(p.plan.Warnings, "Replacing a CNAME requires deleting it before creating addresses; DNS can be unavailable during this gap and recovery must resume the approved plan.")
		}
	}
}

// sortDefinitionChanges orders CNAME deletions first and other obsolete
// addresses last.
func sortDefinitionChanges(changes []DomainDNSChange) {
	sort.SliceStable(changes, func(i, j int) bool {
		return definitionChangeRank(changes[i]) < definitionChangeRank(changes[j])
	})
}

func definitionChangeRank(change DomainDNSChange) int {
	if change.Action == "delete" && change.Type == "CNAME" {
		return 0
	}
	if change.Action == "delete" {
		return 2
	}
	return 1
}

// PlanDefinitionApply authorizes read access before resolving credentials and
// reads only A/AAAA/CNAME at the exact hostname.
func (d *DomainDNS) PlanDefinitionApply(ctx context.Context, actor contract.Actor, req DomainDNSRequest) (DefinitionDNSPlan, error) {
	if err := d.AuthorizeRead(ctx, actor); err != nil {
		return DefinitionDNSPlan{}, err
	}
	zone, client, err := d.clientFor(ctx, req.ZoneID, req.ZoneName)
	if err != nil {
		return DefinitionDNSPlan{}, err
	}
	host, _, marker, err := domainDNSDesired(zone, req)
	if err != nil {
		return DefinitionDNSPlan{}, err
	}
	before, err := definitionRecords(ctx, client, host, marker)
	if err != nil {
		return DefinitionDNSPlan{}, err
	}
	return definitionPlan(zone, req, before)
}

func definitionDrift() error {
	return contract.NewFault(contract.PreconditionFailed, "approved DNS state changed", "Stop and obtain a new plan; do not infer a missing record from a failed read.")
}

// definitionPost recognizes exactly one approved transition, including a create
// whose provider-assigned ID was not returned because the response was lost.
func definitionPost(before, after []DomainDNSRecord, change DomainDNSChange, marker string) bool {
	expected := append([]DomainDNSRecord{}, before...)
	if change.Before != nil {
		index := definitionExpectedIndex(expected, *change.Before)
		if index < 0 {
			return false
		}
		expected = append(expected[:index], expected[index+1:]...)
	}
	if change.After != nil {
		matches := definitionPostMatches(change.After, after)
		added := definitionPostAdded(matches, expected)
		// The matching record must be new relative to the pre-state, not an
		// arbitrary pre-existing owned record or an ambiguous duplicate.
		if len(added) != 1 || added[0].Ownership != "owned" || added[0].Comment != marker {
			return false
		}
		expected = append(expected, added[0])
	}
	definitionSortRecords(expected)
	return reflect.DeepEqual(expected, after)
}

func definitionExpectedIndex(expected []DomainDNSRecord, before DomainDNSRecord) int {
	for i, r := range expected {
		if reflect.DeepEqual(r, before) {
			return i
		}
	}
	return -1
}

func definitionPostMatches(a *DNSRecord, after []DomainDNSRecord) []DomainDNSRecord {
	var matches []DomainDNSRecord
	for _, r := range after {
		if r.Name == a.Name && r.Type == a.Type && r.Content == a.Content && r.TTL == a.TTL && r.Proxied == a.Proxied && r.Comment == a.Comment && (a.ID == "" || a.ID == r.ID) {
			matches = append(matches, r)
		}
	}
	return matches
}

func definitionPostAdded(matches, expected []DomainDNSRecord) []DomainDNSRecord {
	var added []DomainDNSRecord
	for _, r := range matches {
		exists := false
		for _, old := range expected {
			if old.ID == r.ID {
				exists = true
				break
			}
		}
		if !exists {
			added = append(added, r)
		}
	}
	return added
}

func definitionSortRecords(records []DomainDNSRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].Type != records[j].Type {
			return records[i].Type < records[j].Type
		}
		return records[i].ID < records[j].ID
	})
}

// DefinitionDNSObserved verifies the exact approved transition in durable
// progress; an arbitrary non-nil result is not recovery evidence.
func DefinitionDNSObserved(entry DefinitionDNSProgress, marker string) bool {
	return entry.Result != nil && definitionPost(entry.Before, entry.Result, entry.Expected, marker)
}

// ExecuteApprovedDefinitionDNS executes only the supplied, independently
// approved plan. No fresh plan can enlarge its authority. Progress must be
// durable; there is deliberately no in-memory fallback.
func (d *DomainDNS) ExecuteApprovedDefinitionDNS(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest, plan DefinitionDNSPlan, progress DefinitionDNSProgressStore) (int, error) {
	if progress == nil || d == nil || d.audit == nil {
		return 0, contract.NewFault(contract.Unavailable, "DNS progress or audit is not configured", "Durable progress and audit are required before provider effects.")
	}
	if err := d.AuthorizeWrite(ctx, actor); err != nil {
		return 0, err
	}
	if err := opID.Validate(); err != nil {
		return 0, err
	}
	zone, client, err := d.clientFor(ctx, req.ZoneID, req.ZoneName)
	if err != nil {
		return 0, err
	}
	canonical, err := definitionPlan(zone, req, plan.Before)
	if err != nil {
		return 0, err
	}
	if !reflect.DeepEqual(canonical, plan) {
		return 0, definitionDrift()
	}
	entries, err := progress.Load(ctx)
	if err != nil {
		return 0, err
	}
	if len(entries) > len(plan.Changes) {
		return 0, definitionDrift()
	}
	failedIndex, expected, err := verifyDefinitionProgress(entries, plan)
	if err != nil {
		return failedIndex, err
	}
	current, err := definitionReadCurrent(ctx, client, plan, entries)
	if err != nil {
		return 0, err
	}
	execution := definitionExecution{
		d: d, ctx: ctx, actor: actor, opID: opID,
		zone: zone, client: client, plan: plan, progress: progress,
	}
	start, expected, err := execution.resolvePending(entries, current, expected)
	if err != nil {
		return start, err
	}
	for i := start; i < len(plan.Changes); i++ {
		expected, err = execution.executeChange(i, entries, expected)
		if err != nil {
			return i, err
		}
	}
	return len(plan.Changes), nil
}

// definitionReadCurrent reads the exact hostname state. A pending write makes a
// read rejection an unknown outcome: a deterministic provider error cannot
// settle it without a successful exact-record readback.
func definitionReadCurrent(ctx context.Context, client *Cloudflare, plan DefinitionDNSPlan, entries []DefinitionDNSProgress) ([]DomainDNSRecord, error) {
	current, err := definitionRecords(ctx, client, plan.Host, plan.Ownership)
	if err != nil {
		if len(entries) > 0 && entries[len(entries)-1].Result == nil {
			var failure *ProviderError
			if errors.As(err, &failure) {
				return nil, cloudflareFailure(failure.Category, failure.Status, "Restore provider read access and reconcile the pending write before deciding its outcome.")
			}
		}
		return nil, err
	}
	return current, nil
}

type definitionExecution struct {
	d        *DomainDNS
	ctx      context.Context
	actor    contract.Actor
	opID     contract.OperationID
	zone     DNSZone
	client   *Cloudflare
	plan     DefinitionDNSPlan
	progress DefinitionDNSProgressStore
}

// verifyDefinitionProgress checks that the durable prefix is contiguous and
// describes exactly the approved changes. It returns the index of a mismatch.
func verifyDefinitionProgress(entries []DefinitionDNSProgress, plan DefinitionDNSPlan) (int, []DomainDNSRecord, error) {
	expected := plan.Before
	for i, entry := range entries {
		if entry.Index != i || !reflect.DeepEqual(entry.Before, expected) || !reflect.DeepEqual(entry.Expected, plan.Changes[i]) {
			return i, nil, definitionDrift()
		}
		if entry.Result == nil {
			if i != len(entries)-1 {
				return i, nil, definitionDrift()
			}
			break
		}
		if !definitionPost(entry.Before, entry.Result, plan.Changes[i], plan.Ownership) {
			return i, nil, definitionDrift()
		}
		expected = entry.Result
	}
	return 0, expected, nil
}

// resolvePending settles the single trailing intent by observing its exact
// approved post-state.
func (x *definitionExecution) resolvePending(entries []DefinitionDNSProgress, current, expected []DomainDNSRecord) (int, []DomainDNSRecord, error) {
	start := len(entries)
	if start == 0 || entries[start-1].Result != nil {
		if !reflect.DeepEqual(current, expected) {
			return start, expected, definitionDrift()
		}
		return start, expected, nil
	}
	return x.resolvePendingEntry(start, entries[start-1], current)
}

func (x *definitionExecution) resolvePendingEntry(start int, entry DefinitionDNSProgress, current []DomainDNSRecord) (int, []DomainDNSRecord, error) {
	change := x.plan.Changes[start-1]
	if definitionPost(entry.Before, current, change, x.plan.Ownership) {
		entry.Result = current
		action, target := domainDNSChangeTarget(x.plan.Host, change)
		if err := x.d.audit.Record(x.ctx, AuditEntry{At: x.d.now().UTC(), Actor: x.actor.Subject, Action: action, Target: target, Outcome: "succeeded", Detail: auditDetail(x.opID, "recovered observed result")}); err != nil {
			return start - 1, nil, err
		}
		if err := x.progress.Observed(x.ctx, entry); err != nil {
			return start - 1, nil, err
		}
		return start, current, nil
	}
	if reflect.DeepEqual(current, entry.Before) {
		return start - 1, nil, contract.NewFault(contract.Unavailable, "approved DNS write outcome remains unknown", "Keep the operation and reservation; retry read-only reconciliation.")
	}
	return start - 1, nil, definitionDrift()
}

func (x *definitionExecution) executeChange(index int, entries []DefinitionDNSProgress, expected []DomainDNSRecord) ([]DomainDNSRecord, error) {
	change := x.plan.Changes[index]
	current, err := definitionRecords(x.ctx, x.client, x.plan.Host, x.plan.Ownership)
	if err != nil {
		return expected, err
	}
	if !reflect.DeepEqual(current, expected) {
		return expected, definitionDrift()
	}
	action, target := domainDNSChangeTarget(x.plan.Host, change)
	if err = x.d.authz.Authorize(x.ctx, x.actor, action, target); err != nil {
		_ = x.d.audit.Record(x.ctx, AuditEntry{At: x.d.now().UTC(), Actor: x.actor.Subject, Action: action, Target: target, Outcome: "denied", Detail: auditDetail(x.opID, "authorization denied")})
		return expected, err
	}
	if err = x.d.audit.Record(x.ctx, AuditEntry{At: x.d.now().UTC(), Actor: x.actor.Subject, Action: action, Target: target, Outcome: "intent", Detail: auditDetail(x.opID, "zone="+x.zone.ZoneName)}); err != nil {
		return expected, err
	}
	entry := DefinitionDNSProgress{Index: index, Before: expected, Expected: change}
	if index >= len(entries) {
		if err = x.progress.Intent(x.ctx, entry); err != nil {
			return expected, err
		}
	}
	if err = x.progress.Guard(x.ctx); err != nil {
		return expected, err
	}
	if err = x.apply(change); err != nil {
		_ = x.d.audit.Record(x.ctx, AuditEntry{At: x.d.now().UTC(), Actor: x.actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(x.opID, "provider outcome unknown")})
		var failure *ProviderError
		if errors.As(err, &failure) && (contract.Classify(err).Code == contract.Forbidden || contract.Classify(err).Code == contract.PreconditionFailed) {
			copy := *failure
			copy.DefinitiveWrite = true
			return expected, &copy
		}
		return expected, err
	}
	current, err = definitionRecords(x.ctx, x.client, x.plan.Host, x.plan.Ownership)
	if err != nil {
		_ = x.d.audit.Record(x.ctx, AuditEntry{At: x.d.now().UTC(), Actor: x.actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(x.opID, "provider result unverified")})
		var failure *ProviderError
		if errors.As(err, &failure) {
			return expected, cloudflareFailure(failure.Category, failure.Status, "Restore provider read access and reconcile the pending write before deciding its outcome.")
		}
		return expected, err
	}
	if !definitionPost(expected, current, change, x.plan.Ownership) {
		_ = x.d.audit.Record(x.ctx, AuditEntry{At: x.d.now().UTC(), Actor: x.actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(x.opID, "provider result differs from approved transition")})
		return expected, definitionDrift()
	}
	entry.Result = current
	if err = x.d.audit.Record(x.ctx, AuditEntry{At: x.d.now().UTC(), Actor: x.actor.Subject, Action: action, Target: target, Outcome: "succeeded", Detail: auditDetail(x.opID, "zone="+x.zone.ZoneName)}); err != nil {
		return expected, fmt.Errorf("DNS effect observed but audit failed: %w", err)
	}
	if err = x.progress.Observed(x.ctx, entry); err != nil {
		return expected, err
	}
	return current, nil
}

func (x *definitionExecution) apply(change DomainDNSChange) error {
	if change.Action == "update" && change.Before != nil && change.Before.Ownership == "foreign" {
		return x.client.patchDefinitionProxy(x.ctx, change)
	}
	return x.client.apply(x.ctx, domainDNSChangeToDNSChange(change))
}

// ResolveApprovedDefinitionDNS performs no mutations. A pending intent is
// resolved only by observing its exact approved post-state; a pre-state read
// cannot exclude a delayed provider effect.
func (d *DomainDNS) ResolveApprovedDefinitionDNS(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest, plan DefinitionDNSPlan, progress DefinitionDNSProgressStore) error {
	if d == nil || progress == nil || d.audit == nil {
		return contract.NewFault(contract.Unavailable, "DNS recovery is unavailable", "Retain the operation and retry read-only recovery.")
	}
	if err := d.AuthorizeWrite(ctx, actor); err != nil {
		return err
	}
	zone, client, err := d.clientFor(ctx, req.ZoneID, req.ZoneName)
	if err != nil {
		return err
	}
	canonical, err := definitionPlan(zone, req, plan.Before)
	if err != nil || !reflect.DeepEqual(canonical, plan) {
		return definitionDrift()
	}
	entries, err := progress.Load(ctx)
	if err != nil {
		return err
	}
	if len(entries) > len(plan.Changes) {
		return definitionDrift()
	}
	if _, _, err := verifyDefinitionProgress(entries, plan); err != nil {
		return err
	}
	if len(entries) == 0 || entries[len(entries)-1].Result != nil {
		return nil
	}
	return resolveDefinitionPending(ctx, client, progress, plan, entries[len(entries)-1])
}

// resolveDefinitionPending resolves the trailing intent only by observing its
// exact approved post-state; a pre-state read cannot exclude a delayed effect.
func resolveDefinitionPending(ctx context.Context, client *Cloudflare, progress DefinitionDNSProgressStore, plan DefinitionDNSPlan, entry DefinitionDNSProgress) error {
	if err := progress.Guard(ctx); err != nil {
		return err
	}
	current, err := definitionRecords(ctx, client, plan.Host, plan.Ownership)
	if err != nil {
		return contract.NewFault(contract.Unavailable, "approved DNS write outcome remains unknown", "Restore provider read access; the reservation remains held.")
	}
	if !definitionPost(entry.Before, current, entry.Expected, plan.Ownership) {
		return contract.NewFault(contract.Unavailable, "approved DNS write outcome remains unknown", "Keep the reservation and retry read-only recovery; no provider write will be repeated.")
	}
	entry.Result = current
	return progress.Observed(ctx, entry)
}
