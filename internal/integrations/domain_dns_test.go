package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

const testDomainID = "dom_11111111111111111111111111111111"

func testDomainDNS(t *testing.T, fixture *cloudflareFixture, authorizer Authorizer, audit AuditSink) *DomainDNS {
	t.Helper()
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	store := newTestDNSStore(DNSZone{ZoneID: testZoneID, ZoneName: "example.com"}).withToken(testZoneID, testCloudflareToken)
	domainDNS, err := NewDomainDNS(DomainDNSConfig{
		Zones:       store,
		Credentials: store,
		BaseURL:     server.URL + "/client/v4",
		HTTPClient:  server.Client(),
		Authorizer:  authorizer,
		Audit:       audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return domainDNS
}

func dnsAuthorizer(t *testing.T) Authorizer {
	t.Helper()
	return NewOperatorDNSAuthorizer()
}

func TestDomainDNSOwnershipIsImmutableIdentityNotNames(t *testing.T) {
	first := DomainDNSOwnership("dom_11111111111111111111111111111111", testZoneID)
	if first != DomainDNSOwnership("dom_11111111111111111111111111111111", testZoneID) {
		t.Fatal("ownership marker is not stable for one binding")
	}
	second := DomainDNSOwnership("dom_22222222222222222222222222222222", testZoneID)
	if first == second {
		t.Fatal("recreated binding identity reused the previous ownership marker")
	}
	if first == DomainDNSOwnership("dom_11111111111111111111111111111111", "fedcba9876543210fedcba9876543210") {
		t.Fatal("ownership marker ignores the provider zone")
	}
}

func TestDomainDNSAuthorizationIsOperatorOnlyAndPrecedesCredentialResolution(t *testing.T) {
	store := newTestDNSStore(DNSZone{ZoneID: testZoneID, ZoneName: "example.com"}).withToken(testZoneID, testCloudflareToken)
	domainDNS, err := NewDomainDNS(DomainDNSConfig{
		Zones: store, Credentials: store,
		Authorizer: NewOperatorDNSAuthorizer(),
		Audit:      NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	operator := contract.Actor{Kind: "operator", Subject: "ops"}
	if err := domainDNS.AuthorizeRead(context.Background(), operator); err != nil {
		t.Fatalf("operator read authorization = %v", err)
	}
	if err := domainDNS.AuthorizeWrite(context.Background(), operator); err != nil {
		t.Fatalf("operator write authorization = %v", err)
	}
	if err := domainDNS.AuthorizeRead(context.Background(), contract.Actor{Kind: "operator", Subject: "second"}); err != nil {
		t.Fatalf("second operator authorization = %v", err)
	}
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"8.8.8.8"},
	}
	for _, kind := range []string{"ci", "webhook", "system"} {
		actor := contract.Actor{Kind: kind, Subject: "ops"}
		if err := domainDNS.AuthorizeRead(context.Background(), actor); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("%s read authorization = %v", kind, err)
		}
		if _, err := domainDNS.Apply(context.Background(), actor, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("%s apply authorization = %v", kind, err)
		}
		if _, err := domainDNS.Remove(context.Background(), actor, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("%s remove authorization = %v", kind, err)
		}
	}
	if _, reads := store.counts(); reads != 0 {
		t.Fatalf("denied actors resolved the provider credential %d times", reads)
	}
	if err := NewOperatorDNSAuthorizer().Authorize(context.Background(), operator, "dns.unknown", "cloudflare"); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("unknown action authorization = %v", err)
	}
}

func TestValidateDomainDNSHostBelongsToConfiguredZone(t *testing.T) {
	zone := DNSZone{ZoneID: testZoneID, ZoneName: "example.com"}
	for _, host := range []string{"example.com", "app.example.com"} {
		if err := ValidateDomainDNSHost(zone, host); err != nil {
			t.Fatalf("valid host %q = %v", host, err)
		}
	}
	for _, host := range []string{"app.other.example", "badexample.com", "app.example.com.evil"} {
		if err := ValidateDomainDNSHost(zone, host); contract.Classify(err).Code != contract.ValidationFailed {
			t.Fatalf("outside-zone host %q = %v", host, err)
		}
	}
}

