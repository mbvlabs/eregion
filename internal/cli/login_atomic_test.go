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
	"github.com/mbvlabs/eregion/internal/contract"
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
	a.loginSeams.probe = func(context.Context, *host.ContextSSH, string) (ssh.PublicKey, error) {
		return key, nil
	}
	a.loginSeams.ready = func(context.Context, *host.ContextSSH) error { return readyErr }
	a.root.SetArgs(fixture.args(extra...))
	return a.root.Execute()
}

func TestLoginKeepsHostPinsScopedToEachContext(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	first := newLoginFixtureAt(t, "alpha", "10.78.0.1")
	second := newLoginFixtureAt(t, "beta", "10.78.0.1")
	for _, fixture := range []loginFixture{first, second} {
		if err := executeLogin(t, fixture, fixture.hostKey, nil); err != nil {
			t.Fatalf("login %s: %v", fixture.name, err)
		}
	}
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	firstSaved, err := store.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	secondSaved, err := store.Get("beta")
	if err != nil {
		t.Fatal(err)
	}
	firstPath, err := contextKnownHostsPath("alpha")
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := contextKnownHostsPath("beta")
	if err != nil {
		t.Fatal(err)
	}
	if firstSaved.SSH.KnownHosts != firstPath || secondSaved.SSH.KnownHosts != secondPath {
		t.Fatalf("saved known_hosts paths are not context-scoped: %q, %q", firstSaved.SSH.KnownHosts, secondSaved.SSH.KnownHosts)
	}
	if firstPath == secondPath {
		t.Fatal("both contexts share one known_hosts file")
	}
	address := net.JoinHostPort(first.wireGuardAddress, "22")
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

func TestFailedReplacementLeavesBothContextsAndPinsUntouched(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	first := newLoginFixtureAt(t, "alpha", "10.78.0.1")
	second := newLoginFixtureAt(t, "beta", "10.78.0.1")
	for _, fixture := range []loginFixture{first, second} {
		if err := executeLogin(t, fixture, fixture.hostKey, nil); err != nil {
			t.Fatalf("login %s: %v", fixture.name, err)
		}
	}
	before := filesUnder(t, filepath.Dir(configPath))

	// A bundle whose recorded key does not match the node is refused before
	// the replacement can touch either context.
	err := executeLogin(t, second, first.hostKey, nil, "--replace", "beta")
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("mismatched replacement = %v, want precondition_failed", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	// A replacement that proves the key but fails readiness is also a no-op.
	err = executeLogin(t, second, second.hostKey, contract.NewFault(contract.Unavailable, "the node does not answer", "Start the server and retry."), "--replace", "beta")
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("failed replacement = %v, want unavailable", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	secondPath, err := contextKnownHostsPath("beta")
	if err != nil {
		t.Fatal(err)
	}
	if !pinnedKeyMatches(secondPath, net.JoinHostPort(second.wireGuardAddress, "22"), second.hostKey) {
		t.Fatal("the failed replacement changed the existing pin")
	}
}

func TestLoginLeavesTheExistingContextUntouchedOnFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	fixture := newLoginFixture(t, "main")
	dir, err := identityDir(fixture.name)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts, err := contextKnownHostsPath(fixture.name)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	probes, readies := 0, 0
	a.loginSeams.probe = func(context.Context, *host.ContextSSH, string) (ssh.PublicKey, error) {
		probes++
		return fixture.hostKey, nil
	}
	a.loginSeams.ready = func(context.Context, *host.ContextSSH) error {
		readies++
		return nil
	}
	a.root.SetArgs(fixture.args())
	if err := a.root.Execute(); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if probes != 1 || readies != 1 {
		t.Fatalf("first login probe=%d ready=%d, want 1/1", probes, readies)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "ssh_ed25519")); err != nil || strings.TrimSpace(string(raw)) != fixture.sshKey {
		t.Fatalf("imported SSH key = %q, %v", raw, err)
	}
	if !hostKeyPinned(knownHosts, "10.79.0.2:22") {
		t.Fatal("the first login did not pin the host key")
	}
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get("main")
	if err != nil {
		t.Fatalf("saved context: %v", err)
	}
	if saved.SSH == nil || saved.SSH.Identity != filepath.Join(dir, "ssh_ed25519") ||
		saved.SSH.KnownHosts != knownHosts || saved.SSH.WireGuard == nil ||
		saved.SSH.WireGuard.PrivateKeyFile != filepath.Join(dir, "wireguard.key") {
		t.Fatalf("saved route points outside the swapped directory: %+v", saved.SSH)
	}
	before := filesUnder(t, filepath.Dir(configPath))

	// An unreachable node on a replacement leaves every byte in place.
	a.loginSeams.ready = func(context.Context, *host.ContextSSH) error {
		readies++
		return contract.NewFault(contract.Unavailable, "the node does not answer", "Start the server and retry.")
	}
	a.root.SetArgs(fixture.args("--replace", fixture.name))
	err = a.root.Execute()
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unreachable replacement = %v, want unavailable", err)
	}
	if readies != 2 {
		t.Fatalf("ready calls = %d, want 2", readies)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	// Without --replace the same name is a conflict, before probe or ready.
	a.root.SetArgs(fixture.args("--replace", ""))
	err = a.root.Execute()
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("existing context = %v, want conflict", err)
	}
	if probes != 2 || readies != 2 {
		t.Fatalf("conflict ran probe=%d ready=%d, want 2/2", probes, readies)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	// --replace must name the bundle's context exactly.
	a.root.SetArgs(fixture.args("--replace", "other"))
	err = a.root.Execute()
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("--replace other = %v, want conflict", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	servers, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range servers {
		if strings.HasPrefix(entry.Name(), ".eregion-login-") {
			t.Fatalf("failed login left temp directory %s", entry.Name())
		}
	}
}

func TestLoginReplacesTheContextOnlyAfterReady(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	fixture := newLoginFixture(t, "main")
	dir, err := identityDir(fixture.name)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	readyErr := error(nil)
	a.loginSeams.probe = func(context.Context, *host.ContextSSH, string) (ssh.PublicKey, error) {
		return fixture.hostKey, nil
	}
	a.loginSeams.ready = func(context.Context, *host.ContextSSH) error { return readyErr }
	a.root.SetArgs(fixture.args())
	if err := a.root.Execute(); err != nil {
		t.Fatalf("first login: %v", err)
	}
	// Mark the old identity so a replacement is visible.
	if err := os.WriteFile(filepath.Join(dir, "ssh_ed25519"), []byte("old-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := filesUnder(t, filepath.Dir(configPath))

	readyErr = contract.NewFault(contract.Unavailable, "the node does not answer", "Start the server and retry.")
	a.root.SetArgs(fixture.args("--replace", "main"))
	if err := a.root.Execute(); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("failed replacement = %v, want unavailable", err)
	}
	assertFilesEqual(t, before, filesUnder(t, filepath.Dir(configPath)))

	readyErr = nil
	a.root.SetArgs(fixture.args("--replace", "main"))
	if err := a.root.Execute(); err != nil {
		t.Fatalf("replacement login: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ssh_ed25519"))
	if err != nil || strings.TrimSpace(string(raw)) != fixture.sshKey {
		t.Fatalf("replaced SSH key = %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the replaced identity kept a file from the old directory")
	}
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(dir)+".replace-") {
			t.Fatalf("replacement left backup directory %s", entry.Name())
		}
	}
}
