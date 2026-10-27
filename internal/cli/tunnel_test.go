//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTunnelSocketPathIsPrivateAndStable(t *testing.T) {
	dir, err := os.MkdirTemp("", "eregion-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(dir, "contexts.json"))
	got, err := tunnelSocketPath("fingerprint-one")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, filepath.Join(dir, "tunnels")) || !strings.HasSuffix(got, ".sock") {
		t.Fatalf("socket = %s, want under %s", got, filepath.Join(dir, "tunnels"))
	}
	again, err := tunnelSocketPath("fingerprint-one")
	if err != nil || again != got {
		t.Fatalf("socket is not stable: %s, %v", again, err)
	}
	other, err := tunnelSocketPath("fingerprint-two")
	if err != nil || other == got {
		t.Fatalf("two tunnels share a socket: %s, %v", other, err)
	}
	info, err := os.Stat(filepath.Dir(got))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("tunnel directory mode: %v, %v", info, err)
	}
}

func TestPrivateDirRefusesLooseMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "eregion")
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // G301: the test needs a loose mode.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: the test needs a loose mode.
		t.Fatal(err)
	}
	if err := privateDir(dir); err == nil {
		t.Fatal("a 0755 directory was accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(link); err == nil {
		t.Fatal("a symlink was accepted")
	}
}