func TestDomainDNSUnconfiguredZoneFailsBeforeCredentialResolution(t *testing.T) {
	store := newTestDNSStore()
	domainDNS, err := NewDomainDNS(DomainDNSConfig{
		Zones: store, Credentials: store,
		Authorizer: NewOperatorDNSAuthorizer(),
		Audit:      NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"8.8.8.8"},
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("unconfigured zone = %v", err)
	}
	if _, reads := store.counts(); reads != 0 {
		t.Fatalf("unconfigured zone resolved the provider credential %d times", reads)
	}
}

func TestDomainDNSResolvesTheActiveCredentialForEachOperation(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	server := httptest.NewServer(fixture)
	defer server.Close()
	store := newTestDNSStore(DNSZone{ZoneID: testZoneID, ZoneName: "example.com"}).withToken(testZoneID, testCloudflareToken)
	domainDNS, err := NewDomainDNS(DomainDNSConfig{
		Zones: store, Credentials: store,
		BaseURL: server.URL + "/client/v4", HTTPClient: server.Client(),
		Authorizer: NewOperatorDNSAuthorizer(), Audit: NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"10.0.0.2"},
	}
	if _, err := domainDNS.PlanApply(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.tokens[testZoneID] = testCloudflareToken
	store.mu.Unlock()
	if _, err := domainDNS.PlanApply(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, call := range fixture.calls {
		if call != "list" {
			t.Fatalf("plan mutated the provider: %v", fixture.calls)
		}
	}
	if _, reads := store.counts(); reads != 2 {
		t.Fatalf("credential reads = %d, want one per operation", reads)
	}
}

func TestDomainDNSPlanIsReadOnlyAndScoped(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned"] = DNSRecord{ID: "owned", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	fixture.records["txt"] = DNSRecord{ID: "txt", Name: "app.example.com", Type: "TXT", Content: "verification", TTL: 1, Comment: "operator"}
	fixture.records["other"] = DNSRecord{ID: "other", Name: "other.example.com", Type: "A", Content: "10.0.0.9", TTL: 1, Comment: "eregion:managed"}

	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	plan, err := domainDNS.PlanApply(context.Background(), DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"10.0.0.2", "2001:db8::2"}, Proxied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Records) != 2 {
		t.Fatalf("snapshot = %+v", plan.Records)
	}
	for _, record := range plan.Records {
		if record.Type == "TXT" {
			if record.Content != "" || record.Comment != "" || record.TTL != 0 || record.Proxied {
				t.Fatalf("foreign TXT content was not minimized: %+v", record)
			}
		}
		if record.Type == "A" && record.Content != "10.0.0.1" {
			t.Fatalf("owned record content was redacted: %+v", record)
		}
	}
	if plan.Ownership != marker {
		t.Fatalf("ownership = %q, want %q", plan.Ownership, marker)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", plan.Conflicts)
	}
	if len(plan.Changes) != 2 || plan.Changes[0].Action != "update" || plan.Changes[1].Action != "create" {
		t.Fatalf("changes = %+v", plan.Changes)
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("plan mutated Cloudflare: %v", fixture.calls)
	}
	if _, ok := fixture.records["other"]; !ok {
		t.Fatal("unrelated record disappeared during planning")
	}
	encoded, _ := json.Marshal(plan)
	if strings.Contains(string(encoded), testCloudflareToken) {
		t.Fatal("plan leaked the Cloudflare token")
	}
}

func TestDomainDNSApplyIsIdempotentAndPreservesUnrelatedRecords(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned"] = DNSRecord{ID: "owned", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Proxied: false, Comment: marker}
	fixture.records["txt"] = DNSRecord{ID: "txt", Name: "app.example.com", Type: "TXT", Content: "verification", TTL: 1, Comment: "operator"}
	fixture.records["other"] = DNSRecord{ID: "other", Name: "other.example.com", Type: "A", Content: "10.0.0.9", TTL: 1, Comment: "eregion:managed"}
	audit := NewMemoryAudit()
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), audit)
	request := DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"10.0.0.2", "2001:db8::2"}, Proxied: true}
	plan, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied != 2 {
		t.Fatalf("applied = %d, want 2", plan.Applied)
	}
	if _, ok := fixture.records["other"]; !ok {
		t.Fatal("unrelated domain record was deleted")
	}
	if fixture.records["txt"].Content != "verification" {
		t.Fatal("unrelated TXT record was changed")
	}
	if len(audit.Entries()) < 3 {
		t.Fatalf("missing audit entries: %+v", audit.Entries())
	}
	for _, entry := range audit.Entries() {
		if strings.Contains(entry.Detail, testCloudflareToken) || strings.Contains(entry.Target, testCloudflareToken) {
			t.Fatal("audit entry leaked the Cloudflare token")
		}
	}

	fixture.mu.Lock()
	fixture.calls = nil
	fixture.mu.Unlock()
	plan, err = domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied != 0 || plan.Unchanged != 2 {
		t.Fatalf("second apply was not idempotent: %+v", plan)
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("second apply mutated: %v", fixture.calls)
	}
}

