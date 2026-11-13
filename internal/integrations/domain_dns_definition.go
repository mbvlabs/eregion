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
			return nil, cloudflareFailure("invalid-response", 0, contract.Unavailable, "Retry the read before planning DNS changes.")
		}
		if info.TotalPages > 100 {
			return nil, cloudflareFailure("invalid-response", 0, contract.Unavailable, "The hostname recordset exceeds the read limit; review provider state before planning.")
		}
		for _, r := range records {
			if !hostnameEqual(r.Name, host) {
				continue
			}
			t := strings.ToUpper(r.Type)
			if t != "A" && t != "AAAA" && t != "CNAME" {
				continue
			}
			if r.ID == "" || seen[r.ID] {
				return nil, cloudflareFailure("invalid-response", 0, contract.Unavailable, "Retry the read before planning DNS changes.")
			}
			seen[r.ID] = true
			owner := "foreign"
			if t != "CNAME" && r.Comment == marker {
				owner = "owned"
			}
			out = append(out, DomainDNSRecord{ID: r.ID, Name: r.Name, Type: t, Content: r.Content, TTL: r.TTL, Proxied: r.Proxied, Comment: r.Comment, Ownership: owner})
		}
		if (info.TotalPages > 0 && page >= info.TotalPages) || (info.TotalPages == 0 && len(records) < 100) {
			complete = true
			break
		}
	}
	if !complete {
		return nil, cloudflareFailure("invalid-response", 0, contract.Unavailable, "The hostname recordset exceeds the read limit; review provider state before planning.")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func definitionPlan(zone DNSZone, req DomainDNSRequest, before []DomainDNSRecord) (DefinitionDNSPlan, error) {
	host, desired, marker, err := domainDNSDesired(zone, req)
	if err != nil {
		return DefinitionDNSPlan{}, err
	}
	p := DefinitionDNSPlan{Zone: zone.ZoneName, ZoneID: zone.ZoneID, DomainID: req.DomainID, Host: host, Ownership: marker, Before: before, Desired: desired, Changes: []DomainDNSChange{}, ExternalSatisfaction: []DomainDNSRecord{}, Warnings: []string{
		"Matching foreign records remain unowned and will not be removed by managed cleanup.",
		"Cloudflare has no conditional-write API; an external writer can race the final request.",
		"DNS provider state is not route, TLS or public readiness.",
	}}
	used := make([]bool, len(before))
	// Every matching foreign record remains external, even when an owned
	// record or another foreign duplicate already satisfies the same address.
	for i, r := range before {
		if r.Ownership != "foreign" || (r.Type != "A" && r.Type != "AAAA") {
			continue
		}
		for _, want := range desired {
			if r.Type == want.Type && r.Proxied == want.Proxied && domainDNSContentEqual(r.Type, r.Content, want.Content) {
				used[i] = true
				p.ExternalSatisfaction = append(p.ExternalSatisfaction, r)
				break
			}
		}
	}
	// Prefer an owned match; otherwise any preserved foreign match satisfies
	// the desired address without adoption or a new record.
	for _, want := range desired {
		found := -1
		for i, r := range before {
			if !used[i] && r.Ownership == "owned" && r.Type == want.Type && domainDNSContentEqual(r.Type, r.Content, want.Content) {
				found = i
				break
			}
		}
		if found < 0 {
			satisfied := false
			for _, r := range p.ExternalSatisfaction {
				if r.Type == want.Type && domainDNSContentEqual(r.Type, r.Content, want.Content) {
					satisfied = true
					break
				}
			}
			if satisfied {
				continue
			}
			a := want
			p.Changes = append(p.Changes, DomainDNSChange{Action: "create", Type: want.Type, After: &a})
			continue
		}
		used[found] = true
		r := before[found]
		if r.TTL != want.TTL || r.Proxied != want.Proxied || r.Comment != want.Comment {
			a := want
			a.ID = r.ID
			b := r
			p.Changes = append(p.Changes, DomainDNSChange{Action: "update", Type: want.Type, Before: &b, After: &a})
			p.RequiresApproval = true
		}
	}
	for i, r := range before {
		if used[i] {
			continue
		}
		b := r
		p.Changes = append(p.Changes, DomainDNSChange{Action: "delete", Type: r.Type, Before: &b})
		p.RequiresApproval = true
		if r.Type == "CNAME" {
			p.Warnings = append(p.Warnings, "Replacing a CNAME requires deleting it before creating addresses; DNS can be unavailable during this gap and recovery must resume the approved plan.")
		}
	}
	// CNAMEs must go first; other obsolete addresses go last.
	sort.SliceStable(p.Changes, func(i, j int) bool {
		rank := func(c DomainDNSChange) int {
			if c.Action == "delete" && c.Type == "CNAME" {
				return 0
			}
			if c.Action == "delete" {
				return 2
			}
			return 1
		}
		return rank(p.Changes[i]) < rank(p.Changes[j])
	})
	return p, nil
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
		index := -1
		for i, r := range expected {
			if reflect.DeepEqual(r, *change.Before) {
				index = i
				break
			}
		}
		if index < 0 {
			return false
		}
		expected = append(expected[:index], expected[index+1:]...)
	}
	if change.After != nil {
		a := change.After
		var matches []DomainDNSRecord
		for _, r := range after {
			if r.Name == a.Name && r.Type == a.Type && r.Content == a.Content && r.TTL == a.TTL && r.Proxied == a.Proxied && r.Comment == a.Comment && (a.ID == "" || a.ID == r.ID) {
				matches = append(matches, r)
			}
		}
		// The matching record must be new relative to the pre-state, not an
		// arbitrary pre-existing owned record or an ambiguous duplicate.
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
		if len(added) != 1 || added[0].Ownership != "owned" || added[0].Comment != marker {
			return false
		}
		expected = append(expected, added[0])
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].Type != expected[j].Type {
			return expected[i].Type < expected[j].Type
		}
		return expected[i].ID < expected[j].ID
	})
	return reflect.DeepEqual(expected, after)
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
	expected := plan.Before
	for i, e := range entries {
		if e.Index != i || !reflect.DeepEqual(e.Before, expected) || !reflect.DeepEqual(e.Expected, plan.Changes[i]) {
			return i, definitionDrift()
		}
		if e.Result == nil {
			if i != len(entries)-1 {
				return i, definitionDrift()
			}
			break
		}
		if !definitionPost(e.Before, e.Result, plan.Changes[i], plan.Ownership) {
			return i, definitionDrift()
		}
		expected = e.Result
	}
	current, err := definitionRecords(ctx, client, plan.Host, plan.Ownership)
	if err != nil {
		// A previous request may have taken effect. Even a deterministic read
		// rejection cannot settle that unknown write without a successful
		// exact-record readback.
		if len(entries) > 0 && entries[len(entries)-1].Result == nil {
			var failure *ProviderFailure
			if errors.As(err, &failure) {
				return 0, cloudflareFailure(failure.Category, failure.Status, contract.Unavailable, "Restore provider read access and reconcile the pending write before deciding its outcome.")
			}
		}
		return 0, err
	}
	start := len(entries)
	if start > 0 && entries[start-1].Result == nil {
		e := entries[start-1]
		c := plan.Changes[start-1]
		if definitionPost(e.Before, current, c, plan.Ownership) {
			e.Result = current
			action, target := domainDNSChangeTarget(plan.Host, c)
			if err := d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "succeeded", Detail: auditDetail(opID, "recovered observed result")}); err != nil {
				return start - 1, err
			}
			if err := progress.Observed(ctx, e); err != nil {
				return start - 1, err
			}
			expected = current
		} else if reflect.DeepEqual(current, e.Before) {
			return start - 1, contract.NewFault(contract.Unavailable, "approved DNS write outcome remains unknown", "Keep the operation and reservation; retry read-only reconciliation.")
		} else {
			return start - 1, definitionDrift()
		}
	} else if !reflect.DeepEqual(current, expected) {
		return start, definitionDrift()
	}
	for i := start; i < len(plan.Changes); i++ {
		change := plan.Changes[i]
		current, err = definitionRecords(ctx, client, plan.Host, plan.Ownership)
		if err != nil {
			return i, err
		}
		if !reflect.DeepEqual(current, expected) {
			return i, definitionDrift()
		}
		action, target := domainDNSChangeTarget(plan.Host, change)
		if err = d.authz.Authorize(ctx, actor, action, target); err != nil {
			_ = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "denied", Detail: auditDetail(opID, "authorization denied")})
			return i, err
		}
		if err = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "intent", Detail: auditDetail(opID, "zone="+zone.ZoneName)}); err != nil {
			return i, err
		}
		e := DefinitionDNSProgress{Index: i, Before: expected, Expected: change}
		if i >= len(entries) {
			if err = progress.Intent(ctx, e); err != nil {
				return i, err
			}
		}
		if err = progress.Guard(ctx); err != nil {
			return i, err
		}
		if err = client.apply(ctx, domainDNSChangeToDNSChange(change)); err != nil {
			_ = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(opID, "provider outcome unknown")})
			var failure *ProviderFailure
			if errors.As(err, &failure) && (contract.Classify(err).Code == contract.Forbidden || contract.Classify(err).Code == contract.PreconditionFailed) {
				copy := *failure
				copy.DefinitiveWrite = true
				return i, &copy
			}
			return i, err
		}
		current, err = definitionRecords(ctx, client, plan.Host, plan.Ownership)
		if err != nil {
			_ = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(opID, "provider result unverified")})
			var failure *ProviderFailure
			if errors.As(err, &failure) {
				return i, cloudflareFailure(failure.Category, failure.Status, contract.Unavailable, "Restore provider read access and reconcile the pending write before deciding its outcome.")
			}
			return i, err
		}
		if !definitionPost(expected, current, change, plan.Ownership) {
			_ = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(opID, "provider result differs from approved transition")})
			return i, definitionDrift()
		}
		e.Result = current
		if err = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "succeeded", Detail: auditDetail(opID, "zone="+zone.ZoneName)}); err != nil {
			return i, fmt.Errorf("DNS effect observed but audit failed: %w", err)
		}
		if err = progress.Observed(ctx, e); err != nil {
			return i, err
		}
		expected = current
	}
	return len(plan.Changes), nil
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
	expected := plan.Before
	for i, entry := range entries {
		if entry.Index != i || !reflect.DeepEqual(entry.Before, expected) || !reflect.DeepEqual(entry.Expected, plan.Changes[i]) {
			return definitionDrift()
		}
		if entry.Result == nil {
			if i != len(entries)-1 {
				return definitionDrift()
			}
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
			if err := progress.Observed(ctx, entry); err != nil {
				return err
			}
			return nil
		}
		if !definitionPost(entry.Before, entry.Result, entry.Expected, plan.Ownership) {
			return definitionDrift()
		}
		expected = entry.Result
	}
	return nil
}
