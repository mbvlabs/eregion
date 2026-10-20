package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/secrets"
	"golang.org/x/crypto/ssh"
)

// TestSSHTransportDoesNotRedactTheIdentityPath proves that the SSH identity
// file path is not treated as a secret. The path is not a credential, and
// redacting it hides the file an operator has to look at when a handshake
// fails.
func TestSSHTransportDoesNotRedactTheIdentityPath(t *testing.T) {
	directory := t.TempDir()
	identity := filepath.Join(directory, "id_ed25519")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "eregion-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(directory, "known_hosts")
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if err := os.WriteFile(knownHosts, []byte("[127.0.0.1]:2222 "+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_NODE_SSH_ADDRESS", "127.0.0.1:2222")
	t.Setenv("EREGION_NODE_SSH_USER", "eregion")
	t.Setenv("EREGION_NODE_SSH_IDENTITY", identity)
	t.Setenv("EREGION_NODE_SSH_KNOWN_HOSTS", knownHosts)
	registry := &secrets.Registry{}
	a := &app{secrets: registry}
	if _, err := a.sshTransport(); err != nil {
		t.Fatal(err)
	}
	if got := registry.Redact(identity); got != identity {
		t.Fatalf("the identity file path was redacted: %q", got)
	}
}
