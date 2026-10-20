package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// mustGenerateKey returns a fresh host key for a test.
func mustGenerateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
