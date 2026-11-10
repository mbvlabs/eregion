package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/bundle"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"golang.org/x/crypto/ssh"
)

const loginWorkspaceID = "ws_0123456789abcdef01234567"

type workspaceLoginFixture struct {
	bundlePath string
	passPath   string
	name       string
	hostKey    ssh.PublicKey
	workspace  string
	id         string
}

func newWorkspaceLoginFixture(t *testing.T, name, workspace, id string) workspaceLoginFixture {
	t.Helper()
	_, operatorKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(operatorKey, "eregion test operator")
	if err != nil {
		t.Fatal(err)
	}
	sshKey := string(pem.EncodeToMemory(block))
	wg, err := transport.GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	hostPublic := make([]byte, ed25519.PublicKeySize)
	if _, err := rand.Read(hostPublic); err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(ed25519.PublicKey(hostPublic))
	if err != nil {
		t.Fatal(err)
	}
	mainFields := []handoffField{
		{"Context", name},
		{"WireGuard address", "10.79.0.2"},
		{"WireGuard endpoint", "203.0.113.20:51820"},
		{"WireGuard public key", "node-public-key"},
		{"SSH host key", hostKey.Type() + " " + ssh.FingerprintSHA256(hostKey)},
	}
	if workspace != "" || id != "" {
		mainFields = append([]handoffField{{"Workspace", workspace}, {"Workspace ID", id}}, mainFields...)
	}
	body := (&secretsHandoff{node: name, groups: []handoffGroup{
		{"Main node", mainFields},
		{"Operator", []handoffField{
			{"Admin user", "admin"},
			{"SSH private key", "\n" + sshKey},
			{"WireGuard private key", wg.Encode()},
			{"WireGuard address", "10.79.0.1"},
			{"Sudo password", "sudo-" + name},
		}},
	}}).body()
	passphrase := "correct horse battery staple"
	sealed, err := bundle.Encrypt([]byte(body), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "secrets.md")
	if err := os.WriteFile(bundlePath, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	passPath := filepath.Join(dir, "pass.txt")
	if err := os.WriteFile(passPath, []byte(passphrase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspaceLoginFixture{
		bundlePath: bundlePath, passPath: passPath, name: name, hostKey: hostKey,
		workspace: workspace, id: id,
	}
}

func (f workspaceLoginFixture) args(extra ...string) []string {
	return append([]string{"login", "--file", f.bundlePath, "--passphrase-file", f.passPath}, extra...)
}

func executeWorkspaceLogin(t *testing.T, fixture workspaceLoginFixture, nodeWorkspace controlplane.WorkspaceIdentity, verifyErr, readyErr error, extra ...string) error {
	t.Helper()
	a := newApp()
	a.loginSeams.probe = func(context.Context, *host.ContextSSH, string) (ssh.PublicKey, error) {
		return fixture.hostKey, nil
	}
	a.loginSeams.verify = func(context.Context, *host.ContextSSH) (rpcNodeInfoResult, error) {
		return rpcNodeInfoResult{
			Node: fixture.name, Hostname: fixture.name + ".example.com", Platform: "linux/amd64",
			Workspace: nodeWorkspace,
		}, verifyErr
	}
	a.loginSeams.ready = func(context.Context, *host.ContextSSH) error { return readyErr }
	a.root.SetArgs(fixture.args(extra...))
	return a.root.Execute()
}

func TestLoginPublishesVerifiedWorkspaceMetadata(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get("main")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Workspace == nil || saved.Workspace.ID != loginWorkspaceID || saved.Workspace.Name != "thelab" {
		t.Fatalf("saved workspace = %+v", saved.Workspace)
	}
}

func TestLoginWorkspaceMismatchPreservesFilesAndPins(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	first := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	if err := executeWorkspaceLogin(t, first, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	before := filesUnder(t, filepath.Dir(configPath))

	// Same context name, different workspace ID in the bundle. The node still
	// reports the original identity, so an explicit --replace must not publish.
	second := newWorkspaceLoginFixture(t, "main", "thelab", "ws_fedcba9876543210fedcba98")
	err := executeWorkspaceLogin(t, second, node, nil, nil, "--replace", "main")
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("workspace ID mismatch = %v, want conflict", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))
	pin, err := contextKnownHostsPath("main")
	if err != nil {
		t.Fatal(err)
	}
	if !pinnedKeyMatches(pin, net.JoinHostPort("10.79.0.2", "22"), first.hostKey) {
		t.Fatal("the failed workspace import changed the existing pin")
	}
}

func TestLoginConfiguredContextAcceptsOnlyTheSameWorkspace(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	before := filesUnder(t, filepath.Dir(configPath))

	// An old bundle cannot replace a configured context: it would discard the
	// verified identity instead of requiring a refreshed export.
	old := newLoginFixture(t, "main")
	err := executeLogin(t, old, old.hostKey, nil, "--replace", "main")
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("old bundle replacing a configured context = %v, want conflict", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	// The same verified workspace may replace its own context.
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil, "--replace", "main"); err != nil {
		t.Fatalf("same-workspace replacement: %v", err)
	}
}

func TestLoginRefusesTheSameWorkspaceNameWithAnotherIDAcrossContexts(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	alpha := newWorkspaceLoginFixture(t, "alpha", "thelab", loginWorkspaceID)
	if err := executeWorkspaceLogin(t, alpha, controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	before := filesUnder(t, filepath.Dir(configPath))
	betaID := "ws_fedcba9876543210fedcba98"
	beta := newWorkspaceLoginFixture(t, "beta", "thelab", betaID)
	err := executeWorkspaceLogin(t, beta, controlplane.WorkspaceIdentity{Configured: true, ID: betaID, Name: "thelab"}, nil, nil)
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("cross-context workspace collision = %v, want conflict", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("beta"); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("conflicting beta context was saved: %v", err)
	}
}

func TestLoginRefusesWorkspaceBundleAgainstUnconfiguredNode(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	err := executeWorkspaceLogin(t, fixture, controlplane.WorkspaceIdentity{}, nil, nil)
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unconfigured node = %v, want precondition_failed", err)
	}
	store, loadErr := host.LoadContextStore("")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, loadErr := store.Get("main"); contract.Classify(loadErr).Code != contract.NotFound {
		t.Fatalf("a failed workspace import saved a context: %v", loadErr)
	}
}

func TestLoginOldBundleStaysExplicitlyUnconfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	old := newLoginFixture(t, "main")
	if err := executeLogin(t, old, old.hostKey, nil); err != nil {
		t.Fatal(err)
	}
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get("main")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Workspace != nil {
		t.Fatalf("an old bundle invented workspace metadata: %+v", saved.Workspace)
	}
}

func TestLoginSameWorkspaceDifferentLocalContextIsValid(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	for _, name := range []string{"alpha", "beta"} {
		fixture := newWorkspaceLoginFixture(t, name, "thelab", loginWorkspaceID)
		if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
			t.Fatalf("login %s: %v", name, err)
		}
	}
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		saved, err := store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Workspace == nil || saved.Workspace.ID != loginWorkspaceID {
			t.Fatalf("context %s workspace = %+v", name, saved.Workspace)
		}
	}
}

