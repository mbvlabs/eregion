package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAllowedPush(t *testing.T) {
	entry, err := parseAllowedPush(" Acme/Shop@main=shop/production ")
	if err != nil || entry.Repository != "Acme/Shop" || entry.Branch != "main" || entry.App != "shop" || entry.Environment != "production" {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	for _, bad := range []string{"", "acme/shop", "acme/shop@main", "acme/shop@main=shop", "acme/shop@=shop/production", "@main=shop/production", "acme/shop@main=/production"} {
		if _, err := parseAllowedPush(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestReadWebhookSecretRequiresPrivateFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "secret")
	if err := os.WriteFile(path, []byte("0123456789abcdef-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := readWebhookSecret(path)
	if err != nil || string(secret) != "0123456789abcdef-secret" {
		t.Fatalf("secret=%q err=%v", secret, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWebhookSecret(path); err == nil {
		t.Fatal("group/world-readable secret file accepted")
	}
	if _, err := readWebhookSecret(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing secret file accepted")
	}
}
