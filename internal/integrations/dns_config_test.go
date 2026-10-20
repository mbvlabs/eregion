package integrations

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestDNSActionsFailClosedWithoutConfiguredZones(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(tokenPath, []byte(testCloudflareToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	audit := NewMemoryAudit()
	authz, _ := NewStaticAuthorizer(map[string][]string{"ops": {ActionDNSCreate}})
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{Authorizer: authz, Audit: audit}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"zone_id": testZoneID, "zone_name": "example.com", "api_token_file": tokenPath,
		"records": []DNSRecord{{Name: "app.example.com", Type: "A", Content: "10.0.0.3", TTL: 300}},
	})
	normalized, err := registry.Normalize(KindDNSReconcile, "platform", raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1,
		Kind: KindDNSReconcile, Target: "platform", Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	})
	if contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("reconcile without configured zones = %v", err)
	}
	provider, ok := registry.InventoryProvider(InventoryCollectionDNS)
	if !ok {
		t.Fatal("dns inventory is not registered")
	}
	if _, err := provider.Read(context.Background(), platform.InventoryQuery{Collection: InventoryCollectionDNS}); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("dns inventory without configured zones = %v", err)
	}
}

func TestDNSReconcileRejectsAnUnconfiguredZoneBeforeMutation(t *testing.T) {
	fixture := newCloudflareFixture(testZoneID)
	server := httptest.NewServer(fixture)
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(tokenPath, []byte(testCloudflareToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	audit := NewMemoryAudit()
	authz, _ := NewStaticAuthorizer(map[string][]string{"ops": {ActionDNSCreate}})
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{
		Authorizer: authz, Audit: audit,
		DNSZones:          []DNSZone{{ZoneID: testZoneID, ZoneName: "example.com", TokenFile: tokenPath}},
		CloudflareBaseURL: server.URL + "/client/v4", CloudflareClient: server.Client(),
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"zone_id": "ffffffffffffffffffffffffffffffff", "zone_name": "other.example", "api_token_file": tokenPath,
		"records": []DNSRecord{{Name: "app.other.example", Type: "A", Content: "10.0.0.3", TTL: 300}},
	})
	normalized, err := registry.Normalize(KindDNSReconcile, "platform", raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Attempt: 1,
		Kind: KindDNSReconcile, Target: "platform", Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	})
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unconfigured zone = %v", err)
	}
	if len(fixture.records) != 0 {
		t.Fatal("an unconfigured zone reached the Cloudflare API")
	}
}
