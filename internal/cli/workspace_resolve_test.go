package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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

func writeTestWorkspace(t *testing.T, store *host.WorkspaceStore, accessName, workspaceName, workspaceID, nodeAddress, endpoint string) {
	t.Helper()
	dir := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "eregion test workspace")
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
	peer, err := transport.GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	wgPath := filepath.Join(dir, "wireguard.key")
	if err := os.WriteFile(wgPath, []byte(wg.Encode()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if err := pinHostKey(knownHosts, net.JoinHostPort(nodeAddress, "22"), publicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkspace(workspaceID, workspaceName, host.WorkspaceAccess{
		Name: accessName,
		SSH: &host.WorkspaceSSH{
			Address: net.JoinHostPort(nodeAddress, "22"), User: "admin",
			Identity: sshPath, KnownHosts: knownHosts,
			WireGuard: &host.WorkspaceWireGuard{
				PrivateKeyFile: wgPath, Address: "10.78.0.1",
				NodeAddress: nodeAddress, NodePublicKey: peer.PublicKey().String(), Endpoint: endpoint,
			},
		},
		CredentialDir: filepath.Join("servers", accessName),
	}, false); err != nil {
		t.Fatal(err)
	}
}

func workspaceFixture(t *testing.T) (*cobra.Command, *host.WorkspaceStore) {
	t.Helper()
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "")
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestWorkspace(t, store, "alpha", "thelab", "ws_0123456789abcdef01234567", "10.78.0.2", "203.0.113.10:51820")
	writeTestWorkspace(t, store, "beta", "second", "ws_fedcba9876543210fedcba98", "10.79.0.2", "198.51.100.20:51820")
	writeTestWorkspace(t, store, "gamma", "third", "ws_aaaabbbbccccddddeeeeffff", "10.80.0.2", "203.0.113.30:51820")
	if _, err := store.UseWorkspace("thelab"); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "eregion"}
	cmd.PersistentFlags().String("workspace", "", "Workspace name")
	cmd.PersistentFlags().String("workspace-file", "", "Private store")
	return cmd, store
}

