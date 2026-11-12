package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/vault"
)

func TestDefinitionDNSPolicyValidationAndAuthority(t *testing.T) {
	valid := api.DefinitionDNSPolicyInput{Enabled: true, Zone: "example.com", Origins: []string{"2001:4860:4860::8888", "8.8.8.8", "8.8.8.8"}, ProxyMode: "dns-only"}
	origins, err := validateDefinitionDNSPolicyInput(valid)
	if err != nil || strings.Join(origins, ",") != "2001:4860:4860::8888,8.8.8.8" {
		t.Fatalf("canonical origins = %v, %v", origins, err)
	}
	for _, address := range []string{"10.0.0.1", "100.64.0.1", "127.0.0.1", "192.0.2.1", "::1", "fd00::1", "not-an-ip"} {
		input := valid
		input.Origins = []string{address}
		if _, err := validateDefinitionDNSPolicyInput(input); contract.Classify(err).Code != contract.ValidationFailed {
			t.Fatalf("origin %q: %v", address, err)
		}
	}
	for _, input := range []api.DefinitionDNSPolicyInput{{Enabled: true, Zone: "example.com", ProxyMode: "dns-only"}, {Enabled: true, Origins: []string{"8.8.8.8"}, ProxyMode: "dns-only"}, {Enabled: true, Zone: "example.com", Origins: []string{"8.8.8.8"}}, {Zone: "example.com"}} {
		if _, err := validateDefinitionDNSPolicyInput(input); contract.Classify(err).Code != contract.ValidationFailed {
			t.Fatalf("invalid policy %+v: %v", input, err)
		}
	}
	store := NewDefinitionDNSPolicyStore(nil)
	for _, kind := range []string{"ci", "webhook", "system"} {
		actor := contract.Actor{Kind: kind, Subject: "ops"}
		if _, err := store.SetDefinitionDNSPolicy(context.Background(), actor, valid); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("%s write: %v", kind, err)
		}
		if _, err := store.GetDefinitionDNSPolicy(context.Background(), actor); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("%s read: %v", kind, err)
		}
	}
}

type cloudflareValidationFixture struct {
	mu    sync.Mutex
	calls []string
}

func (f *cloudflareValidationFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer cf-setup-token-0123456789" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "result": []any{},
		"result_info": map[string]any{"page": 1, "total_pages": 1},
	})
}

func TestCloudflareSetupRejectsNonOperatorBeforeTokenValidation(t *testing.T) {
	fixture := &cloudflareValidationFixture{}
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := &CloudflareSetup{vault: &vault.Service{}, baseURL: server.URL + "/client/v4", client: server.Client()}
	for _, kind := range []string{"ci", "webhook", "system"} {
		actor := contract.Actor{Kind: kind, Subject: "configured-by"}
		if _, err := setup.PreviewCloudflareCredential(context.Background(), actor, "0123456789abcdef0123456789abcdef", "example.com", []byte("cf-setup-token-0123456789")); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("%s preview = %v", kind, err)
		}
		if _, err := setup.ConfigureCloudflareCredential(context.Background(), actor, "0123456789abcdef0123456789abcdef", "example.com", 0, []byte("cf-setup-token-0123456789")); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("%s configure = %v", kind, err)
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.calls) != 0 {
		t.Fatalf("denied setup reached Cloudflare: %v", fixture.calls)
	}
}

func TestCloudflareTokenValidationIsReadOnlyAndUsesZoneDNSList(t *testing.T) {
	fixture := &cloudflareValidationFixture{}
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := &CloudflareSetup{baseURL: server.URL + "/client/v4", client: server.Client()}
	zone := integrations.DNSZone{ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com"}
	if err := setup.validateToken(context.Background(), zone, "cf-setup-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.calls) != 1 || fixture.calls[0] != "GET /client/v4/zones/0123456789abcdef0123456789abcdef/dns_records" {
		t.Fatalf("validation calls = %v", fixture.calls)
	}
}

func TestCloudflareTokenValidationFailureDoesNotWrite(t *testing.T) {
	fixture := &cloudflareValidationFixture{}
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := &CloudflareSetup{baseURL: server.URL + "/client/v4", client: server.Client()}
	zone := integrations.DNSZone{ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com"}
	if err := setup.validateToken(context.Background(), zone, "wrong-token-0123456789"); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("rejected token validation = %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, call := range fixture.calls {
		if call != "GET /client/v4/zones/0123456789abcdef0123456789abcdef/dns_records" {
			t.Fatalf("validation performed a non-read call: %v", fixture.calls)
		}
	}
}

func TestProviderCommitFaultClassifiesAmbiguityWithoutLeaking(t *testing.T) {
	rollback := providerCommitFault(fmt.Errorf("commit failed: %w", pgx.ErrTxCommitRollback))
	if rollback.Code != contract.Unavailable {
		t.Fatalf("rollback code = %s", rollback.Code)
	}
	if !strings.Contains(rollback.Hint, "was not changed") || !strings.Contains(rollback.Hint, "domain cloudflare status") {
		t.Fatalf("rollback hint = %q", rollback.Hint)
	}
	if strings.Contains(rollback.Error(), "unexpectedly resulted") || strings.Contains(rollback.Hint, "unexpectedly resulted") {
		t.Fatalf("rollback fault leaked the pgx error: %v: %s", rollback, rollback.Hint)
	}

	unknown := providerCommitFault(errors.New("sensitive database password=do-not-leak"))
	if unknown.Code != contract.Unavailable {
		t.Fatalf("unknown code = %s", unknown.Code)
	}
	if !strings.Contains(unknown.Message, "unknown") || !strings.Contains(unknown.Hint, "domain cloudflare status") {
		t.Fatalf("unknown commit fault = %v: %s", unknown, unknown.Hint)
	}
	if strings.Contains(unknown.Error(), "sensitive") || strings.Contains(unknown.Hint, "sensitive") || strings.Contains(unknown.Hint, "do-not-leak") {
		t.Fatalf("unknown commit fault leaked the database error: %v: %s", unknown, unknown.Hint)
	}
}

func TestCloudflareSetupRequiresOperatorAndToken(t *testing.T) {
	setup := &CloudflareSetup{vault: &vault.Service{}}
	if _, _, _, err := setup.prepare("0123456789abcdef0123456789abcdef", "example.com", nil); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("empty token = %v", err)
	}
	if _, _, _, err := setup.prepare("bad", "example.com", []byte("token")); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("bad zone = %v", err)
	}
	if _, _, _, err := setup.prepare("0123456789abcdef0123456789abcdef", "Example.COM.", []byte("token")); err != nil {
		t.Fatalf("normalized zone = %v", err)
	}
	if err := requireCloudflareOperator(contract.Actor{Kind: "operator", Subject: "second"}); err != nil {
		t.Fatalf("second operator = %v", err)
	}
	if err := requireCloudflareOperator(contract.Actor{Kind: "ci", Subject: "operator"}); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("ci operator name = %v", err)
	}
}
