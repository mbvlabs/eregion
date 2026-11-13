package integrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	DomainDNSModeManual     = "manual"
	DomainDNSModeCloudflare = "cloudflare"

	domainDNSOwnershipPrefix = "eregion:dns:"
)

// DomainDNSConfig wires the domain-scoped Cloudflare flow. Unlike the generic
// zone reconciler, this flow reads and writes only the named hostname and only
// records whose comment is this binding's exact ownership identity. Zones and
// credentials are resolved from PostgreSQL on every operation; there is no
// environment or file configuration.
type DomainDNSConfig struct {
	Zones       DNSZoneSource
	Credentials DNSCredentialSource
	BaseURL     string
	HTTPClient  *http.Client
	Authorizer  Authorizer
	Audit       AuditSink
	Now         func() time.Time
}

type DomainDNS struct {
	zones  DNSZoneSource
	creds  DNSCredentialSource
	base   string
	client *http.Client
	authz  Authorizer
	audit  AuditSink
	now    func() time.Time
}

// DomainDNSRequest is one explicit operator request for a durable binding.
// The zone, immutable domain ID and exact ownership marker come from the
// binding ledger; the credential stays in encrypted server state and never
// enters this value.
type DomainDNSRequest struct {
	ZoneID    string   `json:"zone_id"`
	ZoneName  string   `json:"zone_name"`
	DomainID  string   `json:"domain_id"`
	Ownership string   `json:"ownership"`
	Host      string   `json:"host"`
	Addresses []string `json:"addresses,omitempty"`
	Proxied   bool     `json:"proxied"`
}

// DomainDNSRecord is one provider record for the requested hostname. Ownership
// is "owned" only when the record comment exactly equals this binding's marker.
type DomainDNSRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	TTL       int    `json:"ttl"`
	Proxied   bool   `json:"proxied"`
	Comment   string `json:"comment,omitempty"`
	Ownership string `json:"ownership"`
}

type DomainDNSChange struct {
	Action string           `json:"action"`
	Type   string           `json:"type"`
	Before *DomainDNSRecord `json:"before,omitempty"`
	After  *DNSRecord       `json:"after,omitempty"`
}

