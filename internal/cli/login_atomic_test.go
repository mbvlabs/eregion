package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/bundle"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"golang.org/x/crypto/ssh"
)

type loginFixture struct {
	bundlePath       string
	passPath         string
	name             string
	wireGuardAddress string
	hostKey          ssh.PublicKey
	sshKey           string
}

func newLoginFixture(t *testing.T, name string) loginFixture {
	t.Helper()
	return newLoginFixtureAt(t, name, "10.79.0.2")
}

// newLoginFixtureAt builds a handoff for a node whose SSH host is reached at
// wireGuardAddress. Real nodes all use the same WireGuard address, so tests
// use this to prove their pins stay separate.
func newLoginFixtureAt(t *testing.T, name, wireGuardAddress string) loginFixture {
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
	body := (&secretsHandoff{node: name, groups: []handoffGroup{
		{"Main node", []handoffField{
			{"Context", name},
			{"WireGuard address", wireGuardAddress},
			{"WireGuard endpoint", "203.0.113.20:51820"},
			{"WireGuard public key", "node-public-key"},
			{"SSH host key", hostKey.Type() + " " + ssh.FingerprintSHA256(hostKey)},
		}},
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
	return loginFixture{bundlePath: bundlePath, passPath: passPath, name: name, wireGuardAddress: wireGuardAddress, hostKey: hostKey, sshKey: strings.TrimSpace(sshKey)}
}

func (f loginFixture) args(extra ...string) []string {
	return append([]string{"login", "--file", f.bundlePath, "--passphrase-file", f.passPath}, extra...)
}

func filesUnder(t *testing.T, paths ...string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = raw
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return files
}

func assertFilesEqual(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("file set changed: before %d, after %d", len(before), len(after))
	}
	for path, raw := range before {
		if got, ok := after[path]; !ok || string(got) != string(raw) {
			t.Fatalf("%s changed after a failed login", path)
		}
	}
}

// executeLogin runs login with the probe and readiness seams replaced.
func executeLogin(t *testing.T, fixture loginFixture, key ssh.PublicKey, readyErr error, extra ...string) error {
	t.Helper()
	a := newApp()
	a.loginSeams.probe = func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error) {
		return key, nil
	}
	a.loginSeams.ready = func(context.Context, *host.WorkspaceSSH) error { return readyErr }
	a.root.SetArgs(fixture.args(extra...))
	return a.root.Execute()
}

func TestLoginKeepsHostPinsScopedToEachWorkspace(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "workspaces.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	first := newWorkspaceLoginFixture(t, "alpha", "alpha", "ws_000000000000000000000001")
	second := newWorkspaceLoginFixture(t, "beta", "beta", "ws_000000000000000000000002")
	for _, fixture := range []workspaceLoginFixture{first, second} {
		identity := controlplane.WorkspaceIdentity{Configured: true, ID: fixture.id, Name: fixture.workspace}
		if err := executeWorkspaceLogin(t, fixture, identity, nil, nil); err != nil {
			t.Fatalf("login %s: %v", fixture.name, err)
		}
	}
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	firstSaved, firstOK := store.AccessByName("alpha")
	if !firstOK {
		t.Fatal("alpha access missing")
	}
	if err != nil {
		t.Fatal(err)
	}
	secondSaved, secondOK := store.AccessByName("beta")
	if !secondOK {
		t.Fatal("beta access missing")
	}
	if err != nil {
		t.Fatal(err)
	}
	firstPath := firstSaved.SSH.KnownHosts
	secondPath := secondSaved.SSH.KnownHosts
	if firstSaved.SSH.KnownHosts != firstPath || secondSaved.SSH.KnownHosts != secondPath {
		t.Fatalf("saved known_hosts paths are not workspace-scoped: %q, %q", firstSaved.SSH.KnownHosts, secondSaved.SSH.KnownHosts)
	}
	if firstPath == secondPath {
		t.Fatal("both workspaces share one known_hosts file")
	}
	address := net.JoinHostPort("10.79.0.2", "22")
	if !pinnedKeyMatches(firstPath, address, first.hostKey) || pinnedKeyMatches(firstPath, address, second.hostKey) {
		t.Fatal("alpha's known_hosts does not pin only alpha's host key")
	}
	if !pinnedKeyMatches(secondPath, address, second.hostKey) || pinnedKeyMatches(secondPath, address, first.hostKey) {
		t.Fatal("beta's known_hosts does not pin only beta's host key")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "known_hosts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("login wrote the shared known_hosts file: %v", err)
	}
}
