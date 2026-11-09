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

func TestTargetsAndConfigsAreWireGuardOnlyAndUseHTTPDiscovery(t *testing.T) {
	raw, err := TargetsJSON("main", "10.78.0.1")
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
	if len(entries) != 1 || entries[0].Targets[0] != "10.78.0.1:9101" ||
		entries[0].Labels["__metrics_path__"] != MetricsPath ||
		entries[0].Labels["__scheme__"] != "http" ||
		entries[0].Labels["eregion_public"] != "false" ||
		entries[0].Labels["eregion_access"] != "wireguard" {
		t.Fatalf("targets = %s", raw)
	}
	for _, config := range []string{
		PrometheusConfig("main", "10.78.0.1", Username, PasswordPlaceholder),
		AlloyConfig("main", "10.78.0.1", Username, PasswordPlaceholder),
	} {
		if !strings.Contains(config, "http://10.78.0.1:9101/targets.json") || !strings.Contains(config, PasswordPlaceholder) {
			t.Fatalf("config missing discovery or placeholder:\n%s", config)
		}
		if strings.Contains(config, "https://") {
			t.Fatalf("config implied a public TLS endpoint:\n%s", config)
		}
	}
}

func TestEndpointInfoIsNeverPublicOrUnauthenticated(t *testing.T) {
	info := BuildEndpointInfo("main", "10.78.0.1", "http://127.0.0.1:9100", true)
	if err := info.Validate(); err != nil {
		t.Fatal(err)
	}
	if info.Credential != true || info.Access != "wireguard" || info.Authentication != "basic" {
		t.Fatalf("endpoint = %+v", info)
	}
	info.Public = true
	if info.Validate() == nil {
		t.Fatal("a public endpoint was accepted")
	}
	info = BuildEndpointInfo("main", "10.78.0.1", "http://127.0.0.1:9100", false)
	info.Authentication = "none"
	if info.Validate() == nil {
		t.Fatal("an unauthenticated endpoint was accepted")
	}
}
