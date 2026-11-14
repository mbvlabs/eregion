package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
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

func TestIntegrationDeletionPreviewResourceAdmissionAndStateChange(t *testing.T) {
	ctx := context.Background()
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "resource-preview-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "resource-preview-app", map[string]any{
		"name": "previewresources", "environments": []map[string]any{
			{"name": "production", "node": "local"}, {"name": "staging", "node": "local"},
		},
	})
	for _, resource := range []struct{ env, name string }{
		{"production", "changing"}, {"production", "degraded"}, {"production", "retained"}, {"staging", "other"},
	} {
		platformSubmit(t, s, platform.KindResourceApply, "resource/previewresources/"+resource.env+"/"+resource.name,
			"resource-preview-"+resource.name, map[string]any{
				"image": "postgres:17", "health": map[string]any{"type": "exec", "command": []string{"pg_isready"}},
			})
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state=CASE name
		WHEN 'changing' THEN 'provisioning' WHEN 'degraded' THEN 'degraded'
		WHEN 'retained' THEN 'destroyed' ELSE 'ready' END,
		volumes=CASE WHEN name='retained' THEN '[{"name":"retained-data"}]'::jsonb ELSE volumes END
		WHERE app_id=(SELECT id FROM eregion_platform_apps WHERE name='previewresources')`); err != nil {
		t.Fatal(err)
	}
	factorCalls := 0
	s.humanFactor = func(string) error { factorCalls++; return nil }
	var beforeOps, afterOps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&beforeOps); err != nil {
		t.Fatal(err)
	}
	env, err := previewSubmission(t, s, platform.KindEnvironmentDelete, "env/previewresources/production", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"production/changing (state: provisioning)", "production/degraded (state: degraded)", "Default destroy retains named data volumes", "separate explicit choice"} {
		if !deletionWarningContains(env.Warnings, fragment) {
			t.Fatalf("missing %q in %v", fragment, env.Warnings)
		}
	}
	if deletionWarningContains(env.Warnings, "production/retained") || deletionWarningContains(env.Warnings, "staging/other") {
		t.Fatalf("wrong preview scope: %v", env.Warnings)
	}
	app, err := previewSubmission(t, s, platform.KindAppDelete, "app/previewresources", map[string]any{})
	if err != nil || !deletionWarningContains(app.Warnings, "staging/other (state: ready)") {
		t.Fatalf("application preview: %+v, %v", app, err)
	}
	encoded, err := json.Marshal(env)
	if err != nil || !strings.Contains(string(encoded), "production/changing (state: provisioning)") {
		t.Fatalf("JSON preview: %s, %v", encoded, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&afterOps); err != nil {
		t.Fatal(err)
	}
	if beforeOps != afterOps || factorCalls != 0 {
		t.Fatalf("preview changed operations or requested factor: %d -> %d, factor calls %d", beforeOps, afterOps, factorCalls)
	}
	var retained string
	if err := pool.QueryRow(ctx, `SELECT volumes::text FROM eregion_platform_resources WHERE name='retained'`).Scan(&retained); err != nil || !strings.Contains(retained, "retained-data") {
		t.Fatalf("preview changed retained data ledger: %s, %v", retained, err)
	}
	submission := platformSubmission(platform.KindEnvironmentDelete, "env/previewresources/production", "resource-preview-refused", map[string]any{"confirm": "previewresources/production"})
	submission.HumanFactor = "test"
	if _, err := s.Submit(ctx, submission); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("live resource admission: %v", err)
	}
	appSubmission := platformSubmission(platform.KindAppDelete, "app/previewresources", "resource-preview-app-refused", map[string]any{"confirm": "previewresources"})
	appSubmission.HumanFactor = "test"
	if _, err := s.Submit(ctx, appSubmission); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("app resource admission: %v", err)
	}

	// Preview sees the committed destroyed state while a transaction holds the
	// app lock and changes it back. Admission waits for that lock and rechecks.
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state='destroyed' WHERE app_id=(SELECT id FROM eregion_platform_apps WHERE name='previewresources')`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT id FROM eregion_platform_apps WHERE name='previewresources' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_resources SET state='degraded' WHERE name='changing'`); err != nil {
		t.Fatal(err)
	}
	preview, err := previewSubmission(t, s, platform.KindEnvironmentDelete, "env/previewresources/production", map[string]any{})
	if err != nil || deletionWarningContains(preview.Warnings, "Deletion blocked by resource") {
		t.Fatalf("committed snapshot preview: %+v, %v", preview, err)
	}
	result := make(chan error, 1)
	submission = platformSubmission(platform.KindEnvironmentDelete, "env/previewresources/production", "resource-preview-race", map[string]any{"confirm": "previewresources/production"})
	submission.HumanFactor = "test"
	go func() { _, err := s.Submit(ctx, submission); result <- err }()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	waitPointerLockWaiter(t, waitCtx, pool, "FROM eregion_platform_apps WHERE name=$1 FOR UPDATE")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("admission did not recheck changed resource: %v", err)
	}
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
