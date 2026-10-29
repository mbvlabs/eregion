package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// Exercise the exact query used by all route writers, both while the switch is
// running (before acknowledgement) and during public verification. A periodic
// reconciliation or another environment's switch must not restore the old
// serving pointer, or remove a first deploy's site, during that interval.
func TestIntegrationRouteWritersPreserveCandidateUntilPublicVerification(t *testing.T) {
	pool, _ := testPool(t, true)
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 2, TestOnly: true, Transport: newScriptedDeployTransport(), NodeStateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	ctx := context.Background()
	checkCandidate := func(ctx context.Context, env platform.Environment) error {
		var expected []byte
		err := pool.QueryRow(ctx, `SELECT instances FROM eregion_platform_deployments WHERE environment_id=$1 AND state='running' ORDER BY created_at DESC LIMIT 1`, env.ID).Scan(&expected)
		if err != nil {
			return err
		}
		selected, err := routeInstancesForTest(ctx, s, "route.example.com")
		if err != nil {
			return err
		}
		var want []platform.DeploymentInstance
		if err := json.Unmarshal(expected, &want); err != nil {
			return err
		}
		if len(want) == 0 || !reflect.DeepEqual(selected, want) {
			t.Errorf("route selected %+v, want active candidate %+v", selected, want)
		}
		return nil
	}
	var switches, verifications atomic.Int32
	var failPublic atomic.Bool
	s.routeSwitch = func(ctx context.Context, env platform.Environment, _ []string) error {
		// The post-success resync occurs after recordServing, but while the
		// deployment remains running; it must still select the same instances.
		switches.Add(1)
		return checkCandidate(ctx, env)
	}
	s.publicVerify = func(ctx context.Context, env platform.Environment, _ string) error {
		verifications.Add(1)
		if err := checkCandidate(ctx, env); err != nil {
			return err
		}
		if failPublic.Load() {
			return contract.NewFault(contract.PreconditionFailed, "public probe failed", "test")
		}
		return nil
	}
	startWorkers(t, run)
	platformAwait(t, s, platformSubmit(t, s, platform.KindNodeRegister, "platform", "route-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindAppCreate, "platform", "route-app", map[string]any{"name": "route"}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindEnvironmentCreate, "app/route", "route-env", map[string]any{
		"name": "production", "node": "local", "container_port": 80, "health": map[string]any{"timeout_seconds": 2},
		"domain": "route.example.com", "domain_primary": true,
	}), contract.Succeeded)
	platformAwait(t, s, platformSubmit(t, s, platform.KindReleaseCreate, "env/route/production", "route-release", map[string]any{
		"source": map[string]any{"type": "image", "image": "registry.example/route:v1"},
	}), contract.Succeeded)
	release := latestRelease(t, pool)
	platformAwait(t, s, platformSubmit(t, s, platform.KindDeploy, "env/route/production", "route-first", map[string]any{"release_id": release}), contract.Succeeded)
	previous, err := routeInstancesForTest(ctx, s, "route.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(previous) == 0 {
		t.Fatal("no serving instances after first deploy")
	}
	failPublic.Store(true)
	platformAwait(t, s, platformSubmit(t, s, platform.KindDeploy, "env/route/production", "route-failed", map[string]any{"release_id": release}), contract.Failed)
	restored, err := routeInstancesForTest(ctx, s, "route.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, previous) {
		t.Fatalf("failed candidate remains selected: %+v, want serving %+v", restored, previous)
	}
	if switches.Load() < 2 || verifications.Load() != 2 {
		t.Fatalf("switches=%d verifications=%d", switches.Load(), verifications.Load())
	}
}

func routeInstancesForTest(ctx context.Context, s *Service, domain string) ([]platform.DeploymentInstance, error) {
	rows, err := s.pool.Query(ctx, desiredRoutesQuery, deploymentRouteStep, deploymentPublicStep, deploymentRestoreRouteStep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, environment string
		var primary bool
		var raw []byte
		if err := rows.Scan(&name, &environment, &primary, &raw); err != nil {
			return nil, err
		}
		if name == domain {
			var instances []platform.DeploymentInstance
			err := json.Unmarshal(raw, &instances)
			return instances, err
		}
	}
	return nil, rows.Err()
}
