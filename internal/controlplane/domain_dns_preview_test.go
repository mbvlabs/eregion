package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

type domainDNSPreviewFixture struct {
	mu    sync.Mutex
	calls []string
}

func (f *domainDNSPreviewFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer cf-preview-token-0123456789" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{},
		"result": []map[string]any{{
			"id": "rec1", "name": "app.example.com", "type": "A", "content": "8.8.8.8", "ttl": 1,
			"proxied": false, "comment": "operator",
		}},
		"result_info": map[string]any{"page": 1, "total_pages": 1},
	})
}

func TestPreviewDomainDNSReadsProviderWithoutMutation(t *testing.T) {
	fixture := &domainDNSPreviewFixture{}
	server := httptest.NewServer(fixture)
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(tokenPath, []byte("cf-preview-token-0123456789\n"), 0600); err != nil {
		t.Fatal(err)
	}
	domainDNS, err := integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones:      []integrations.DNSZone{{ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com", TokenFile: tokenPath}},
		BaseURL:    server.URL + "/client/v4",
		HTTPClient: server.Client(),
		Authorizer: mustPreviewAuthorizer(t),
		Audit:      integrations.NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{domainDNS: domainDNS}
	proxied := true
	raw, _ := json.Marshal(platform.DomainInput{
		App: "api", Environment: "production", Domain: "app.example.com",
		Addresses: []string{"8.8.8.8"}, DNSOrigins: []string{"8.8.8.8"},
		DNSMode: "cloudflare", DNSZone: "example.com", DNSProxied: &proxied,
	})
	normalized, err := platform.Default().Normalize(platform.KindDomainAdd, "env/api/production", raw)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, normalized, "")
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := preview.Data["dns_provider"].(integrations.DomainDNSPlan)
	if !ok {
		t.Fatalf("provider preview = %T", preview.Data["dns_provider"])
	}
	if len(provider.Conflicts) == 0 {
		t.Fatalf("foreign record was not reported as a conflict: %+v", provider)
	}
	if len(fixture.calls) != 1 || fixture.calls[0] != "GET /client/v4/zones/0123456789abcdef0123456789abcdef/dns_records" {
		t.Fatalf("preview calls = %v", fixture.calls)
	}
}

func TestPreviewDomainDNSFailsClosedWithoutConfiguration(t *testing.T) {
	service := &Service{}
	proxied := true
	input := &platform.DomainInput{
		App: "api", Environment: "production", Domain: "app.example.com",
		DNSMode: "cloudflare", DNSZone: "example.com", DNSProxied: &proxied,
	}
	if _, err := service.previewDomainDNS(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, input, platform.KindDomainAdd); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("unconfigured error = %v", err)
	}
	if _, err := service.previewDomainDNS(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, input, platform.KindDomainRemove); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("unconfigured removal error = %v", err)
	}
}

func TestPreviewDomainDNSAuthorizesReadBeforeTokenAccess(t *testing.T) {
	authz, err := integrations.NewStaticAuthorizer(map[string][]string{"ops": {integrations.ActionDNSWrite}})
	if err != nil {
		t.Fatal(err)
	}
	domainDNS, err := integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones:      []integrations.DNSZone{{ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com", TokenFile: filepath.Join(t.TempDir(), "missing-token")}},
		Authorizer: authz,
		Audit:      integrations.NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{domainDNS: domainDNS}
	proxied := true
	input := &platform.DomainInput{
		App: "api", Environment: "production", Domain: "app.example.com",
		DNSMode: "cloudflare", DNSZone: "example.com", DNSOrigins: []string{"8.8.8.8"}, DNSProxied: &proxied,
	}
	if _, err := service.previewDomainDNS(context.Background(), contract.Actor{Kind: "operator", Subject: "ops"}, input, platform.KindDomainAdd); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("read authorization error = %v", err)
	}
}

func mustPreviewAuthorizer(t *testing.T) integrations.Authorizer {
	t.Helper()
	authz, err := integrations.NewStaticAuthorizer(map[string][]string{"ops": {"*"}})
	if err != nil {
		t.Fatal(err)
	}
	return authz
}