// DomainDNSPlan is a read-only provider snapshot plus the exact changes this
// binding would make. Conflicts fail an apply before any mutation; they never
// cause adoption of a record this binding does not own.
type DomainDNSPlan struct {
	Zone       string            `json:"zone"`
	ZoneID     string            `json:"zone_id"`
	DomainID   string            `json:"domain_id"`
	Host       string            `json:"host"`
	Ownership  string            `json:"ownership"`
	Proxied    bool              `json:"proxied"`
	ObservedAt time.Time         `json:"observed_at"`
	Records    []DomainDNSRecord `json:"records"`
	Changes    []DomainDNSChange `json:"changes"`
	Conflicts  []string          `json:"conflicts,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
	Applied    int               `json:"applied"`
	Unchanged  int               `json:"unchanged"`

	// desired is the complete verified target set for an apply plan. It is not
	// serialized; deletion is held until this set is visible at the provider.
	desired []DNSRecord
	cleanup bool
}

func NewDomainDNS(cfg DomainDNSConfig) (*DomainDNS, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &DomainDNS{
		zones: cfg.Zones, creds: cfg.Credentials, base: cfg.BaseURL,
		client: cfg.HTTPClient, authz: cfg.Authorizer, audit: cfg.Audit, now: now,
	}, nil
}

func (d *DomainDNS) AuthorizeRead(ctx context.Context, actor contract.Actor) error {
	if d == nil || d.authz == nil {
		return contract.NewFault(contract.Unavailable, "Cloudflare DNS authorization is not configured", "Wire the operator DNS authorizer; provider reads fail closed.")
	}
	if err := actor.Validate(); err != nil {
		return err
	}
	return d.authz.Authorize(ctx, actor, ActionDNSRead, "cloudflare")
}

func (d *DomainDNS) AuthorizeWrite(ctx context.Context, actor contract.Actor) error {
	if d == nil || d.authz == nil {
		return contract.NewFault(contract.Unavailable, "Cloudflare DNS authorization is not configured", "Wire the operator DNS authorizer; managed DNS changes fail closed.")
	}
	if err := actor.Validate(); err != nil {
		return err
	}
	return d.authz.Authorize(ctx, actor, ActionDNSWrite, "cloudflare")
}

func (d *DomainDNS) clientFor(ctx context.Context, zoneID, zoneName string) (DNSZone, *Cloudflare, error) {
	if d == nil || d.zones == nil || d.creds == nil {
		return DNSZone{}, nil, contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure a provider credential with eregion dns configure --provider cloudflare; manual DNS remains the default.")
	}
	zone, ok, err := d.zones.DNSZone(ctx, zoneID, zoneName)
	if err != nil {
		return DNSZone{}, nil, err
	}
	if !ok {
		return DNSZone{}, nil, contract.NewFault(contract.InputRequired, "Cloudflare zone is not configured on this control plane", "Configure it with eregion dns configure --provider cloudflare; manual DNS remains the default.")
	}
	raw, err := d.creds.DNSCredential(ctx, DNSProviderCloudflare, zone.ZoneID)
	if err != nil {
		return DNSZone{}, nil, err
	}
	defer clearBytes(raw)
	client, err := NewCloudflare(CloudflareConfig{
		APIToken: strings.TrimRight(string(raw), "\r\n"),
		ZoneID:   zone.ZoneID, ZoneName: zone.ZoneName,
		BaseURL: d.base, HTTPClient: d.client,
	})
	if err != nil {
		return DNSZone{}, nil, err
	}
	return zone, client, nil
}

// ResolveZone maps an operator zone reference (configured name or ID) to the
// exact configured zone through PostgreSQL. Admission persists the ID/name so
// later cleanup does not depend on operator flags.
func (d *DomainDNS) ResolveZone(ctx context.Context, ref string) (DNSZone, error) {
	if d == nil || d.zones == nil {
		return DNSZone{}, contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure a provider credential with eregion dns configure --provider cloudflare; manual DNS remains the default.")
	}
	zone, ok, err := d.zones.DNSZone(ctx, "", ref)
	if err != nil {
		return DNSZone{}, err
	}
	if !ok {
		return DNSZone{}, contract.NewFault(contract.InputRequired, "Cloudflare zone is not configured on this control plane", "Configure it with eregion dns configure --provider cloudflare; manual DNS remains the default.")
	}
	return zone, nil
}

// ValidateDomainDNSHost requires the hostname to be the configured zone or a
// subdomain of it. Admission calls this before persisting any intent or cleanup
// ledger row.
func ValidateDomainDNSHost(zone DNSZone, host string) error {
	host, err := normalizeDomainDNSHost(host)
	if err != nil {
		return err
	}
	if !hostnameBelongsToZone(host, zone.ZoneName) {
		return contract.NewFault(contract.ValidationFailed, "hostname is outside the configured zone", "Use a hostname equal to or below the selected Cloudflare zone.")
	}
	return nil
}

// PlanApply returns a real provider snapshot for one hostname and the exact
// create/update/delete set an apply would make. It performs no mutation.
func (d *DomainDNS) PlanApply(ctx context.Context, req DomainDNSRequest) (DomainDNSPlan, error) {
	zone, client, err := d.clientFor(ctx, req.ZoneID, req.ZoneName)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	return d.planApply(ctx, zone, client, req)
}

func (d *DomainDNS) planApply(ctx context.Context, zone DNSZone, client *Cloudflare, req DomainDNSRequest) (DomainDNSPlan, error) {
	host, desired, marker, err := domainDNSDesired(zone, req)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	plan, err := d.snapshot(ctx, zone, client, host, marker, req.Proxied)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	plan.DomainID = req.DomainID
	plan.desired = append([]DNSRecord(nil), desired...)
	owned, conflicts := domainDNSClassify(plan.Records, marker)
	plan.Conflicts = conflicts
	plan.Changes, plan.Unchanged = domainDNSApplyChanges(desired, owned)
	plan.Warnings = append(plan.Warnings,
		"Cloudflare provider state is not public readiness; resolver, route, certificate and public response remain separate observations.",
		"Cloudflare has no conditional-write API; ownership is re-read immediately before each mutation, but a concurrent external writer can still race the final request.",
		"Foreign record content, comments, TTL and proxy state are redacted from this snapshot.")
	if len(conflicts) > 0 {
		plan.Warnings = append(plan.Warnings, "Apply would fail closed: existing records at this hostname are not owned by this binding.")
	}
	return plan, nil
}

// PlanRemove returns the exact owned records an explicit removal would delete.
// Foreign records are left untouched and do not prevent exact-owned cleanup.
func (d *DomainDNS) PlanRemove(ctx context.Context, req DomainDNSRequest) (DomainDNSPlan, error) {
	zone, client, err := d.clientFor(ctx, req.ZoneID, req.ZoneName)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	return d.planRemove(ctx, zone, client, req)
}

func (d *DomainDNS) planRemove(ctx context.Context, zone DNSZone, client *Cloudflare, req DomainDNSRequest) (DomainDNSPlan, error) {
	host, err := normalizeDomainDNSHost(req.Host)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	if strings.TrimSpace(req.DomainID) == "" || strings.TrimSpace(req.Ownership) == "" {
		return DomainDNSPlan{}, contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs the durable binding identity", "Plan managed DNS from a persisted domain binding ledger, never from a zone-wide request.")
	}
	if !hostnameBelongsToZone(host, zone.ZoneName) {
		return DomainDNSPlan{}, contract.NewFault(contract.ValidationFailed, "hostname is outside the configured zone", "Use a hostname equal to or below the selected Cloudflare zone.")
	}
	marker := req.Ownership
	if marker != DomainDNSOwnership(req.DomainID, zone.ZoneID) {
		return DomainDNSPlan{}, contract.NewFault(contract.PreconditionFailed, "managed DNS ownership marker does not match the durable binding identity", "Do not apply this plan; inspect the binding ledger and retry.")
	}
	plan, err := d.snapshot(ctx, zone, client, host, marker, false)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	plan.DomainID = req.DomainID
	plan.cleanup = true
	for _, record := range plan.Records {
		if record.Ownership == "owned" && record.Comment == marker {
			before := record
			plan.Changes = append(plan.Changes, DomainDNSChange{Action: "delete", Type: record.Type, Before: &before})
		}
	}
	sortDomainDNSChanges(plan.Changes)
	plan.Warnings = append(plan.Warnings,
		"Only records whose comment exactly matches this binding's ownership marker are removed; unrelated records are preserved.",
		"Cloudflare has no conditional-write API; ownership is re-read immediately before each deletion, but a concurrent external writer can still race the final request.",
		"Foreign record content, comments, TTL and proxy state are redacted from this snapshot.")
	return plan, nil
}

// Apply authorizes, audits and applies exactly the planned binding changes.
// Any ownership conflict fails before the first mutation.
func (d *DomainDNS) Apply(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest) (DomainDNSPlan, error) {
	return d.apply(ctx, actor, opID, req, nil)
}

// ApplyWithGuard is Apply with an operation boundary checked immediately before
// each provider mutation. A stopped guard aborts before that mutation; earlier
// convergent changes remain visible to a retry.
func (d *DomainDNS) ApplyWithGuard(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest, guard func(context.Context) error) (DomainDNSPlan, error) {
	return d.apply(ctx, actor, opID, req, guard)
}

func (d *DomainDNS) apply(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest, guard func(context.Context) error) (DomainDNSPlan, error) {
	if d == nil || d.authz == nil || d.audit == nil {
		return DomainDNSPlan{}, contract.NewFault(contract.Unavailable, "Cloudflare DNS authorization or audit is not configured", "Wire the operator DNS authorizer and the durable audit sink; DNS changes fail closed.")
	}
	if err := d.AuthorizeWrite(ctx, actor); err != nil {
		return DomainDNSPlan{}, err
	}
	zone, client, err := d.clientFor(ctx, req.ZoneID, req.ZoneName)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	plan, err := d.planApply(ctx, zone, client, req)
	if err != nil {
		return plan, err
	}
	return d.applyPlan(ctx, actor, opID, zone, client, plan, guard)
}

// Remove authorizes, audits and deletes only this binding's exact owned
// address records. Foreign records are preserved.
func (d *DomainDNS) Remove(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest) (DomainDNSPlan, error) {
	return d.remove(ctx, actor, opID, req, nil)
}

// RemoveWithGuard is Remove with an operation boundary checked before each
// provider deletion.
func (d *DomainDNS) RemoveWithGuard(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest, guard func(context.Context) error) (DomainDNSPlan, error) {
	return d.remove(ctx, actor, opID, req, guard)
}

func (d *DomainDNS) remove(ctx context.Context, actor contract.Actor, opID contract.OperationID, req DomainDNSRequest, guard func(context.Context) error) (DomainDNSPlan, error) {
	if d == nil || d.authz == nil || d.audit == nil {
		return DomainDNSPlan{}, contract.NewFault(contract.Unavailable, "Cloudflare DNS authorization or audit is not configured", "Wire the operator DNS authorizer and the durable audit sink; DNS changes fail closed.")
	}
	if err := d.AuthorizeWrite(ctx, actor); err != nil {
		return DomainDNSPlan{}, err
	}
	zone, client, err := d.clientFor(ctx, req.ZoneID, req.ZoneName)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	plan, err := d.planRemove(ctx, zone, client, req)
	if err != nil {
		return plan, err
	}
	plan, err = d.applyPlan(ctx, actor, opID, zone, client, plan, guard)
	if err != nil {
		return plan, err
	}
	// A delete acknowledgement is not cleanup proof. Re-read the hostname and
	// refuse verification while any exact-owned A/AAAA remains.
	if err := d.verifyRemoved(ctx, zone, client, req); err != nil {
		return plan, err
	}
	return plan, nil
}

func (d *DomainDNS) applyPlan(ctx context.Context, actor contract.Actor, opID contract.OperationID, zone DNSZone, client *Cloudflare, plan DomainDNSPlan, guard func(context.Context) error) (DomainDNSPlan, error) {
	if d.authz == nil || d.audit == nil {
		return plan, contract.NewFault(contract.Unavailable, "Cloudflare DNS authorization or audit is not configured", "Wire the operator DNS authorizer and the durable audit sink; DNS changes fail closed.")
	}
	if err := actor.Validate(); err != nil {
		return plan, err
	}
	if err := d.AuthorizeWrite(ctx, actor); err != nil {
		return plan, err
	}
	if opID != "" {
		if err := opID.Validate(); err != nil {
			return plan, err
		}
	}
	if len(plan.Conflicts) > 0 {
		fault := contract.NewFault(contract.PreconditionFailed,
			"Cloudflare DNS ownership could not be established for this hostname",
			"Inspect the existing records and remove or adopt them deliberately; eregion never overwrites or deletes a record it does not own.")
		fault.Details = map[string]any{
			"zone": plan.Zone, "host": plan.Host, "ownership": plan.Ownership,
			"conflicts": plan.Conflicts, "records": plan.Records,
		}
		return plan, fault
	}
	if len(plan.Changes) == 0 {
		return plan, nil
	}
	for _, change := range plan.Changes {
		action, target := domainDNSChangeTarget(plan.Host, change)
		if err := d.authz.Authorize(ctx, actor, action, target); err != nil {
			_ = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "denied", Detail: auditDetail(opID, "authorization denied")})
			return plan, err
		}
		if err := d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "intent", Detail: auditDetail(opID, "zone="+zone.ZoneName+" ownership="+plan.Ownership)}); err != nil {
			return plan, err
		}
	}
	var additions, deletions []DomainDNSChange
	for _, change := range plan.Changes {
		if change.Action == "delete" {
			deletions = append(deletions, change)
		} else {
			additions = append(additions, change)
		}
	}
	for _, change := range additions {
		if err := d.applyOneChange(ctx, actor, opID, zone, client, &plan, change, guard); err != nil {
			return plan, err
		}
	}
	if len(deletions) > 0 {
		// Never remove the last old address before the replacement set is
		// visible at the provider.
		if err := d.verifyDesired(ctx, zone, client, plan); err != nil {
			return plan, err
		}
	}
	for _, change := range deletions {
		if err := d.applyOneChange(ctx, actor, opID, zone, client, &plan, change, guard); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func (d *DomainDNS) applyOneChange(ctx context.Context, actor contract.Actor, opID contract.OperationID, zone DNSZone, client *Cloudflare, plan *DomainDNSPlan, change DomainDNSChange, guard func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	action, target := domainDNSChangeTarget(plan.Host, change)
	skip, err := d.confirmDomainDNSChange(ctx, zone, client, *plan, change)
	if err != nil {
		_ = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(opID, "ownership recheck failed")})
		return err
	}
	if skip {
		if err := d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "unchanged", Detail: auditDetail(opID, "external state already converged")}); err != nil {
			return err
		}
		plan.Unchanged++
		return nil
	}
	if guard != nil {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	if err := client.apply(ctx, domainDNSChangeToDNSChange(change)); err != nil {
		_ = d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: auditDetail(opID, "zone="+zone.ZoneName)})
		var failure *ProviderFailure
		if change.Action == "delete" && change.Before != nil && errors.As(err, &failure) &&
			(contract.Classify(err).Code == contract.Forbidden || contract.Classify(err).Code == contract.PreconditionFailed) {
			copy := *failure
			copy.DefinitiveWrite = true
			digest := sha256.Sum256([]byte(change.Before.ID))
			copy.DefinitiveRecordDigest = hex.EncodeToString(digest[:])
			return &copy
		}
		return err
	}
	plan.Applied++
	if err := d.audit.Record(ctx, AuditEntry{At: d.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "succeeded", Detail: auditDetail(opID, "zone="+zone.ZoneName)}); err != nil {
		return contract.NewFault(contract.Unavailable, "DNS change was applied but its audit record failed", "Stop and inspect the audit sink before continuing; do not assume remaining changes were applied.")
	}
	return nil
}

// verifyDesired proves that the complete replacement set exists at the
// provider before any obsolete owned A/AAAA record is deleted. A missing,
// changed or conflicting record leaves the old address in place for retry.
func (d *DomainDNS) verifyDesired(ctx context.Context, zone DNSZone, client *Cloudflare, plan DomainDNSPlan) error {
	if len(plan.desired) == 0 {
		return nil
	}
	current, err := d.snapshot(ctx, zone, client, plan.Host, plan.Ownership, plan.Proxied)
	if err != nil {
		return err
	}
	owned, conflicts := domainDNSClassify(current.Records, plan.Ownership)
	if len(conflicts) > 0 {
		fault := contract.NewFault(contract.PreconditionFailed,
			"Cloudflare DNS ownership changed before obsolete records could be deleted",
			"The replacement was not verified; no obsolete record was deleted. Inspect the hostname and retry from a fresh plan.")
		fault.Details = map[string]any{"zone": plan.Zone, "host": plan.Host, "conflicts": conflicts}
		return fault
	}
	for _, want := range plan.desired {
		found := false
		for _, record := range owned[want.Type] {
			if domainDNSContentEqual(want.Type, record.Content, want.Content) &&
				record.TTL == want.TTL && record.Proxied == want.Proxied && record.Comment == want.Comment {
				found = true
				break
			}
		}
		if !found {
			fault := contract.NewFault(contract.PreconditionFailed,
				"the desired DNS replacement is not verified at the provider",
				"Keep the old address in place and retry; no obsolete record was deleted.")
			fault.Details = map[string]any{"zone": plan.Zone, "host": plan.Host, "desired_type": want.Type, "desired_content": want.Content}
			return fault
		}
	}
	return nil
}

// verifyRemoved proves that the post-delete provider state has no exact-owned
// A/AAAA records. A failed read or a reappearance retains the ledger for retry;
// unrelated provider records do not become this binding's responsibility.
func (d *DomainDNS) verifyRemoved(ctx context.Context, zone DNSZone, client *Cloudflare, req DomainDNSRequest) error {
	host, err := normalizeDomainDNSHost(req.Host)
	if err != nil {
		return err
	}
	current, err := d.snapshot(ctx, zone, client, host, req.Ownership, false)
	if err != nil {
		return err
	}
	owned := 0
	for _, record := range current.Records {
		if record.Ownership == "owned" && record.Comment == req.Ownership {
			owned++
		}
	}
	if owned > 0 {
		fault := contract.NewFault(contract.PreconditionFailed,
			"exact-owned DNS records remain after deletion requests",
			"The cleanup is not verified; the ledger is retained. Retry the cleanup.")
		fault.Details = map[string]any{"zone": zone.ZoneName, "host": req.Host, "owned_records": owned}
		return fault
	}
	return nil
}

// confirmDomainDNSChange re-reads exact-host ownership immediately before one
// mutation. Cloudflare has no conditional-write API, so this narrows but cannot
// eliminate the final external-writer race; a conflicting writer is reported on
// the next plan or reconciliation.
func (d *DomainDNS) confirmDomainDNSChange(ctx context.Context, zone DNSZone, client *Cloudflare, plan DomainDNSPlan, change DomainDNSChange) (bool, error) {
	current, err := d.snapshot(ctx, zone, client, plan.Host, plan.Ownership, plan.Proxied)
	if err != nil {
		return false, err
	}
	owned, conflicts := domainDNSClassify(current.Records, plan.Ownership)
	if len(conflicts) > 0 && !plan.cleanup {
		fault := contract.NewFault(contract.PreconditionFailed,
			"Cloudflare DNS ownership changed immediately before the mutation",
			"No record was changed by this attempt; inspect the hostname and retry from a fresh plan.")
		fault.Details = map[string]any{"zone": plan.Zone, "host": plan.Host, "conflicts": conflicts}
		return false, fault
	}
	switch change.Action {
	case "create":
		if change.After == nil {
			return false, contract.NewFault(contract.Internal, "DNS create change has no desired record", "Do not apply this plan.")
		}
		for _, record := range owned[change.Type] {
			if !domainDNSContentEqual(change.Type, record.Content, change.After.Content) {
				continue
			}
			if record.TTL == change.After.TTL && record.Proxied == change.After.Proxied && record.Comment == change.After.Comment {
				return true, nil
			}
			return false, contract.NewFault(contract.PreconditionFailed,
				"an owned record with the desired content appeared with different fields",
				"Retry from a fresh plan; no duplicate record was created.")
		}
		return false, nil
	case "update":
		if change.After == nil || change.After.ID == "" {
			return false, contract.NewFault(contract.Internal, "DNS update change has no target ID", "Do not apply this plan.")
		}
		for _, record := range current.Records {
			if record.ID != change.After.ID {
				continue
			}
			if !strings.EqualFold(record.Type, change.Type) || !hostnameEqual(record.Name, plan.Host) || record.Comment != plan.Ownership {
				return false, contract.NewFault(contract.PreconditionFailed,
					"the owned record changed immediately before the update",
					"No update was made; inspect the hostname and retry from a fresh plan.")
			}
			if domainDNSContentEqual(change.Type, record.Content, change.After.Content) &&
				record.TTL == change.After.TTL && record.Proxied == change.After.Proxied && record.Comment == change.After.Comment {
				return true, nil
			}
			return false, nil
		}
		return false, contract.NewFault(contract.PreconditionFailed,
			"the owned record disappeared before the update",
			"No update was made; retry from a fresh plan.")
	default:
		if change.Before == nil || change.Before.ID == "" {
			return false, contract.NewFault(contract.Internal, "DNS delete change has no target ID", "Do not apply this plan.")
		}
		for _, record := range current.Records {
			if record.ID != change.Before.ID {
				continue
			}
			if !strings.EqualFold(record.Type, change.Type) || !hostnameEqual(record.Name, plan.Host) || record.Comment != plan.Ownership {
				return false, contract.NewFault(contract.PreconditionFailed,
					"the owned record changed immediately before deletion",
					"No deletion was made; inspect the hostname and retry from a fresh plan.")
			}
			if !domainDNSContentEqual(change.Type, record.Content, change.Before.Content) || record.TTL != change.Before.TTL || record.Proxied != change.Before.Proxied {
				return false, contract.NewFault(contract.PreconditionFailed, "the owned record changed immediately before deletion", "No deletion was made; inspect the hostname and retry from a fresh plan.")
			}
			return false, nil
		}
		// A concurrent writer may already have removed the exact-owned record.
		// Skipping is safe; a different record is never deleted.
		return true, nil
	}
}

func (d *DomainDNS) snapshot(ctx context.Context, zone DNSZone, client *Cloudflare, host, marker string, proxied bool) (DomainDNSPlan, error) {
	records, err := client.ListNameRecords(ctx, host)
	if err != nil {
		return DomainDNSPlan{}, err
	}
	plan := DomainDNSPlan{
		Zone: zone.ZoneName, ZoneID: zone.ZoneID, Host: host, Ownership: marker,
		Proxied: proxied, ObservedAt: d.now().UTC(), Records: []DomainDNSRecord{},
	}
	for _, record := range records {
		recordType := strings.ToUpper(record.Type)
		ownership := "foreign"
		// Only A/AAAA records can be managed. A CNAME is a conflict even when
		// its comment exactly matches the binding marker; it is never adopted,
		// updated or deleted.
		if (recordType == "A" || recordType == "AAAA") && record.Comment == marker {
			ownership = "owned"
		}
		item := DomainDNSRecord{
			ID: record.ID, Name: record.Name, Type: record.Type, Content: record.Content,
			TTL: record.TTL, Proxied: record.Proxied, Comment: record.Comment, Ownership: ownership,
		}
		if ownership != "owned" {
			// Foreign/non-managed TXT and CNAME content and comments can contain
			// verification secrets or unrelated provider data. Preserve only the
			// identity and type needed to explain the conflict.
			item.Content, item.Comment, item.TTL, item.Proxied = "", "", 0, false
		}
		plan.Records = append(plan.Records, item)
	}
	sort.Slice(plan.Records, func(i, j int) bool {
		if plan.Records[i].Type == plan.Records[j].Type {
			return plan.Records[i].ID < plan.Records[j].ID
		}
		return plan.Records[i].Type < plan.Records[j].Type
	})
	return plan, nil
}

func domainDNSDesired(zone DNSZone, req DomainDNSRequest) (string, []DNSRecord, string, error) {
	host, err := normalizeDomainDNSHost(req.Host)
	if err != nil {
		return "", nil, "", err
	}
	if strings.TrimSpace(req.DomainID) == "" || strings.TrimSpace(req.Ownership) == "" {
		return "", nil, "", contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs the durable binding identity", "Plan managed DNS from a persisted domain binding ledger, never from a zone-wide request.")
	}
	if !hostnameBelongsToZone(host, zone.ZoneName) {
		return "", nil, "", contract.NewFault(contract.ValidationFailed, "hostname is outside the configured zone", "Use a hostname equal to or below the selected Cloudflare zone.")
	}
	if len(req.Addresses) == 0 || len(req.Addresses) > 8 {
		return "", nil, "", contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs 1–8 explicit public origin addresses", "Pass --dns-origin; the WireGuard management endpoint is not used.")
	}
	marker := req.Ownership
	if marker != DomainDNSOwnership(req.DomainID, zone.ZoneID) {
		return "", nil, "", contract.NewFault(contract.PreconditionFailed, "managed DNS ownership marker does not match the durable binding identity", "Do not apply this plan; inspect the binding ledger and retry.")
	}
	seen := map[string]bool{}
	desired := make([]DNSRecord, 0, len(req.Addresses))
	for _, value := range req.Addresses {
		ip := net.ParseIP(strings.TrimSpace(value))
		if ip == nil {
			return "", nil, "", contract.NewFault(contract.ValidationFailed, "invalid main-node address for Cloudflare DNS", "Use IPv4 or IPv6 addresses from the selected context.")
		}
		recordType, content := "AAAA", ip.String()
		if v4 := ip.To4(); v4 != nil {
			recordType, content = "A", v4.String()
		}
		key := recordType + "\x00" + content
		if seen[key] {
			continue
		}
		seen[key] = true
		desired = append(desired, DNSRecord{
			Name: host, Type: recordType, Content: content, TTL: 1, Proxied: req.Proxied, Comment: marker,
		})
	}
	if len(desired) == 0 {
		return "", nil, "", contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs at least one usable address", "Run domain add from a workspace whose main node has a public address.")
	}
	sort.Slice(desired, func(i, j int) bool {
		if desired[i].Type == desired[j].Type {
			return desired[i].Content < desired[j].Content
		}
		return desired[i].Type < desired[j].Type
	})
	return host, desired, marker, nil
}

func normalizeDomainDNSHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" || !dnsNamePattern.MatchString(host) || strings.Contains(host, "_") {
		return "", contract.NewFault(contract.ValidationFailed, "invalid DNS hostname", "Use a lowercase hostname without wildcards, spaces or control characters.")
	}
	return host, nil
}

// DomainDNSOwnership derives the exact provider comment from immutable
// binding identity and zone. Names are deliberately not part of the identity:
// deleting and recreating a hostname creates a new binding ID and cannot adopt
// the previous incarnation's record.
func DomainDNSOwnership(domainID, zoneID string) string {
	sum := sha256.Sum256([]byte(domainID + "\x00" + zoneID))
	return domainDNSOwnershipPrefix + hex.EncodeToString(sum[:])
}

// domainDNSClassify splits records into exact-owned A/AAAA records and
// conflicts. Any foreign A/AAAA at the hostname is a conflict, and any CNAME at
// the hostname is a conflict even when it carries the exact marker: this flow
// manages only A/AAAA and never adopts, overwrites or deletes a CNAME. Multiple
// owned records are allowed for different addresses of one type; duplicate
// content is ambiguous and fails closed. Unrelated types stay untouched.
func domainDNSClassify(records []DomainDNSRecord, marker string) (map[string][]DomainDNSRecord, []string) {
	owned := map[string][]DomainDNSRecord{}
	seen := map[string]bool{}
	var conflicts []string
	for _, record := range records {
		recordType := strings.ToUpper(record.Type)
		switch recordType {
		case "A", "AAAA":
		case "CNAME":
			// CNAME is never managed by this flow, regardless of its comment.
			conflicts = append(conflicts, fmt.Sprintf("CNAME record %s at %s is outside the managed A/AAAA policy", record.ID, record.Name))
			continue
		default:
			continue
		}
		if record.Comment != marker || record.Ownership != "owned" {
			conflicts = append(conflicts, fmt.Sprintf("%s record %s at %s is not owned by this binding", recordType, record.ID, record.Name))
			continue
		}
		key := recordType + "\x00" + record.Content
		if seen[key] {
			conflicts = append(conflicts, fmt.Sprintf("multiple owned %s records share %s at %s", recordType, record.Content, record.Name))
			continue
		}
		seen[key] = true
		owned[recordType] = append(owned[recordType], record)
	}
	for recordType := range owned {
		sort.Slice(owned[recordType], func(i, j int) bool {
			if owned[recordType][i].Content == owned[recordType][j].Content {
				return owned[recordType][i].ID < owned[recordType][j].ID
			}
			return owned[recordType][i].Content < owned[recordType][j].Content
		})
	}
	sort.Strings(conflicts)
	return owned, conflicts
}

func domainDNSApplyChanges(desired []DNSRecord, owned map[string][]DomainDNSRecord) ([]DomainDNSChange, int) {
	var changes []DomainDNSChange
	unchanged := 0
	desiredByType := map[string][]DNSRecord{}
	for _, want := range desired {
		desiredByType[want.Type] = append(desiredByType[want.Type], want)
	}
	types := map[string]bool{}
	for recordType := range desiredByType {
		types[recordType] = true
	}
	for recordType := range owned {
		types[recordType] = true
	}
	for recordType := range types {
		wanted := desiredByType[recordType]
		current := owned[recordType]
		used := make([]bool, len(current))
		remaining := make([]DNSRecord, 0, len(wanted))
		for _, want := range wanted {
			matched := false
			for index, existing := range current {
				if used[index] || !domainDNSContentEqual(recordType, existing.Content, want.Content) {
					continue
				}
				used[index] = true
				matched = true
				if existing.TTL == want.TTL && existing.Proxied == want.Proxied && existing.Comment == want.Comment {
					unchanged++
					break
				}
				after := want
				after.ID = existing.ID
				before := existing
				changes = append(changes, DomainDNSChange{Action: "update", Type: recordType, Before: &before, After: &after})
				break
			}
			if !matched {
				remaining = append(remaining, want)
			}
		}
		for _, want := range remaining {
			matched := false
			for index, existing := range current {
				if used[index] {
					continue
				}
				used[index] = true
				matched = true
				after := want
				after.ID = existing.ID
				before := existing
				changes = append(changes, DomainDNSChange{Action: "update", Type: recordType, Before: &before, After: &after})
				break
			}
			if !matched {
				after := want
				changes = append(changes, DomainDNSChange{Action: "create", Type: recordType, After: &after})
			}
		}
		for index, existing := range current {
			if used[index] {
				continue
			}
			before := existing
			changes = append(changes, DomainDNSChange{Action: "delete", Type: recordType, Before: &before})
		}
	}
	sortDomainDNSChanges(changes)
	return changes, unchanged
}

func sortDomainDNSChanges(changes []DomainDNSChange) {
	rank := func(action string) int {
		switch action {
		case "update":
			return 0
		case "create":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Action != changes[j].Action {
			return rank(changes[i].Action) < rank(changes[j].Action)
		}
		if changes[i].Type != changes[j].Type {
			return changes[i].Type < changes[j].Type
		}
		if changes[i].Action == "create" || changes[i].Action == "update" {
			left, right := "", ""
			if changes[i].After != nil {
				left = changes[i].After.ID + "\x00" + changes[i].After.Content
			}
			if changes[j].After != nil {
				right = changes[j].After.ID + "\x00" + changes[j].After.Content
			}
			return left < right
		}
		left, right := "", ""
		if changes[i].Before != nil {
			left = changes[i].Before.ID
		}
		if changes[j].Before != nil {
			right = changes[j].Before.ID
		}
		return left < right
	})
}

func domainDNSContentEqual(recordType, left, right string) bool {
	if recordType == "A" || recordType == "AAAA" {
		leftIP, rightIP := net.ParseIP(strings.TrimSpace(left)), net.ParseIP(strings.TrimSpace(right))
		return leftIP != nil && rightIP != nil && leftIP.Equal(rightIP)
	}
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(left), "."), strings.TrimSuffix(strings.TrimSpace(right), "."))
}

func domainDNSChangeTarget(host string, change DomainDNSChange) (string, string) {
	target := host + "/" + change.Type
	switch change.Action {
	case "create":
		return ActionDNSCreate, target
	case "update":
		return ActionDNSUpdate, target
	default:
		return ActionDNSDelete, target
	}
}

func domainDNSChangeToDNSChange(change DomainDNSChange) DNSChange {
	result := DNSChange{Action: change.Action}
	if change.Before != nil {
		before := DNSRecord{
			ID: change.Before.ID, Name: change.Before.Name, Type: change.Before.Type,
			Content: change.Before.Content, TTL: change.Before.TTL, Proxied: change.Before.Proxied,
			Comment: change.Before.Comment,
		}
		result.Before = &before
	}
	if change.After != nil {
		after := *change.After
		result.After = &after
	}
	return result
}
