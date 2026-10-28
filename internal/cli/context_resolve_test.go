package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

func writeTestContext(t *testing.T, store *host.ContextStore, name, nodeAddress, endpoint string) {
	t.Helper()
	dir, err := identityDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "eregion test context")
	if err != nil {
		t.Fatal(err)
	}
	sshPath := filepath.Join(dir, "ssh_ed25519")
	if err := os.WriteFile(sshPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	wg, err := transport.GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	wgPath := filepath.Join(dir, "wireguard.key")
	if err := os.WriteFile(wgPath, []byte(wg.Encode()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	passPath := filepath.Join(dir, "sudo-password")
	if err := os.WriteFile(passPath, []byte("sudo-"+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	peer, err := transport.GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	knownHosts, err := knownHostsPath()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinHostKey(knownHosts, net.JoinHostPort(nodeAddress, "22"), signer.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveContext(host.Context{Name: name, SSH: &host.ContextSSH{
		Address: net.JoinHostPort(nodeAddress, "22"), User: "admin",
		Identity: sshPath, KnownHosts: knownHosts,
		WireGuard: &host.ContextWireGuard{
			PrivateKeyFile: wgPath, Address: "10.78.0.100",
			NodeAddress: nodeAddress, NodePublicKey: peer.PublicKey().String(),
			Endpoint: endpoint,
		},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestNodeClientResolvesContextFlagThenEnvironmentThenCurrent(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_TUNNEL", "inprocess")
	t.Setenv("EREGION_CONTEXT", "")
	store, err := host.LoadContextStore("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestContext(t, store, "a", "10.78.0.2", "203.0.113.10:51820")
	writeTestContext(t, store, "b", "10.79.0.2", "198.51.100.20:51820")
	writeTestContext(t, store, "c", "10.80.0.2", "203.0.113.30:51820")
	if _, err := store.Use("a"); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "eregion"}
	cmd.PersistentFlags().String("context", "", "Context name")
	a := &app{secrets: &secrets.Registry{}}
	client, err := a.nodeClientFor(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.contextName != "a" {
		t.Fatalf("current context = %s, want a", client.contextName)
	}

	if err := cmd.PersistentFlags().Set("context", "b"); err != nil {
		t.Fatal(err)
	}
	client, err = a.nodeClientFor(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.contextName != "b" {
		t.Fatalf("--context b reached %s", client.contextName)
	}

	// The flag wins over the environment variable.
	t.Setenv("EREGION_CONTEXT", "c")
	client, err = a.nodeClientFor(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.contextName != "b" {
		t.Fatalf("flag with EREGION_CONTEXT=c reached %s, want b", client.contextName)
	}

	if err := cmd.PersistentFlags().Set("context", ""); err != nil {
		t.Fatal(err)
	}
	client, err = a.nodeClientFor(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.contextName != "c" {
		t.Fatalf("EREGION_CONTEXT=c reached %s", client.contextName)
	}

	if err := os.Unsetenv("EREGION_CONTEXT"); err != nil {
		t.Fatal(err)
	}
	client, err = a.nodeClientFor(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.contextName != "a" {
		t.Fatalf("without a flag or variable reached %s, want the current a", client.contextName)
	}

	if err := cmd.PersistentFlags().Set("context", "ghost"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.nodeClientFor(cmd, ""); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("unknown context = %v, want not_found", err)
	} else if !strings.Contains(err.Error(), "context ghost") || !strings.Contains(contract.Classify(err).Hint, "eregion context list") {
		t.Fatalf("unknown context fault is not actionable: %v", err)
	}
}

func TestContextMustAgreeWhenTheCommandCreatesIt(t *testing.T) {
	cmd := &cobra.Command{Use: "eregion"}
	cmd.PersistentFlags().String("context", "", "Context name")

	if err := requireAgreedContext(cmd, "main"); err != nil {
		t.Fatalf("empty --context refused: %v", err)
	}
	if err := cmd.PersistentFlags().Set("context", "main"); err != nil {
		t.Fatal(err)
	}
	if err := requireAgreedContext(cmd, "main"); err != nil {
		t.Fatalf("agreeing --context refused: %v", err)
	}
	if err := cmd.PersistentFlags().Set("context", "other"); err != nil {
		t.Fatal(err)
	}
	if err := requireAgreedContext(cmd, "main"); contract.Classify(err).Code != contract.Usage {
		t.Fatalf("disagreeing --context = %v, want usage_error", err)
	}
}

func TestServerInitRefusesADisagreeingContext(t *testing.T) {
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	cmd := &cobra.Command{Use: "init"}
	cmd.Flags().String("host", "", "")
	cmd.Flags().String("hostname", "", "")
	cmd.Flags().String("context-name", "", "")
	cmd.PersistentFlags().String("context", "", "")
	for name, value := range map[string]string{
		"host": "root@203.0.113.10", "hostname": "app.example.com", "context-name": "main", "context": "other",
	} {
		flags := cmd.Flags()
		if name == "context" {
			flags = cmd.PersistentFlags()
		}
		if err := flags.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	r := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: cmd}
	if err := r.collect(); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := r.validate(); contract.Classify(err).Code != contract.Usage {
		t.Fatalf("server init --context other = %v, want usage_error", err)
	}
	if err := cmd.PersistentFlags().Set("context", "main"); err != nil {
		t.Fatal(err)
	}
	if err := r.collect(); err != nil {
		t.Fatalf("collect with the agreeing context: %v", err)
	}
	if err := r.validate(); contract.Classify(err).Code == contract.Usage {
		t.Fatalf("server init --context main still refuses: %v", err)
	}
}

func TestLoginRefusesADifferentContextFlag(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	fixture := newLoginFixture(t, "main")
	a := newApp()
	if err := a.root.PersistentFlags().Set("context", "other"); err != nil {
		t.Fatal(err)
	}
	a.root.SetArgs(fixture.args())
	err := a.root.Execute()
	if contract.Classify(err).Code != contract.Usage {
		t.Fatalf("login --context other = %v, want usage_error", err)
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("a refused login wrote the context store: %v", statErr)
	}
}

func selectedContextFixture(t *testing.T) (*cobra.Command, *host.ContextStore) {
	t.Helper()
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_CONTEXT", "")
	store, err := host.LoadContextStore("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestContext(t, store, "a", "10.78.0.2", "203.0.113.10:51820")
	writeTestContext(t, store, "b", "10.79.0.2", "198.51.100.20:51820")
	if _, err := store.Use("a"); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "eregion"}
	cmd.PersistentFlags().String("context", "", "Context name")
	return cmd, store
}

func TestLocalReadsFollowTheSelectedContext(t *testing.T) {
	cmd, _ := selectedContextFixture(t)

	if got := mainNodeAddresses(cmd); len(got) != 1 || got[0] != "203.0.113.10" {
		t.Fatalf("current main addresses = %v", got)
	}
	if err := cmd.PersistentFlags().Set("context", "b"); err != nil {
		t.Fatal(err)
	}
	if got := mainNodeAddresses(cmd); len(got) != 1 || got[0] != "198.51.100.20" {
		t.Fatalf("--context b main addresses = %v", got)
	}
	if got := apiEndpoint(cmd); got != "ssh admin@10.79.0.2:22" {
		t.Fatalf("--context b endpoint = %q", got)
	}
	if got := workspaceHost(cmd); got != "admin@10.79.0.2:22" {
		t.Fatalf("--context b workspace host = %q", got)
	}
	if got := currentContextName(cmd); got != "b" {
		t.Fatalf("--context b authenticator label = %q", got)
	}
	if record, ok := domainDNSRecord("app.example.com", cmd); !ok || record.Value != "198.51.100.20" {
		t.Fatalf("--context b DNS record = %+v (%t)", record, ok)
	}
	input := domainAddInput(cmd, []string{"api", "production", "app.example.com"}, false)
	if len(input.Addresses) != 1 || input.Addresses[0] != "198.51.100.20" {
		t.Fatalf("--context b domain add addresses = %v", input.Addresses)
	}

	// EREGION_CONTEXT selects the same local reads when the flag is absent.
	if err := cmd.PersistentFlags().Set("context", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_CONTEXT", "b")
	if got := mainNodeAddresses(cmd); len(got) != 1 || got[0] != "198.51.100.20" {
		t.Fatalf("EREGION_CONTEXT=b main addresses = %v", got)
	}
}

func TestEnvironmentSpecUsesTheSelectedContextAddresses(t *testing.T) {
	cmd, _ := selectedContextFixture(t)
	if err := cmd.PersistentFlags().Set("context", "b"); err != nil {
		t.Fatal(err)
	}
	ans := &setupAnswers{
		App: "api", Environment: "production", Node: "main",
		SourceKind: "image", Image: "ghcr.io/acme/api:1",
		HealthType: "http", HealthPath: "/", HealthTimeout: 30,
		Domain: "app.example.com",
	}
	spec, err := ans.environmentSpec(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.DomainAddresses) != 1 || spec.DomainAddresses[0] != "198.51.100.20" {
		t.Fatalf("environment domain addresses = %v", spec.DomainAddresses)
	}
}

func TestHandoffExportUsesTheSelectedContextIdentity(t *testing.T) {
	cmd, _ := selectedContextFixture(t)
	if err := cmd.PersistentFlags().Set("context", "b"); err != nil {
		t.Fatal(err)
	}
	bDir, err := identityDir("b")
	if err != nil {
		t.Fatal(err)
	}
	wantSSH, err := os.ReadFile(filepath.Join(bDir, "ssh_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	wantWG, err := transport.ReadWireGuardKey(filepath.Join(bDir, "wireguard.key"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := loadHandoffExportIdentity(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if identity.context.Name != "b" || identity.sshKey != strings.TrimSpace(string(wantSSH)) ||
		identity.wgKey.Encode() != wantWG.Encode() || identity.sudoPassword != "sudo-b" {
		t.Fatalf("export identity did not come from b: %+v", identity)
	}
	exported := rpcHandoffExportResult{
		Node: "b", Hostname: "b.example.com", Platform: "debian/amd64", SSHHostKey: "ssh-ed25519 AAAA",
		DatabaseURL: "postgres://b", VaultKeyring: "vault-keyring-b",
	}
	input := handoffExportInput(identity, exported)
	for name, pair := range map[string][2]string{
		"context":             {input.Context, "b"},
		"node":                {input.NodeName, "b"},
		"hostname":            {input.Hostname, "b.example.com"},
		"public address":      {input.PublicAddress, "198.51.100.20"},
		"wireguard endpoint":  {input.WireGuardEndpoint, "198.51.100.20:51820"},
		"wireguard node":      {input.WireGuardNodeAddress, "10.79.0.2"},
		"admin user":          {input.AdminUser, "admin"},
		"sudo password":       {input.SudoPassword, "sudo-b"},
		"operator wg address": {input.OperatorWireGuardAddress, "10.78.0.100"},
		"operator ssh key":    {input.OperatorSSHKey, strings.TrimSpace(string(wantSSH))},
		"operator wireguard":  {input.OperatorWireGuardKey, wantWG.Encode()},
		"database url":        {input.DatabaseURL, "postgres://b"},
		"vault keyring":       {input.VaultKeyring, "vault-keyring-b"},
		"platform":            {input.Platform, "debian/amd64"},
		"ssh host key":        {input.SSHHostKey, "ssh-ed25519 AAAA"},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("export %s = %q, want %q", name, pair[0], pair[1])
		}
	}

	// The environment variable selects the same identity when the flag is absent.
	if err := cmd.PersistentFlags().Set("context", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_CONTEXT", "b")
	fromEnv, err := loadHandoffExportIdentity(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.context.Name != "b" || fromEnv.sshKey != identity.sshKey {
		t.Fatalf("EREGION_CONTEXT=b export identity = %+v", fromEnv)
	}
}

func TestServerInitResumeRefusesADisagreeingContext(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_DATABASE_URL", "")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Context: "a", Hostname: "app.example.com", AdminUser: "admin",
		Phases: map[string]string{},
	}
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	a := newApp()
	if err := a.root.PersistentFlags().Set("context", "b"); err != nil {
		t.Fatal(err)
	}
	a.root.SetArgs([]string{"server", "init", "--resume", "--json"})
	err := a.root.Execute()
	if contract.Classify(err).Code != contract.Usage {
		t.Fatalf("resume --context b = %v, want usage_error", err)
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("resume disagreement message = %v", err)
	}
}
