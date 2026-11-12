package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// newArtifactAdmissionFixture creates one registered node, an app with an
// explicit private repository and a production environment without a Git
// source. Artifact admission is then exercised through the same submission
// service operators use.
func newArtifactAdmissionFixture(t *testing.T) (*Service, *deploymentRetryFixture) {
	t.Helper()
	fixture := newDeploymentRetryFixtureWithTransport(t, newScriptedDeployTransport())
	fixture.service.registryEndpoint = "127.0.0.1:5000"
	// The fixture app already has "live" as its default repository. Artifact
	// tests use the same environment and explicitly select a tag.
	return fixture.service, fixture
}

func artifactAdmissionResolved(request artifactRequest, manifest, config string) resolvedArtifact {
	return resolvedArtifact{
		Kind: request.Kind, Requested: request.Requested,
		Registry: request.Registry, Repository: request.Repository,
		ManifestDigest: manifest, ConfigDigest: config,
		CanonicalImage: request.Registry + "/" + request.Repository + "@" + manifest,
	}
}

func TestIntegrationAppCreateRejectsCanonicalRepositoryOverflowBeforeCreation(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	longRepository := strings.Repeat("a", platform.MaxImageRepositoryLength)
	raw, _ := json.Marshal(map[string]any{"name": "toolong", "image_repository": longRepository})
	_, err := service.Submit(fixture.ctx, operations.Submission{
		Kind: platform.KindAppCreate, Target: platform.PlatformTarget().String(),
		Actor: testActor, IdempotencyKey: "artifact-long-repo", Input: raw,
	})
	if err == nil {
		t.Fatal("an app repository whose canonical reference exceeds 255 characters was accepted")
	}
	var apps int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_apps WHERE name='toolong'`).Scan(&apps); err != nil {
		t.Fatal(err)
	}
	if apps != 0 {
		t.Fatalf("rejected app left %d row(s)", apps)
	}
}

func TestIntegrationArtifactPublicDifferentRepositoryIsAdmitted(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	manifest := "sha256:" + strings.Repeat("a", 64)
	config := "sha256:" + strings.Repeat("b", 64)
	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		return resolvedArtifact{
			Kind: request.Kind, Requested: request.Requested,
			Registry: "ghcr.io", Repository: "acme/public-api",
			ManifestDigest: manifest, ConfigDigest: config,
			CanonicalImage: "ghcr.io/acme/public-api@" + manifest,
		}, nil
	}
	op, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-public-other-repo",
		map[string]any{"image": "ghcr.io/acme/public-api:v1"}))
	if err != nil {
		t.Fatalf("a public image from another repository was refused: %v", err)
	}
	var image string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT image FROM eregion_platform_releases WHERE operation_id=$1`, op.ID).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if image != "ghcr.io/acme/public-api@"+manifest {
		t.Fatalf("release image = %s", image)
	}
}

func TestIntegrationArtifactAdmissionPinsCanonicalManifestAndConfigBeforeDeployment(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	manifest := "sha256:" + strings.Repeat("1", 64)
	config := "sha256:" + strings.Repeat("2", 64)
	var calls atomic.Int32
	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		calls.Add(1)
		if request.Repository != "live" {
			t.Fatalf("repository = %q, want the durable app repository", request.Repository)
		}
		return artifactAdmissionResolved(request, manifest, config), nil
	}
	op, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-pin", map[string]any{"image_tag": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls.Load())
	}
	var image, imageID, rawSource string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT image,image_id,source::text FROM eregion_platform_releases WHERE operation_id=$1`, op.ID).Scan(&image, &imageID, &rawSource); err != nil {
		t.Fatal(err)
	}
	wantImage := platform.CanonicalRegistryAddress(service.registryEndpoint)
	if wantImage == "" {
		wantImage = service.registryEndpoint
	}
	if image != wantImage+"/live@"+manifest || imageID != config {
		t.Fatalf("release image/config = %s/%s", image, imageID)
	}
	if !strings.Contains(rawSource, `"selector":"v1"`) || !strings.Contains(rawSource, `"selector_kind":"image-tag"`) || !strings.Contains(rawSource, `"type":"image"`) {
		t.Fatalf("release source provenance = %s", rawSource)
	}
	var depRelease, depState string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT release_id,state FROM eregion_platform_deployments WHERE operation_id=$1`, op.ID).Scan(&depRelease, &depState); err != nil {
		t.Fatal(err)
	}
	if depState != "pending" {
		t.Fatalf("deployment state = %s", depState)
	}
	var pending, ready int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FILTER (WHERE status='pending'), count(*) FILTER (WHERE status='ready') FROM eregion_platform_releases WHERE operation_id=$1`, op.ID).Scan(&pending, &ready); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || ready != 0 {
		t.Fatalf("release status pending=%d ready=%d", pending, ready)
	}
}

func TestIntegrationArtifactReplaySkipsRegistryAfterTagMoves(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	firstManifest := "sha256:" + strings.Repeat("3", 64)
	secondManifest := "sha256:" + strings.Repeat("4", 64)
	config := "sha256:" + strings.Repeat("5", 64)
	var calls atomic.Int32
	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		if calls.Add(1) == 1 {
			return artifactAdmissionResolved(request, firstManifest, config), nil
		}
		return artifactAdmissionResolved(request, secondManifest, config), nil
	}
	first, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-replay", map[string]any{"image_tag": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-replay", map[string]any{"image_tag": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID || calls.Load() != 1 {
		t.Fatalf("replay = %s, resolver calls = %d", replay.ID, calls.Load())
	}
	var image string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT image FROM eregion_platform_releases WHERE operation_id=$1`, first.ID).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(image, "@"+firstManifest) {
		t.Fatalf("replayed release followed the moved tag: %s", image)
	}
	second, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-new-key", map[string]any{"image_tag": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	var secondImage string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT image FROM eregion_platform_releases WHERE operation_id=$1`, second.ID).Scan(&secondImage); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(secondImage, "@"+secondManifest) {
		t.Fatalf("new key did not resolve the moved tag: %s", secondImage)
	}
	if _, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-replay", map[string]any{"image_tag": "v2"})); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed replay = %v, want conflict", err)
	}
}

