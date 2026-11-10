package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	a.loginSeams.probe = func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error) {
		return fixture.hostKey, nil
	}
	a.loginSeams.verify = func(context.Context, *host.WorkspaceSSH) (rpcNodeInfoResult, error) {
		return rpcNodeInfoResult{
			Node: fixture.name, Hostname: fixture.name + ".example.com", Platform: "linux/amd64",
			Workspace: nodeWorkspace,
		}, verifyErr
	}
	a.loginSeams.ready = func(context.Context, *host.WorkspaceSSH) error { return readyErr }
	a.root.SetArgs(fixture.args(extra...))
	return a.root.Execute()
}

func executeWorkspaceLoginWith(t *testing.T, fixture workspaceLoginFixture, node controlplane.WorkspaceIdentity, probeKey ssh.PublicKey, verifyHook func(), readyErr error, extra ...string) error {
	t.Helper()
	a := newApp()
	a.loginSeams.probe = func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error) {
		if probeKey != nil {
			return probeKey, nil
		}
		return fixture.hostKey, nil
	}
	a.loginSeams.verify = func(context.Context, *host.WorkspaceSSH) (rpcNodeInfoResult, error) {
		if verifyHook != nil {
			verifyHook()
		}
		return rpcNodeInfoResult{
			Node: fixture.name, Hostname: fixture.name + ".example.com", Platform: "linux/amd64",
			Workspace: node,
		}, nil
	}
	a.loginSeams.ready = func(context.Context, *host.WorkspaceSSH) error { return readyErr }
	a.root.SetArgs(fixture.args(extra...))
	return a.root.Execute()
}

