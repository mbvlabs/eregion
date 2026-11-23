package controlplane

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// This deliberately uses the real local dispatcher and an isolated Docker
// daemon. No Docker workload is created unless the fixture explicitly opts in.
func checkPgBouncerResource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run *Runtime) {
	t.Helper()
	if os.Getenv("EREGION_TEST_PGBOUNCER_DOCKER") != "1" {
		return
	}
	s := run.Service
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleApplication}, []string{platform.CapabilityDocker}, ""); err != nil {
		t.Fatal(err)
	}
	platformAwaitContext(ctx, t, s, platformSubmitContext(ctx, t, s, platform.KindAppCreate, "platform", "pooler-resource-app", map[string]any{
		"name": "poolerfixture",
		"environments": []map[string]any{{
			"name": "production", "node": "local", "container_port": 8080,
		}},
	}), contract.Succeeded)
	claims := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	holder := claimID("pooler-resource-holder", "pooler-resource-holder-token")
	if !claims.acquire(claims.a, holder, "env/poolerfixture/production") {
		t.Fatal("reserve environment")
	}
	op := platformSubmitContext(ctx, t, s, platform.KindResourceApply, "resource/poolerfixture/production/db", "pooler-resource", map[string]any{
		"image": "postgres:17",
		"env":   map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"},
		"health": map[string]any{
			"type": "exec", "command": []string{"pg_isready", "-U", "postgres"},
		},
		"mounts": []map[string]any{{"name": "data", "destination": "/var/lib/postgresql/data"}},
	})
	awaitPoolerSnooze(ctx, t, pool, op.ID)
	if !claims.transition(claims.a, holder, "seal") || !claims.transition(claims.a, holder, "release") {
		t.Fatal("release environment")
	}
	platformAwaitContext(ctx, t, s, op, contract.Succeeded)
	assertPoolerResourceDocker(t, ctx, s)
	await(t, 10*time.Second, func() bool {
		var state string
		err := pool.QueryRow(ctx, `SELECT j.state FROM river_job j JOIN eregion_operations o ON o.job_id=j.id WHERE o.id=$1`, op.ID).Scan(&state)
		return err == nil && state == "completed"
	})
	checkPgBouncerBuildTarget(ctx, t, pool, s)
}

func checkPgBouncerBuildTarget(ctx context.Context, t *testing.T, pool *pgxpool.Pool, s *Service) {
	t.Helper()
	env, err := s.environment(ctx, "poolerfixture", "production")
	if err != nil {
		t.Fatal(err)
	}
	seed := seedPoolerBuildOperation(ctx, t, s)
	seedDirectoryBuildRelease(t, ctx, s, seed, env, t.TempDir())
	build, found, err := s.buildByOperation(ctx, seed)
	if err != nil || !found {
		t.Fatalf("seed build: %v %v", found, err)
	}
	target := "build/" + string(build.ID)
	f := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	holder := claimID("pooler-stop-holder", "pooler-stop-holder-token")
	if !f.acquire(f.a, holder, target) {
		t.Fatal("reserve build target")
	}
	op := platformSubmitContext(ctx, t, s, platform.KindBuildStop, target, "pooler-contended-stop", map[string]any{})
	awaitPoolerSnooze(ctx, t, pool, op.ID)
	if !f.transition(f.a, holder, "seal") || !f.transition(f.a, holder, "release") {
		t.Fatal("release build target")
	}
	platformAwaitContext(ctx, t, s, op, contract.Succeeded)
}

func awaitPoolerSnooze(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id contract.OperationID) {
	t.Helper()
	await(t, 10*time.Second, func() bool {
		var snoozes int
		err := pool.QueryRow(ctx, `SELECT COALESCE((j.metadata->>'snoozes')::int, 0)
			FROM river_job j JOIN eregion_operations o ON o.job_id=j.id WHERE o.id=$1`, id).Scan(&snoozes)
		return err == nil && snoozes > 0
	})
}

func seedPoolerBuildOperation(ctx context.Context, t *testing.T, s *Service) contract.OperationID {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{}`)
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		ok, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id, Key: entities.OperationKey{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "pooler-build-seed"},
			Input: raw, InputHash: hashInput(raw),
		})
		if err == nil && !ok {
			t.Fatal("fixture operation already exists")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertPoolerResourceDocker(t *testing.T, ctx context.Context, s *Service) {
	t.Helper()
	resource, err := s.resourceByName(ctx, "poolerfixture", "production", "db")
	if err != nil || resource.State != platform.ResourceReady || resource.ContainerName == "" || len(resource.Volumes) != 1 {
		t.Fatalf("resource was not durably provisioned: %+v: %v", resource, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		for _, args := range [][]string{
			{"container", "rm", "-f", resource.ContainerName},
			{"volume", "rm", resource.Volumes[0].Name},
		} {
			out, err := exec.CommandContext(cleanup, "docker", args...).CombinedOutput()
			if err != nil {
				t.Errorf("remove fixture %s: %s: %v", args[0], out, err)
			}
		}
	})
	for _, target := range []struct{ kind, name string }{
		{"container", resource.ContainerName},
		{"volume", resource.Volumes[0].Name},
	} {
		format := `{{index .Labels "io.eregion.resource-id"}}`
		if target.kind == "container" {
			format = `{{index .Config.Labels "io.eregion.resource-id"}}`
		}
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--type", target.kind, "--format", format, target.name).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != string(resource.ID) {
			t.Fatalf("inspect Docker %s %s: %s: %v", target.kind, target.name, out, err)
		}
	}
	t.Logf("real Docker resource and volume have the recorded owner %s", resource.ID)
}
