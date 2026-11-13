package controlplane

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func deletionWarningContains(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}

// TestIntegrationDeletionPreviewScopesTargetCountsAndWarnsOnlyForTheLastEnvironment
// exercises the real counts query: the preview of a named environment counts
// only that environment's resources, while the last-environment warning uses
// the application's total environment count. An application delete still
// reports every environment.
func TestIntegrationDeletionPreviewScopesTargetCountsAndWarnsOnlyForTheLastEnvironment(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service

	// A declared application node keeps the test independent of whether the
	// host running the integration suite has Docker or pack installed.
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "delete-preview-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "delete-preview-create", map[string]any{
		"name": "deletepreview",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"domain": "deletepreview-production.example.com",
		}},
	})
	platformSubmit(t, s, platform.KindEnvironmentCreate, "app/deletepreview", "delete-preview-staging", map[string]any{
		"name": "staging", "node": "local",
		"domain": "deletepreview-staging.example.com",
	})
	for _, environment := range []string{"production", "staging"} {
		seedArtifactRelease(t, s, "deletepreview", environment, "ghcr.io/acme/deletepreview:"+environment)
	}

	staging, err := previewSubmission(t, s, platform.KindEnvironmentDelete, "env/deletepreview/staging", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	const stagingSummary = "Delete env/deletepreview/staging: 1 environment(s), 1 release(s), 0 deployment(s), 0 build(s), 1 domain(s), 0 secret namespace(s)."
	if staging.Summary != stagingSummary {
		t.Fatalf("environment preview summary = %q, want %q", staging.Summary, stagingSummary)
	}
	if deletionWarningContains(staging.Warnings, "This is the app's last environment") {
		t.Fatalf("environment preview claimed to delete the app although a sibling remains: %v", staging.Warnings)
	}

	application, err := previewSubmission(t, s, platform.KindAppDelete, "app/deletepreview", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	const applicationSummary = "Delete app/deletepreview: 2 environment(s), 2 release(s), 0 deployment(s), 0 build(s), 2 domain(s), 0 secret namespace(s)."
	if application.Summary != applicationSummary {
		t.Fatalf("application preview summary = %q, want %q", application.Summary, applicationSummary)
	}
	if !deletionWarningContains(application.Warnings, "Every environment of the application is deleted") {
		t.Fatalf("application preview did not name every environment: %v", application.Warnings)
	}

	platformSubmit(t, s, platform.KindAppCreate, "platform", "delete-preview-single-create", map[string]any{
		"name": "deletepreviewsingle",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
		}},
	})
	single, err := previewSubmission(t, s, platform.KindEnvironmentDelete, "env/deletepreviewsingle/production", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	const singleSummary = "Delete env/deletepreviewsingle/production: 1 environment(s), 0 release(s), 0 deployment(s), 0 build(s), 0 domain(s), 0 secret namespace(s)."
	if single.Summary != singleSummary {
		t.Fatalf("single-environment preview summary = %q, want %q", single.Summary, singleSummary)
	}
	if !deletionWarningContains(single.Warnings, "This is the app's last environment") {
		t.Fatalf("last-environment preview did not warn that the app is deleted: %v", single.Warnings)
	}
}
