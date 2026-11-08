package cli

import (
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

func parseDomainDNSFlags(t *testing.T, allowProxy bool, args ...string) (*platform.DomainInput, error) {
	t.Helper()
	cmd := &cobra.Command{Use: "domain"}
	addDomainDNSFlags(cmd, allowProxy)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	input := &platform.DomainInput{
		App: "api", Environment: "production", Domain: "app.example.com",
		Addresses: []string{"198.51.100.20"},
	}
	return input, applyDomainDNSFlags(cmd, input, allowProxy)
}

func TestDomainDNSFlagsKeepManualAsDefault(t *testing.T) {
	input, err := parseDomainDNSFlags(t, true)
	if err != nil {
		t.Fatal(err)
	}
	if input.DNSMode != "" || input.DNSZone != "" || input.DNSProxied != nil {
		t.Fatalf("default input = %+v", input)
	}
	input, err = parseDomainDNSFlags(t, true, "--dns", "manual")
	if err != nil || input.DNSMode != "manual" {
		t.Fatalf("explicit manual = %+v, %v", input, err)
	}
	for _, args := range [][]string{
		{"--dns-zone", "example.com"},
		{"--dns", "cloudflare", "--dns-zone", "example.com"},
		{"--dns", "manual", "--dns-zone", "example.com"},
		{"--dns", "manual", "--dns-proxy", "proxied"},
		{"--dns", "manual", "--dns-origin", "8.8.8.8"},
		{"--dns", "other", "--dns-zone", "example.com"},
		{"--dns", "cloudflare", "--dns-zone", "example.com", "--dns-proxy", "orange", "--dns-origin", "8.8.8.8"},
		{"--dns", "cloudflare", "--dns-zone", "example.com", "--dns-proxy", "dns-only"},
		{"--dns", "cloudflare", "--dns-zone", "example.com", "--dns-proxy", "dns-only", "--dns-origin", "127.0.0.1"},
		{"--dns", "cloudflare", "--dns-zone", "example.com", "--dns-proxy", "dns-only", "--dns-origin", "management.internal"},
	} {
		if _, err := parseDomainDNSFlags(t, true, args...); contract.Classify(err).Code != contract.Usage && contract.Classify(err).Code != contract.ValidationFailed {
			t.Fatalf("%v error = %v", args, err)
		}
	}
}

func TestDomainDNSFlagsRequireExplicitPublicOriginsForAdd(t *testing.T) {
	for proxy, want := range map[string]bool{"dns-only": false, "proxied": true} {
		input, err := parseDomainDNSFlags(t, true, "--dns", "cloudflare", "--dns-zone", "Example.COM.", "--dns-origin", "8.8.8.8", "--dns-origin", "2001:4860:4860::8888", "--dns-proxy", proxy)
		if err != nil {
			t.Fatal(err)
		}
		if input.DNSMode != "cloudflare" || input.DNSZone != "example.com" || input.DNSProxied == nil || *input.DNSProxied != want {
			t.Fatalf("%s input = %+v", proxy, input)
		}
		if len(input.DNSOrigins) != 2 || input.DNSOrigins[0] != "8.8.8.8" || input.DNSOrigins[1] != "2001:4860:4860::8888" {
			t.Fatalf("origins = %+v", input.DNSOrigins)
		}
		if len(input.Addresses) != 2 || input.Addresses[0] != "8.8.8.8" {
			t.Fatalf("addresses were not replaced by explicit origins: %+v", input.Addresses)
		}
	}
	input := &platform.DomainInput{Addresses: []string{"198.51.100.20"}}
	cmd := &cobra.Command{Use: "domain"}
	addDomainDNSFlags(cmd, true)
	if err := cmd.Flags().Parse([]string{"--dns", "cloudflare", "--dns-zone", "example.com", "--dns-proxy", "dns-only"}); err != nil {
		t.Fatal(err)
	}
	if err := applyDomainDNSFlags(cmd, input, true); contract.Classify(err).Code != contract.Usage {
		t.Fatalf("missing origin error = %v", err)
	}
}

func TestDomainDNSFlagsForRemovalHaveNoProxyChoice(t *testing.T) {
	input, err := parseDomainDNSFlags(t, false, "--dns", "cloudflare", "--dns-zone", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if input.DNSMode != "cloudflare" || input.DNSZone != "example.com" || input.DNSProxied != nil {
		t.Fatalf("remove input = %+v", input)
	}
}

func TestDomainAddPreviewCarriesManagedState(t *testing.T) {
	raw := json.RawMessage(`{
		"kind":"platform.domain.add","target":"env/hello/production","dry_run":true,
		"summary":"Promote app.example.com.",
		"data":{"dns_managed":{"provider":"cloudflare","zone_id":"zone","zone_name":"example.com","origins":["8.8.8.8"],"proxied":true,"state":"applied","generation":2,"applied_generation":2}}
	}`)
	data, err := domainAddPreview(raw, "app.example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		DNSManaged map[string]any `json:"dns_managed"`
		DNSRecords []domainRecord `json:"dns_records"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DNSManaged["zone_name"] != "example.com" || payload.DNSManaged["state"] != "applied" {
		t.Fatalf("managed state = %+v", payload.DNSManaged)
	}
	if len(payload.DNSRecords) != 1 || payload.DNSRecords[0].Value != "8.8.8.8" {
		t.Fatalf("managed records = %+v", payload.DNSRecords)
	}
}

func TestManagedBindingResultOverridesFalseManualInput(t *testing.T) {
	data := domainChangeData{
		Domain: "app.example.com", Primary: true,
		DNSRecords: []domainRecord{{Type: "A", Name: "app.example.com", Value: "198.51.100.20"}},
	}
	binding := &platform.DomainBinding{
		Domain: "app.example.com",
		ManagedDNS: &platform.ManagedDNSBinding{
			Provider: "cloudflare", ZoneName: "example.com", Origins: []string{"8.8.8.8"}, Proxied: true,
			State: "applied", Generation: 2, AppliedGeneration: 2,
		},
	}
	applyManagedDNSBinding(&data, binding)
	if data.DNSMode != "cloudflare" || data.DNSZone != "example.com" || data.DNSProxied == nil || !*data.DNSProxied {
		t.Fatalf("managed result = %+v", data)
	}
	if len(data.DNSRecords) != 1 || data.DNSRecords[0].Value != "8.8.8.8" {
		t.Fatalf("managed result records = %+v", data.DNSRecords)
	}
}

func TestDomainAddPreviewCarriesProviderSnapshot(t *testing.T) {
	raw := json.RawMessage(`{
		"kind":"platform.domain.add","target":"env/hello/production","dry_run":true,
		"summary":"Bind app.example.com.",
		"data":{"dns_provider":{"zone":"example.com","host":"app.example.com","ownership":"eregion:dns:abc","changes":[{"action":"create","type":"A"}]}}
	}`)
	data, err := domainAddPreview(raw, "app.example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Preview     map[string]any `json:"preview"`
		DNSProvider map[string]any `json:"dns_provider"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Preview["kind"] != "platform.domain.add" {
		t.Fatalf("preview = %+v", payload.Preview)
	}
	if payload.DNSProvider["zone"] != "example.com" || payload.DNSProvider["host"] != "app.example.com" {
		t.Fatalf("provider snapshot = %+v", payload.DNSProvider)
	}
}

func TestDomainDNSFlagsRefuseWithoutPromptingInJSON(t *testing.T) {
	code, stdout, _ := invoke(t, "domain", "add", "api", "production", "app.example.com", "--dns-zone", "example.com", "--json")
	if code != 2 {
		t.Fatalf("exit = %d", code)
	}
	envelope := decodeOne(t, stdout)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.Usage {
		t.Fatalf("envelope = %+v", envelope)
	}
}
