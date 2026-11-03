package host

import (
	"context"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

type registryStepExec struct {
	check    string
	checkErr error
	scripts  []string
}

func (e *registryStepExec) Output(context.Context, string) (string, error) {
	return e.check, e.checkErr
}

func (e *registryStepExec) Run(_ context.Context, _ string, script string, _ map[string]string, _ engine.Reporter) error {
	e.scripts = append(e.scripts, script)
	return nil
}

// v1RegistryConfig is the previous step's registry configuration: deletes
// disabled. An upgraded node still has it on disk until host-registry-v2 runs.
const v1RegistryConfig = `version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: /var/lib/registry
  delete:
    enabled: false
http:
  addr: :5000
  headers:
    X-Content-Type-Options: [nosniff]
`

func TestRegistryV2EnablesManifestDeletesAndPinsTheSameImage(t *testing.T) {
	step := &registryStep{cfg: DefaultConfig(RoleMain, "")}
	if step.ID() != "host-registry-v2" {
		t.Fatalf("enabling deletes changed the registry contract; use a new step version, got %q", step.ID())
	}
	config := string(step.config())
	if !strings.Contains(config, "delete:\n    enabled: true") {
		t.Fatalf("registry config does not enable deletes:\n%s", config)
	}
	unit := string(step.unit())
	if !strings.Contains(unit, "REGISTRY_STORAGE_DELETE_ENABLED=true") {
		t.Fatalf("registry unit does not set REGISTRY_STORAGE_DELETE_ENABLED:\n%s", unit)
	}
	if !strings.Contains(unit, platform.RegistryImage) {
		t.Fatalf("registry unit does not run the pinned image:\n%s", unit)
	}
}

func TestRegistryCheckRequiresTheLiveDeleteFlag(t *testing.T) {
	cfg := fixture(t, RoleMain)
	exec := &registryStepExec{check: "enabled:ok\nactive:ok\nimage:ok\ndeletion:missing"}
	step := &registryStep{cfg: cfg, exec: exec}
	put(t, step.configPath(), string(step.config()), 0o644)
	put(t, step.unitPath(), string(step.unit()), 0o644)

	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete || !strings.Contains(check.Detail, "deletion") {
		t.Fatalf("a running registry without deletes enabled must stay pending: %+v", check)
	}
}

func TestRegistryV1ToV2AppliesOnceAndTheSecondPlanIsANoop(t *testing.T) {
	cfg := fixture(t, RoleMain)
	exec := &registryStepExec{check: "enabled:ok\nactive:ok\nimage:ok\ndeletion:ok"}
	step := &registryStep{cfg: cfg, exec: exec}

	// Simulate a node that already ran v1: same paths, old content.
	put(t, step.configPath(), v1RegistryConfig, 0o644)
	v1Unit := strings.ReplaceAll(string(step.unit()), " --env REGISTRY_STORAGE_DELETE_ENABLED=true", "")
	put(t, step.unitPath(), v1Unit, 0o644)

	first, err := planEntry(context.Background(), step, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Complete {
		t.Fatal("a v1 node must report host-registry-v2 pending")
	}
	if !diffMentions(first.Diff, "enabled: true") {
		t.Fatalf("the upgrade diff does not show the delete switch: %+v", first.Diff)
	}

	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(exec.scripts) != 1 || !strings.Contains(exec.scripts[0], "systemctl restart eregion-registry.service") {
		t.Fatalf("the new configuration must restart the registry: %v", exec.scripts)
	}
	if strings.Contains(exec.scripts[0], "enable --now") {
		t.Fatal("enable --now does not restart an already-running registry")
	}

	second, err := planEntry(context.Background(), step, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Complete || len(second.Diff) != 0 {
		t.Fatalf("a second plan must be a no-op: %+v", second)
	}
}

func diffMentions(diff []string, want string) bool {
	for _, line := range diff {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
