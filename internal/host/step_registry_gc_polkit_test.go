package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestRegistryGCPolkitRuleIsNarrowAndNamesTheService(t *testing.T) {
	cfg := fixture(t, RoleMain)
	step := &registryGCPolkitStep{cfg: cfg, dir: t.TempDir()}
	rule := string(step.rule())
	for _, want := range []string{
		`action.id != "org.freedesktop.systemd1.manage-units"`,
		`subject.user != "` + cfg.Layout.User + `"`,
		`action.lookup("unit") != "` + platform.RegistryUnit + `"`,
		`verb == "start" || verb == "stop" || verb == "restart"`,
		"return polkit.Result.YES;",
	} {
		if !strings.Contains(rule, want) {
			t.Fatalf("the polkit rule lacks %q:\n%s", want, rule)
		}
	}
	if strings.Contains(rule, "manage-unit-files") || strings.Contains(rule, "return polkit.Result.NO;") {
		t.Fatalf("the polkit rule grants more than the registry unit:\n%s", rule)
	}
}

func TestRegistryGCPolkitStepAppliesOnceAndTheSecondPlanIsANoop(t *testing.T) {
	cfg := fixture(t, RoleMain)
	dir := filepath.Join(t.TempDir(), "rules.d")
	step := &registryGCPolkitStep{cfg: cfg, dir: dir, ownership: allowOwnership}

	first, err := planEntry(context.Background(), step, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Complete {
		t.Fatal("a missing polkit rule must be pending")
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("rules directory mode = %04o, want 0750", info.Mode().Perm())
	}
	info, err = os.Stat(step.rulePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("rule mode = %04o, want 0644", info.Mode().Perm())
	}

	second, err := planEntry(context.Background(), step, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Complete || len(second.Diff) != 0 {
		t.Fatalf("a second plan must be a no-op: %+v", second)
	}
}

func TestRegistryGCPolkitStepRepairsContentAndModeDrift(t *testing.T) {
	cfg := fixture(t, RoleMain)
	dir := filepath.Join(t.TempDir(), "rules.d")
	step := &registryGCPolkitStep{cfg: cfg, dir: dir, ownership: allowOwnership}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("rules directory mode drift = %+v, %v", check, err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(step.rulePath(), 0o600); err != nil {
		t.Fatal(err)
	}
	check, err = step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("rule mode drift = %+v, %v", check, err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	check, err = step.Check(context.Background(), engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("rule after repair = %+v, %v", check, err)
	}
}

func TestRegistryGCPolkitStepRunsOnlyForTheRegistryRole(t *testing.T) {
	ids := func(role Role) string {
		cfg := Config{Role: role, Hostname: "main.example.com"}
		steps, err := (&Runner{Config: cfg}).steps(Credentials{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, step := range steps {
			out = append(out, step.ID())
		}
		return strings.Join(out, " ")
	}
	if got := ids(RoleMain); !strings.Contains(got, "host-registry-gc-polkit-v1") {
		t.Fatalf("main node steps: %s", got)
	}
	if got := ids(RoleApplication); strings.Contains(got, "host-registry-gc-polkit-v1") {
		t.Fatalf("application node steps: %s", got)
	}
}

func TestWorkerUnitDoesNotBlockTheSystemDBus(t *testing.T) {
	unit := string(workerServiceUnit(fixture(t, RoleMain)))
	for _, blocking := range []string{"RestrictAddressFamilies", "PrivateUsers=yes", "PrivateNetwork=yes"} {
		if strings.Contains(unit, blocking) {
			t.Fatalf("the worker unit contains %q, which blocks or isolates the system D-Bus call used by registry GC:\n%s", blocking, unit)
		}
	}
	if !strings.Contains(unit, "NoNewPrivileges=yes") || !strings.Contains(unit, "ProtectSystem=strict") {
		t.Fatalf("the worker unit lost its expected hardening:\n%s", unit)
	}
}
