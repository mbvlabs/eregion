package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/upgrade"
)

func TestSigningKeyFilePermissions(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(private.Seed())
	path := filepath.Join(t.TempDir(), "release.key")
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := signingKey("UNUSED", path)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		t.Fatalf("read 0600 key: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := signingKey("UNUSED", path); err == nil {
		t.Fatal("group/world-readable signing key was accepted")
	}
	link := filepath.Join(t.TempDir(), "release.link")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err == nil {
		if _, err := signingKey("UNUSED", link); err == nil {
			t.Fatal("symlinked signing key was accepted")
		}
	}
}

func TestSigningKeyRequiresEnvironment(t *testing.T) {
	t.Setenv("EREGION_RELEASE_SIGNING_KEY", "")
	if _, err := signingKey("EREGION_RELEASE_SIGNING_KEY", ""); err == nil {
		t.Fatal("missing signing secret was accepted")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_RELEASE_SIGNING_KEY", base64.StdEncoding.EncodeToString(private.Seed()))
	if _, err := signingKey("EREGION_RELEASE_SIGNING_KEY", ""); err != nil {
		t.Fatalf("environment signing key rejected: %v", err)
	}
}

func TestArchiveNamesMatchManifestRules(t *testing.T) {
	operator := archiveName("1.2.3", artifactSpec{target: upgrade.TargetOperator, goos: "linux", arch: "amd64"})
	if operator != "eregion_1.2.3_linux_amd64.tar.gz" {
		t.Fatalf("operator archive = %q", operator)
	}
	node := archiveName("1.2.3", artifactSpec{target: upgrade.TargetNode, goos: "linux", arch: "arm64"})
	// The node target ships the same eregion archive.
	if node != "eregion_1.2.3_linux_arm64.tar.gz" {
		t.Fatalf("node archive = %q", node)
	}
}
