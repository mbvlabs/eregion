package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

type cloudflareFixture struct {
	mu      sync.Mutex
	zoneID  string
	records map[string]DNSRecord
	calls   []string
	nextID  int
	status  int
}

func newCloudflareFixture(zoneID string) *cloudflareFixture {
	return &cloudflareFixture{zoneID: zoneID, records: map[string]DNSRecord{}}
}

func (f *cloudflareFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testCloudflareToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		http.Error(w, "injected", f.status)
		return
	}
	base := "/client/v4/zones/" + f.zoneID + "/dns_records"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		f.calls = append(f.calls, "list")
		values := make([]DNSRecord, 0, len(f.records))
		for _, record := range f.records {
			values = append(values, record)
		}
		writeEnvelope(w, values, map[string]any{"page": 1, "total_pages": 1})
	case r.Method == http.MethodPost && r.URL.Path == base:
		f.calls = append(f.calls, "create")
		var record DNSRecord
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f.nextID++
		record.ID = fmt.Sprintf("rec%08d", f.nextID)
		f.records[record.ID] = record
		writeEnvelope(w, record, nil)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, base+"/"):
		f.calls = append(f.calls, "update")
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		if _, ok := f.records[id]; !ok {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		var record DNSRecord
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		record.ID = id
		f.records[id] = record
		writeEnvelope(w, record, nil)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/"):
		f.calls = append(f.calls, "delete")
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		delete(f.records, id)
		writeEnvelope(w, map[string]string{"id": id}, nil)
	default:
		http.NotFound(w, r)
	}
}

