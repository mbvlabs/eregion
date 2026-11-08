package controlplane

import (
	"os"
	"testing"
)

// TestMain keeps temporary credential and state directories under the sticky
// /tmp root instead of an ambient group-writable GOTMPDIR.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "eregion-controlplane-tests-")
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
