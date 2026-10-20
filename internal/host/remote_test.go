package host

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mbvlabs/eregion/internal/transport"
)

func remoteIdentity(t *testing.T, dir string, mode os.FileMode) (string, ssh.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return path, signer
}

func TestRemoteRequiresAPinnedHostKeyAndPrivateIdentity(t *testing.T) {
	dir := t.TempDir()
	identity, public := remoteIdentity(t, dir, 0o600)
	address := "127.0.0.1:22"
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(transport.HostID(address)+" "+string(ssh.MarshalAuthorizedKey(public))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote, err := NewRemote(RemoteConfig{Address: address, User: "root", IdentityFile: identity, KnownHosts: knownHosts})
	if err != nil {
		t.Fatalf("pinned target rejected: %v", err)
	}
	if remote == nil {
		t.Fatal("no remote")
	}
	empty := filepath.Join(dir, "empty_known_hosts")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRemote(RemoteConfig{Address: address, User: "root", IdentityFile: identity, KnownHosts: empty}); err == nil {
		t.Fatal("an unpinned host was accepted")
	}
	publicIdentity, _ := remoteIdentity(t, dir, 0o644)
	if _, err := NewRemote(RemoteConfig{Address: address, User: "root", IdentityFile: publicIdentity, KnownHosts: knownHosts}); err == nil {
		t.Fatal("a group-readable identity was accepted")
	}
}
