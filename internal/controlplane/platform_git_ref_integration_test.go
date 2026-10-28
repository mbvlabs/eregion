package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const submittedGitCommit = "0123456789abcdef0123456789abcdef01234567"

// gitDeployFixture records an app and one deployable environment whose source
// is a git revision. No worker is started: these tests inspect the rows a
// submission writes.
func gitDeployFixture(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "node-app-1", map[string]any{
		"name": "app-1", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "app-shop", map[string]any{"name": "shop"})
	platformSubmit(t, s, platform.KindEnvironmentCreate, "app/shop", "env-production", map[string]any{
		"name": "production", "node": "app-1",
		"source": map[string]any{
			"kind": "git", "url": "https://github.com/acme/app.git", "ref": "main",
			"builder": platform.DefaultBuilder,
		},
		"domain": "shop.example.com", "domain_addresses": []string{"203.0.113.10"},
	})
	return s, pool
}

func TestIntegrationDeployStoresTheResolvedCommitAtInsert(t *testing.T) {
	s, pool := gitDeployFixture(t)
	ctx := context.Background()
	var resolvedURL, resolvedRef string
	s.resolveGitRef = func(_ context.Context, url, ref string, credential []byte) (string, error) {
		resolvedURL, resolvedRef = url, ref
		if len(credential) != 0 {
			t.Fatalf("an uncredentialed source received a credential")
		}
		return submittedGitCommit, nil
	}
	op := platformSubmit(t, s, platform.KindDeploy, "env/shop/production", "deploy-main", map[string]any{})
	if resolvedURL != "https://github.com/acme/app.git" || resolvedRef != "main" {
		t.Fatalf("resolved (%q, %q), want the revision's git source", resolvedURL, resolvedRef)
	}
	var buildCommit, releaseCommit, buildRef string
	if err := pool.QueryRow(ctx, `SELECT b.commit_sha,r.commit_sha,b.source->>'ref'
        FROM eregion_platform_builds b JOIN eregion_platform_releases r ON r.build_id=b.id
        WHERE b.operation_id=$1`, op.ID).Scan(&buildCommit, &releaseCommit, &buildRef); err != nil {
		t.Fatal(err)
	}
	if buildCommit != submittedGitCommit || releaseCommit != submittedGitCommit {
		t.Fatalf("build commit=%q release commit=%q, want the resolved %q", buildCommit, releaseCommit, submittedGitCommit)
	}
	if buildRef != "main" {
		t.Fatalf("the build records ref %q, want the revision's ref while commit_sha carries the exact commit", buildRef)
	}
}

