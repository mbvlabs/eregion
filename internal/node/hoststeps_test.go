package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestAttestationIsBoundToTheChallengeAndThePinnedKey(t *testing.T) {
	root := t.TempDir()
	nonce := IdentityNonce("op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "builder-1")
	attestation, err := Attest(root, "builder-1", nonce)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := VerifyAttestation(attestation, "builder-1", nonce, "", "")
	if err != nil || fingerprint != attestation.Fingerprint {
		t.Fatalf("first-use attestation: %q %v", fingerprint, err)
	}
	// The same node key is returned on the next attestation: the identity is stable.
	again, _ := Attest(root, "builder-1", nonce)
	if again.PublicKey != attestation.PublicKey {
		t.Fatal("a node must keep its identity key")
	}
	if _, err := VerifyAttestation(attestation, "builder-1", nonce, attestation.PublicKey, attestation.Fingerprint); err != nil {
		t.Fatalf("pinned key must verify: %v", err)
	}

	// A different node name, a different nonce, or a tampered signature fail.
	for name, call := range map[string]func() error{
		"another node name": func() error { _, err := VerifyAttestation(attestation, "app-1", nonce, "", ""); return err },
		"another nonce": func() error {
			_, err := VerifyAttestation(attestation, "builder-1", IdentityNonce("op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "builder-1"), "", "")
			return err
		},
		"tampered signature": func() error {
			bad := attestation
			bad.Signature = bad.Signature[:len(bad.Signature)-6] + "AAAAA="
			_, err := VerifyAttestation(bad, "builder-1", nonce, "", "")
			return err
		},
		"unreadable key": func() error {
			bad := attestation
			bad.PublicKey = "AAAA"
			_, err := VerifyAttestation(bad, "builder-1", nonce, "", "")
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("%s: want ErrIdentityMismatch, got %v", name, err)
		}
	}

	// An impostor with its own key and a valid signature is not the pinned node.
	impostor, err := Attest(t.TempDir(), "builder-1", nonce)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAttestation(impostor, "builder-1", nonce, attestation.PublicKey, ""); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("an impostor presented a different key but was accepted: %v", err)
	}
	if _, err := VerifyAttestation(impostor, "builder-1", nonce, "", attestation.Fingerprint); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("an impostor was accepted against the pinned fingerprint: %v", err)
	}
}

func TestIdentityKeyIsPrivateToTheNode(t *testing.T) {
	root := t.TempDir()
	if _, err := Attest(root, "builder-1", IdentityNonce("op", "builder-1")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(identityPath(root))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity key mode: %v %v", info, err)
	}
	raw, _ := json.Marshal(mustAttest(t, root))
	seed, _ := os.ReadFile(identityPath(root))
	if strings.Contains(string(raw), strings.TrimSpace(string(seed))) {
		t.Fatal("the attestation must not contain the private seed")
	}
}

func mustAttest(t *testing.T, root string) Attestation {
	t.Helper()
	a, err := Attest(root, "builder-1", IdentityNonce("op", "builder-1"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func fakeCommander(known map[string]string) Commander {
	return func(_ context.Context, argv ...string) (string, error) {
		if out, ok := known[argv[0]]; ok {
			return out, nil
		}
		return "", errors.New("not found")
	}
}

func TestProbeReportsWhatTheHostProved(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		path := filepath.Join(dir, "os-release")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	debian := write("PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\n")
	full := fakeCommander(map[string]string{"docker": "27.3.1\n", "pack": "0.36.0+git\n", "wg": "wireguard-tools v1.0\n", "id": "eregion\n"})

	probe := Probe(context.Background(), debian, "amd64", full)
	if !probe.Supported || probe.OS.ID != "debian" || probe.OS.VersionID != "12" {
		t.Fatalf("%+v", probe)
	}
	if got := strings.Join(probe.Capabilities, ","); got != "buildpacks,docker" {
		t.Fatalf("capabilities: %s", got)
	}
	if !probe.WireGuard.Present || probe.User != "eregion" {
		t.Fatalf("%+v", probe)
	}

	// A tool that is missing is a fact: the capability is absent, never assumed.
	noPack := Probe(context.Background(), debian, "arm64", fakeCommander(map[string]string{"docker": "27.3.1"}))
	if got := strings.Join(noPack.Capabilities, ","); got != "docker" || noPack.Pack.Present {
		t.Fatalf("pack must be absent: %+v", noPack)
	}

	unsupported := map[string]struct{ release, arch string }{
		"debian 11":   {"ID=debian\nVERSION_ID=\"11\"\n", "amd64"},
		"ubuntu 20":   {"ID=ubuntu\nVERSION_ID=\"20.04\"\n", "amd64"},
		"alpine":      {"ID=alpine\nVERSION_ID=3.20.1\n", "amd64"},
		"32-bit arch": {"ID=ubuntu\nVERSION_ID=\"24.04\"\n", "386"},
	}
	for name, tc := range unsupported {
		result := Probe(context.Background(), write(tc.release), tc.arch, full)
		if result.Supported || result.Unsupported == "" {
			t.Errorf("%s must be unsupported: %+v", name, result)
		}
	}
	if missing := Probe(context.Background(), filepath.Join(dir, "absent"), "amd64", full); missing.Supported {
		t.Error("a host with no os-release is not supported")
	}
}

func runStepOnNode(t *testing.T, root, stepID string, payload any, settings HostSettings) (engine.TaskStatus, error) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	exec := engine.Execution{OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1, Step: stepID, RequestID: "r1"}
	step, err := NewStepRegistryWithHost(settings).Build(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	nodeEngine, err := engine.New(root, "builder-1", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	return nodeEngine.Run(context.Background(), step, exec, nil)
}

func TestHostcfgBuilderGetsIsolatedBuildDirectories(t *testing.T) {
	root := t.TempDir()
	payload := HostcfgPayload{Step: "node-hostcfg-01-v1", Roles: []string{"builder"}, BuildConcurrency: 2}
	status, err := runStepOnNode(t, root, "node-hostcfg-01-v1", payload, HostSettings{})
	if err != nil || status.Status != engine.StatusCompleted {
		t.Fatalf("%+v %v", status, err)
	}
	for _, dir := range BuildDirs(root) {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("build directory %s: %v %v", dir, info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "runtime")); err == nil {
		t.Error("a builder-only node must not get the runtime directory")
	}

	// Check reports the node as converged, so a second pass applies nothing.
	step, _ := NewStepRegistryWithHost(HostSettings{}).Build(engine.Execution{OperationID: "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Attempt: 1, Step: "node-hostcfg-01-v1", RequestID: "r2"}, mustJSON(payload))
	check, err := step.Check(context.Background(), engine.Runtime{Root: root})
	if err != nil || !check.Complete {
		t.Fatalf("a configured node must check complete: %+v %v", check, err)
	}
	if err := os.Chmod(filepath.Join(root, "build", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if check, _ = step.Check(context.Background(), engine.Runtime{Root: root}); check.Complete {
		t.Fatal("drifted directory permissions must be repaired by Apply")
	}

	app := t.TempDir()
	if _, err := runStepOnNode(t, app, "node-hostcfg-01-v1", HostcfgPayload{Step: "node-hostcfg-01-v1", Roles: []string{"application"}}, HostSettings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app, "build")); err == nil {
		t.Error("an application node must never get build directories")
	}
	if _, err := os.Stat(filepath.Join(app, "runtime")); err != nil {
		t.Error("an application node gets a runtime directory")
	}
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func TestHostcfgRefusesAMissingDedicatedBuildUser(t *testing.T) {
	root := t.TempDir()
	payload := HostcfgPayload{Step: "node-hostcfg-01-v1", Roles: []string{"builder"}, BuildUser: "no-such-build-user-xyz"}
	_, err := runStepOnNode(t, root, "node-hostcfg-01-v1", payload, HostSettings{})
	if err == nil || !errors.Is(err, engine.ErrPermanent) {
		t.Fatalf("a missing build user must fail permanently, got %v", err)
	}
}

func TestHostcfgRejectsUnknownRolesAndPayloads(t *testing.T) {
	registry := NewStepRegistry()
	exec := engine.Execution{OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1, Step: "node-hostcfg-01-v1", RequestID: "r"}
	for name, payload := range map[string]string{
		"control plane": `{"step":"node-hostcfg-01-v1","roles":["control-plane"]}`,
		"no roles":      `{"step":"node-hostcfg-01-v1","roles":[]}`,
		"extra field":   `{"step":"node-hostcfg-01-v1","roles":["builder"],"command":"rm -rf /"}`,
		"wrong step":    `{"step":"node-hostcfg-02-v1","roles":["builder"]}`,
		"build user":    `{"step":"node-hostcfg-01-v1","roles":["builder"],"build_user":"A b"}`,
	} {
		if _, err := registry.Build(exec, json.RawMessage(payload)); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestWireGuardKeyIsGeneratedOnTheNodeAndNeverReturned(t *testing.T) {
	root := t.TempDir()
	status, err := runStepOnNode(t, root, "node-wireguard-01-v1", WireGuardPayload{Step: "node-wireguard-01-v1", Action: "keygen"}, HostSettings{})
	if err != nil {
		t.Fatal(err)
	}
	var result WireGuardResult
	if err := json.Unmarshal(status.Result, &result); err != nil || result.PublicKey == "" {
		t.Fatalf("%s %v", status.Result, err)
	}
	private, err := os.ReadFile(filepath.Join(wgDir(root), "private.key"))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(wgDir(root), "private.key"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v", info.Mode())
	}
	if strings.Contains(string(status.Result), strings.TrimSpace(string(private))) {
		t.Fatal("the private key must never be part of a step result")
	}
	// The key is stable: a second keygen reports the same public key.
	again, err := runStepOnNode(t, root, "node-wireguard-01-v1", WireGuardPayload{Step: "node-wireguard-01-v1", Action: "keygen"}, HostSettings{})
	var second WireGuardResult
	_ = json.Unmarshal(again.Result, &second)
	if err != nil || second.PublicKey != result.PublicKey {
		t.Fatalf("keygen must be stable: %s vs %s %v", again.Result, status.Result, err)
	}
}

func TestWireGuardApplyRendersPeersAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	helperLog := filepath.Join(t.TempDir(), "calls")
	helper := filepath.Join(t.TempDir(), "apply.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho \"$1\" >> "+helperLog+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	peer := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	payload := WireGuardPayload{Step: "node-wireguard-02-v1", Action: "apply", Address: "10.77.0.2/24", ListenPort: 51820,
		Peers: []WireGuardPeer{{PublicKey: peer, AllowedIPs: []string{"10.77.0.1/32"}}}}

	// Without a privileged helper the configuration is rendered, never claimed applied.
	rendered, err := runStepOnNode(t, root, "node-wireguard-02-v1", payload, HostSettings{})
	if err != nil {
		t.Fatal(err)
	}
	var result WireGuardResult
	_ = json.Unmarshal(rendered.Result, &result)
	if result.State != "rendered" || result.PeerCount != 1 {
		t.Fatalf("%+v", result)
	}
	config, _ := os.ReadFile(filepath.Join(wgDir(root), "wg0.conf"))
	for _, want := range []string{"[Interface]", "PrivateKey = ", "Address = 10.77.0.2/24", "ListenPort = 51820", "[Peer]", "PublicKey = " + peer, "AllowedIPs = 10.77.0.1/32"} {
		if !strings.Contains(string(config), want) {
			t.Errorf("config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(string(rendered.Result), "PrivateKey") {
		t.Fatal("the rendered config must not be in the step result")
	}

	// With the helper it is applied exactly once; the same peers change nothing.
	settings := HostSettings{WireGuardApply: []string{helper}}
	for i, op := range []string{"op_cccccccccccccccccccccccccccccccc", "op_dddddddddddddddddddddddddddddddd"} {
		raw, _ := json.Marshal(payload)
		exec := engine.Execution{OperationID: contract.OperationID(op), Attempt: 1, Step: "node-wireguard-02-v1", RequestID: "r"}
		step, err := NewStepRegistryWithHost(settings).Build(exec, raw)
		if err != nil {
			t.Fatal(err)
		}
		nodeEngine, _ := engine.New(root, "builder-1", &secrets.Registry{})
		status, err := nodeEngine.Run(context.Background(), step, exec, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(status.Result, &result)
		if result.State != "applied" {
			t.Fatalf("run %d: %+v", i, result)
		}
	}
	calls, _ := os.ReadFile(helperLog)
	if strings.Count(string(calls), "\n") != 1 {
		t.Fatalf("the apply helper ran %d times for an unchanged configuration:\n%s", strings.Count(string(calls), "\n"), calls)
	}
}

func TestWireGuardPayloadValidation(t *testing.T) {
	registry := NewStepRegistry()
	exec := engine.Execution{OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1, Step: "node-wireguard-02-v1", RequestID: "r"}
	good := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for name, payload := range map[string]string{
		"bad key":        `{"step":"node-wireguard-02-v1","action":"apply","address":"10.77.0.2/24","listen_port":51820,"peers":[{"public_key":"nope","allowed_ips":["10.77.0.1/32"]}]}`,
		"dup key":        `{"step":"node-wireguard-02-v1","action":"apply","address":"10.77.0.2/24","listen_port":51820,"peers":[{"public_key":"` + good + `","allowed_ips":["10.77.0.1/32"]},{"public_key":"` + good + `","allowed_ips":["10.77.0.3/32"]}]}`,
		"newline inject": `{"step":"node-wireguard-02-v1","action":"apply","address":"10.77.0.2/24","listen_port":51820,"peers":[{"public_key":"` + good + `","allowed_ips":["10.77.0.1/32\nPostUp=rm -rf /"]}]}`,
		"endpoint inj":   `{"step":"node-wireguard-02-v1","action":"apply","address":"10.77.0.2/24","listen_port":51820,"peers":[{"public_key":"` + good + `","allowed_ips":["10.77.0.1/32"],"endpoint":"a:1\nPostUp=x"}]}`,
		"bad address":    `{"step":"node-wireguard-02-v1","action":"apply","address":"x","listen_port":51820}`,
		"bad port":       `{"step":"node-wireguard-02-v1","action":"apply","address":"10.77.0.2/24","listen_port":0}`,
		"keygen peers":   `{"step":"node-wireguard-02-v1","action":"keygen","address":"10.77.0.2/24"}`,
		"unknown action": `{"step":"node-wireguard-02-v1","action":"rm"}`,
	} {
		if _, err := registry.Build(exec, json.RawMessage(payload)); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
