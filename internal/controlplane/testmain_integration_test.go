package controlplane

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// secureTestTempRoot creates a private temporary root and resolves every
// symlink in its path. Tests that need a canonical private temporary directory
// use it; macOS exposes /tmp and /var through /private.
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

// TestMain keeps temporary credential and state directories under a private
// canonical root instead of an ambient group-writable GOTMPDIR.
func TestMain(m *testing.M) {
	dir, err := secureTestTempRoot("/tmp", "eregion-controlplane-tests-")
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
