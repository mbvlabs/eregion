package host

import (
	"strings"
	"testing"
)

func TestParsePublicHostKeySummary(t *testing.T) {
	keyType, fingerprint, err := parsePublicHostKeySummary("ssh-ed25519 SHA256:" + strings.Repeat("A", 43))
	if err != nil {
		t.Fatal(err)
	}
	if keyType != "ssh-ed25519" || fingerprint != "SHA256:"+strings.Repeat("A", 43) {
		t.Fatalf("summary = %q %q", keyType, fingerprint)
	}
	for _, value := range []string{"", "ssh-ed25519", "ssh-ed25519 ", " SHA256:abc"} {
		if _, _, err := parsePublicHostKeySummary(value); err == nil {
			t.Fatalf("invalid summary %q was accepted", value)
		}
	}
}