func TestDomainDNSHandlesMultipleAddressesOfOneType(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned"] = DNSRecord{ID: "owned", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com",
		Addresses: []string{"10.0.0.1", "10.0.0.2"}, Proxied: false,
	}
	plan, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied != 1 || plan.Unchanged != 1 || len(plan.Changes) != 1 || plan.Changes[0].Action != "create" {
		t.Fatalf("multi-address plan = %+v", plan)
	}
	addresses := map[string]bool{}
	for _, record := range fixture.records {
		if record.Name == "app.example.com" && record.Type == "A" {
			addresses[record.Content] = true
		}
	}
	if !addresses["10.0.0.1"] || !addresses["10.0.0.2"] {
		t.Fatalf("addresses = %+v", addresses)
	}
	plan, err = domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request)
	if err != nil || plan.Applied != 0 || plan.Unchanged != 2 {
		t.Fatalf("second multi-address apply = %+v, %v", plan, err)
	}
	plan, err = domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_cccccccccccccccccccccccccccccccc", DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com"})
	if err != nil || plan.Applied != 2 {
		t.Fatalf("multi-address removal = %+v, %v", plan, err)
	}
	if len(fixture.records) != 0 {
		t.Fatalf("removal left records: %+v", fixture.records)
	}
}

func TestDomainDNSMutationGuardStopsBetweenMultiRecordChanges(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com",
		Addresses: []string{"10.0.0.1", "10.0.0.2"}, Proxied: false,
	}
	stop := errors.New("cancel at record boundary")
	guardCalls := 0
	guard := func(context.Context) error {
		guardCalls++
		if guardCalls == 2 {
			return stop
		}
		return nil
	}
	plan, err := domainDNS.ApplyWithGuard(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request, guard)
	if !errors.Is(err, stop) || guardCalls != 2 {
		t.Fatalf("guarded apply = %+v, calls=%d, err=%v", plan, guardCalls, err)
	}
	if countMutations(fixture.calls) != 1 {
		t.Fatalf("guarded apply mutations = %v, want exactly one record before the stop", fixture.calls)
	}
	fixture.mu.Lock()
	applied := 0
	for _, record := range fixture.records {
		if record.Name == "app.example.com" && record.Type == "A" {
			applied++
		}
	}
	fixture.mu.Unlock()
	if applied != 1 {
		t.Fatalf("guarded apply left %d records, want one convergent partial record", applied)
	}
	plan, err = domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request)
	if err != nil || plan.Applied != 1 || plan.Unchanged != 1 {
		t.Fatalf("retry after guarded apply = %+v, %v", plan, err)
	}

	fixture = newCloudflareFixture(testZoneID)
	domainDNS = testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned1"] = DNSRecord{ID: "owned1", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	fixture.records["owned2"] = DNSRecord{ID: "owned2", Name: "app.example.com", Type: "A", Content: "10.0.0.2", TTL: 1, Comment: marker}
	guardCalls = 0
	guard = func(context.Context) error {
		guardCalls++
		if guardCalls == 2 {
			return stop
		}
		return nil
	}
	removeRequest := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com",
	}
	removePlan, err := domainDNS.RemoveWithGuard(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_cccccccccccccccccccccccccccccccc", removeRequest, guard)
	if !errors.Is(err, stop) || guardCalls != 2 {
		t.Fatalf("guarded remove = %+v, calls=%d, err=%v", removePlan, guardCalls, err)
	}
	fixture.mu.Lock()
	remainingOwned := 0
	for _, record := range fixture.records {
		if record.Name == "app.example.com" && record.Type == "A" {
			remainingOwned++
		}
	}
	fixture.mu.Unlock()
	if remainingOwned != 1 {
		t.Fatalf("guarded remove left %d records, want one retryable record", remainingOwned)
	}
	removePlan, err = domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_dddddddddddddddddddddddddddddddd", removeRequest)
	if err != nil || removePlan.Applied != 1 {
		t.Fatalf("retry after guarded remove = %+v, %v", removePlan, err)
	}
}

