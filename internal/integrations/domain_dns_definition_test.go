package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

type definitionTestProgress struct {
	entries                             []DefinitionDNSProgress
	stopIntent, stopObserved, stopGuard bool
}

func (p *definitionTestProgress) Load(context.Context) ([]DefinitionDNSProgress, error) {
	return p.entries, nil
}
func (p *definitionTestProgress) Intent(_ context.Context, e DefinitionDNSProgress) error {
	if p.stopIntent {
		return errors.New("intent failure")
	}
	p.entries = append(p.entries, e)
	return nil
}
func (p *definitionTestProgress) Observed(_ context.Context, e DefinitionDNSProgress) error {
	if p.stopObserved {
		return errors.New("observed failure")
	}
	p.entries[e.Index] = e
	return nil
}
func (p *definitionTestProgress) Guard(context.Context) error {
	if p.stopGuard {
		return errors.New("stopped")
	}
	return nil
}

func definitionRequest() DomainDNSRequest {
	return DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"8.8.8.8"}}
}

func TestDefinitionDNSPlanExternalAndConflicts(t *testing.T) {
	f := newCloudflareFixture(testZoneID)
	f.records["matching"] = DNSRecord{ID: "matching", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 300, Comment: "external"}
	f.records["extra"] = DNSRecord{ID: "extra", Name: "app.example.com", Type: "A", Content: "1.1.1.1", TTL: 42, Comment: "external"}
	f.records["txt"] = DNSRecord{ID: "txt", Name: "app.example.com", Type: "TXT", Content: "secret"}
	d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
	actor := contract.Actor{Kind: "operator", Subject: "ops"}
	p, err := d.PlanDefinitionApply(context.Background(), actor, definitionRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !p.RequiresApproval || len(p.Before) != 2 || len(p.ExternalSatisfaction) != 1 || len(p.Changes) != 1 || p.Changes[0].Before.ID != "extra" {
		t.Fatalf("plan: %+v", p)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DefinitionDNSPlan
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, decoded) {
		t.Fatalf("serialization changed plan: %s", data)
	}
	if countMutations(f.calls) != 0 {
		t.Fatal(f.calls)
	}
	if _, err = d.PlanDefinitionApply(context.Background(), contract.Actor{Kind: "ci", Subject: "ci"}, definitionRequest()); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("CI read: %v", err)
	}
}

func TestDefinitionDNSApprovedCNAMEAndRetry(t *testing.T) {
	f := newCloudflareFixture(testZoneID)
	f.records["cname"] = DNSRecord{ID: "cname", Name: "app.example.com", Type: "CNAME", Content: "origin.example.net", TTL: 300, Comment: "external"}
	d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
	ctx := context.Background()
	actor := contract.Actor{Kind: "operator", Subject: "ops"}
	req := definitionRequest()
	p, err := d.PlanDefinitionApply(ctx, actor, req)
	if err != nil {
		t.Fatal(err)
	}
	if !p.RequiresApproval || len(p.Changes) != 2 || p.Changes[0].Type != "CNAME" || p.Changes[1].Action != "create" {
		t.Fatalf("plan %+v", p)
	}
	progress := &definitionTestProgress{stopObserved: true}
	op := contract.OperationID("op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, op, req, p, progress); err == nil {
		t.Fatal("expected failed durable observation")
	}
	if len(progress.entries) != 1 || progress.entries[0].Result != nil {
		t.Fatal(progress.entries)
	}
	progress.stopObserved = false
	if n, err := d.ExecuteApprovedDefinitionDNS(ctx, actor, op, req, p, progress); err != nil || n != 2 {
		t.Fatalf("retry %d %v", n, err)
	}
	if countMutations(f.calls) != 2 {
		t.Fatalf("repeated mutation: %v", f.calls)
	}
	if len(progress.entries) != 2 {
		t.Fatal(progress.entries)
	}
}

func TestDefinitionDNSFailsClosedBeforeEffectAndOnDrift(t *testing.T) {
	f := newCloudflareFixture(testZoneID)
	d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
	ctx := context.Background()
	actor := contract.Actor{Kind: "operator", Subject: "ops"}
	req := definitionRequest()
	op := contract.OperationID("op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	p, err := d.PlanDefinitionApply(ctx, actor, req)
	if err != nil {
		t.Fatal(err)
	}
	progress := &definitionTestProgress{stopIntent: true}
	if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, op, req, p, progress); err == nil {
		t.Fatal("intent failure ignored")
	}
	if countMutations(f.calls) != 0 {
		t.Fatal(f.calls)
	}
	progress.stopIntent = false
	progress.stopGuard = true
	if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, op, req, p, progress); err == nil {
		t.Fatal("guard failure ignored")
	}
	if countMutations(f.calls) != 0 {
		t.Fatal(f.calls)
	}
	f.records["foreign"] = DNSRecord{ID: "foreign", Name: req.Host, Type: "A", Content: "1.1.1.1", TTL: 1}
	progress.stopGuard = false
	if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, op, req, p, progress); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("drift: %v", err)
	}
	if countMutations(f.calls) != 0 {
		t.Fatal(f.calls)
	}
}