func TestLoginWorkspaceVerificationFailureDoesNotCallReady(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	readyCalled := false
	a := newApp()
	a.loginSeams.probe = func(context.Context, *host.ContextSSH, string) (ssh.PublicKey, error) {
		return fixture.hostKey, nil
	}
	a.loginSeams.verify = func(context.Context, *host.ContextSSH) (rpcNodeInfoResult, error) {
		return rpcNodeInfoResult{}, contract.NewFault(contract.Unavailable, "identity read failed", "Retry.")
	}
	a.loginSeams.ready = func(context.Context, *host.ContextSSH) error {
		readyCalled = true
		return nil
	}
	a.root.SetArgs(fixture.args())
	if err := a.root.Execute(); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("verification failure = %v", err)
	}
	if readyCalled {
		t.Fatal("readiness ran after the workspace identity could not be verified")
	}
	if _, err := host.LoadContextStore(""); err != nil {
		t.Fatalf("context store was corrupted: %v", err)
	}
}

func TestLoginFailsClosedWhenTheWorkspaceReadIsUnknown(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	a := newApp()
	a.loginSeams.probe = func(context.Context, *host.ContextSSH, string) (ssh.PublicKey, error) {
		return fixture.hostKey, nil
	}
	a.loginSeams.verify = func(context.Context, *host.ContextSSH) (rpcNodeInfoResult, error) {
		return rpcNodeInfoResult{Node: "main", WorkspaceError: "control database unavailable"}, nil
	}
	a.loginSeams.ready = func(context.Context, *host.ContextSSH) error { return nil }
	a.root.SetArgs(fixture.args())
	err := a.root.Execute()
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unknown workspace read = %v, want unavailable", err)
	}
	store, loadErr := host.LoadContextStore("")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, loadErr := store.Get("main"); contract.Classify(loadErr).Code != contract.NotFound {
		t.Fatalf("unknown workspace read saved a context: %v", loadErr)
	}
}

func TestLoginWorkspaceBundleRequiresBothFields(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", "")
	if err := executeWorkspaceLogin(t, fixture, controlplane.WorkspaceIdentity{}, nil, nil); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("incomplete workspace bundle = %v, want validation_failed", err)
	}
}
