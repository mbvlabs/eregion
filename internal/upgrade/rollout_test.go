package upgrade

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestRolloutPlanOrdersMigrateWorkersAPINodes(t *testing.T) {
	f := newFixture(t)
	plan, err := BuildRolloutPlan(f.manifest, f.manager.Env, DatabasePlan{Current: 3, Required: 3}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Direction != "rolling-upgrade" || plan.ReleaseVersion != "1.1.0" || plan.SchemaVersion != 3 {
		t.Fatalf("unexpected plan identity: %+v", plan)
	}
	want := []string{"migrate", "workers", "api", "nodes"}
	if len(plan.Steps) != len(want) {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	for index, stage := range want {
		step := plan.Steps[index]
		if step.Stage != stage || step.Order != index+1 {
			t.Fatalf("step %d = %+v, want stage %q", index, step, stage)
		}
		if len(step.Commands) == 0 || len(step.Verify) == 0 || step.OnFailure == "" {
			t.Fatalf("step %q is not actionable: %+v", stage, step)
		}
	}
	if len(plan.Warnings) == 0 || !strings.Contains(plan.Warnings[0], "no control-plane inventory") {
		t.Fatalf("missing unverified-inventory warning: %+v", plan.Warnings)
	}
	rendered := RenderRolloutPlan(plan)
	for _, stage := range want {
		if !strings.Contains(rendered, stage) {
			t.Fatalf("rendered plan lost stage %q:\n%s", stage, rendered)
		}
	}
	if !strings.Contains(rendered, "No command was run") {
		t.Fatalf("rendered plan implies it ran something:\n%s", rendered)
	}
}

func TestRolloutPlanRejectsIncompatibleSchemaAndInventory(t *testing.T) {
	f := newFixture(t)
	if _, err := BuildRolloutPlan(f.manifest, Environment{
		Target: TargetOperator, OS: "linux", Arch: "amd64", Version: "1.0.0",
		Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 4,
	}, DatabasePlan{Current: 4, Required: 4}, nil); err == nil {
		t.Fatal("newer database schema was accepted")
	} else if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unexpected code: %v", contract.Classify(err).Code)
	}
	_, err := BuildRolloutPlan(f.manifest, f.manager.Env, DatabasePlan{Current: 3, Required: 3}, []ControlPlaneInstance{{
		ID: "api-2", Version: "1.0.0", Contract: "eregion/v2", NodeProtocol: 1, SchemaVersion: 3,
	}})
	if err == nil {
		t.Fatal("incompatible inventory was accepted")
	}
}

func TestRolloutPlanReportsPendingMigrations(t *testing.T) {
	f := newFixture(t)
	plan, err := BuildRolloutPlan(f.manifest, f.manager.Env, DatabasePlan{Current: 2, Required: 4}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	found := false
	for _, warning := range plan.Warnings {
		if strings.Contains(warning, "pending migrations") {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending migrations were hidden: %+v", plan.Warnings)
	}
}

func TestValidateRolloutInventory(t *testing.T) {
	if err := ValidateRolloutInventory(nil); err != nil {
		t.Fatal(err)
	}
	for name, inventory := range map[string][]ControlPlaneInstance{
		"missing id":      {{Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3}},
		"duplicate id":    {{ID: "a", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3}, {ID: "a", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3}},
		"zero protocol":   {{ID: "a", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 0, SchemaVersion: 3}},
		"missing version": {{ID: "a", Version: "", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3}},
		"zero schema":     {{ID: "a", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 0}},
	} {
		if err := ValidateRolloutInventory(inventory); err == nil {
			t.Fatalf("%s inventory was accepted", name)
		}
	}
}