func TestLoginAtomicStagingAndPublicationFailuresPreserveOldAccess(t *testing.T) {
	t.Run("wrong host key after staging", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "contexts.json")
		t.Setenv("EREGION_WORKSPACE_FILE", configPath)
		fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
		_, otherKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		otherPublic, err := ssh.NewPublicKey(otherKey.Public())
		if err != nil {
			t.Fatal(err)
		}
		readyCalled := false
		a := newApp()
		a.loginSeams.probe = func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error) { return otherPublic, nil }
		a.loginSeams.verify = func(context.Context, *host.WorkspaceSSH) (rpcNodeInfoResult, error) {
			return rpcNodeInfoResult{}, nil
		}
		a.loginSeams.ready = func(context.Context, *host.WorkspaceSSH) error { readyCalled = true; return nil }
		a.root.SetArgs(fixture.args())
		if err := a.root.Execute(); contract.Classify(err).Code != contract.PreconditionFailed {
			t.Fatalf("wrong host key = %v, want precondition_failed", err)
		}
		if readyCalled {
			t.Fatal("readiness ran after a host-key mismatch")
		}
		store, err := host.LoadWorkspaceStore(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(store.Access) != 0 {
			t.Fatalf("failed login published access: %+v", store.Access)
		}
		for entry := range filesUnder(t, filepath.Dir(configPath)) {
			if strings.Contains(entry, ".eregion-login-") {
				t.Fatalf("staged login directory remains: %s", entry)
			}
		}
	})

	t.Run("readiness failure after verification", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "contexts.json")
		t.Setenv("EREGION_WORKSPACE_FILE", configPath)
		fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
		node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
		err := executeWorkspaceLoginWith(t, fixture, node, nil, nil, contract.NewFault(contract.Unavailable, "readiness failed", "Retry."))
		if contract.Classify(err).Code != contract.Unavailable {
			t.Fatalf("readiness failure = %v", err)
		}
		store, loadErr := host.LoadWorkspaceStore(configPath)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if len(store.Access) != 0 {
			t.Fatalf("readiness failure published access: %+v", store.Access)
		}
	})

	t.Run("publication failure after verification", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission failure is not enforceable as root")
		}
		configPath := filepath.Join(t.TempDir(), "contexts.json")
		t.Setenv("EREGION_WORKSPACE_FILE", configPath)
		fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
		node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
		if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
			t.Fatal(err)
		}
		storeBefore, err := host.LoadWorkspaceStore(configPath)
		if err != nil {
			t.Fatal(err)
		}
		accessBefore, ok := storeBefore.AccessByName("thelab")
		if !ok {
			t.Fatal("initial verified access missing")
		}
		credentialDir, err := storeBefore.CredentialDirFor(accessBefore)
		if err != nil {
			t.Fatal(err)
		}
		readyErr := error(nil)
		a := newApp()
		a.loginSeams.probe = func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error) { return fixture.hostKey, nil }
		a.loginSeams.verify = func(context.Context, *host.WorkspaceSSH) (rpcNodeInfoResult, error) {
			return rpcNodeInfoResult{Node: "main", Hostname: "main.example.com", Workspace: node}, nil
		}
		a.loginSeams.ready = func(context.Context, *host.WorkspaceSSH) error {
			_ = os.Remove(configPath + ".lock")
			if err := os.Mkdir(configPath+".lock", 0o700); err != nil {
				return err
			}
			return readyErr
		}
		a.root.SetArgs(fixture.args("--replace", "thelab"))
		err = a.root.Execute()
		_ = os.RemoveAll(configPath + ".lock")
		if err == nil {
			t.Fatal("publication failure was reported as success")
		}
		storeAfter, loadErr := host.LoadWorkspaceStore(configPath)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		accessAfter, ok := storeAfter.AccessByName("thelab")
		if !ok || accessAfter.CredentialDir != accessBefore.CredentialDir {
			t.Fatalf("failed publication changed local access: before=%+v after=%+v", accessBefore, accessAfter)
		}
		if _, err := os.Stat(credentialDir); err != nil {
			t.Fatalf("failed publication deleted the active credential directory: %v", err)
		}
		for entry := range filesUnder(t, filepath.Dir(configPath)) {
			if strings.Contains(entry, ".eregion-login-") {
				t.Fatalf("failed publication left a staged directory: %s", entry)
			}
		}
	})

	t.Run("unrelated store update during verification", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "contexts.json")
		t.Setenv("EREGION_WORKSPACE_FILE", configPath)
		fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
		node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
		hook := func() {
			store, err := host.LoadWorkspaceStore(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishWorkspace("ws_fedcba9876543210fedcba98", "unrelated", host.WorkspaceAccess{
				Name: "unrelated", SSH: &host.WorkspaceSSH{Address: "10.79.0.2:22", User: "admin", KnownHosts: "/dev/null"},
			}, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := executeWorkspaceLoginWith(t, fixture, node, nil, hook, nil); err != nil {
			t.Fatal(err)
		}
		store, err := host.LoadWorkspaceStore(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := store.AccessByName("unrelated"); !ok {
			t.Fatal("concurrent unrelated workspace was lost")
		}
		if store.Current != "thelab" {
			t.Fatalf("current = %q, want thelab", store.Current)
		}
	})

	t.Run("target conflict during verification", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "contexts.json")
		t.Setenv("EREGION_WORKSPACE_FILE", configPath)
		fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
		node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
		hook := func() {
			store, err := host.LoadWorkspaceStore(configPath)
			if err != nil {
				t.Fatal(err)
			}
			store.Access = append(store.Access, host.WorkspaceAccess{
				Name: "conflict", SSH: &host.WorkspaceSSH{Address: "10.79.0.2:22", User: "admin", KnownHosts: "/dev/null"},
				Workspace: &host.WorkspaceIdentity{ID: "ws_fedcba9876543210fedcba98", Name: "thelab"},
			})
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
		}
		err := executeWorkspaceLoginWith(t, fixture, node, nil, hook, nil)
		if contract.Classify(err).Code != contract.Conflict {
			t.Fatalf("target conflict during verification = %v, want conflict", err)
		}
		store, loadErr := host.LoadWorkspaceStore(configPath)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if store.Current != "" || len(store.Access) != 1 || store.Access[0].Name != "conflict" {
			t.Fatalf("conflicting verification mutated store: current=%q access=%+v", store.Current, store.Access)
		}
		for entry := range filesUnder(t, filepath.Dir(configPath)) {
			if strings.Contains(entry, ".eregion-login-") {
				t.Fatalf("staged login directory remains: %s", entry)
			}
		}
	})
}

