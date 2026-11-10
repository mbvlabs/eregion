package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCredentialRoundTripAndHash(t *testing.T) {
	credential, err := NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	if credential.Validate() != nil {
		t.Fatalf("credential invalid: %+v", credential)
	}
	hash, err := HashPassword(credential.Password)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidHash(hash) || !VerifyPassword(hash, credential.Password) {
		t.Fatal("the generated hash does not verify the credential")
	}
	if VerifyPassword(hash, "wrong") {
		t.Fatal("a wrong password verified")
	}
	if ValidHash("plaintext") {
		t.Fatal("a plaintext value was accepted as a bcrypt hash")
	}
	bad := credential
	bad.Username = "other"
	if bad.Validate() == nil {
		t.Fatal("an unexpected username was accepted")
	}
}

func TestTargetsAndConfigsUseOnlyThePublicHTTPSHostname(t *testing.T) {
	raw, err := TargetsJSON("main", "main.example.com")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Targets) != 1 || entries[0].Targets[0] != "main.example.com:443" ||
		entries[0].Labels["__metrics_path__"] != MetricsPath ||
		entries[0].Labels["__scheme__"] != "https" ||
		entries[0].Labels["eregion_node"] != "main" ||
		entries[0].Labels["eregion_component"] != "node" ||
		entries[0].Labels["eregion_public"] != "true" ||
		entries[0].Labels["eregion_access"] != "public_https" {
		t.Fatalf("targets = %s", raw)
	}
	for _, target := range entries[0].Targets {
		for _, forbidden := range []string{"10.78.0.1", "127.0.0.1", ":9101", ":9100"} {
			if strings.Contains(target, forbidden) {
				t.Fatalf("target %q was reconstructed from a listener or WireGuard address", target)
			}
		}
	}

	prometheus, err := PrometheusConfig("main", "main.example.com", Username, PasswordPlaceholder)
	if err != nil {
		t.Fatal(err)
	}
	alloy, err := AlloyConfig("main", "main.example.com", Username, PasswordPlaceholder)
	if err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]string{"prometheus": prometheus, "alloy": alloy} {
		if !strings.Contains(config, "https://main.example.com/metrics/targets.json") ||
			!strings.Contains(config, PasswordPlaceholder) {
			t.Fatalf("%s config missing public HTTPS discovery or placeholder:\n%s", name, config)
		}
		if strings.Count(config, "basic_auth") < 2 {
			t.Fatalf("%s config must authenticate both discovery and scrape:\n%s", name, config)
		}
		if !strings.Contains(config, `scheme`) || !strings.Contains(config, "https") {
			t.Fatalf("%s config does not scrape with HTTPS:\n%s", name, config)
		}
		for _, forbidden := range []string{"10.78.0.1", "127.0.0.1:9101", "http://", "insecure_skip_verify"} {
			if strings.Contains(config, forbidden) {
				t.Fatalf("%s config contains %q:\n%s", name, forbidden, config)
			}
		}
	}
}

func TestMissingHostnameIsUnconfiguredWithoutAListenerFallback(t *testing.T) {
	for name, call := range map[string]func() error{
		"targets": func() error { _, err := TargetsJSON("main", ""); return err },
		"prometheus": func() error {
			_, err := PrometheusConfig("main", "", Username, PasswordPlaceholder)
			return err
		},
		"alloy": func() error {
			_, err := AlloyConfig("main", "", Username, PasswordPlaceholder)
			return err
		},
		"metrics URL": func() error { _, err := PublicMetricsURL(""); return err },
		"discovery URL": func() error {
			_, err := PublicDiscoveryURL("")
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "unconfigured") {
			t.Fatalf("%s without a hostname = %v, want unconfigured", name, err)
		}
	}
	info, err := BuildEndpointInfo("main", "", "http://127.0.0.1:9100", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != EndpointStateUnconfigured || info.MetricsURL != "" || info.DiscoveryURL != "" || info.Public {
		t.Fatalf("unconfigured endpoint = %+v", info)
	}
	if err := info.Validate(); err != nil {
		t.Fatalf("unconfigured endpoint did not validate: %v", err)
	}
}

func TestEndpointInfoIsPublicHTTPSAuthenticatedAndExact(t *testing.T) {
	info, err := BuildEndpointInfo("main", "main.example.com", "http://127.0.0.1:9100", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := info.Validate(); err != nil {
		t.Fatal(err)
	}
	if info.State != EndpointStateConfigured || !info.Credential || info.Access != "public_https" ||
		!info.Public || info.Authentication != "basic" || info.Listen != GatewayListenAddress ||
		info.MetricsURL != "https://main.example.com/metrics/node" ||
		info.DiscoveryURL != "https://main.example.com/metrics/targets.json" {
		t.Fatalf("endpoint = %+v", info)
	}
	if !strings.Contains(info.Note, "desired") || !strings.Contains(info.Note, "reachability") {
		t.Fatalf("endpoint note overclaims configuration:\n%s", info.Note)
	}

	pending, err := BuildEndpointInfo("main", "main.example.com", "http://127.0.0.1:9100", false)
	if err != nil || pending.State != EndpointStatePendingCredential || pending.Validate() != nil {
		t.Fatalf("pending endpoint = %+v err=%v", pending, err)
	}

	needsApply := info
	needsApply.State = EndpointStateNeedsApply
	if err := needsApply.Validate(); err != nil {
		t.Fatalf("a credentialed gateway awaiting apply did not validate: %v", err)
	}

	for name, mutate := range map[string]func(*EndpointInfo){
		"loopback target": func(e *EndpointInfo) {
			e.MetricsURL = "https://127.0.0.1/metrics/node"
		},
		"WireGuard target": func(e *EndpointInfo) {
			e.DiscoveryURL = "https://10.78.0.1/metrics/targets.json"
		},
		"listener port": func(e *EndpointInfo) {
			e.MetricsURL = "https://main.example.com:9101/metrics/node"
		},
		"http scheme": func(e *EndpointInfo) {
			e.MetricsURL = "http://main.example.com/metrics/node"
		},
		"unauthenticated": func(e *EndpointInfo) {
			e.Authentication = "none"
		},
		"not public": func(e *EndpointInfo) {
			e.Public = false
		},
		"wrong listen": func(e *EndpointInfo) {
			e.Listen = "0.0.0.0:9101"
		},
		"configured without credential": func(e *EndpointInfo) {
			e.Credential = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := info
			mutate(&copy)
			if err := copy.Validate(); err == nil {
				t.Fatalf("endpoint accepted unsafe mutation: %+v", copy)
			}
		})
	}
	if _, err := BuildEndpointInfo("main", "127.0.0.1", "http://127.0.0.1:9100", true); err == nil {
		t.Fatal("a loopback hostname was accepted as the public endpoint")
	}
}