func TestDomainDNSReplacementCreatesBeforeDeletingOldAddress(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["old"] = DNSRecord{ID: "old", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com", Addresses: []string{"2001:4860:4860::8888"},
	}
	plan, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied != 2 {
		t.Fatalf("replacement plan = %+v", plan)
	}
	fixture.mu.Lock()
	mutations := make([]string, 0, len(fixture.calls))
	for _, call := range fixture.calls {
		if call != "list" {
			mutations = append(mutations, call)
		}
	}
	remaining := make([]DNSRecord, 0, len(fixture.records))
	for _, record := range fixture.records {
		remaining = append(remaining, record)
	}
	fixture.mu.Unlock()
	if len(mutations) != 2 || mutations[0] != "create" || mutations[1] != "delete" {
		t.Fatalf("replacement mutation order = %v, want create then delete", mutations)
	}
	if len(remaining) != 1 || remaining[0].Type != "AAAA" || remaining[0].Content != "2001:4860:4860::8888" {
		t.Fatalf("replacement records = %+v", remaining)
	}
}

func TestDomainDNSReplacementCreateFailureKeepsOldAddress(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["old"] = DNSRecord{ID: "old", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	fixture.mu.Lock()
	fixture.failCreate = true
	fixture.mu.Unlock()
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com", Addresses: []string{"2001:4860:4860::8888"},
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); err == nil {
		t.Fatal("replacement create failure was accepted")
	}
	fixture.mu.Lock()
	_, oldIntact := fixture.records["old"]
	mutations := 0
	for _, call := range fixture.calls {
		if call != "list" {
			mutations++
		}
	}
	fixture.mu.Unlock()
	if !oldIntact {
		t.Fatal("old address was deleted after the replacement create failed")
	}
	if mutations != 1 {
		t.Fatalf("failed replacement mutations = %d, want only the failed create", mutations)
	}
	fixture.mu.Lock()
	fixture.failCreate = false
	fixture.mu.Unlock()
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request); err != nil {
		t.Fatalf("retry after create failure = %v", err)
	}
	fixture.mu.Lock()
	remaining := make([]DNSRecord, 0, len(fixture.records))
	for _, record := range fixture.records {
		remaining = append(remaining, record)
	}
	fixture.mu.Unlock()
	if len(remaining) != 1 || remaining[0].Type != "AAAA" {
		t.Fatalf("retry replacement records = %+v", remaining)
	}
}

func TestDomainDNSReplacementUnverifiedCreateKeepsOldAddress(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["old"] = DNSRecord{ID: "old", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	fixture.mu.Lock()
	fixture.dropCreate = true
	fixture.mu.Unlock()
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com", Addresses: []string{"2001:4860:4860::8888"},
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unverified replacement error = %v", err)
	}
	fixture.mu.Lock()
	_, oldIntact := fixture.records["old"]
	mutations := 0
	for _, call := range fixture.calls {
		if call != "list" {
			mutations++
		}
	}
	fixture.dropCreate = false
	fixture.mu.Unlock()
	if !oldIntact {
		t.Fatal("old address was deleted after the replacement could not be verified")
	}
	if mutations != 1 {
		t.Fatalf("unverified replacement mutations = %d, want only the create", mutations)
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request); err != nil {
		t.Fatalf("retry after unverified create = %v", err)
	}
	fixture.mu.Lock()
	remaining := make([]DNSRecord, 0, len(fixture.records))
	for _, record := range fixture.records {
		remaining = append(remaining, record)
	}
	fixture.mu.Unlock()
	if len(remaining) != 1 || remaining[0].Type != "AAAA" {
		t.Fatalf("retry after unverified create records = %+v", remaining)
	}
}