func TestLoginPublishesVerifiedWorkspaceMetadata(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := store.AccessByName("thelab")
	if !ok {
		t.Fatal("thelab access missing")
	}
	if err != nil {
		t.Fatal(err)
	}
	if saved.Workspace == nil || saved.Workspace.ID != loginWorkspaceID || saved.Workspace.Name != "thelab" {
		t.Fatalf("saved workspace = %+v", saved.Workspace)
	}
}

func TestLoginSuccessCardConfiguredLeadsVerifiedWorkspace(t *testing.T) {
	const route = "admin@10.78.0.1 over WireGuard"
	title, rows, plain := loginSuccessPresentation("main", route, "thelab", loginWorkspaceID, true)
	if title != "Logged in to workspace" {
		t.Fatalf("card title = %q", title)
	}
	want := [][2]string{
		{"workspace", "thelab (current)"},
		{"workspace id", loginWorkspaceID},
		{"node", "main"},
		{"route", route},
	}
	if len(rows) != len(want) {
		t.Fatalf("card rows = %+v", rows)
	}
	workspaceRows := 0
	for index, row := range rows {
		if row != want[index] {
			t.Fatalf("card row %d = %+v, want %+v", index, row, want[index])
		}
		if row[0] == "context" {
			t.Fatalf("card still presents a context row: %+v", rows)
		}
		if row[0] == "workspace" {
			workspaceRows++
		}
	}
	if workspaceRows != 1 {
		t.Fatalf("card has %d workspace rows: %+v", workspaceRows, rows)
	}
	if plain != "logged in to workspace thelab ("+loginWorkspaceID+")" {
		t.Fatalf("plain success = %q", plain)
	}
}

func TestLoginSuccessCardSeparatesDivergentLocalAlias(t *testing.T) {
	const localAlias = "second-machine"
	const workspace = "thelab"
	const route = "admin@10.78.0.1 over WireGuard"
	data := loginSuccessData("main", route, workspace, loginWorkspaceID, true)
	title, rows, plain := loginSuccessPresentation("main", route, workspace, loginWorkspaceID, true)
	rendered := title + fmt.Sprint(rows) + plain
	if _, ok := data["context"]; ok {
		t.Fatalf("machine payload still exposes a context key: %+v", data)
	}
	if strings.Contains(rendered, localAlias) || strings.Contains(rendered, "context") {
		t.Fatalf("human card used the local alias or context label: %q", rendered)
	}
	if !strings.Contains(rendered, workspace) || !strings.Contains(rendered, loginWorkspaceID) {
		t.Fatalf("human card does not lead with the verified workspace: %q", rendered)
	}
}

func TestLoginSuccessUnconfiguredIsTruthful(t *testing.T) {
	const localAlias = "legacy-main"
	const route = "admin@10.78.0.1 over WireGuard"
	title, rows, plain := loginSuccessPresentation("main", route, "", "", false)
	data := loginSuccessData("main", route, "", "", false)
	if title != "Logged in" {
		t.Fatalf("unconfigured title = %q", title)
	}
	if len(rows) != 3 || rows[0] != [2]string{"workspace", "not configured (older bundle)"} ||
		rows[1] != [2]string{"node", "main"} || rows[2] != [2]string{"route", route} {
		t.Fatalf("unconfigured rows = %+v", rows)
	}
	rendered := title + fmt.Sprint(rows) + plain
	if strings.Contains(rendered, localAlias) {
		t.Fatalf("unconfigured card reused the local alias: %q", rendered)
	}
	for _, row := range rows {
		if row[0] == "context" || row[0] == "workspace id" {
			t.Fatalf("unconfigured card invented workspace identity: %+v", rows)
		}
	}
	if !strings.Contains(plain, "workspace identity not configured") {
		t.Fatalf("unconfigured plain message = %q", plain)
	}
	if data["workspace_configured"] != false {
		t.Fatalf("unconfigured machine payload = %+v", data)
	}
	if _, ok := data["workspace"]; ok {
		t.Fatalf("unconfigured payload invented workspace identity: %+v", data)
	}
}

