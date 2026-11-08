package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeDNSOriginsRejectsRFCNonPublicAddresses(t *testing.T) {
	for _, value := range []string{
		"8.8.8.8", "1.1.1.1", "192.0.1.1", "198.51.99.1", "203.0.114.1",
		"2000::1", "2400:cb00::1", "2606:4700:4700::1111", "2001:4860:4860::8888", "2a00:1450:4001::1",
	} {
		if got, err := NormalizeDNSOrigins([]string{value}); err != nil || len(got) != 1 {
			t.Errorf("public origin %q refused: %+v, %v", value, got, err)
		}
	}
	for _, value := range []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "100.64.0.1", "127.0.0.1",
		"169.254.1.1", "172.16.0.1", "192.0.0.1", "192.0.2.1", "192.88.99.1",
		"192.168.0.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1",
		"240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "100::1", "64:ff9b::1",
		"64:ff9b:1::1", "1fff::1", "2001::1", "2001:2::1", "2001:db8::1",
		"2001:10::1", "2001:20::1", "2002::1", "3fff::1", "5f00::1", "4000::1",
		"8000::1", "c000::1", "fc00::1", "fe80::1", "fec0::1",
		"ff02::1", "2001:4860:4860::8888%eth0", "management.internal",
	} {
		if _, err := NormalizeDNSOrigins([]string{value}); err == nil {
			t.Errorf("non-public origin %q accepted", value)
		}
	}
	mapped, err := NormalizeDNSOrigins([]string{"::ffff:8.8.8.8", "8.8.8.8"})
	if err != nil || len(mapped) != 1 || mapped[0] != "8.8.8.8" {
		t.Fatalf("mapped public origin = %+v, %v", mapped, err)
	}
}

func TestDomainDNSInputValidation(t *testing.T) {
	registry := NewRegistry()
	normalize := func(value map[string]any) (*DomainInput, error) {
		if _, ok := value["domain"]; !ok {
			value["domain"] = "api.example.com"
		}
		raw, _ := json.Marshal(value)
		normalized, err := registry.Normalize(KindDomainAdd, "env/api/production", raw)
		if err != nil {
			return nil, err
		}
		return normalized.Input.(*DomainInput), nil
	}

	input, err := normalize(map[string]any{})
	if err != nil || input.DNSMode != "" || input.DNSZone != "" || input.DNSProxied != nil {
		t.Fatalf("default manual input = %+v, %v", input, err)
	}
	input, err = normalize(map[string]any{"dns_mode": "manual"})
	if err != nil || input.DNSMode != "" {
		t.Fatalf("explicit manual input = %+v, %v", input, err)
	}
	if _, err := normalize(map[string]any{"dns_mode": "manual", "dns_zone": "example.com"}); err == nil {
		t.Fatal("manual mode accepted a provider zone")
	}
	if _, err := normalize(map[string]any{"dns_mode": "manual", "dns_origins": []string{"8.8.8.8"}}); err == nil {
		t.Fatal("manual mode accepted explicit origins")
	}
	if _, err := normalize(map[string]any{"dns_mode": "cloudflare"}); err == nil {
		t.Fatal("cloudflare mode accepted without a zone")
	}
	if _, err := normalize(map[string]any{"dns_mode": "cloudflare", "dns_zone": "bad zone"}); err == nil {
		t.Fatal("cloudflare mode accepted an invalid zone")
	}
	if _, err := normalize(map[string]any{"dns_mode": "cloudflare", "dns_zone": "example.com", "dns_origins": []string{"127.0.0.1"}}); err == nil {
		t.Fatal("cloudflare mode accepted a loopback origin")
	}
	if _, err := normalize(map[string]any{"dns_mode": "cloudflare", "dns_zone": "example.com", "dns_origins": []string{"10.0.0.1"}}); err == nil {
		t.Fatal("cloudflare mode accepted a private origin")
	}
	if _, err := normalize(map[string]any{"dns_mode": "cloudflare", "dns_zone": "example.com", "dns_origins": []string{"management.internal"}}); err == nil {
		t.Fatal("cloudflare mode accepted a hostname origin")
	}
	proxied := true
	input, err = normalize(map[string]any{
		"dns_mode": "cloudflare", "dns_zone": "Example.COM.", "dns_proxied": proxied,
		"addresses": []string{"198.51.100.20"}, "dns_origins": []string{"8.8.8.8", "2001:4860:4860::8888", "8.8.8.8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.DNSMode != "cloudflare" || input.DNSZone != "example.com" || input.DNSProxied == nil || !*input.DNSProxied {
		t.Fatalf("cloudflare input = %+v", input)
	}
	if len(input.DNSOrigins) != 2 || input.DNSOrigins[0] != "8.8.8.8" || input.DNSOrigins[1] != "2001:4860:4860::8888" {
		t.Fatalf("cloudflare origins = %+v", input.DNSOrigins)
	}
	encoded, _ := json.Marshal(input)
	if strings.Contains(string(encoded), "api_token") || strings.Contains(string(encoded), "token_file") {
		t.Fatalf("provider credential reference entered canonical input: %s", encoded)
	}
}

func TestDomainDNSInputAcceptsConfiguredZoneID(t *testing.T) {
	registry := NewRegistry()
	raw, _ := json.Marshal(map[string]any{
		"domain": "api.example.com", "dns_mode": "cloudflare",
		"dns_zone": "0123456789abcdef0123456789abcdef",
	})
	if _, err := registry.Normalize(KindDomainAdd, "env/api/production", raw); err != nil {
		t.Fatal(err)
	}
}