func TestDomainDNSReplacementDeleteFailureRetainsBothUntilRetry(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["old"] = DNSRecord{ID: "old", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	fixture.mu.Lock()
	fixture.failDelete = true
	fixture.mu.Unlock()
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com", Addresses: []string{"2001:4860:4860::8888"},
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); err == nil {
		t.Fatal("replacement delete failure was accepted")
	}
	fixture.mu.Lock()
	_, oldIntact := fixture.records["old"]
	newPresent := false
	for _, record := range fixture.records {
		if record.Type == "AAAA" && record.Content == "2001:4860:4860::8888" {
			newPresent = true
		}
	}
	fixture.failDelete = false
	fixture.mu.Unlock()
	if !oldIntact || !newPresent {
		t.Fatalf("partial replacement = old=%t new=%t, want both until retry", oldIntact, newPresent)
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request); err != nil {
		t.Fatalf("retry after delete failure = %v", err)
	}
	fixture.mu.Lock()
	remaining := make([]DNSRecord, 0, len(fixture.records))
	for _, record := range fixture.records {
		remaining = append(remaining, record)
	}
	fixture.mu.Unlock()
	if len(remaining) != 1 || remaining[0].Type != "AAAA" {
		t.Fatalf("retry final records = %+v", remaining)
	}
}

func TestDomainDNSRemoveFailsClosedWhenOwnedRecordReappears(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned"] = DNSRecord{ID: "owned", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1, Comment: marker}
	fixture.mu.Lock()
	fixture.reappearDelete = true
	fixture.mu.Unlock()
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com",
	}
	if _, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("reappeared record removal = %v", err)
	}
	fixture.mu.Lock()
	remaining := len(fixture.records)
	fixture.reappearDelete = false
	fixture.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("reappeared record was not observed: %+v", fixture.records)
	}
	if _, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request); err != nil {
		t.Fatalf("retry after reappearance = %v", err)
	}
	fixture.mu.Lock()
	remaining = len(fixture.records)
	fixture.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("retry left records: %+v", fixture.records)
	}
}

func TestDomainDNSRemoveFailsClosedWhenPostDeleteReadFails(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned"] = DNSRecord{ID: "owned", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1, Comment: marker}
	fixture.mu.Lock()
	fixture.failReadAfterDelete = true
	fixture.mu.Unlock()
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com",
	}
	if _, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("post-delete read failure removal = %v", err)
	}
	fixture.mu.Lock()
	fixture.status = 0
	fixture.failReadAfterDelete = false
	fixture.mu.Unlock()
	if _, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request); err != nil {
		t.Fatalf("retry after post-delete read failure = %v", err)
	}
	fixture.mu.Lock()
	remaining := len(fixture.records)
	fixture.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("retry left records: %+v", fixture.records)
	}
}

func TestDomainDNSRedactsForeignCNAMEContentAndComment(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	fixture.records["foreign"] = DNSRecord{
		ID: "foreign", Name: "app.example.com", Type: "CNAME",
		Content: "verification-token.internal.example", TTL: 300, Proxied: true,
		Comment: "contains-private-verification-data",
	}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	plan, err := domainDNS.PlanApply(context.Background(), DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com",
		Addresses: []string{"8.8.8.8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) == 0 || len(plan.Records) != 1 {
		t.Fatalf("foreign CNAME plan = %+v", plan)
	}
	record := plan.Records[0]
	if record.Content != "" || record.Comment != "" || record.TTL != 0 || record.Proxied {
		t.Fatalf("foreign CNAME fields were not redacted: %+v", record)
	}
	if strings.Contains(mustJSON(plan), "verification-token") || strings.Contains(mustJSON(plan), "contains-private") {
		t.Fatal("foreign CNAME content or comment leaked through the plan")
	}
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestDomainDNSExactMarkerCNAMEIsNeverOwned(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["cname"] = DNSRecord{
		ID: "cname", Name: "app.example.com", Type: "CNAME",
		Content: "verification-token.internal.example", TTL: 60,
		Comment: marker,
	}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com", Addresses: []string{"8.8.8.8"},
	}
	plan, err := domainDNS.PlanApply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) == 0 || len(plan.Records) != 1 || plan.Records[0].Ownership == "owned" {
		t.Fatalf("exact-marker CNAME plan = %+v", plan)
	}
	if plan.Records[0].Content != "" || plan.Records[0].Comment != "" {
		t.Fatalf("exact-marker CNAME content was not redacted: %+v", plan.Records[0])
	}
	if strings.Contains(mustJSON(plan), "verification-token") {
		t.Fatal("exact-marker CNAME content leaked through the plan")
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("exact-marker CNAME apply = %v", err)
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("exact-marker CNAME apply mutated: %v", fixture.calls)
	}
	removeRequest := DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID,
		Ownership: marker, Host: "app.example.com",
	}
	removePlan, err := domainDNS.PlanRemove(context.Background(), removeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(removePlan.Conflicts) != 0 || len(removePlan.Changes) != 0 {
		t.Fatalf("exact-marker CNAME removal plan = %+v", removePlan)
	}
	if _, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", removeRequest); err != nil {
		t.Fatalf("exact-marker CNAME removal = %v", err)
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("exact-marker CNAME removal mutated: %v", fixture.calls)
	}
	if _, ok := fixture.records["cname"]; !ok {
		t.Fatal("exact-marker CNAME was deleted")
	}
}

