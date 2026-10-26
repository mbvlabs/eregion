package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestRebuiltHostKeyIsDetectedAndUnpinned(t *testing.T) {
	newKey := func() ssh.PublicKey {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		key, _ := ssh.NewPublicKey(pub)
		return key
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	old, rebuilt := newKey(), newKey()
	if err := pinHostKey(path, "203.0.113.10:22", old); err != nil {
		t.Fatal(err)
	}
	if err := pinHostKey(path, "10.78.0.1:22", old); err != nil {
		t.Fatal(err)
	}
	if !pinnedKeyMatches(path, "203.0.113.10:22", old) || pinnedKeyMatches(path, "203.0.113.10:22", rebuilt) {
		t.Fatal("pinned key match is wrong")
	}
	if err := unpinHostKey(path, "203.0.113.10:22"); err != nil {
		t.Fatal(err)
	}
	if hostKeyPinned(path, "203.0.113.10:22") || !hostKeyPinned(path, "10.78.0.1:22") {
		t.Fatal("unpin removed the wrong entries")
	}
}