func TestDefinitionDNSCreateRecoveryAndAmbiguousDuplicate(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "duplicate"}[duplicate], func(t *testing.T) {
			f := newCloudflareFixture(testZoneID)
			d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
			ctx, actor, req := context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, definitionRequest()
			p, err := d.PlanDefinitionApply(ctx, actor, req)
			if err != nil {
				t.Fatal(err)
			}
			progress := &definitionTestProgress{stopObserved: true}
			op := contract.OperationID("op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, op, req, p, progress); err == nil {
				t.Fatal("missing observation failure")
			}
			if duplicate {
				r := f.records["rec00000001"]
				r.ID = "duplicate"
				f.records[r.ID] = r
			}
			progress.stopObserved = false
			_, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, op, req, p, progress)
			if duplicate && contract.Classify(err).Code != contract.PreconditionFailed {
				t.Fatalf("ambiguous create: %v", err)
			}
			if !duplicate && err != nil {
				t.Fatal(err)
			}
			if countMutations(f.calls) != 1 {
				t.Fatal(f.calls)
			}
		})
	}
}

func TestDefinitionDNSAddressTransitionAndMixedOwnership(t *testing.T) {
	f := newCloudflareFixture(testZoneID)
	req := definitionRequest()
	req.Addresses = []string{"8.8.8.8", "2001:4860:4860::8888"}
	f.records["external"] = DNSRecord{ID: "external", Name: req.Host, Type: "A", Content: "8.8.8.8", TTL: 300}
	f.records["old"] = DNSRecord{ID: "old", Name: req.Host, Type: "AAAA", Content: "2001:4860:4860::8844", TTL: 1, Comment: req.Ownership}
	d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
	ctx := context.Background()
	actor := contract.Actor{Kind: "operator", Subject: "ops"}
	p, err := d.PlanDefinitionApply(ctx, actor, req)
	if err != nil {
		t.Fatal(err)
	}
	if !p.RequiresApproval || len(p.ExternalSatisfaction) != 1 || len(p.Changes) != 2 || p.Changes[0].Action != "create" || p.Changes[1].Action != "delete" {
		t.Fatalf("plan: %+v", p)
	}
	if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", req, p, &definitionTestProgress{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.records["external"]; !ok {
		t.Fatal("external satisfaction removed")
	}
	var writes []string
	for _, call := range f.calls {
		if call != "list" {
			writes = append(writes, call)
		}
	}
	if !reflect.DeepEqual(writes, []string{"create", "delete"}) {
		t.Fatal(writes)
	}
}

