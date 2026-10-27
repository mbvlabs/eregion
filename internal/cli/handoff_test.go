package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
)

// The handoff is the only source login has; a renamed section or label silently
// breaks importing it on another machine.
func TestHandoffGroupsCarryEverythingLoginNeeds(t *testing.T) {
	wg, err := transport.GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	init := &serverInit{
		answers: serverInitAnswers{
			Context: "prod", Node: "main", Hostname: "app.example.com", Host: "203.0.113.10",
			AdminUser: "admin", DatabaseURL: "postgres://user:pass@db.example.com/app",
		},
		pending:      &host.PendingInit{NodePublicKey: "node-public-key"},
		identity:     &operatorIdentity{wg: wg, sshKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----", sudoPassword: "sudo-pass"},
		hostKey:      "ssh-ed25519 SHA256:host",
		vaultKeyring: `{"format":"eregion/vault-keyring/v1"}`,
		platform:     host.Platform{ID: "debian", VersionID: "13", PrettyName: "Debian 13", Arch: "amd64"},
	}
	h := &secretsHandoff{node: "prod", groups: init.handoffGroups()}
	rec := parseHandoffRecord(h.body())
	for _, want := range []struct{ section, label, value string }{
		{"Main node", "Context", "prod"},
		{"Main node", "WireGuard endpoint", "203.0.113.10:51820"},
		{"Main node", "WireGuard address", "10.78.0.1"},
		{"Main node", "WireGuard public key", "node-public-key"},
		{"Main node", "SSH host key", "ssh-ed25519 SHA256:host"},
		{"Operator", "Admin user", "admin"},
		{"Operator", "WireGuard address", "10.78.0.100"},
		{"Operator", "WireGuard private key", wg.Encode()},
		{"Operator", "Sudo password", "sudo-pass"},
		{"Control-plane database", "URL", "postgres://user:pass@db.example.com/app"},
		{"Vault keyring", "EREGION_VAULT_KEY_FILE content", `{"format":"eregion/vault-keyring/v1"}`},
	} {
		if got := rec.field(want.section, want.label); got != want.value {
			t.Errorf("%s/%s = %q, want %q", want.section, want.label, got, want.value)
		}
	}
	if sshKey := rec.field("Operator", "SSH private key"); sshKey == "" {
		t.Fatal("the SSH private key was lost")
	}
}

func TestSecretsOutIsOwnerOnlyAndNeverOverwritten(t *testing.T) {
	h := &secretsHandoff{node: "prod", groups: []handoffGroup{{title: "Operator", fields: []handoffField{{"Sudo password", "sudo-pass"}}}}, passphrase: "word-word"}
	path := filepath.Join(t.TempDir(), "prod-secrets.md")
	if err := h.writeRecord(path, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec := parseHandoffRecord(string(raw)); rec.field("Operator", "Sudo password") != "sudo-pass" {
		t.Fatal("the written record does not parse")
	}
	if err := h.writeRecord(path, false); err == nil {
		t.Fatal("an existing record was overwritten")
	}
	if err := h.writeRecord(path, true); err != nil {
		t.Fatalf("resume could not replace its own record: %v", err)
	}
}