func TestLoginSuccessDataKeepsWorkspaceFieldsWithoutContextKey(t *testing.T) {
	const localAlias = "second-machine"
	const route = "admin@10.78.0.1 over WireGuard"
	data := loginSuccessData("main", route, "thelab", loginWorkspaceID, true)
	workspace, ok := data["workspace"].(map[string]string)
	if !ok || workspace["name"] != "thelab" || workspace["id"] != loginWorkspaceID {
		t.Fatalf("workspace payload = %+v", data["workspace"])
	}
	if data["node"] != "main" || data["route"] != route || data["workspace_configured"] != true {
		t.Fatalf("machine payload changed = %+v", data)
	}
}

func TestLoginReplacementLeavesOldCredentialFilesUntouched(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := store.AccessByName("thelab")
	if !ok {
		t.Fatal("first verified access missing")
	}
	oldDir, err := store.CredentialDirFor(before)
	if err != nil {
		t.Fatal(err)
	}
	oldFiles := filesUnder(t, oldDir)
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil, "--replace", "thelab"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := reloaded.AccessByName("thelab")
	if !ok {
		t.Fatal("replacement access missing")
	}
	newDir, err := reloaded.CredentialDirFor(after)
	if err != nil {
		t.Fatal(err)
	}
	if newDir == oldDir {
		t.Fatal("canonical replacement overwrote the old credential directory")
	}
	assertFilesEqual(t, oldFiles, filesUnder(t, oldDir))
}

func TestLoginWorkspaceMismatchPreservesFilesAndPins(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	first := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	if err := executeWorkspaceLogin(t, first, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	before := filesUnder(t, filepath.Dir(configPath))

	// Same context name, different workspace ID in the bundle. The node still
	// reports the original identity, so an explicit --replace must not publish.
	second := newWorkspaceLoginFixture(t, "main", "thelab", "ws_fedcba9876543210fedcba98")
	err := executeWorkspaceLogin(t, second, node, nil, nil, "--replace", "thelab")
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("workspace ID mismatch = %v, want conflict", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := store.AccessByName("thelab")
	if !ok {
		t.Fatal("existing verified access missing")
	}
	if !pinnedKeyMatches(saved.SSH.KnownHosts, net.JoinHostPort("10.79.0.2", "22"), first.hostKey) {
		t.Fatal("the failed workspace import changed the existing pin")
	}
}

func TestLoginConfiguredContextAcceptsOnlyTheSameWorkspace(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	before := filesUnder(t, filepath.Dir(configPath))

	// An old bundle cannot replace a configured context: it would discard the
	// verified identity instead of requiring a refreshed export.
	old := newLoginFixture(t, "main")
	err := executeLogin(t, old, old.hostKey, nil)
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("old bundle alongside a verified workspace = %v, want conflict", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	// The same verified workspace may replace its own canonical access.
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil, "--replace", "thelab"); err != nil {
		t.Fatalf("same-workspace replacement: %v", err)
	}
}

func TestLoginRefusesTheSameWorkspaceNameWithAnotherIDAcrossContexts(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
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
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved, ok := store.AccessByName("thelab"); !ok || saved.Workspace.ID != loginWorkspaceID {
		t.Fatalf("conflicting workspace replaced the existing access: %+v", saved)
	}
}

func TestLoginRefusesWorkspaceBundleAgainstUnconfiguredNode(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	err := executeWorkspaceLogin(t, fixture, controlplane.WorkspaceIdentity{}, nil, nil)
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unconfigured node = %v, want precondition_failed", err)
	}
	store, loadErr := host.LoadWorkspaceStore("")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, ok := store.AccessByName("thelab"); ok {
		t.Fatal("a failed workspace import saved local access")
	}
}

func TestLoginOldBundleStaysExplicitlyUnconfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	old := newLoginFixture(t, "main")
	if err := executeLogin(t, old, old.hostKey, nil); err != nil {
		t.Fatal(err)
	}
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := store.AccessByName("main")
	if !ok {
		t.Fatal("legacy access missing")
	}
	if saved.Workspace != nil {
		t.Fatalf("an old bundle invented workspace metadata: %+v", saved.Workspace)
	}
}

