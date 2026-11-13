package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationPrimaryCheckPathUsesPinnedRevision proves the public check
// reads the deployment's pinned revision, not the environment's newer desired
// revision. A rollback to a release with a different health path must not be
// checked at the desired revision's path.
func TestIntegrationPrimaryCheckPathUsesPinnedRevision(t *testing.T) {
	pool, _ := testPool(t, true)
	service := &Service{pool: pool}
	ctx := context.Background()

	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	pinnedID := platform.RevisionID("erv_" + strings.Repeat("a", 32))
	desiredID := platform.RevisionID("erv_" + strings.Repeat("b", 32))
	if err := insertFixtureNode(ctx, pool, "main", []string{"application"}, []string{"docker"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureApp(ctx, pool, string(appID), "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, string(envID), string(appID), "production", "main", 20002, 30002, 8080); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureRevision(ctx, pool, string(pinnedID), string(envID), 1, `{"health_path":"/pinned"}`); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureRevision(ctx, pool, string(desiredID), string(envID), 2, `{"health_path":"/desired"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environments SET current_revision_id=$2 WHERE id=$1`, envID, desiredID); err != nil {
		t.Fatal(err)
	}
	insertFixtureDomain(t, ctx, pool, "live.example.com", string(envID), true, time.Now())

	env := platform.Environment{
		ID: envID, App: "demo", Name: "production", PrimaryDomain: "live.example.com",
		Revision: &platform.EnvironmentRevision{
			ID: pinnedID,
			RevisionState: platform.RevisionState{
				Runtime: platform.RuntimeSpec{HealthPath: "/pinned"},
			},
		},
		Health: platform.HealthConfig{Path: "/pinned"},
	}
	if got := service.primaryCheckPath(ctx, env); got != "/pinned" {
		t.Fatalf("public check path = %q, want the pinned revision path", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_domains SET check_path='/alias' WHERE name='live.example.com'`); err != nil {
		t.Fatal(err)
	}
	if got := service.primaryCheckPath(ctx, env); got != "/alias" {
		t.Fatalf("public check path = %q, want the explicit binding path", got)
	}
}