func writeEnvelope(w http.ResponseWriter, result any, info any) {
	w.Header().Set("Content-Type", "application/json")
	envelope := map[string]any{"success": true, "errors": []any{}, "result": result}
	if info != nil {
		envelope["result_info"] = info
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

const (
	testCloudflareToken = "cf-token-0123456789abcdef0123456789"
	testZoneID          = "0123456789abcdef0123456789abcdef"
)

func testCloudflare(t *testing.T, fixture *cloudflareFixture, authorizer Authorizer, audit AuditSink) (*Cloudflare, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client, err := NewCloudflare(CloudflareConfig{
		APIToken: testCloudflareToken, ZoneID: testZoneID, ZoneName: "example.com",
		BaseURL: server.URL + "/client/v4", HTTPClient: server.Client(),
		Authorizer: authorizer, Audit: audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func TestCloudflareReconcileIsManagedAndIdempotent(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	fixture.records["keep"] = DNSRecord{ID: "keep", Name: "same.example.com", Type: "A", Content: "10.0.0.1", TTL: 300, Proxied: false, Comment: "eregion:managed"}
	fixture.records["unmanaged"] = DNSRecord{ID: "unmanaged", Name: "other.example.com", Type: "TXT", Content: "manual", TTL: 300, Comment: "operator"}
	fixture.records["stale"] = DNSRecord{ID: "stale", Name: "old.example.com", Type: "CNAME", Content: "old.example.net", TTL: 300, Comment: "eregion:managed"}
	audit := NewMemoryAudit()
	authz, err := NewStaticAuthorizer(map[string][]string{"ops": {ActionDNSCreate, ActionDNSUpdate, ActionDNSDelete}})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := testCloudflare(t, fixture, authz, audit)
	desired := []DNSRecord{
		{Name: "same.example.com", Type: "A", Content: "10.0.0.1", TTL: 300},
		{Name: "new.example.com", Type: "CNAME", Content: "app.example.net", TTL: 60, Proxied: true, Comment: "route"},
	}
	actor := contract.Actor{Kind: "operator", Subject: "ops"}
	report, err := client.Reconcile(context.Background(), actor, desired)
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 2 || len(report.Changes) != 2 || report.Unchanged != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if _, ok := fixture.records["unmanaged"]; !ok {
		t.Fatal("unmanaged record was deleted")
	}
	if _, ok := fixture.records["stale"]; ok {
		t.Fatal("stale managed record was not deleted")
	}
	created := false
	for _, record := range fixture.records {
		if record.Name == "new.example.com" {
			created = true
			if record.Comment != "eregion:managed; route" || !record.Proxied {
				t.Fatalf("created record is wrong: %+v", record)
			}
		}
	}
	if !created {
		t.Fatal("desired record was not created")
	}
	mutations := countMutations(fixture.calls)
	if mutations != 2 {
		t.Fatalf("expected 2 mutations, got %v", fixture.calls)
	}
	fixture.mu.Lock()
	fixture.calls = nil
	fixture.mu.Unlock()
	report, err = client.Reconcile(context.Background(), actor, desired)
	if err != nil || report.Applied != 0 || report.Unchanged != 2 {
		t.Fatalf("second reconcile was not idempotent: %+v, %v", report, err)
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatalf("second reconcile mutated: %v", fixture.calls)
	}
	if len(audit.Entries()) < 4 {
		t.Fatalf("missing audit entries: %+v", audit.Entries())
	}
}

func countMutations(calls []string) int {
	count := 0
	for _, call := range calls {
		if call != "list" {
			count++
		}
	}
	return count
}

func TestCloudflareRefusesUnmanagedConflictAndUnauthorized(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	fixture.records["occupied"] = DNSRecord{ID: "occupied", Name: "app.example.com", Type: "CNAME", Content: "other.example.net", TTL: 300, Comment: "manual"}
	audit := NewMemoryAudit()
	authz, _ := NewStaticAuthorizer(map[string][]string{"ops": {ActionDNSCreate, ActionDNSUpdate, ActionDNSDelete}})
	client, _ := testCloudflare(t, fixture, authz, audit)
	desired := []DNSRecord{{Name: "app.example.com", Type: "CNAME", Content: "target.example.net", TTL: 300}}
	if _, err := client.Reconcile(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, desired); err == nil {
		t.Fatal("unmanaged conflict was overwritten")
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatal("conflict still mutated")
	}
	denied, _ := NewStaticAuthorizer(map[string][]string{"ops": {ActionDNSUpdate}})
	client, _ = testCloudflare(t, newCloudflareFixture(testZoneID), denied, NewMemoryAudit())
	if _, err := client.Reconcile(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, desired); err == nil {
		t.Fatal("unauthorized reconcile succeeded")
	}
}

func TestCloudflareAuditFailurePreventsMutation(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	authz, _ := NewStaticAuthorizer(map[string][]string{"ops": {ActionDNSCreate}})
	client, _ := testCloudflare(t, fixture, authz, failingAudit{})
	desired := []DNSRecord{{Name: "app.example.com", Type: "A", Content: "10.0.0.2", TTL: 300}}
	if _, err := client.Reconcile(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, desired); err == nil {
		t.Fatal("reconcile succeeded without an audit record")
	}
	if countMutations(fixture.calls) != 0 {
		t.Fatal("mutation happened before its audit intent")
	}
}

type failingAudit struct{}

func (failingAudit) Record(context.Context, AuditEntry) error {
	return contract.NewFault(contract.Unavailable, "audit unavailable", "")
}

func TestAccessGrantsFailClosedAndExpire(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	audit := NewMemoryAudit()
	controller, err := NewAccessController(AccessConfig{AdminSubjects: []string{"admin"}, Audit: audit, Store: NewMemoryGrantStore(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	admin := contract.Actor{Kind: "operator", Subject: "admin"}
	if _, err := controller.CreateGrant(context.Background(), contract.Actor{Kind: "operator", Subject: "other"}, GrantSpec{Subject: "alice", DeviceID: "laptop-1", Routes: []string{"app.internal/api"}}); err == nil {
		t.Fatal("non-admin created a grant")
	}
	grant, err := controller.CreateGrant(context.Background(), admin, GrantSpec{
		Subject: "alice", DeviceID: "laptop-1", Routes: []string{"app.internal/api", "10.0.0.0/8"}, ExpiresAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := controller.Authorize(context.Background(), "alice", "laptop-1", "app.internal/api")
	if err != nil || !decision.Allowed || decision.GrantID != grant.ID {
		t.Fatalf("grant did not authorize: %+v, %v", decision, err)
	}
	decision, _ = controller.Authorize(context.Background(), "alice", "laptop-1", "app.internal/admin")
	if decision.Allowed {
		t.Fatal("route not granted was allowed")
	}
	now = now.Add(3 * time.Hour)
	decision, _ = controller.Authorize(context.Background(), "alice", "laptop-1", "app.internal/api")
	if decision.Allowed {
		t.Fatal("expired grant was allowed")
	}
	if _, err := controller.RevokeGrant(context.Background(), admin, grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RevokeGrant(context.Background(), admin, grant.ID); err != nil {
		t.Fatal("idempotent revoke failed")
	}
	if grants, err := controller.ListGrants(context.Background(), "alice", ""); err != nil || len(grants) != 1 {
		t.Fatal("grant inventory lost revoked grant")
	}
}

func TestAccessAuditFailureDenies(t *testing.T) {
	controller, err := NewAccessController(AccessConfig{AdminSubjects: []string{"admin"}, Audit: failingAudit{}, Store: NewMemoryGrantStore()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.CreateGrant(context.Background(), contract.Actor{Kind: "operator", Subject: "admin"}, GrantSpec{Subject: "alice", DeviceID: "laptop-1", Routes: []string{"app.internal"}}); err == nil {
		t.Fatal("grant created without audit")
	}
	audit := NewMemoryAudit()
	controller, _ = NewAccessController(AccessConfig{AdminSubjects: []string{"admin"}, Audit: audit, Store: NewMemoryGrantStore()})
	grant, _ := controller.CreateGrant(context.Background(), contract.Actor{Kind: "operator", Subject: "admin"}, GrantSpec{Subject: "alice", DeviceID: "laptop-1", Routes: []string{"app.internal"}})
	_ = grant
	controller.audit = failingAudit{}
	decision, err := controller.Authorize(context.Background(), "alice", "laptop-1", "app.internal")
	if err == nil || decision.Allowed {
		t.Fatal("access was allowed when its audit failed")
	}
}

func TestIntegrationActionsConfiguredAndSecretFree(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	server := httptest.NewServer(fixture)
	defer server.Close()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "cloudflare-token")
	if err := os.WriteFile(tokenPath, []byte(testCloudflareToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	audit := NewMemoryAudit()
	authz, _ := NewStaticAuthorizer(map[string][]string{"ops": {ActionDNSCreate, ActionDNSUpdate, ActionDNSDelete}})
	access, err := NewAccessController(AccessConfig{AdminSubjects: []string{"ops"}, Audit: audit, Store: NewMemoryGrantStore()})
	if err != nil {
		t.Fatal(err)
	}
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{
		Authorizer: authz, Audit: audit, Access: access,
		DNSZones:          []DNSZone{{ZoneID: testZoneID, ZoneName: "example.com", TokenFile: tokenPath}},
		CloudflareBaseURL: server.URL + "/client/v4", CloudflareClient: server.Client(),
	}); err != nil {
		t.Fatal(err)
	}
	dnsRaw, _ := json.Marshal(map[string]any{
		"zone_id": testZoneID, "zone_name": "example.com", "api_token_file": tokenPath,
		"records": []DNSRecord{{Name: "app.example.com", Type: "A", Content: "10.0.0.3", TTL: 300}},
	})
	normalized, err := registry.Normalize(KindDNSReconcile, "platform", dnsRaw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(normalized.Canonical), testCloudflareToken) {
		t.Fatal("credential appeared in canonical action input")
	}
	result, err := normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1,
		Kind: KindDNSReconcile, Target: "platform", Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.records) != 1 {
		t.Fatal("DNS action did not create the record")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), testCloudflareToken) {
		t.Fatal("action result leaked the Cloudflare token")
	}

	grantRaw, _ := json.Marshal(map[string]any{
		"subject": "alice", "device_id": "laptop-1", "routes": []string{"app.internal/api"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	normalized, err = registry.Normalize(KindAccessGrant, "platform", grantRaw)
	if err != nil {
		t.Fatal(err)
	}
	execution := platform.Execution{
		OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1,
		Kind: KindAccessGrant, Target: "platform", Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	}
	result, err = normalized.Executor().Execute(context.Background(), execution)
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(map[string]any)
	grantID, _ := data["id"].(string)
	revokeRaw, _ := json.Marshal(map[string]string{"grant_id": grantID})
	normalized, err = registry.Normalize(KindAccessRevoke, "platform", revokeRaw)
	if err != nil {
		t.Fatal(err)
	}
	execution.Kind = KindAccessRevoke
	execution.Input = normalized.Canonical
	if _, err := normalized.Executor().Execute(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	provider, ok := registry.InventoryProvider(InventoryCollection)
	if !ok {
		t.Fatal("access inventory is not registered")
	}
	inventory, err := provider.Read(context.Background(), platform.InventoryQuery{Collection: InventoryCollection, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(inventory)
	if !strings.Contains(string(encoded), grantID) {
		t.Fatal("access inventory omitted the grant")
	}
}

func TestDNSInventoryReadsConfiguredZonesWithoutMutationRuntime(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	fixture.records["existing"] = DNSRecord{ID: "existing", Name: "app.example.com", Type: "A", Content: "10.0.0.4", TTL: 300, Comment: "eregion:managed"}
	server := httptest.NewServer(fixture)
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(tokenPath, []byte(testCloudflareToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{
		DNSZones:          []DNSZone{{ZoneID: testZoneID, ZoneName: "example.com", TokenFile: tokenPath}},
		CloudflareBaseURL: server.URL + "/client/v4", CloudflareClient: server.Client(),
	}); err != nil {
		t.Fatal(err)
	}
	provider, ok := registry.InventoryProvider(InventoryCollectionDNS)
	if !ok {
		t.Fatal("dns inventory is not registered")
	}
	inventory, err := provider.Read(context.Background(), platform.InventoryQuery{Collection: InventoryCollectionDNS, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	page, ok := inventory.(platform.Inventory)
	if !ok {
		t.Fatalf("unexpected inventory type %T", inventory)
	}
	routes, ok := page.Items.([]DNSRoute)
	if !ok || len(routes) != 1 || routes[0].Name != "app.example.com" || routes[0].ManagedBy != "eregion" {
		t.Fatalf("unexpected DNS inventory: %#v", page.Items)
	}
	encoded, _ := json.Marshal(inventory)
	if strings.Contains(string(encoded), testCloudflareToken) {
		t.Fatal("DNS inventory leaked the API token")
	}
	if err := Configure(platform.NewRegistry(), RuntimeConfig{DNSZones: []DNSZone{{ZoneID: "bad", ZoneName: "example.com", TokenFile: tokenPath}}}); err == nil {
		t.Fatal("invalid DNS inventory zone was accepted")
	}
}

func TestIntegrationActionWithoutRuntimeFailsClosed(t *testing.T) {
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"subject":"alice","device_id":"laptop-1","routes":["app.internal"]}`)
	normalized, err := registry.Normalize(KindAccessGrant, "platform", raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1,
		Kind: KindAccessGrant, Target: "platform", Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	})
	if err == nil || contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unconfigured action did not fail closed: %v", err)
	}
}
