package appdef

import (
	"strings"
	"testing"
)

func TestResolveSelectionAutomationAndTarget(t *testing.T) {
	raw := []byte(`version = 1
app = "api"
[automation]
auto_deploy = true
webhook_secret_name = "WEBHOOK_SECRET"
[environments.staging.automation]
auto_deploy = false
`)
	production, err := ResolveSelection(raw, "api", "production")
	if err != nil {
		t.Fatal(err)
	}
	if production.Automation == nil || production.Automation.AutoDeploy == nil || !*production.Automation.AutoDeploy || production.Automation.WebhookSecretName == nil || *production.Automation.WebhookSecretName != "WEBHOOK_SECRET" {
		t.Fatalf("production automation = %+v", production.Automation)
	}
	staging, err := ResolveSelection(raw, "api", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if staging.Automation == nil || staging.Automation.AutoDeploy == nil || *staging.Automation.AutoDeploy || staging.Automation.WebhookSecretName == nil || *staging.Automation.WebhookSecretName != "WEBHOOK_SECRET" {
		t.Fatalf("staging automation = %+v", staging.Automation)
	}
	for name, input := range map[string]struct{ app, environment string }{
		"different app":       {"other", "production"},
		"invalid app":         {"BAD", "production"},
		"missing environment": {"api", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveSelection(raw, input.app, input.environment); err == nil {
				t.Fatal("accepted invalid selection")
			}
		})
	}
	selected, err := ResolveSelection([]byte("version = 1\n"), "api", "production")
	if err != nil || selected.App != "api" || selected.Automation != nil {
		t.Fatalf("explicit target: resolved=%+v err=%v", selected, err)
	}
	if _, err := ResolveSelection([]byte(strings.Repeat(" ", MaxDefinitionBytes+1)), "api", "production"); err == nil {
		t.Fatal("accepted oversized definition")
	}
}

func TestResolveSelectionDefinitionSizeBoundary(t *testing.T) {
	const prefix = "version = 1\n#"
	raw := prefix + strings.Repeat("x", MaxDefinitionBytes-len(prefix))
	if _, err := ResolveSelection([]byte(raw), "api", "production"); err != nil {
		t.Fatalf("definition at limit: %v", err)
	}
	if _, err := ResolveSelection([]byte(raw+"x"), "api", "production"); err == nil || !strings.Contains(err.Error(), "128 KiB") {
		t.Fatalf("oversized definition: %v", err)
	}
}