func TestIntegrationArtifactAdmissionRechecksPlacementFactsInsideTransaction(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	manifest := "sha256:" + strings.Repeat("6", 64)
	config := "sha256:" + strings.Repeat("7", 64)
	service.resolveArtifactFn = func(ctx context.Context, request artifactRequest) (resolvedArtifact, error) {
		if _, err := fixture.pool.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities)
            VALUES('other',ARRAY['application'],ARRAY['docker']) ON CONFLICT (name) DO NOTHING`); err != nil {
			return resolvedArtifact{}, err
		}
		// Simulate a concurrent placement change after the read-only resolution
		// but before the admission transaction opens.
		if _, err := fixture.pool.Exec(ctx, `UPDATE eregion_platform_environments SET node_name='other' WHERE name='production'`); err != nil {
			return resolvedArtifact{}, err
		}
		return artifactAdmissionResolved(request, manifest, config), nil
	}
	_, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-placement", map[string]any{"image_tag": "v1"}))
	if err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("placement change = %v, want conflict", err)
	}
	var releases int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_releases WHERE operation_id IN
        (SELECT id FROM eregion_operations WHERE idempotency_key='artifact-placement')`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 0 {
		t.Fatalf("placement conflict wrote %d release rows", releases)
	}
}

func TestIntegrationArtifactConcurrentFirstSubmissionHasOneWinner(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	manifest := "sha256:" + strings.Repeat("8", 64)
	config := "sha256:" + strings.Repeat("9", 64)
	var calls atomic.Int32
	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		calls.Add(1)
		return artifactAdmissionResolved(request, manifest, config), nil
	}
	submission := platformSubmission(platform.KindDeploy, "env/live/production", "artifact-concurrent", map[string]any{"image_tag": "v1"})
	var wg sync.WaitGroup
	results := make([]contract.Operation, 2)
	errs := make([]error, 2)
	for index := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = service.Submit(fixture.ctx, submission)
		}(index)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent submissions = %v / %v", errs[0], errs[1])
	}
	if results[0].ID != results[1].ID {
		t.Fatalf("concurrent submissions created %s and %s", results[0].ID, results[1].ID)
	}
	var operations int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key='artifact-concurrent'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 1 {
		t.Fatalf("concurrent submissions created %d operations", operations)
	}
}

func TestIntegrationArtifactRegistryOutageDoesNotFailAReplay(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	manifest := "sha256:" + strings.Repeat("a", 64)
	config := "sha256:" + strings.Repeat("b", 64)
	var calls atomic.Int32
	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		if calls.Add(1) > 1 {
			return resolvedArtifact{}, contract.NewFault(contract.Unavailable, "simulated registry outage", "retry later")
		}
		return artifactAdmissionResolved(request, manifest, config), nil
	}
	first, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-outage", map[string]any{"image_digest": manifest}))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-outage", map[string]any{"image_digest": manifest}))
	if err != nil {
		t.Fatalf("replay during registry outage failed: %v", err)
	}
	if replay.ID != first.ID || calls.Load() != 1 {
		t.Fatalf("replay = %s, resolver calls = %d", replay.ID, calls.Load())
	}
}

