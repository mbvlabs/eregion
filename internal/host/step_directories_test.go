package host

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

// allowOwnership ignores the on-disk owner in a unit test. A non-root test
// cannot create root-owned directories, so it verifies the mode and desired
// owner/group separately.
func allowOwnership(os.FileInfo, string, string) (bool, error) { return true, nil }

func TestDirectoriesV2MakesRegistryStorageReadableByTheWorkerGroup(t *testing.T) {
	cfg := fixture(t, RoleMain)
	role, err := ConfigForRole(RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	step := &directoriesStep{cfg: cfg, role: role}
	if step.ID() != "host-directories-v2" {
		t.Fatalf("changing the registry directory access must be a new step version, got %q", step.ID())
	}
	var registry *dirSpec
	for _, spec := range step.specs() {
		spec := spec
		if spec.Path == cfg.Layout.RegistryDir {
			registry = &spec
		}
	}
	if registry == nil {
		t.Fatal("the main role does not converge the registry directory")
	}
	if registry.Mode != 0o750 || registry.Owner != "root" || registry.Group != cfg.Layout.Group {
		t.Fatalf("registry directory spec = %+v, want root:%s 0750", *registry, cfg.Layout.Group)
	}
	if registry.Mode&0o022 != 0 {
		t.Fatalf("the registry directory is group- or world-writable: %04o", registry.Mode)
	}
}

func TestDirectoriesV2UpgradesTheRegistryModeOnceAndTheSecondPlanIsANoop(t *testing.T) {
	cfg := fixture(t, RoleMain)
	role, err := ConfigForRole(RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	step := &directoriesStep{cfg: cfg, role: role, ownership: allowOwnership}

	for _, spec := range step.specs() {
		if err := ensureDir(spec.Path, spec.Mode); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a node that already ran host-directories-v1: everything is
	// present, but the registry root is root:root 0700.
	if err := os.Chmod(cfg.Layout.RegistryDir, 0o700); err != nil {
		t.Fatal(err)
	}

	first, err := planEntry(context.Background(), step, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Complete {
		t.Fatal("a v1 registry directory must report host-directories-v2 pending")
	}
	if !diffMentions(first.Diff, "0700 -> 0750") {
		t.Fatalf("the plan does not render the access change: %+v", first.Diff)
	}

	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.Layout.RegistryDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("registry directory mode = %04o, want 0750", info.Mode().Perm())
	}

	second, err := planEntry(context.Background(), step, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Complete || len(second.Diff) != 0 {
		t.Fatalf("a second plan must be a no-op: %+v", second)
	}
}

func TestDirectoriesV2KeepsTheRegistryOutOfNonRegistryRoles(t *testing.T) {
	cfg := fixture(t, RoleApplication)
	role, err := ConfigForRole(RoleApplication)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range (&directoriesStep{cfg: cfg, role: role}).specs() {
		if strings.TrimSpace(spec.Path) == strings.TrimSpace(cfg.Layout.RegistryDir) {
			t.Fatalf("the application role owns an unexpected registry directory: %+v", spec)
		}
	}
}