func TestNodeClientResolvesWorkspaceFlagThenEnvironmentThenCurrent(t *testing.T) {
	t.Setenv("EREGION_TUNNEL", "inprocess")
	cmd, _ := workspaceFixture(t)
	a := &app{secrets: &secrets.Registry{}}

	client, err := a.nodeClient(cmd)
	if err != nil {
		t.Fatalf("%v (hint: %s)", err, contract.Classify(err).Hint)
	}
	if client.workspaceID != "ws_0123456789abcdef01234567" || client.accessName != "alpha" {
		t.Fatalf("current workspace = %s/%s, want thelab/alpha", client.workspaceID, client.accessName)
	}

	if err := cmd.PersistentFlags().Set("workspace", "second"); err != nil {
		t.Fatal(err)
	}
	client, err = a.nodeClient(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if client.workspaceID != "ws_fedcba9876543210fedcba98" {
		t.Fatalf("--workspace second reached %s", client.workspaceID)
	}

	// The flag wins over the environment variable.
	t.Setenv("EREGION_WORKSPACE", "third")
	client, err = a.nodeClient(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if client.workspaceID != "ws_fedcba9876543210fedcba98" {
		t.Fatalf("flag with EREGION_WORKSPACE=third reached %s, want second", client.workspaceID)
	}

	if err := cmd.PersistentFlags().Set("workspace", ""); err != nil {
		t.Fatal(err)
	}
	client, err = a.nodeClient(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if client.workspaceID != "ws_aaaabbbbccccddddeeeeffff" {
		t.Fatalf("EREGION_WORKSPACE=third reached %s", client.workspaceID)
	}

	if err := os.Unsetenv("EREGION_WORKSPACE"); err != nil {
		t.Fatal(err)
	}
	client, err = a.nodeClient(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if client.workspaceID != "ws_0123456789abcdef01234567" {
		t.Fatalf("without a flag or variable reached %s, want the saved current thelab", client.workspaceID)
	}

	if err := cmd.PersistentFlags().Set("workspace", "ghost"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.nodeClient(cmd); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("unknown workspace = %v, want not_found", err)
	} else if !strings.Contains(err.Error(), "workspace ghost") || !strings.Contains(contract.Classify(err).Hint, "eregion workspace list") {
		t.Fatalf("unknown workspace fault is not actionable: %v", err)
	}
}

func TestSSHResolvesOnlyVerifiedWorkspaceAndValidatesActualNode(t *testing.T) {
	t.Setenv("EREGION_TUNNEL", "inprocess")
	cmd, _ := workspaceFixture(t)
	fakeClient := func(access, workspace, id, node string) *nodeClient {
		return &nodeClient{
			remote:     &fakeNodeTransport{info: rpcNodeInfoResult{Node: node, Hostname: node + ".example.com", Platform: "linux/amd64"}},
			accessName: access, workspaceName: workspace, workspaceID: id,
			checked: true,
		}
	}
	a := &app{secrets: &secrets.Registry{}, nodeClients: map[string]*nodeClient{
		"alpha": fakeClient("alpha", "thelab", "ws_0123456789abcdef01234567", "main"),
		"beta":  fakeClient("beta", "second", "ws_fedcba9876543210fedcba98", "other"),
		"gamma": fakeClient("gamma", "third", "ws_aaaabbbbccccddddeeeeffff", "third"),
	}}
	client, err := a.sshClientFor(cmd, nil)
	if err != nil || client.workspaceID != "ws_0123456789abcdef01234567" {
		t.Fatalf("default ssh client = %+v, %v", client, err)
	}
	if err := cmd.PersistentFlags().Set("workspace", "second"); err != nil {
		t.Fatal(err)
	}
	client, err = a.sshClientFor(cmd, nil)
	if err != nil || client.workspaceID != "ws_fedcba9876543210fedcba98" {
		t.Fatalf("flagged ssh client = %+v, %v", client, err)
	}
	// The historical private access alias never selects or routes a target.
	if _, err := a.sshClientFor(cmd, []string{"beta"}); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("historical alias positional target = %v, want not_found", err)
	}
	if _, err := a.sshClientFor(cmd, []string{"main"}); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("wrong node for selected workspace = %v, want not_found", err)
	}
	if client, err := a.sshClientFor(cmd, []string{"other"}); err != nil || client.workspaceID != "ws_fedcba9876543210fedcba98" {
		t.Fatalf("matching node target = %+v, %v", client, err)
	}
	if err := cmd.PersistentFlags().Set("workspace", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("EREGION_WORKSPACE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_WORKSPACE", "third")
	client, err = a.sshClientFor(cmd, nil)
	if err != nil || client.workspaceID != "ws_aaaabbbbccccddddeeeeffff" {
		t.Fatalf("environment-selected ssh client = %+v, %v", client, err)
	}
}

func TestWorkspaceAliasIsNotAPublicSelector(t *testing.T) {
	cmd, _ := workspaceFixture(t)
	// alpha is only the private access key; the public target is the verified
	// workspace name.
	if err := cmd.PersistentFlags().Set("workspace", "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := selectedWorkspaceAccess(cmd); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("historical alias selected a workspace: %v", err)
	}
	if err := cmd.PersistentFlags().Set("workspace", "thelab"); err != nil {
		t.Fatal(err)
	}
	access, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if access.Name != "alpha" || access.Workspace.ID != "ws_0123456789abcdef01234567" {
		t.Fatalf("verified workspace resolved to %+v", access)
	}
}

func TestWorkspaceFlagWorksBeforeAndAfterTheCommand(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "")
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestWorkspace(t, store, "alpha", "thelab", "ws_0123456789abcdef01234567", "10.78.0.2", "203.0.113.10:51820")
	for _, args := range [][]string{
		{"--workspace", "thelab", "version", "--json"},
		{"version", "--workspace", "thelab", "--json"},
	} {
		var stdout, stderr bytes.Buffer
		a := newApp()
		code := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
		if code != 0 {
			t.Fatalf("%v exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestObsoleteWorkspaceEnvironmentFailsExplicitly(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	if _, err := host.LoadWorkspaceStore(""); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("obsolete EREGION_CONTEXT_FILE = %v, want validation_failed", err)
	}
	code, stdout, _ := invoke(t, "version", "--json")
	if code != 2 {
		t.Fatalf("obsolete env CLI exit = %d", code)
	}
	if envelope := decodeOne(t, stdout); envelope.Error == nil || envelope.Error.Code != contract.ValidationFailed {
		t.Fatalf("obsolete env envelope = %+v", envelope)
	}
	t.Setenv("EREGION_CONTEXT_FILE", "")
	t.Setenv("EREGION_CONTEXT", "thelab")
	if _, err := host.LoadWorkspaceStore(""); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("obsolete EREGION_CONTEXT = %v, want validation_failed", err)
	}
}

func TestWorkspaceUseAndRemoveRequireWorkspaceIdentityNotAlias(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestWorkspace(t, store, "alpha", "thelab", "ws_0123456789abcdef01234567", "10.78.0.2", "203.0.113.10:51820")
	cmd := &cobra.Command{}
	cmd.Flags().String("workspace-file", "", "")
	cmd.Flags().String("confirm", "", "")
	if err := cmd.Flags().Set("workspace-file", store.Path); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("confirm", "alpha"); err != nil {
		t.Fatal(err)
	}
	// Removal by private alias is not a workspace selector and finds nothing.
	spec := (&app{}).workspaceRemoveSpec(func(summary, example string, mutating bool) CommandMeta {
		return readMeta(summary, []string{"test"}, []string{example})
	})
	if _, err := spec.run(cmd, []string{"alpha"}); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("alias removal = %v, want not_found", err)
	}
	if err := cmd.Flags().Set("confirm", "ws_0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	if _, err := spec.run(cmd, []string{"ws_0123456789abcdef01234567"}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceCredentialDirectoryUsesExistingPrivateFact(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "")
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	access := host.WorkspaceAccess{
		Name: "historical-alias", SSH: &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/dev/null"},
	}
	if _, err := store.PublishWorkspace("ws_0123456789abcdef01234567", "thelab", access, false); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.Flags().String("workspace-file", "", "")
	if err := cmd.Flags().Set("workspace-file", store.Path); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceCredentialDir(cmd, access)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(store.Path), "servers", "historical-alias")
	if dir != want {
		t.Fatalf("credential directory = %q, want historical private fact %q", dir, want)
	}
	custom := host.WorkspaceAccess{
		Name: "custom", CredentialDir: filepath.Join("servers", "custom-dir"),
		SSH: &host.WorkspaceSSH{Address: "10.79.0.1:22", User: "admin", KnownHosts: "/dev/null"},
	}
	if _, err := store.PublishWorkspace("ws_fedcba9876543210fedcba98", "custom", custom, false); err != nil {
		t.Fatal(err)
	}
	dir, err = workspaceCredentialDir(cmd, custom)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(store.Path), "servers", "custom-dir"); dir != want {
		t.Fatalf("custom credential directory = %q, want %q", dir, want)
	}
}

func TestWorkspaceSnapshotIsStableWithinInvocation(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "")
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestWorkspace(t, store, "alpha", "thelab", "ws_0123456789abcdef01234567", "10.78.0.2", "203.0.113.10:51820")
	cmd := &cobra.Command{}
	cmd.Flags().String("workspace", "", "")
	cmd.Flags().String("workspace-file", "", "")
	if err := cmd.Flags().Set("workspace", "thelab"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("workspace-file", store.Path); err != nil {
		t.Fatal(err)
	}
	cmd.SetContext(context.WithValue(context.Background(), workspaceSnapshotKey{}, &workspaceSnapshot{}))
	first, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := host.LoadWorkspaceStore(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	for index := range updated.Access {
		if updated.Access[index].Name == "alpha" {
			updated.Access[index].SSH.Address = "10.99.0.9:22"
		}
	}
	if err := updated.Save(); err != nil {
		t.Fatal(err)
	}
	second, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if first.SSH.Address != "10.78.0.2:22" || second.SSH.Address != first.SSH.Address {
		t.Fatalf("invocation snapshot diverged: first=%q second=%q", first.SSH.Address, second.SSH.Address)
	}
}

func TestTunnelSelectionUsesTheSharedVerifiedWorkspace(t *testing.T) {
	cmd, _ := workspaceFixture(t)
	cfg, err := tunnelWorkspaceConfig(cmd)
	if err != nil || cfg.Endpoint != "203.0.113.10:51820" {
		t.Fatalf("default tunnel endpoint = %q, %v", cfg.Endpoint, err)
	}
	if err := cmd.PersistentFlags().Set("workspace", "second"); err != nil {
		t.Fatal(err)
	}
	cfg, err = tunnelWorkspaceConfig(cmd)
	if err != nil || cfg.Endpoint != "198.51.100.20:51820" {
		t.Fatalf("flagged tunnel endpoint = %q, %v", cfg.Endpoint, err)
	}
	if err := cmd.PersistentFlags().Set("workspace", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("EREGION_WORKSPACE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_WORKSPACE", "third")
	cfg, err = tunnelWorkspaceConfig(cmd)
	if err != nil || cfg.Endpoint != "203.0.113.30:51820" {
		t.Fatalf("environment tunnel endpoint = %q, %v", cfg.Endpoint, err)
	}
}

func TestWorkspaceLabelsNeverExposePrivateAlias(t *testing.T) {
	cmd, _ := workspaceFixture(t)
	if err := cmd.PersistentFlags().Set("workspace", "thelab"); err != nil {
		t.Fatal(err)
	}
	if label := currentWorkspaceName(cmd); label != "thelab" || label == "alpha" {
		t.Fatalf("authenticator label = %q", label)
	}
	verified := host.WorkspaceAccess{Name: "alpha", Workspace: &host.WorkspaceIdentity{ID: "ws_0123456789abcdef01234567", Name: "thelab"}}
	if label := imagePushTarget(&nodeClient{accessName: verified.Name, workspaceName: verifiedWorkspaceName(verified)}); label != "thelab" || label == "alpha" {
		t.Fatalf("image push label = %q", label)
	}
	unconfigured := host.WorkspaceAccess{Name: "historical-alias"}
	if label := imagePushTarget(&nodeClient{accessName: unconfigured.Name, workspaceName: verifiedWorkspaceName(unconfigured)}); label == "historical-alias" {
		t.Fatalf("unconfigured image push exposed private alias: %q", label)
	}
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "")
	unconfiguredStore, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	unconfiguredStore.Access = []host.WorkspaceAccess{unconfigured}
	unconfiguredStore.Current = unconfigured.Name
	if err := unconfiguredStore.Save(); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unconfigured access was not refused before persistence: %v", err)
	}
	if _, err := os.Stat(unconfiguredStore.Path); !os.IsNotExist(err) {
		t.Fatalf("unconfigured access created a store: %v", err)
	}
	unconfiguredCmd := &cobra.Command{}
	unconfiguredCmd.Flags().String("workspace-file", "", "")
	if err := unconfiguredCmd.Flags().Set("workspace-file", unconfiguredStore.Path); err != nil {
		t.Fatal(err)
	}
	if label := currentWorkspaceName(unconfiguredCmd); label != "unconfigured workspace" {
		t.Fatalf("unconfigured authenticator label = %q", label)
	}
}

func TestApplyRejectsUnconfiguredWorkspace(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "")
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	store.Access = []host.WorkspaceAccess{{
		Name: "historical", SSH: &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/dev/null"},
	}}
	store.Current = "historical"
	if err := store.Save(); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unconfigured access was not refused before persistence: %v", err)
	}
	// Simulate a malformed on-disk record without weakening the publication
	// validator merely to construct the fixture.
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := invoke(t, "apply", "--json")
	if code == 0 {
		t.Fatal("apply accepted unconfigured workspace")
	}
	if envelope := decodeOne(t, stdout); envelope.Error == nil || envelope.Error.Code != contract.PreconditionFailed {
		t.Fatalf("unconfigured workspace error = %+v", envelope.Error)
	}
	if after, err := os.ReadFile(store.Path); err != nil || !bytes.Equal(raw, after) {
		t.Fatalf("refused apply changed the malformed store: %v", err)
	}
}

