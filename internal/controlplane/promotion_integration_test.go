package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func promotionServingRelease(t *testing.T, pool *pgxpool.Pool, app, environment string) string {
	t.Helper()
	var id *string
	if err := pool.QueryRow(context.Background(), `SELECT e.serving_release_id
        FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name=$1 AND e.name=$2`, app, environment).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id == nil {
		return ""
	}
	return *id
}

func promotionReleaseImageID(t *testing.T, pool *pgxpool.Pool, releaseID string) string {
	t.Helper()
	var imageID string
	if err := pool.QueryRow(context.Background(), `SELECT image_id FROM eregion_platform_releases WHERE id=$1`, releaseID).Scan(&imageID); err != nil {
		t.Fatal(err)
	}
	return imageID
}

// TestIntegrationPromotionPinsTheServingArtifactAndUsesProductionState proves
// the dedicated promotion path creates a new production release from the exact
// source image digest, runs no build, uses production's revision/config/secrets
// and release command, leaves the previous production release serving until the
// blue/green switch succeeds, and refuses a duplicate artifact.
func TestIntegrationPromotionPinsTheServingArtifactAndUsesProductionState(t *testing.T) {
	pool, _ := testPool(t, true)
	resolver := newSnapshotSecretResolver()
	resolver.set("PROD_ONLY", 1, "production-secret")
	resolver.set("STAGE_ONLY", 1, "staging-secret")
	fake := newScriptedDeployTransport()
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		LocalNodeName: "local", SecretResolver: resolver, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	ctx := context.Background()

	runOperation := func(op contract.Operation) {
		t.Helper()
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatalf("%s attempt: %v", op.Kind, err)
		}
		platformAwait(t, s, op, contract.Succeeded)
	}

	runOperation(platformSubmit(t, s, platform.KindNodeRegister, "platform", "promote-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}))
	runOperation(platformSubmit(t, s, platform.KindAppCreate, "platform", "promote-app", map[string]any{"name": "live"}))

	createEnvironment := func(key, name, domain, role, secret, releaseCommand string) {
		runOperation(platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", key, map[string]any{
			"name": name, "node": "local", "container_port": 80,
			"health": map[string]any{"timeout_seconds": 2},
			"domain": domain, "domain_primary": true,
			"config":       map[string]string{"ROLE": role},
			"secret_names": []string{secret},
			"runtime": map[string]any{
				"release_command": map[string]any{"argv": []string{"/bin/migrate", releaseCommand}, "timeout_seconds": 5},
			},
		}))
	}
	createEnvironment("promote-prod-env", "production", "prod.example.com", "production", "PROD_ONLY", "production")
	createEnvironment("promote-stage-env", "staging", "staging.example.com", "staging", "STAGE_ONLY", "staging")

	createRelease := func(_, environment, image string) platform.ReleaseID {
		return seedArtifactRelease(t, s, "live", environment, image).ID
	}
	productionRelease := createRelease("promote-prod-release", "production", "registry.example/live:v1")
	runOperation(platformSubmit(t, s, platform.KindDeploy, "env/live/production", "promote-prod-deploy", map[string]any{"release_id": productionRelease}))
	stagingRelease := createRelease("promote-stage-release", "staging", "registry.example/live:v2")
	runOperation(platformSubmit(t, s, platform.KindDeploy, "env/live/staging", "promote-stage-deploy", map[string]any{"release_id": stagingRelease}))

	if got := promotionServingRelease(t, pool, "live", "production"); got != string(productionRelease) {
		t.Fatalf("production serving = %q, want %q", got, productionRelease)
	}
	if got := promotionServingRelease(t, pool, "live", "staging"); got != string(stagingRelease) {
		t.Fatalf("staging serving = %q, want %q", got, stagingRelease)
	}
	var domainsBefore string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(d.name, ',' ORDER BY d.name),'')
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='live'`).Scan(&domainsBefore); err != nil {
		t.Fatal(err)
	}
	stagingImageID := promotionReleaseImageID(t, pool, string(stagingRelease))

	input := platform.PromoteInput{App: "live", SourceEnvironment: "staging"}
	raw, _ := json.Marshal(input)
	normalized, err := platform.Default().Normalize(platform.KindPromote, "env/live/production", raw)
	if err != nil {
		t.Fatal(err)
	}
	before := operationCount(t, pool)
	preview, err := s.Preview(ctx, testActor, normalized, "")
	if err != nil {
		t.Fatalf("promotion preview: %v", err)
	}
	if preview.Replay || preview.Data["source_release_id"] != string(stagingRelease) ||
		preview.Data["source_image_id"] != stagingImageID ||
		preview.Data["target_environment"] != "production" {
		t.Fatalf("preview = %+v", preview)
	}
	if after := operationCount(t, pool); after != before {
		t.Fatalf("promotion preview accepted an operation: %d -> %d", before, after)
	}

	promotion := platformSubmit(t, s, platform.KindPromote, "env/live/production", "promote-1", input)
	runOperation(promotion)

	var targetReleaseID, targetImage, targetImageRef, targetCommand, targetBuild string
	if err := pool.QueryRow(ctx, `SELECT id,image_id,image,coalesce(command::text,''),coalesce(build_id,'')
        FROM eregion_platform_releases WHERE operation_id=$1`, promotion.ID).
		Scan(&targetReleaseID, &targetImage, &targetImageRef, &targetCommand, &targetBuild); err != nil {
		t.Fatal(err)
	}
	if targetReleaseID == "" || targetReleaseID == string(productionRelease) || targetReleaseID == string(stagingRelease) {
		t.Fatalf("promotion release ID = %q", targetReleaseID)
	}
	var stagingImageRef string
	if err := pool.QueryRow(ctx, `SELECT image FROM eregion_platform_releases WHERE id=$1`, stagingRelease).Scan(&stagingImageRef); err != nil {
		t.Fatal(err)
	}
	if targetImage != stagingImageID {
		t.Fatalf("promotion image ID = %q, want staging image %q", targetImage, stagingImageID)
	}
	if targetImageRef != stagingImageRef {
		t.Fatalf("promotion image reference = %q, want staging reference %q", targetImageRef, stagingImageRef)
	}
	if targetCommand != "" || targetBuild != "" {
		t.Fatalf("promotion copied source command/build: command=%q build=%q", targetCommand, targetBuild)
	}
	if got := promotionServingRelease(t, pool, "live", "production"); got != targetReleaseID {
		t.Fatalf("production serving = %q, want promoted %q", got, targetReleaseID)
	}
	if got := promotionServingRelease(t, pool, "live", "staging"); got != string(stagingRelease) {
		t.Fatalf("staging serving changed to %q", got)
	}
	var domainsAfter string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(d.name, ',' ORDER BY d.name),'')
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='live'`).Scan(&domainsAfter); err != nil {
		t.Fatal(err)
	}
	if domainsAfter != domainsBefore {
		t.Fatalf("promotion changed domains: %q -> %q", domainsBefore, domainsAfter)
	}
	if promotion.Kind != platform.KindPromote || promotion.Target != "env/live/production" {
		t.Fatalf("promotion operation = %s %s", promotion.Kind, promotion.Target)
	}
	if fake.runCount(promotion.ID, deploymentPackStep) != 0 {
		t.Fatal("promotion requested a build")
	}
	if fake.runCount(promotion.ID, deploymentCommandStep) != 1 {
		t.Fatalf("release command runs = %d, want 1", fake.runCount(promotion.ID, deploymentCommandStep))
	}
	var commandPayload platform.ReleaseCommandPayload
	if err := json.Unmarshal(fake.payload(promotion.ID, deploymentCommandStep), &commandPayload); err != nil {
		t.Fatal(err)
	}
	if strings.Join(commandPayload.Command, " ") != "/bin/migrate production" {
		t.Fatalf("release command = %v, want production's command", commandPayload.Command)
	}
	var containerPayload platform.ContainerPayload
	if err := json.Unmarshal(fake.payload(promotion.ID, containerStepID("web", "1")), &containerPayload); err != nil {
		t.Fatal(err)
	}
	if containerPayload.Image != stagingImageID || containerPayload.Env["ROLE"] != "production" {
		t.Fatalf("production container payload = image %q env %+v", containerPayload.Image, containerPayload.Env)
	}
	calls := resolver.snapshot()
	if len(calls) == 0 {
		t.Fatal("promotion resolved no secrets")
	}
	last := calls[len(calls)-1]
	if last.app != "live" || last.environment != "production" || len(last.names) != 1 || last.names[0] != "PROD_ONLY" {
		t.Fatalf("promotion secret resolution = %+v, want production's PROD_ONLY", last)
	}
	var changeKind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM eregion_platform_changes WHERE operation_id=$1`, promotion.ID).Scan(&changeKind); err != nil {
		t.Fatal(err)
	}
	if changeKind != "promotion.succeeded" {
		t.Fatalf("change kind = %q", changeKind)
	}

	// The same idempotency key replays the accepted operation without a second
	// release, even though a new submission would now be refused.
	replayed, err := s.Submit(ctx, platformSubmission(platform.KindPromote, "env/live/production", "promote-1", input))
	if err != nil || replayed.ID != promotion.ID {
		t.Fatalf("same-key replay = %+v, %v", replayed, err)
	}
	// A preview with the same key and identical normalized input replays the
	// accepted operation without re-resolving or applying conflict checks.
	replayedPreview, err := s.Preview(ctx, testActor, normalized, "promote-1")
	if err != nil || !replayedPreview.Replay || replayedPreview.Operation == nil || replayedPreview.Operation.ID != promotion.ID {
		t.Fatalf("same-key preview replay = %+v, %v", replayedPreview, err)
	}
	// The same key with changed input is a conflict before any source
	// resolution or conflict checks, exactly like submit.
	changed := input
	changed.SourceEnvironment = "not-staging"
	changedRaw, _ := json.Marshal(changed)
	changedNormalized, err := platform.Default().Normalize(platform.KindPromote, "env/live/production", changedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview(ctx, testActor, changedNormalized, "promote-1"); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed-input preview with the same key = %v, want conflict", err)
	}
	var productionReleases int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_releases r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='live' AND e.name='production'`).Scan(&productionReleases); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, platformSubmission(platform.KindPromote, "env/live/production", "promote-2", input)); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("duplicate promotion = %v, want conflict", err)
	}
	var afterDuplicate int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_releases r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='live' AND e.name='production'`).Scan(&afterDuplicate); err != nil {
		t.Fatal(err)
	}
	if afterDuplicate != productionReleases {
		t.Fatalf("duplicate promotion recorded a release: %d -> %d", productionReleases, afterDuplicate)
	}
}

func TestIntegrationPromotionAdmissionSerializesProductionDeployments(t *testing.T) {
	pool, _ := testPool(t, true)
	fake := newScriptedDeployTransport()
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: fake, NodeStateDir: t.TempDir(),
		LocalNodeName: "local", RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	ctx := context.Background()

	runOperation := func(op contract.Operation) {
		t.Helper()
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatalf("%s attempt: %v", op.Kind, err)
		}
		platformAwait(t, s, op, contract.Succeeded)
	}
	runOperation(platformSubmit(t, s, platform.KindNodeRegister, "platform", "serialize-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}))
	runOperation(platformSubmit(t, s, platform.KindAppCreate, "platform", "serialize-app", map[string]any{"name": "live"}))
	for _, env := range []struct{ key, name, domain string }{
		{"serialize-prod-env", "production", "prod.example.com"},
		{"serialize-stage-env", "staging", "staging.example.com"},
	} {
		runOperation(platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", env.key, map[string]any{
			"name": env.name, "node": "local", "container_port": 80,
			"health": map[string]any{"timeout_seconds": 2},
			"domain": env.domain, "domain_primary": true,
		}))
	}
	createRelease := func(_, environment, image string) platform.ReleaseID {
		return seedArtifactRelease(t, s, "live", environment, image).ID
	}
	first := createRelease("serialize-prod-a", "production", "registry.example/live:a")
	runOperation(platformSubmit(t, s, platform.KindDeploy, "env/live/production", "serialize-deploy-a", map[string]any{"release_id": first}))
	staging := createRelease("serialize-stage-s", "staging", "registry.example/live:s")
	runOperation(platformSubmit(t, s, platform.KindDeploy, "env/live/staging", "serialize-deploy-s", map[string]any{"release_id": staging}))
	second := createRelease("serialize-prod-b", "production", "registry.example/live:b")

	// A queued ordinary deploy for a different artifact must block promotion
	// admission; otherwise the promotion would capture the stale previous
	// deployment from the environment row.
	queued := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "serialize-deploy-b", map[string]any{"release_id": second})
	promotionInput := platform.PromoteInput{App: "live", SourceEnvironment: "staging"}
	if _, err := s.Submit(ctx, platformSubmission(platform.KindPromote, "env/live/production", "serialize-promote-blocked", promotionInput)); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("promotion behind queued deploy = %v, want conflict", err)
	}
	var blockedPromotions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1`, platform.KindPromote).Scan(&blockedPromotions); err != nil {
		t.Fatal(err)
	}
	if blockedPromotions != 0 {
		t.Fatalf("refused promotion was accepted: %d operations", blockedPromotions)
	}
	runOperation(queued)

	// Concurrent promotions serialize on the production row; exactly one is
	// accepted and the other sees its active deployment.
	type submitResult struct {
		op  contract.Operation
		err error
	}
	results := make(chan submitResult, 2)
	for _, key := range []string{"serialize-promote-1", "serialize-promote-2"} {
		key := key
		go func() {
			op, err := s.Submit(ctx, platformSubmission(platform.KindPromote, "env/live/production", key, promotionInput))
			results <- submitResult{op: op, err: err}
		}()
	}
	var promotion contract.Operation
	conflicts := 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			promotion = result.op
		case contract.Classify(result.err).Code == contract.Conflict:
			conflicts++
		default:
			t.Fatalf("concurrent promotion = %v", result.err)
		}
	}
	if promotion.ID == "" || conflicts != 1 {
		t.Fatalf("concurrent promotions: accepted=%q conflicts=%d", promotion.ID, conflicts)
	}

	// An ordinary deploy queued after the promotion may run first. The
	// promotion must rebind its previous release/deployment at execution time
	// so cleanup and rollback target the deployment that actually preceded it.
	third := createRelease("serialize-prod-c", "production", "registry.example/live:c")
	later := platformSubmit(t, s, platform.KindDeploy, "env/live/production", "serialize-deploy-c", map[string]any{"release_id": third})
	runOperation(later)
	runOperation(promotion)

	var promotedRelease string
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_platform_releases WHERE operation_id=$1`, promotion.ID).Scan(&promotedRelease); err != nil {
		t.Fatal(err)
	}
	if got := promotionServingRelease(t, pool, "live", "production"); got != promotedRelease {
		t.Fatalf("promotion serving = %q, want %q", got, promotedRelease)
	}
	var laterDeployment string
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_platform_deployments WHERE operation_id=$1`, later.ID).Scan(&laterDeployment); err != nil {
		t.Fatal(err)
	}
	var previousRelease, previousDeployment string
	if err := pool.QueryRow(ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
        FROM eregion_platform_deployments WHERE operation_id=$1`, promotion.ID).Scan(&previousRelease, &previousDeployment); err != nil {
		t.Fatal(err)
	}
	if previousRelease != string(third) || previousDeployment != laterDeployment {
		t.Fatalf("promotion previous = %s/%s, want later deploy %s/%s", previousRelease, previousDeployment, third, laterDeployment)
	}
}

func operationCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_operations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

var _ = time.Second
