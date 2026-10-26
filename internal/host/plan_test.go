package host

import (
	"os"
	"strings"
	"testing"
)

func TestPlanReportsStepVersionsAndPendingChanges(t *testing.T) {
	if got := StepVersion("host-packages-v12"); got != "v12" {
		t.Fatalf("version: %q", got)
	}
	if got := StepVersion("host-platform"); got != "" {
		t.Fatalf("unversioned step reported %q", got)
	}
	plan := Plan{Steps: []PlanEntry{
		{ID: "host-packages-v2", Version: "v2", Complete: false},
		{ID: "host-users-v1", Version: "v1", Complete: true},
		{ID: "host-node-config-v1", Version: "v1", Complete: false, Error: "cannot read the node configuration"},
	}}
	pending := plan.Pending()
	if len(pending) != 2 {
		t.Fatalf("pending: %+v", pending)
	}
	if pending[0].ID != "host-packages-v2" || pending[0].Version != "v2" {
		t.Fatalf("a new step version is not reported as a change: %+v", pending[0])
	}
	if pending[1].Error == "" {
		t.Fatalf("an errored step is not reported: %+v", pending[1])
	}
	if plan.Pending()[0].ID == "host-users-v1" {
		t.Fatal("a complete step is reported as pending")
	}
}

func TestRecordedIntentCarriesNoDatabaseURL(t *testing.T) {
	cfg := fixture(t, RoleMain)
	if err := saveIntent(cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(intentPath(cfg.Layout))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cfg.DatabaseURL) || strings.Contains(string(raw), "private-password") {
		t.Fatal("the recorded intent contains the control database URL")
	}
	loaded, err := LoadIntent(cfg.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Node != cfg.Node || loaded.Role != cfg.Role || loaded.DatabaseURL != "" {
		t.Fatalf("loaded intent: %+v", loaded)
	}
	loaded.DatabaseURL = cfg.DatabaseURL
	if err := loaded.Validate(); err != nil {
		t.Fatalf("the recorded intent does not validate with the node's own database URL: %v", err)
	}
}