func TestApplyTargetsSelectedVerifiedWorkspace(t *testing.T) {
	cmd, _ := workspaceFixture(t)
	cmd.Flags().Bool("dry-run", true, "")
	if err := cmd.PersistentFlags().Set("workspace", "second"); err != nil {
		t.Fatal(err)
	}
	first, second := &workspaceApplyTransport{}, &workspaceApplyTransport{}
	a := &app{nodeClients: map[string]*nodeClient{
		"alpha": {remote: first, checked: true},
		"beta":  {remote: second, checked: true},
	}}
	if _, err := a.hostApply(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if first.run != 0 || second.run != 1 || second.params.Method != methodHostApply {
		t.Fatalf("apply calls: first=%d second=%d method=%q", first.run, second.run, second.params.Method)
	}
}

func TestObsoleteContextCommandAndFlagAreRemoved(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	for _, args := range [][]string{
		{"context", "list", "--json"},
		{"--context", "thelab", "version", "--json"},
	} {
		code, stdout, _ := invoke(t, args...)
		if code != 2 {
			t.Fatalf("%v exit=%d, want usage exit 2", args, code)
		}
		envelope := decodeOne(t, stdout)
		if envelope.Error == nil || envelope.Error.Code != contract.Usage {
			t.Fatalf("%v error = %+v", args, envelope.Error)
		}
	}
}

func TestWorkspaceCommandsAndServerInitRejectExplicitSelection(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.PersistentFlags().String("workspace", "", "")
	if err := cmd.PersistentFlags().Set("workspace", "thelab"); err != nil {
		t.Fatal(err)
	}
	if err := rejectWorkspaceSelection(cmd, "server init"); contract.Classify(err).Code != contract.Usage {
		t.Fatalf("server init --workspace = %v, want usage_error", err)
	}
}
