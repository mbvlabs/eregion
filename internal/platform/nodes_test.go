package platform

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testHostKey(t *testing.T) (string, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " root@host", ssh.FingerprintSHA256(key)
}

func TestEnrollRoleCombinations(t *testing.T) {
	allowed := [][]string{
		{RoleApplication}, {RoleEdge}, {RoleApplication, RoleEdge}, {RoleBuilder}, {RoleRegistry},
		{RoleBuilder, RoleRegistry}, {RoleTelemetry},
	}
	for _, roles := range allowed {
		if err := ValidateEnrollRoles(roles); err != nil {
			t.Errorf("%v must be allowed: %v", roles, err)
		}
	}
	refused := [][]string{
		{RoleBuilder, RoleApplication},               // source execution never shares a node with runtime workloads
		{RoleBuilder, RoleApplication, RoleEdge},     //
		{RoleTelemetry, RoleApplication},             // telemetry is alone
		{RoleControlPlane},                           // only the main node
		{RoleApplication, RoleControlPlane},          //
		{RoleBuilder, RoleEdge},                      //
		{RoleRegistry, RoleApplication},              //
		{RoleApplication, RoleBuilder, RoleRegistry}, //
	}
	for _, roles := range refused {
		if err := ValidateEnrollRoles(roles); err == nil {
			t.Errorf("%v must be refused", roles)
		}
	}
}

func TestSupportedReleaseMatrix(t *testing.T) {
	for _, ok := range [][2]string{{"debian", "12"}, {"debian", "13"}, {"ubuntu", "22.04"}, {"ubuntu", "24.04"}, {"Ubuntu", "24.04"}} {
		if !SupportedRelease(ok[0], ok[1]) {
			t.Errorf("%v must be supported", ok)
		}
	}
	for _, bad := range [][2]string{{"debian", "11"}, {"debian", "12.5"}, {"ubuntu", "20.04"}, {"ubuntu", "26.04"}, {"alpine", "3.20"}, {"", ""}, {"fedora", "42"}} {
		if SupportedRelease(bad[0], bad[1]) {
			t.Errorf("%v must not be supported", bad)
		}
	}
	if !SupportedArch("amd64") || !SupportedArch("arm64") || SupportedArch("386") || SupportedArch("riscv64") {
		t.Error("architecture matrix is wrong")
	}
}

func TestRequiredCapabilitiesAndRegistryScope(t *testing.T) {
	if got := RequiredCapabilities([]string{RoleApplication}); len(got) != 1 || got[0] != CapabilityDocker {
		t.Fatalf("application needs docker: %v", got)
	}
	if got := RequiredCapabilities([]string{RoleBuilder}); len(got) != 2 {
		t.Fatalf("builder needs docker and buildpacks: %v", got)
	}
	if got := RequiredCapabilities([]string{RoleEdge, RoleTelemetry}); len(got) != 0 {
		t.Fatalf("edge and telemetry have no probe requirement: %v", got)
	}
	if RegistryScopeFor([]string{RoleBuilder, RoleRegistry}) != RegistryPush || RegistryScopeFor([]string{RoleApplication}) != RegistryPull {
		t.Fatal("only a builder may hold push scope")
	}
}

func TestNormalizeNodeEnroll(t *testing.T) {
	hostKey, fingerprint := testHostKey(t)
	registry := NewRegistry()
	enroll := func(target string, input map[string]any) (Normalized, error) {
		raw, _ := json.Marshal(input)
		return registry.Normalize(KindNodeEnroll, target, raw)
	}
	good, err := enroll("node/builder-1", map[string]any{"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey, "host_key_fingerprint": fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	input := good.Input.(*NodeEnrollInput)
	if input.User != DefaultSSHUser || input.BuildConcurrency != 1 || input.HostKey != strings.TrimSuffix(hostKey, " root@host") || input.HostKeyFingerprint != fingerprint {
		t.Fatalf("not normalized: %+v", input)
	}
	if strings.Contains(string(good.Canonical), "root@host") {
		t.Fatal("the key comment must not be part of the canonical input")
	}
	for name, bad := range map[string]map[string]any{
		"no host key":         {"roles": []string{"builder"}, "address": "203.0.113.10:22"},
		"bad host key":        {"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": "ssh-ed25519 nope"},
		"fingerprint differs": {"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey, "host_key_fingerprint": "SHA256:" + strings.Repeat("A", 43)},
		"root user":           {"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey, "user": "root"},
		"no address":          {"roles": []string{"builder"}, "host_key": hostKey},
		"builder + app":       {"roles": []string{"builder", "application"}, "address": "203.0.113.10:22", "host_key": hostKey},
		"build on app node":   {"roles": []string{"application"}, "address": "203.0.113.10:22", "host_key": hostKey, "build_concurrency": 3},
		"too many builds":     {"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey, "build_concurrency": 99},
		"bad identity":        {"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey, "identity_fingerprint": "abc"},
		"bad endpoint":        {"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey, "private_network": map[string]any{"endpoint": "nope"}},
		"unknown field":       {"roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey, "password": "x"},
	} {
		if _, err := enroll("node/builder-1", bad); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if _, err := enroll("platform", map[string]any{"roles": []string{"builder"}}); err == nil {
		t.Error("enrollment needs a node target")
	}
	if _, err := enroll("node/builder-1", map[string]any{"node": "other", "roles": []string{"builder"}, "address": "203.0.113.10:22", "host_key": hostKey}); err == nil {
		t.Error("the node field must match the target")
	}
	if _, err := enroll("node/builder-1", map[string]any{"retry": true}); err != nil {
		t.Errorf("a retry carries no facts: %v", err)
	}
	if _, err := enroll("node/builder-1", map[string]any{"retry": true, "address": "203.0.113.10:22"}); err == nil {
		t.Error("a retry must not carry new facts")
	}
}
