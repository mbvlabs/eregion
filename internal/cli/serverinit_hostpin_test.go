package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"golang.org/x/crypto/ssh"
)

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestInitPinsWireGuardKeyInTheContextKnownHosts(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	pending, err := host.PendingKnownHosts("prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		t.Fatal(err)
	}
	publicAddress := net.JoinHostPort("203.0.113.10", "22")
	wireGuardAddress := net.JoinHostPort("10.78.0.1", "22")
	key := testHostKey(t)
	if err := pinHostKey(pending, publicAddress, key); err != nil {
		t.Fatal(err)
	}
	publicPin, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}

	scoped, err := contextKnownHostsPath("prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := pinContextKnownHosts(pending, scoped, publicAddress, wireGuardAddress); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(pending); err != nil || string(after) != string(publicPin) {
		t.Fatalf("the public first-contact pin changed: %q, %v", after, err)
	}

	if !pinnedKeyMatches(scoped, wireGuardAddress, key) {
		t.Fatal("the context known_hosts does not pin the WireGuard address")
	}
	if hostKeyPinned(scoped, publicAddress) {
		t.Fatal("the context known_hosts also pins the public first-contact address")
	}
	if !pinnedKeyMatches(pending, publicAddress, key) {
		t.Fatal("the pending known_hosts no longer verifies the public address")
	}
	if hostKeyPinned(pending, wireGuardAddress) {
		t.Fatal("the pending first-contact known_hosts also pins the WireGuard address")
	}
	info, err := os.Stat(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("context known_hosts mode = %o, want 600", info.Mode().Perm())
	}
}

func TestInitResumeRebuildsScopedKnownHostsFromVerifiedPendingPin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "eregion", "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	pending, err := host.PendingKnownHosts("prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		t.Fatal(err)
	}
	publicAddress := net.JoinHostPort("203.0.113.10", "22")
	wireGuardAddress := net.JoinHostPort(access.NodeAddress, "22")
	key := testHostKey(t)
	if err := pinHostKey(pending, publicAddress, key); err != nil {
		t.Fatal(err)
	}
	publicPin, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := contextKnownHostsPath("prod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scoped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fixture unexpectedly already has a scoped known_hosts: %v", err)
	}

	// This is the resumed state after access setup completed but the context
	// was not saved: only the pending public-address pin survives.
	r := &serverInit{
		answers:    serverInitAnswers{Context: "prod", Host: "203.0.113.10", Port: "22", AdminUser: "admin"},
		pending:    &host.PendingInit{NodePublicKey: "node-public-key", Phases: map[string]string{phaseAccess: "done"}},
		knownHosts: pending,
	}
	route, err := r.routeFromPending()
	if err != nil {
		t.Fatal(err)
	}
	if route.KnownHosts != scoped {
		t.Fatalf("resumed route known_hosts = %q, want %q", route.KnownHosts, scoped)
	}
	if route.Address != wireGuardAddress {
		t.Fatalf("resumed route address = %q", route.Address)
	}
	if !pinnedKeyMatches(scoped, wireGuardAddress, key) {
		t.Fatal("the resumed route does not trust the verified WireGuard key")
	}
	if hostKeyPinned(scoped, publicAddress) {
		t.Fatal("the rebuilt scoped known_hosts also pins the public address")
	}
	if after, err := os.ReadFile(pending); err != nil || string(after) != string(publicPin) {
		t.Fatalf("the verified public-address pin changed: %q, %v", after, err)
	}
}

func TestInitResumeWithoutVerifiedPendingPinFailsClosed(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "eregion", "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	// A pin in the old shared location is not a verified first-contact source.
	wireGuardAddress := net.JoinHostPort(access.NodeAddress, "22")
	global := filepath.Join(filepath.Dir(configPath), "known_hosts")
	if err := pinHostKey(global, wireGuardAddress, testHostKey(t)); err != nil {
		t.Fatal(err)
	}
	pending, err := host.PendingKnownHosts("prod")
	if err != nil {
		t.Fatal(err)
	}
	r := &serverInit{
		answers:    serverInitAnswers{Context: "prod", Host: "203.0.113.10", Port: "22", AdminUser: "admin"},
		pending:    &host.PendingInit{NodePublicKey: "node-public-key", Phases: map[string]string{phaseAccess: "done"}},
		knownHosts: pending,
	}
	route, err := r.routeFromPending()
	if err == nil {
		t.Fatal("resume without a verified pending pin succeeded")
	}
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("resume without a verified pending pin = %v, want precondition_failed", err)
	}
	if route != nil {
		t.Fatalf("resume without a verified pending pin returned a route: %+v", route)
	}
	scoped, err := contextKnownHostsPath("prod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scoped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume created scoped trust from an unverified source: %v", err)
	}
}
