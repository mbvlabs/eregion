package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/transport"
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
	if _, _, err := a.sshTransport(slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if got := registry.Redact(identity); got != identity {
		t.Fatalf("the identity file path was redacted: %q", got)
	}
}

// wireGuardEnvironment writes a strict-mode WireGuard key and sets the SSH
// and WireGuard environment of a worker. It returns the key's base64 form.
func wireGuardEnvironment(t *testing.T) (string, string) {
	t.Helper()
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
	if err := os.WriteFile(knownHosts, []byte("10.77.0.2 "+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	private := make([]byte, 32)
	if _, err := rand.Read(private); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(private)
	keyFile := filepath.Join(directory, "wg.key")
	if err := os.WriteFile(keyFile, []byte(encoded+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	peer, err := transport.GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_NODE_TRANSPORT", "ssh")
	t.Setenv("EREGION_NODE_SSH_ADDRESS", "10.77.0.2:22")
	t.Setenv("EREGION_NODE_SSH_USER", "eregion")
	t.Setenv("EREGION_NODE_SSH_IDENTITY", identity)
	t.Setenv("EREGION_NODE_SSH_KNOWN_HOSTS", knownHosts)
	t.Setenv("EREGION_NODE_WG_PRIVATE_KEY_FILE", keyFile)
	t.Setenv("EREGION_NODE_WG_ADDRESS", "10.77.0.1")
	t.Setenv("EREGION_NODE_WG_PEER_PUBLIC_KEY", peer.PublicKey().String())
	t.Setenv("EREGION_NODE_WG_ENDPOINT", "node.example:51820")
	t.Setenv("EREGION_NODE_WG_ALLOWED_IPS", "10.77.0.0/24")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	return encoded, keyFile
}

// TestWireGuardTransportRegistersTheKeyForRedaction proves that the worker's
// WireGuard key is registered before anything can print it: a log line that
// carries it is redacted, while the key file path stays readable.
func TestWireGuardTransportRegistersTheKeyForRedaction(t *testing.T) {
	encoded, keyFile := wireGuardEnvironment(t)
	registry := &secrets.Registry{}
	a := &app{secrets: registry}
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(safeLogWriter{out: &logged, registry: registry}, nil))
	client, closeTransport, err := a.nodeTransport(logger)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	if client == nil || client.Name() != "ssh" {
		t.Fatalf("the WireGuard environment did not build the SSH transport: %v", client)
	}
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	logger.Info("leak attempt", "base64", encoded, "hex", hex.EncodeToString(raw))
	if strings.Contains(logged.String(), encoded) || strings.Contains(logged.String(), hex.EncodeToString(raw)) {
		t.Fatalf("the WireGuard key reached a log line: %s", logged.String())
	}
	if got := registry.Redact(keyFile); got != keyFile {
		t.Fatalf("the key file path was redacted: %q", got)
	}
}

// TestWireGuardTransportHoldsTheKeyUntilClosed proves that a second worker on
// this host cannot take the same key while the first holds it.
func TestWireGuardTransportHoldsTheKeyUntilClosed(t *testing.T) {
	wireGuardEnvironment(t)
	a := &app{secrets: &secrets.Registry{}}
	_, closeFirst, err := a.nodeTransport(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.nodeTransport(slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "WireGuard tunnel is not usable") {
		t.Fatalf("a second transport took the same WireGuard key: %v", err)
	}
	closeFirst()
	_, closeSecond, err := a.nodeTransport(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("the key was not released: %v", err)
	}
	closeSecond()
}

func TestWireGuardTransportRefusesAnUnsafeConfiguration(t *testing.T) {
	cases := map[string]func(t *testing.T, keyFile string){
		"group-readable key": func(t *testing.T, keyFile string) {
			if err := os.Chmod(keyFile, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"missing endpoint":     func(t *testing.T, _ string) { t.Setenv("EREGION_NODE_WG_ENDPOINT", "") },
		"host name as address": func(t *testing.T, _ string) { t.Setenv("EREGION_NODE_SSH_ADDRESS", "node.example:22") },
		"default route":        func(t *testing.T, _ string) { t.Setenv("EREGION_NODE_WG_ALLOWED_IPS", "0.0.0.0/0") },
		"bad keepalive":        func(t *testing.T, _ string) { t.Setenv("EREGION_NODE_WG_KEEPALIVE", "0") },
		"bad peer key":         func(t *testing.T, _ string) { t.Setenv("EREGION_NODE_WG_PEER_PUBLIC_KEY", "nope") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			encoded, keyFile := wireGuardEnvironment(t)
			mutate(t, keyFile)
			a := &app{secrets: &secrets.Registry{}}
			_, _, err := a.nodeTransport(slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("an unsafe WireGuard configuration was accepted")
			}
			if strings.Contains(err.Error(), encoded) {
				t.Fatalf("the error carries the key: %v", err)
			}
		})
	}
}