func TestLoginCanonicalReplacementPreservesUnrelatedWorkspace(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	node := controlplane.WorkspaceIdentity{Configured: true, ID: loginWorkspaceID, Name: "thelab"}
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil); err != nil {
		t.Fatal(err)
	}
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	otherID := "ws_fedcba9876543210fedcba98"
	if _, err := store.PublishWorkspace(otherID, "other", host.WorkspaceAccess{
		Name: "other", SSH: &host.WorkspaceSSH{Address: "10.79.0.2:22", User: "admin", KnownHosts: "/dev/null"},
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("repeat login without --replace = %v, want conflict", err)
	}
	if err := executeWorkspaceLogin(t, fixture, node, nil, nil, "--replace", "thelab"); err != nil {
		t.Fatalf("canonical replacement: %v", err)
	}
	reloaded, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	views := reloaded.ListWorkspaces()
	if len(views) != 2 {
		t.Fatalf("replacement disturbed unrelated workspaces: %+v", views)
	}
	if _, ok := reloaded.AccessByName("other"); !ok {
		t.Fatal("unrelated workspace access was lost")
	}
	count := 0
	for _, access := range reloaded.Access {
		if access.Workspace != nil && access.Workspace.ID == loginWorkspaceID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("canonical workspace has %d access records, want 1", count)
	}
}

func TestLoginWorkspaceVerificationFailureDoesNotCallReady(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	readyCalled := false
	a := newApp()
	a.loginSeams.probe = func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error) {
		return fixture.hostKey, nil
	}
	a.loginSeams.verify = func(context.Context, *host.WorkspaceSSH) (rpcNodeInfoResult, error) {
		return rpcNodeInfoResult{}, contract.NewFault(contract.Unavailable, "identity read failed", "Retry.")
	}
	a.loginSeams.ready = func(context.Context, *host.WorkspaceSSH) error {
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
	if _, err := host.LoadWorkspaceStore(""); err != nil {
		t.Fatalf("context store was corrupted: %v", err)
	}
}

func TestLoginFailsClosedWhenTheWorkspaceReadIsUnknown(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", loginWorkspaceID)
	a := newApp()
	a.loginSeams.probe = func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error) {
		return fixture.hostKey, nil
	}
	a.loginSeams.verify = func(context.Context, *host.WorkspaceSSH) (rpcNodeInfoResult, error) {
		return rpcNodeInfoResult{Node: "main", WorkspaceError: "control database unavailable"}, nil
	}
	a.loginSeams.ready = func(context.Context, *host.WorkspaceSSH) error { return nil }
	a.root.SetArgs(fixture.args())
	err := a.root.Execute()
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unknown workspace read = %v, want unavailable", err)
	}
	store, loadErr := host.LoadWorkspaceStore("")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, ok := store.AccessByName("thelab"); ok {
		t.Fatal("unknown workspace read saved local access")
	}
}

func TestLoginWorkspaceBundleRequiresBothFields(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	fixture := newWorkspaceLoginFixture(t, "main", "thelab", "")
	if err := executeWorkspaceLogin(t, fixture, controlplane.WorkspaceIdentity{}, nil, nil); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("incomplete workspace bundle = %v, want validation_failed", err)
	}
}
