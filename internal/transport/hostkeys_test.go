package transport

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// writeKnownHosts writes a known_hosts file for a test.
func writeKnownHosts(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// authorizedLine returns the authorized_keys form of a host key.
func authorizedLine(t *testing.T, key ssh.PublicKey) string {
	t.Helper()
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// TestHostKeysRefuseARevokedKey proves that a "@revoked" line never pins the
// key it revokes.
func TestHostKeysRefuseARevokedKey(t *testing.T) {
	server := newTestServer(t)
	host := HostID(server.address())
	line := authorizedLine(t, server.hostKey)
	path := writeKnownHosts(t, "@revoked "+host+" "+line)
	keys, err := LoadHostKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if keys.Pinned(server.address()) {
		t.Fatal("a revoked key was pinned")
	}
	callback := keys.Callback(server.address())
	err = callback("", nil, server.hostKey)
	var failure *HostKeyError
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !failure.Revoked {
		t.Fatalf("a revoked key is not reported as revoked: %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the revocation is not in the message: %v", err)
	}
}

// TestHostKeysRefuseARevokedKeyThatAnotherLinePins proves that a revocation
// wins over a pin of the same key.
func TestHostKeysRefuseARevokedKeyThatAnotherLinePins(t *testing.T) {
	server := newTestServer(t)
	host := HostID(server.address())
	line := authorizedLine(t, server.hostKey)
	path := writeKnownHosts(t, "@revoked "+host+" "+line, host+" "+line)
	keys, err := LoadHostKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	callback := keys.Callback(server.address())
	err = callback("", nil, server.hostKey)
	var failure *HostKeyError
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !failure.Revoked {
		t.Fatalf("a revoked key was accepted because another line pins it: %v", err)
	}
}

// TestHostKeysSkipCertAuthorityLines proves that a "@cert-authority" line
// never becomes a plain pin.
func TestHostKeysSkipCertAuthorityLines(t *testing.T) {
	server := newTestServer(t)
	host := HostID(server.address())
	line := authorizedLine(t, server.hostKey)
	path := writeKnownHosts(t, "@cert-authority "+host+" "+line)
	keys, err := LoadHostKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if keys.Pinned(server.address()) {
		t.Fatal("a @cert-authority line became a plain pin")
	}
	if len(keys.Keys(server.address())) != 0 {
		t.Fatal("a @cert-authority line became a plain pin")
	}
	callback := keys.Callback(server.address())
	if err := callback("", nil, server.hostKey); err == nil {
		t.Fatal("a @cert-authority line authorised a host key")
	}
}

// TestHostKeysMatchHashedEntries proves that a hashed known_hosts entry pins
// its host instead of leaving it unpinned.
func TestHostKeysMatchHashedEntries(t *testing.T) {
	server := newTestServer(t)
	host := HostID(server.address())
	hashed := knownhosts.HashHostname(host)
	line := authorizedLine(t, server.hostKey)
	path := writeKnownHosts(t, hashed+" "+line)
	keys, err := LoadHostKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if !keys.Pinned(server.address()) {
		t.Fatal("a hashed known_hosts entry did not pin the host")
	}
	callback := keys.Callback(server.address())
	if err := callback("", nil, server.hostKey); err != nil {
		t.Fatalf("the hashed pin was rejected: %v", err)
	}
	other, err := ssh.NewSignerFromKey(mustGenerateKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("", nil, other.PublicKey()); err == nil {
		t.Fatal("an unpinned key was accepted")
	}
}

// TestHostKeysRefuseARevokedHashedEntry proves that a revoked hashed entry is
// refused like a revoked plain one.
func TestHostKeysRefuseARevokedHashedEntry(t *testing.T) {
	server := newTestServer(t)
	host := HostID(server.address())
	hashed := knownhosts.HashHostname(host)
	line := authorizedLine(t, server.hostKey)
	path := writeKnownHosts(t, "@revoked "+hashed+" "+line)
	keys, err := LoadHostKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if keys.Pinned(server.address()) {
		t.Fatal("a revoked hashed entry was pinned")
	}
	callback := keys.Callback(server.address())
	err = callback("", nil, server.hostKey)
	var failure *HostKeyError
	if !errors.As(err, &failure) || !failure.Revoked {
		t.Fatalf("a revoked hashed key is not reported as revoked: %v", err)
	}
}

// TestHostKeysRejectAnUnsupportedMarker proves that an unknown marker fails
// closed instead of becoming a pin.
func TestHostKeysRejectAnUnsupportedMarker(t *testing.T) {
	server := newTestServer(t)
	host := HostID(server.address())
	line := authorizedLine(t, server.hostKey)
	path := writeKnownHosts(t, "@unknown "+host+" "+line)
	if _, err := LoadHostKeys(path); err == nil {
		t.Fatal("an unsupported known_hosts marker was accepted")
	}
}

// TestHostKeysRejectAMalformedHashedEntry proves that a hashed entry the
// loader cannot read fails closed instead of being ignored.
func TestHostKeysRejectAMalformedHashedEntry(t *testing.T) {
	server := newTestServer(t)
	line := authorizedLine(t, server.hostKey)
	path := writeKnownHosts(t, "|1|not-base64!|hash "+line)
	if _, err := LoadHostKeys(path); err == nil {
		t.Fatal("a malformed hashed known_hosts entry was accepted")
	}
}
