package integrations

import (
	"os"
	"testing"
)

// TestMain uses a private temporary root under the sticky /tmp so credential
// tests exercise descriptor-safe paths even when the ambient GOTMPDIR is
// group-writable.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "eregion-integration-tests-")
	if err != nil {
		os.Exit(m.Run())
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
