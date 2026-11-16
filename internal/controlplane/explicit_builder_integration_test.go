package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const explicitBuilderCommit = "1234567890abcdef1234567890abcdef12345678"

// TestIntegrationExplicitBuilderRequired proves a Git build recipe must carry
// its own builder. Missing builder is an actionable admission error before any
// durable operation or build row, while an explicit artifact/release remains
// builder-independent.
func TestIntegrationExplicitBuilderRequired(t *testing.T) {
	service, fixture := newArtifactAdmissionFixture(t)
	service.resolveGitRef = func(_ context.Context, _, ref string, _ []byte) (string, error) {
		return ref, nil
	}
	ctx := context.Background()
	fixture.run(platformSubmit(t, service, platform.KindAppCreate, "platform", "explicit-builder-app", map[string]any{"name": "explicit"}))
	incomplete := platformSubmit(t, service, platform.KindEnvironmentCreate, "app/explicit", "explicit-incomplete", map[string]any{
		"name": "incomplete", "node": "local", "container_port": 80,
		"domain": "incomplete.example.com", "domain_primary": true,
		"source": map[string]any{"url": "https://github.com/acme/app.git", "ref": explicitBuilderCommit},
	})
	fixture.run(incomplete)

	key := "explicit-builder-git-deploy"
	_, err := service.Submit(ctx, platformSubmission(platform.KindDeploy, "env/explicit/incomplete", key,
		map[string]any{"ref": explicitBuilderCommit}))
	if err == nil {
		t.Fatal("a Git deploy without a builder was accepted")
	}
	fault := contract.Classify(err)
	if fault.Code != contract.PreconditionFailed || !strings.Contains(strings.ToLower(fault.Message), "builder") {
		t.Fatalf("missing builder error=%v", err)
	}
	if !strings.Contains(strings.ToLower(fault.Hint), "--builder") && !strings.Contains(strings.ToLower(fault.Hint), "source.builder") {
		t.Fatalf("missing builder hint is not actionable: %q", fault.Hint)
	}
	var operations int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, key).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("missing builder left %d operation(s), err=%v", operations, err)
	}
	var jobs int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM river_job j
        JOIN eregion_operations o ON o.job_id=j.id WHERE o.idempotency_key=$1`, key).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("missing builder left %d job(s), err=%v", jobs, err)
	}
	var builds int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_builds b
        JOIN eregion_platform_releases r ON r.id=b.release_id
        WHERE r.operation_id IN (SELECT id FROM eregion_operations WHERE idempotency_key=$1)`, key).Scan(&builds); err != nil || builds != 0 {
		t.Fatalf("missing builder left %d build(s), err=%v", builds, err)
	}

	// Explicit artifact/release deployment must remain builder-independent even
	// when the Git recipe is incomplete.
	release := seedArtifactRelease(t, fixture.pool, service, "explicit", "incomplete", "registry.example/explicit:v1")
	artifactOp, err := service.Submit(ctx, platformSubmission(platform.KindDeploy, "env/explicit/incomplete", "explicit-builder-release",
		map[string]any{"release_id": release.ID}))
	if err != nil {
		t.Fatalf("explicit release deploy was blocked by the missing builder: %v", err)
	}
	if artifactOp.ID == "" {
		t.Fatal("explicit release deploy did not return an operation")
	}
	assertNoBuildForExplicitOperation(t, fixture.pool, artifactOp.ID)

	service.resolveArtifactFn = func(_ context.Context, request artifactRequest) (resolvedArtifact, error) {
		return resolvedArtifact{
			Kind: request.Kind, Requested: request.Requested,
			Registry: "ghcr.io", Repository: "acme/explicit",
			ManifestDigest: "sha256:" + strings.Repeat("1", 64),
			ConfigDigest:   "sha256:" + strings.Repeat("2", 64),
			CanonicalImage: "ghcr.io/acme/explicit@sha256:" + strings.Repeat("1", 64),
		}, nil
	}
	for name, input := range map[string]map[string]any{
		"image tag":    {"image_tag": "v1"},
		"image digest": {"image_digest": "sha256:" + strings.Repeat("1", 64)},
		"image":        {"image": "ghcr.io/acme/explicit:v1"},
	} {
		op, err := service.Submit(ctx, platformSubmission(platform.KindDeploy, "env/explicit/incomplete",
			"explicit-builder-"+strings.ReplaceAll(name, " ", "-"), input))
		if err != nil {
			t.Fatalf("explicit %s deploy was blocked by the missing builder: %v", name, err)
		}
		assertNoBuildForExplicitOperation(t, fixture.pool, op.ID)
	}
}

func assertNoBuildForExplicitOperation(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID) {
	t.Helper()
	var builds int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_builds b
        JOIN eregion_platform_releases r ON r.id=b.release_id WHERE r.operation_id=$1`, operationID).Scan(&builds); err != nil {
		t.Fatal(err)
	}
	if builds != 0 {
		t.Fatalf("explicit operation %s created %d build row(s)", operationID, builds)
	}
}
