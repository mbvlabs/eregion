package controlplane

import (
	"context"
	"strings"
	"testing"

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
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities) VALUES('main',ARRAY['application'],ARRAY['docker'])`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_apps(id,name) VALUES($1,'demo')`, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environments(id,app_id,name,node_name,host_port,candidate_port,container_port)
        VALUES($1,$2,'production','main',20002,30002,8080)`, envID, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environment_revisions(id,environment_id,number,operation,runtime)
        VALUES($1,$2,1,'env.changed','{"health_path":"/pinned"}'),($3,$2,2,'env.changed','{"health_path":"/desired"}')`,
		pinnedID, envID, desiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environments SET current_revision_id=$2 WHERE id=$1`, envID, desiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_domains(name,environment_id,is_primary)
        VALUES('live.example.com',$1,true)`, envID); err != nil {
		t.Fatal(err)
	}

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
