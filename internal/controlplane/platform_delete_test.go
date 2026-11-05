package controlplane

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

// The environment count in deletionCounts is scoped to the target. A named
// environment delete on an app with a sibling must not reuse that count for the
// "last environment" warning: the app-wide count decides it.
func TestDeletionWarningsUseApplicationWideEnvironmentCount(t *testing.T) {
	const lastEnvironment = "This is the app's last environment"
	const everyEnvironment = "Every environment of the application is deleted"

	for _, tc := range []struct {
		name        string
		counts      deletionCounts
		environment string
		wantLast    bool
		wantEvery   bool
	}{
		{
			name:        "environment delete with one sibling",
			counts:      deletionCounts{environments: 1, appEnvironments: 2},
			environment: "staging",
		},
		{
			name:        "environment delete with several siblings",
			counts:      deletionCounts{environments: 1, appEnvironments: 3},
			environment: "staging",
		},
		{
			name:        "environment delete of the last environment",
			counts:      deletionCounts{environments: 1, appEnvironments: 1},
			environment: "production",
			wantLast:    true,
		},
		{
			name:      "application delete",
			counts:    deletionCounts{environments: 2, appEnvironments: 2},
			wantEvery: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := deletionWarnings(tc.counts, tc.environment)
			if got := deletionWarningContains(warnings, lastEnvironment); got != tc.wantLast {
				t.Fatalf("last-environment warning = %t, want %t; warnings=%v", got, tc.wantLast, warnings)
			}
			if got := deletionWarningContains(warnings, everyEnvironment); got != tc.wantEvery {
				t.Fatalf("every-environment warning = %t, want %t; warnings=%v", got, tc.wantEvery, warnings)
			}
		})
	}
}

// Adding the app-wide count must not widen the summary: an environment delete
// still reports only the rows the named environment owns.
func TestDeletionSummaryKeepsTargetScopedCounts(t *testing.T) {
	counts := deletionCounts{
		environments:    1,
		appEnvironments: 2,
		releases:        3,
		deployments:     4,
		builds:          5,
		domains:         6,
		secrets:         7,
	}
	const want = "Delete env/shop/staging: 1 environment(s), 3 release(s), 4 deployment(s), 5 build(s), 6 domain(s), 7 secret namespace(s)."
	if got := deletionSummary("env/shop/staging", counts); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

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
		"environment": map[string]any{
			"name": "production", "node": "local",
			"domain": "deletepreview-production.example.com",
		},
	})
	platformSubmit(t, s, platform.KindEnvironmentCreate, "app/deletepreview", "delete-preview-staging", map[string]any{
		"name": "staging", "node": "local",
		"domain": "deletepreview-staging.example.com",
	})
	for _, environment := range []string{"production", "staging"} {
		platformSubmit(t, s, platform.KindReleaseCreate, "env/deletepreview/"+environment, "delete-preview-release-"+environment, map[string]any{
			"source": map[string]any{"type": "image", "image": "ghcr.io/acme/deletepreview:" + environment},
		})
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
		"environment": map[string]any{
			"name": "production", "node": "local",
		},
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