func TestDefinitionDNSPreservesAllMatchingForeignRecords(t *testing.T) {
	for _, tc := range []struct {
		name       string
		owned      bool
		foreignIDs []string
		conflict   bool
	}{
		{name: "owned and external", owned: true, foreignIDs: []string{"external"}},
		{name: "multiple external matches", foreignIDs: []string{"external-a", "external-b"}},
		{name: "mixed matching and conflicting", owned: true, foreignIDs: []string{"external-a", "external-b"}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCloudflareFixture(testZoneID)
			req := definitionRequest()
			if tc.owned {
				f.records["owned"] = DNSRecord{ID: "owned", Name: req.Host, Type: "A", Content: "8.8.8.8", TTL: 1, Comment: req.Ownership}
			}
			for _, id := range tc.foreignIDs {
				f.records[id] = DNSRecord{ID: id, Name: req.Host, Type: "A", Content: "8.8.8.8", TTL: 300, Comment: "external"}
			}
			if tc.conflict {
				f.records["wrong-address"] = DNSRecord{ID: "wrong-address", Name: req.Host, Type: "A", Content: "1.1.1.1", TTL: 300}
				f.records["wrong-proxy"] = DNSRecord{ID: "wrong-proxy", Name: req.Host, Type: "A", Content: "8.8.8.8", TTL: 300, Proxied: true}
			}
			ctx, actor := context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}
			d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
			plan, err := d.PlanDefinitionApply(ctx, actor, req)
			if err != nil {
				t.Fatal(err)
			}
			var external []string
			for _, r := range plan.ExternalSatisfaction {
				external = append(external, r.ID)
				if r.Ownership != "foreign" || r.Comment != "external" {
					t.Fatalf("adopted record: %+v", r)
				}
			}
			if !reflect.DeepEqual(external, tc.foreignIDs) {
				t.Fatalf("external satisfaction: %v", external)
			}
			if plan.RequiresApproval != tc.conflict {
				t.Fatalf("approval flag: %+v", plan)
			}
			var deleted []string
			for _, c := range plan.Changes {
				if c.Action != "delete" || c.Before == nil {
					t.Fatalf("unexpected change: %+v", c)
				}
				deleted = append(deleted, c.Before.ID)
			}
			if tc.conflict && !reflect.DeepEqual(deleted, []string{"wrong-address", "wrong-proxy"}) {
				t.Fatalf("deletions: %v", deleted)
			}
			if !tc.conflict && len(deleted) != 0 {
				t.Fatalf("deletions: %v", deleted)
			}
			// Even an allegedly approved plan may not add a deletion that the
			// canonical classification excludes.
			forged := plan
			matching := plan.ExternalSatisfaction[0]
			forged.Changes = append(append([]DomainDNSChange{}, plan.Changes...), DomainDNSChange{Action: "delete", Type: matching.Type, Before: &matching})
			forged.RequiresApproval = true
			if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", req, forged, &definitionTestProgress{}); contract.Classify(err).Code != contract.PreconditionFailed {
				t.Fatalf("forged matching-foreign deletion: %v", err)
			}
			if countMutations(f.calls) != 0 {
				t.Fatalf("forged plan mutated provider: %v", f.calls)
			}
			if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", req, plan, &definitionTestProgress{}); err != nil {
				t.Fatal(err)
			}
			for _, id := range tc.foreignIDs {
				if _, ok := f.records[id]; !ok {
					t.Fatalf("matching foreign record %s removed", id)
				}
			}
			if countMutations(f.calls) != len(deleted) {
				t.Fatalf("unexpected mutations: %v", f.calls)
			}
		})
	}
}

func TestDefinitionDNSApprovalForOwnedChangesAndNotCreate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		record   *DNSRecord
		approval bool
		action   string
	}{
		{name: "create", action: "create"},
		{name: "obsolete owned", record: &DNSRecord{ID: "old", Type: "A", Content: "1.1.1.1", TTL: 1}, approval: true, action: "create"},
		{name: "owned proxy change", record: &DNSRecord{ID: "old", Type: "A", Content: "8.8.8.8", TTL: 1, Proxied: true}, approval: true, action: "update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCloudflareFixture(testZoneID)
			req := definitionRequest()
			if tc.record != nil {
				r := *tc.record
				r.Name = req.Host
				r.Comment = req.Ownership
				f.records[r.ID] = r
			}
			d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
			ctx := context.Background()
			actor := contract.Actor{Kind: "operator", Subject: "ops"}
			plan, err := d.PlanDefinitionApply(ctx, actor, req)
			if err != nil {
				t.Fatal(err)
			}
			if plan.RequiresApproval != tc.approval || len(plan.Changes) == 0 || plan.Changes[0].Action != tc.action {
				t.Fatalf("plan: %+v", plan)
			}
			tampered := plan
			tampered.RequiresApproval = !plan.RequiresApproval
			if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", req, tampered, &definitionTestProgress{}); contract.Classify(err).Code != contract.PreconditionFailed {
				t.Fatalf("tampered plan: %v", err)
			}
			if countMutations(f.calls) != 0 {
				t.Fatalf("tampered plan mutated provider: %v", f.calls)
			}
		})
	}
}

func TestDefinitionDNSReadFailureIsNotMissing(t *testing.T) {
	f := newCloudflareFixture(testZoneID)
	d := testDomainDNS(t, f, dnsAuthorizer(t), NewMemoryAudit())
	ctx, actor, req := context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, definitionRequest()
	p, err := d.PlanDefinitionApply(ctx, actor, req)
	if err != nil {
		t.Fatal(err)
	}
	f.status = http.StatusInternalServerError
	if _, err = d.PlanDefinitionApply(ctx, actor, req); err == nil {
		t.Fatal("failed provider read treated as empty")
	}
	if _, err = d.ExecuteApprovedDefinitionDNS(ctx, actor, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", req, p, &definitionTestProgress{}); err == nil {
		t.Fatal("failed provider read allowed execution")
	}
	if countMutations(f.calls) != 0 {
		t.Fatal(f.calls)
	}
}
