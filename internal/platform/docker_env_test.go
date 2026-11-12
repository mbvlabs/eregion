package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestIsolatedDockerEnvironmentIgnoresAmbientConfigAndHelpers(t *testing.T) {
	sentinel := t.TempDir()
	ambientConfig := filepath.Join(sentinel, "config.json")
	if err := os.WriteFile(ambientConfig, []byte(`{"credsStore":"sentinel-helper"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", sentinel)
	t.Setenv("HOME", sentinel)
	t.Setenv("XDG_CONFIG_HOME", sentinel)

	env, cleanup, err := IsolatedDockerEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for _, key := range []string{"DOCKER_CONFIG", "HOME", "XDG_CONFIG_HOME"} {
		value := values[key]
		if value == "" || value == sentinel {
			t.Fatalf("%s = %q, want a private isolated directory", key, value)
		}
		if !strings.HasPrefix(filepath.Clean(value), filepath.Clean(os.TempDir())) {
			t.Fatalf("%s = %q, want a temp path", key, value)
		}
	}
	if values["DOCKER_CONFIG"] != values["HOME"] || values["HOME"] != values["XDG_CONFIG_HOME"] {
		t.Fatalf("isolated Docker paths disagree: %+v", values)
	}
	entries, err := os.ReadDir(values["DOCKER_CONFIG"])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("isolated Docker config is not empty: %+v", entries)
	}
	if _, err := os.Stat(ambientConfig); err != nil {
		t.Fatal("the ambient config sentinel was changed")
	}
}

func TestArgvStepIsolatedDockerDoesNotInheritAmbientConfig(t *testing.T) {
	sentinel := t.TempDir()
	t.Setenv("DOCKER_CONFIG", sentinel)
	t.Setenv("HOME", sentinel)
	t.Setenv("XDG_CONFIG_HOME", sentinel)
	step := &ArgvStep{payload: ArgvPayload{IsolatedDocker: true}}
	env, cleanup, err := step.commandEnv(engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, line := range env {
		key, value, _ := strings.Cut(line, "=")
		if key != "DOCKER_CONFIG" && key != "HOME" && key != "XDG_CONFIG_HOME" {
			continue
		}
		if value == "" || value == sentinel || !strings.HasPrefix(filepath.Clean(value), filepath.Clean(os.TempDir())) {
			t.Fatalf("%s inherited ambient config: %q", key, value)
		}
	}
}

func TestArgvStepIsolatedDockerFailsClosedWhenConfigCannotBeCreated(t *testing.T) {
	// A file used as TMPDIR proves the step refuses rather than falling back to
	// the ambient HOME/DOCKER_CONFIG.
	notDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", notDir)
	if _, cleanup, err := (&ArgvStep{payload: ArgvPayload{IsolatedDocker: true}}).commandEnv(engine.Runtime{}); err == nil {
		cleanup()
		t.Fatal("an isolated Docker step ran without an isolated config")
	} else if contract.Classify(err).Code != contract.Internal && !strings.Contains(err.Error(), "isolated Docker configuration") {
		t.Fatalf("isolated config failure = %v", err)
	}
}