func TestDomainDNSRefusesAmbiguousDuplicateOwnedRecords(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["one"] = DNSRecord{ID: "one", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	fixture.records["two"] = DNSRecord{ID: "two", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"10.0.0.1"}, Proxied: false}
	plan, err := domainDNS.PlanApply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) == 0 {
		t.Fatalf("ambiguous duplicate was not a conflict: %+v", plan)
	}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("ambiguous apply error = %v", err)
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("ambiguous apply mutated: %v", fixture.calls)
	}
}

func TestDomainDNSNeverAdoptsForeignOrLegacyManagedRecords(t *testing.T) {
	for _, comment := range []string{"manual", "eregion:managed", DomainDNSOwnership("dom_22222222222222222222222222222222", testZoneID)} {
		t.Run(comment, func(t *testing.T) {
			fixture := newCloudflareFixture(testZoneID)
			fixture.records["foreign"] = DNSRecord{ID: "foreign", Name: "app.example.com", Type: "A", Content: "10.0.0.9", TTL: 1, Comment: comment}
			domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
			request := DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"10.0.0.2"}, Proxied: false}
			plan, err := domainDNS.PlanApply(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Conflicts) == 0 {
				t.Fatalf("foreign record was not a conflict: %+v", plan)
			}
			if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.PreconditionFailed {
				t.Fatalf("apply error = %v", err)
			}
			if countMutations(fixture.calls) != 0 {
				t.Fatalf("conflicting apply mutated: %v", fixture.calls)
			}
			if fixture.records["foreign"].Content != "10.0.0.9" {
				t.Fatal("foreign record was overwritten")
			}
		})
	}
}

func TestDomainDNSRemoveDeletesOnlyExactOwnedRecords(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned"] = DNSRecord{ID: "owned", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1, Comment: marker}
	fixture.records["txt"] = DNSRecord{ID: "txt", Name: "app.example.com", Type: "TXT", Content: "verification", TTL: 1, Comment: "operator"}
	fixture.records["other"] = DNSRecord{ID: "other", Name: "other.example.com", Type: "A", Content: "10.0.0.9", TTL: 1, Comment: "eregion:managed"}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com"}
	plan, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied != 1 || len(plan.Changes) != 1 || plan.Changes[0].Type != "A" {
		t.Fatalf("remove plan = %+v", plan)
	}
	if _, ok := fixture.records["owned"]; ok {
		t.Fatal("owned record was not removed")
	}
	if fixture.records["txt"].Content != "verification" {
		t.Fatal("unrelated TXT record was changed")
	}
	if _, ok := fixture.records["other"]; !ok {
		t.Fatal("unrelated domain record was deleted")
	}
	plan, err = domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", request)
	if err != nil || plan.Applied != 0 || len(plan.Changes) != 0 {
		t.Fatalf("second remove was not idempotent: %+v, %v", plan, err)
	}
}