func TestIntegrationAppImageRepositoryIsUniqueAndAtomicWithEnvironments(t *testing.T) {
	fixture := newDeploymentRetryFixtureWithTransport(t, newScriptedDeployTransport())
	service := fixture.service
	create := func(name, repository string) (contract.Operation, error) {
		raw, _ := json.Marshal(map[string]any{
			"name": name, "image_repository": repository,
			"environments": []map[string]any{
				{"name": "production", "node": "local", "container_port": 8080},
				{"name": "staging", "node": "local", "container_port": 8081},
			},
		})
		return service.Submit(fixture.ctx, operations.Submission{
			Kind: platform.KindAppCreate, Target: platform.PlatformTarget().String(),
			Actor: testActor, IdempotencyKey: "artifact-app-" + name, Input: raw,
		})
	}
	if _, err := create("imageonly", "mbvlabs/imageonly"); err != nil {
		t.Fatal(err)
	}
	var repo string
	var environments int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT image_repository FROM eregion_platform_apps WHERE name='imageonly'`).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	if repo != "mbvlabs/imageonly" {
		t.Fatalf("repository = %q", repo)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='imageonly'`).Scan(&environments); err != nil {
		t.Fatal(err)
	}
	if environments != 2 {
		t.Fatalf("environments = %d", environments)
	}
	if _, err := create("duplicate", "mbvlabs/imageonly"); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("duplicate repository = %v, want conflict", err)
	}
	var apps, duplicateEnvs int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT (SELECT count(*) FROM eregion_platform_apps WHERE name='duplicate'), (SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='duplicate')`).Scan(&apps, &duplicateEnvs); err != nil {
		t.Fatal(err)
	}
	if apps != 0 || duplicateEnvs != 0 {
		t.Fatalf("duplicate repository left partial rows: apps=%d envs=%d", apps, duplicateEnvs)
	}
}

func TestIntegrationArtifactPreviewIsReadOnlyAndDoesNotResolveTheRegistry(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	service.resolveArtifactFn = func(context.Context, artifactRequest) (resolvedArtifact, error) {
		t.Fatal("preview contacted the artifact resolver")
		return resolvedArtifact{}, nil
	}
	normalized, err := platform.Default().Normalize(platform.KindDeploy, "env/live/production",
		[]byte(`{"image_tag":"v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	beforeOps, beforeReleases := countArtifactRows(t, fixture)
	preview, err := service.Preview(fixture.ctx, testActor, normalized, "artifact-preview")
	if err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || preview.Replay {
		t.Fatalf("preview = %+v", preview)
	}
	if !strings.Contains(strings.Join(preview.Warnings, " "), "UNVERIFIED") {
		t.Fatalf("preview did not disclose unverified availability: %+v", preview.Warnings)
	}
	afterOps, afterReleases := countArtifactRows(t, fixture)
	if beforeOps != afterOps || beforeReleases != afterReleases {
		t.Fatalf("preview wrote operations/releases: %d/%d then %d/%d", beforeOps, beforeReleases, afterOps, afterReleases)
	}
}

func countArtifactRows(t *testing.T, fixture *deploymentRetryFixture) (operations, releases int) {
	t.Helper()
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT (SELECT count(*) FROM eregion_operations), (SELECT count(*) FROM eregion_platform_releases)`).Scan(&operations, &releases); err != nil {
		t.Fatal(err)
	}
	return operations, releases
}

func TestIntegrationArtifactPreviewReplaySkipsRegistryAndFencesChangedInput(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	manifest := "sha256:" + strings.Repeat("e", 64)
	config := "sha256:" + strings.Repeat("f", 64)
	var calls atomic.Int32
	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		calls.Add(1)
		return artifactAdmissionResolved(request, manifest, config), nil
	}
	accepted, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-preview-replay", map[string]any{"image_tag": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := platform.Default().Normalize(platform.KindDeploy, "env/live/production", []byte(`{"image_tag":"v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(fixture.ctx, testActor, normalized, "artifact-preview-replay")
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Replay || preview.Operation == nil || preview.Operation.ID != accepted.ID {
		t.Fatalf("preview replay = %+v", preview)
	}
	if calls.Load() != 1 {
		t.Fatalf("preview replay called the registry %d time(s)", calls.Load()-1)
	}
	changed, err := platform.Default().Normalize(platform.KindDeploy, "env/live/production", []byte(`{"image_tag":"v2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Preview(fixture.ctx, testActor, changed, "artifact-preview-replay"); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed preview replay = %v, want conflict", err)
	}
}

func TestIntegrationArtifactNoGitImageOnlyAdmissionNeedsNoBuildPlacement(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	manifest := "sha256:" + strings.Repeat("c", 64)
	config := "sha256:" + strings.Repeat("d", 64)
	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		return artifactAdmissionResolved(request, manifest, config), nil
	}
	// The fixture node has both roles, so remove the builder capability from the
	// admission path's point of view by not making it relevant: no build record
	// and no pack call may be created for an explicit artifact.
	op, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-no-build", map[string]any{"image_tag": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	var builds int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_builds b JOIN eregion_platform_releases r ON r.id=b.release_id WHERE r.operation_id=$1`, op.ID).Scan(&builds); err != nil {
		t.Fatal(err)
	}
	if builds != 0 {
		t.Fatalf("explicit artifact admission created %d build rows", builds)
	}
	if _, err := service.Submit(fixture.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "artifact-guidance", map[string]any{"ref": "main"})); err == nil || !strings.Contains(err.Error(), "artifact") {
		t.Fatalf("Git-less ref deploy = %v, want explicit artifact guidance", err)
	}
}
