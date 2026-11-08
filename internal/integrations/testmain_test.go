package integrations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secureTestTempRoot creates a private temporary root and resolves every
// symlink in its path. The production credential reader walks the path with
// O_NOFOLLOW and refuses symlink ancestors; macOS exposes /tmp and /var through
// /private, so GOTMPDIR must be the canonical path for t.TempDir() to exercise
// the same safe layout the production reader accepts.
func secureTestTempRoot(base, pattern string) (string, error) {
	dir, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		_ = os.RemoveAll(resolved)
		return "", err
	}
	return resolved, nil
}

// TestMain uses a private canonical temporary root instead of an ambient
// GOTMPDIR, so credential tests exercise descriptor-safe paths on macOS as
// well as Linux.
func TestMain(m *testing.M) {
	dir, err := secureTestTempRoot("/tmp", "eregion-integration-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test temporary root:", err)
		os.Exit(1)
	}
	old := os.Getenv("GOTMPDIR")
	_ = os.Setenv("GOTMPDIR", dir)
	code := m.Run()
	if old == "" {
		_ = os.Unsetenv("GOTMPDIR")
	} else {
		_ = os.Setenv("GOTMPDIR", old)
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestSecureTestTempRootCanonicalizesSymlinkedBase simulates the macOS
// /tmp -> /private/tmp layout and proves a credential below the resolved root
// is accepted by the production reader without weakening its no-follow walk.
func TestSecureTestTempRootCanonicalizesSymlinkedBase(t *testing.T) {
	got := os.Getenv("GOTMPDIR")
	if got == "" {
		t.Fatal("TestMain did not set GOTMPDIR")
	}
	if resolved, err := filepath.EvalSymlinks(got); err != nil || resolved != got {
		t.Fatalf("GOTMPDIR %q is not canonical: resolved=%q err=%v", got, resolved, err)
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode().Perm() != 0o700 {
		t.Fatalf("GOTMPDIR mode = %o, want 700", gotInfo.Mode().Perm())
	}

	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	root, err := secureTestTempRoot(link, "canonical-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		t.Fatalf("test root %q is not canonical: resolved=%q err=%v", root, resolved, err)
	}
	if !strings.HasPrefix(root, real+string(os.PathSeparator)) {
		t.Fatalf("test root %q is outside the resolved base %q", root, real)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("test root mode = %o, want 700", info.Mode().Perm())
	}
	token := filepath.Join(root, "cloudflare-token")
	if err := os.WriteFile(token, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(token, 4096); err != nil {
		t.Fatalf("credential under canonicalized test root = %v", err)
	}
	// The same file reached through the symlinked base must still be refused;
	// canonicalizing the test root must not weaken the production no-follow walk.
	linkedToken := filepath.Join(link, filepath.Base(root), "cloudflare-token")
	if _, err := ReadSecretFile(linkedToken, 4096); err == nil {
		t.Fatal("production reader accepted the symlinked path")
	}
}