func TestDomainDNSRemovePreservesForeignAddressRecord(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	fixture.records["owned"] = DNSRecord{ID: "owned", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1, Comment: marker}
	fixture.records["foreign"] = DNSRecord{ID: "foreign", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1, Comment: "operator"}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	_, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com"})
	if err != nil {
		t.Fatalf("remove error = %v", err)
	}
	if countMutations(fixture.calls) != 1 {
		t.Fatalf("expected only owned deletion: %v", fixture.calls)
	}
	if _, ok := fixture.records["owned"]; ok {
		t.Fatal("owned record was not deleted")
	}
	if _, ok := fixture.records["foreign"]; !ok {
		t.Fatal("foreign record was deleted")
	}
}

func TestDomainDNSRemoveCleansDuplicateOwnedRecords(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	marker := DomainDNSOwnership(testDomainID, testZoneID)
	for _, id := range []string{"one", "two"} {
		fixture.records[id] = DNSRecord{ID: id, Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1, Comment: marker}
	}
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	request := DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: marker, Host: "app.example.com"}
	plan, err := domainDNS.Remove(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request)
	if err != nil || plan.Applied != 2 || len(fixture.records) != 0 {
		t.Fatalf("duplicate owned cleanup = %+v, records=%+v, err=%v", plan, fixture.records, err)
	}
}

func TestDomainDNSAuthorizationAndAuditFailClosed(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	domainDNS := testDomainDNS(t, fixture, NewOperatorDNSAuthorizer(), NewMemoryAudit())
	request := DomainDNSRequest{ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com", Addresses: []string{"10.0.0.2"}, Proxied: false}
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "ci", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("authorization error = %v", err)
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("denied apply mutated: %v", fixture.calls)
	}

	fixture = newCloudflareFixture(testZoneID)
	domainDNS = testDomainDNS(t, fixture, dnsAuthorizer(t), failingAudit{})
	if _, err := domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request); err == nil {
		t.Fatal("apply succeeded without a durable audit intent")
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("audit failure mutated: %v", fixture.calls)
	}
}

func TestDomainDNSRejectsHostOutsideConfiguredZone(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	domainDNS := testDomainDNS(t, fixture, dnsAuthorizer(t), NewMemoryAudit())
	_, err := domainDNS.PlanApply(context.Background(), DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.other.example", Addresses: []string{"10.0.0.2"},
	})
	if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("cross-zone error = %v", err)
	}
	if len(fixture.calls) != 0 {
		t.Fatalf("cross-zone plan reached Cloudflare: %v", fixture.calls)
	}
}

type domainDNSReauthFixture struct {
	mu        sync.Mutex
	records   []DNSRecord
	lists     int
	mutations int
}

func (f *domainDNSReauthFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.mutations++
		http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		return
	}
	f.lists++
	if f.lists == 2 && len(f.records) > 0 {
		f.records[0].Comment = "operator"
	}
	writeEnvelope(w, f.records, map[string]any{"page": 1, "total_pages": 1})
}

func TestDomainDNSRechecksOwnershipImmediatelyBeforeMutation(t *testing.T) {
	fixture := &domainDNSReauthFixture{records: []DNSRecord{{
		ID: "owned", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 1,
		Comment: DomainDNSOwnership(testDomainID, testZoneID),
	}}}
	server := httptest.NewServer(fixture)
	defer server.Close()
	store := newTestDNSStore(DNSZone{ZoneID: testZoneID, ZoneName: "example.com"}).withToken(testZoneID, testCloudflareToken)
	domainDNS, err := NewDomainDNS(DomainDNSConfig{
		Zones: store, Credentials: store,
		BaseURL:    server.URL + "/client/v4",
		HTTPClient: server.Client(),
		Authorizer: dnsAuthorizer(t),
		Audit:      NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = domainDNS.Apply(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DomainDNSRequest{
		ZoneID: testZoneID, ZoneName: "example.com", DomainID: testDomainID, Ownership: DomainDNSOwnership(testDomainID, testZoneID), Host: "app.example.com",
		Addresses: []string{"10.0.0.2"}, Proxied: false,
	})
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("ownership recheck error = %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.mutations != 0 || fixture.records[0].Content != "10.0.0.1" {
		t.Fatalf("mutation before ownership recheck: %+v mutations=%d", fixture.records, fixture.mutations)
	}
}