func TestIntegrationDeployRefusesARevisionThatChangedAfterResolution(t *testing.T) {
	s, pool := gitDeployFixture(t)
	ctx := context.Background()
	var revisionID string
	if err := pool.QueryRow(ctx, `SELECT e.current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='shop' AND e.name='production'`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	s.resolveGitRef = func(ctx context.Context, url, ref string, credential []byte) (string, error) {
		// Move the revision after it was read for resolution but before the
		// submission transaction reads it again.
		if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environment_revisions
            SET source=jsonb_set(source,'{ref}','"other"') WHERE id=$1`, revisionID); err != nil {
			return "", err
		}
		return submittedGitCommit, nil
	}
	_, err := s.Submit(ctx, platformSubmission(platform.KindDeploy, "env/shop/production", "deploy-moved", map[string]any{}))
	if code := contract.Classify(err).Code; code != contract.Conflict || !strings.Contains(err.Error(), "environment source changed during submission") {
		t.Fatalf("moved revision refusal = %s, %v", code, err)
	}
	var operations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1`, platform.KindDeploy).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 0 {
		t.Fatalf("%d deploy operation(s) were created for a refused submission", operations)
	}
}

func TestIntegrationDeployRefusesAGitToImageRevisionChange(t *testing.T) {
	s, pool := gitDeployFixture(t)
	ctx := context.Background()
	var revisionID string
	if err := pool.QueryRow(ctx, `SELECT e.current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='shop' AND e.name='production'`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	s.resolveGitRef = func(ctx context.Context, url, ref string, credential []byte) (string, error) {
		// The revision becomes an image source after the ref was resolved but
		// before the submission transaction reads it again.
		if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environment_revisions
            SET source='{"kind":"image","image":"example/app:1"}'::jsonb WHERE id=$1`, revisionID); err != nil {
			return "", err
		}
		return submittedGitCommit, nil
	}
	_, err := s.Submit(ctx, platformSubmission(platform.KindDeploy, "env/shop/production", "deploy-kind-moved", map[string]any{}))
	if code := contract.Classify(err).Code; code != contract.Conflict || !strings.Contains(err.Error(), "environment source changed during submission") {
		t.Fatalf("source-kind change refusal = %s, %v", code, err)
	}
	var operations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1`, platform.KindDeploy).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 0 {
		t.Fatalf("%d deploy operation(s) were created for a refused submission", operations)
	}
}

func TestIntegrationDeployRefChangeWithOverrideDoesNotConflict(t *testing.T) {
	s, pool := gitDeployFixture(t)
	ctx := context.Background()
	var revisionID string
	if err := pool.QueryRow(ctx, `SELECT e.current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='shop' AND e.name='production'`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	var resolvedRef string
	s.resolveGitRef = func(ctx context.Context, url, ref string, credential []byte) (string, error) {
		resolvedRef = ref
		if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environment_revisions
            SET source=jsonb_set(source,'{ref}','"other"') WHERE id=$1`, revisionID); err != nil {
			return "", err
		}
		return submittedGitCommit, nil
	}
	op := platformSubmit(t, s, platform.KindDeploy, "env/shop/production", "deploy-override", map[string]any{"ref": "main"})
	if resolvedRef != "main" {
		t.Fatalf("resolved ref %q, want the explicit override", resolvedRef)
	}
	var buildCommit, releaseCommit, buildRef string
	if err := pool.QueryRow(ctx, `SELECT b.commit_sha,r.commit_sha,b.source->>'ref'
        FROM eregion_platform_builds b JOIN eregion_platform_releases r ON r.build_id=b.id
        WHERE b.operation_id=$1`, op.ID).Scan(&buildCommit, &releaseCommit, &buildRef); err != nil {
		t.Fatal(err)
	}
	if buildCommit != submittedGitCommit || releaseCommit != submittedGitCommit || buildRef != "main" {
		t.Fatalf("override build commit=%q release commit=%q ref=%q", buildCommit, releaseCommit, buildRef)
	}
}

func TestIntegrationIdenticalDeployReplaySkipsRefResolution(t *testing.T) {
	s, _ := gitDeployFixture(t)
	ctx := context.Background()
	calls := 0
	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) {
		calls++
		return submittedGitCommit, nil
	}
	first := platformSubmit(t, s, platform.KindDeploy, "env/shop/production", "deploy-replay", map[string]any{})
	if calls != 1 {
		t.Fatalf("first submission resolved %d time(s), want 1", calls)
	}

	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) {
		t.Fatal("a same-key replay resolved the git ref again")
		return "", contract.NewFault(contract.Unavailable, "the git repository could not be reached", "Retry later.")
	}
	second, err := s.Submit(ctx, platformSubmission(platform.KindDeploy, "env/shop/production", "deploy-replay", map[string]any{}))
	if err != nil {
		t.Fatalf("identical replay failed: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("identical replay returned %s, want the original %s", second.ID, first.ID)
	}

	_, err = s.Submit(ctx, platformSubmission(platform.KindDeploy, "env/shop/production", "deploy-replay", map[string]any{"ref": "main"}))
	if code := contract.Classify(err).Code; code != contract.Conflict {
		t.Fatalf("changed input code = %s, want %s: %v", code, contract.Conflict, err)
	}
}
