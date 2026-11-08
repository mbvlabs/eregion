package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func insertRouteStep(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID, state, errorText string) {
	t.Helper()
	execution := engine.Execution{OperationID: operationID, Attempt: 1, Step: deploymentRouteStep}
	completedAt := "NULL"
	if state == "completed" {
		completedAt = "clock_timestamp()"
	}
	query := `INSERT INTO eregion_operation_steps
        (operation_id,step,inputs_hash,state,attempt,execution_id,started_at,completed_at,error)
        VALUES($1,$2,$3,$4,1,$5,clock_timestamp(),` + completedAt + `,$6)`
	if _, err := pool.Exec(context.Background(), query,
		operationID, deploymentRouteStep, strings.Repeat("a", 64), state, execution.ExecutionID(), errorText); err != nil {
		t.Fatal(err)
	}
}

func insertRunningRouteStep(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID) {
	t.Helper()
	insertRouteStep(t, pool, operationID, "running", "")
}

func insertFailedRouteStep(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID) {
	t.Helper()
	insertRouteStep(t, pool, operationID, "failed", "route writer unavailable")
}

func insertCompletedStep(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID, step, hash string) {
	t.Helper()
	execution := engine.Execution{OperationID: operationID, Attempt: 1, Step: step}
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_operation_steps
        (operation_id,step,inputs_hash,state,attempt,execution_id,started_at,completed_at)
        VALUES($1,$2,$3,'completed',1,$4,clock_timestamp(),clock_timestamp())`,
		operationID, step, hash, execution.ExecutionID()); err != nil {
		t.Fatal(err)
	}
}

func deploymentForOperation(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM eregion_platform_deployments WHERE operation_id=$1`, operationID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type deploymentRetryFixture struct {
	pool          *pgxpool.Pool
	dsn           string
	service       *Service
	ctx           context.Context
	run           func(contract.Operation)
	createRelease func(key, image string) platform.ReleaseID
}

func newDeploymentRetryFixture(t *testing.T) *deploymentRetryFixture {
	t.Helper()
	return newDeploymentRetryFixtureWithTransport(t, newScriptedDeployTransport())
}

func newDeploymentRetryFixtureWithTransport(t *testing.T, transport node.Transport) *deploymentRetryFixture {
	t.Helper()
	pool, dsn := testPool(t, true)
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: transport, NodeStateDir: t.TempDir(),
		LocalNodeName: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.routeSync = func(context.Context) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	ctx := context.Background()
	runOperation := func(op contract.Operation) {
		t.Helper()
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatalf("%s attempt: %v", op.Kind, err)
		}
		platformAwait(t, s, op, contract.Succeeded)
	}
	runOperation(platformSubmit(t, s, platform.KindNodeRegister, "platform", "retry-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	}))
	runOperation(platformSubmit(t, s, platform.KindAppCreate, "platform", "retry-app", map[string]any{"name": "live"}))
	runOperation(platformSubmit(t, s, platform.KindEnvironmentCreate, "app/live", "retry-env", map[string]any{
		"name": "production", "node": "local", "container_port": 80,
		"health": map[string]any{"timeout_seconds": 2},
		"domain": "retry.example.com", "domain_primary": true,
	}))
	return &deploymentRetryFixture{
		pool: pool, dsn: dsn, service: s, ctx: ctx, run: runOperation,
		createRelease: func(key, image string) platform.ReleaseID {
			runOperation(platformSubmit(t, s, platform.KindReleaseCreate, "env/live/production", key, map[string]any{
				"source": map[string]any{"type": "image", "image": image},
			}))
			return platform.ReleaseID(latestRelease(t, pool))
		},
	}
}

func (f *deploymentRetryFixture) environment(t *testing.T) platform.Environment {
	t.Helper()
	env, err := f.service.environment(f.ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (f *deploymentRetryFixture) serving(t *testing.T) (platform.ReleaseID, platform.DeploymentID) {
	t.Helper()
	env := f.environment(t)
	return env.ServingRelease, env.ServingDeploymentID
}

func (f *deploymentRetryFixture) deploy(t *testing.T) platform.ReleaseID {
	t.Helper()
	release := f.createRelease("deploy-"+fmt.Sprint(time.Now().UnixNano()), "registry.example/live:base")
	f.run(platformSubmit(t, f.service, platform.KindDeploy, "env/live/production", "deploy-"+string(release), map[string]any{"release_id": release}))
	return release
}

func insertDeploymentOwner(t *testing.T, pool *pgxpool.Pool, env platform.Environment, owner deploymentOwner) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_platform_deployment_owners
        (environment_id,operation_id,deployment_id,owner_attempt,phase,route_target_deployment_id,restore_release_id,restore_deployment_id)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		env.ID, nullableOperationID(owner.OperationID), nullableDeploymentID(owner.DeploymentID), owner.Attempt,
		string(owner.Phase), nullableDeploymentID(owner.RouteTarget),
		nullableReleaseID(owner.RestoreReleaseID), nullableDeploymentID(owner.RestoreDeploymentID)); err != nil {
		t.Fatal(err)
	}
}

func (f *deploymentRetryFixture) prepareOwnedCandidate(t *testing.T, key, image string, routeState string) (contract.Operation, platform.DeploymentID, platform.Environment, deploymentOwner) {
	t.Helper()
	release := f.createRelease(key, image)
	op := platformSubmit(t, f.service, platform.KindDeploy, "env/live/production", key, map[string]any{"release_id": release})
	depID := platform.DeploymentID(deploymentForOperation(t, f.pool, op.ID))
	env := f.environment(t)
	name := key + "-candidate"
	if _, err := f.pool.Exec(f.ctx, `UPDATE eregion_platform_deployments
        SET state='running',instances=$2 WHERE id=$1`,
		depID, fmt.Sprintf(`[{"name":%q,"process":"web","kind":"web","replica":"1","state":"running"}]`, name)); err != nil {
		t.Fatal(err)
	}
	switch routeState {
	case "running":
		insertRunningRouteStep(t, f.pool, op.ID)
	case "failed":
		insertFailedRouteStep(t, f.pool, op.ID)
	case "completed":
		insertRouteStep(t, f.pool, op.ID, "completed", "")
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	owner := deploymentOwner{
		EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: depID, Attempt: 1,
		Phase: deploymentOwnerSwitching, RouteTarget: depID,
		RestoreReleaseID: env.ServingRelease, RestoreDeploymentID: env.ServingDeploymentID,
	}
	insertDeploymentOwner(t, f.pool, env, owner)
	return op, depID, env, owner
}

func instanceNames(instances []platform.DeploymentInstance) []string {
	out := make([]string, 0, len(instances))
	for _, instance := range instances {
		out = append(out, instance.Name)
	}
	return out
}

func routeNames(t *testing.T, fixture *deploymentRetryFixture) []string {
	t.Helper()
	instances, err := routeInstancesForTest(fixture.ctx, fixture.service, "retry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return instanceNames(instances)
}

func ownerForFixture(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment) (deploymentOwner, bool) {
	t.Helper()
	owner, has, err := fixture.service.deploymentOwnerByEnvironment(fixture.ctx, fixture.pool, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	return owner, has
}

type recordingDeployTransport struct {
	*scriptedDeployTransport
	events []string
}

func (r *recordingDeployTransport) Call(ctx context.Context, request node.Request, onEvent func(node.Event)) (node.Response, error) {
	if request.Method == node.MethodRun {
		r.events = append(r.events, "run:"+request.Execution.Step)
	}
	return r.scriptedDeployTransport.Call(ctx, request, onEvent)
}

func (r *recordingDeployTransport) eventIndex(prefix string) int {
	for index, event := range r.events {
		if strings.HasPrefix(event, prefix) {
			return index
		}
	}
	return -1
}

// healthFaultTransport fails only the health node step while every other step
// behaves like the normal scripted node. Clearing failure lets the same
// operation retry against the already-created containers.
type healthFaultTransport struct {
	*recordingDeployTransport
	failure error
}

func (h *healthFaultTransport) Call(ctx context.Context, request node.Request, onEvent func(node.Event)) (node.Response, error) {
	if h.failure != nil && request.Method == node.MethodRun && strings.HasPrefix(request.Execution.Step, "platform-health-") {
		return node.Response{}, h.failure
	}
	return h.recordingDeployTransport.Call(ctx, request, onEvent)
}

func TestIntegrationDeploymentOwnerRouteSelectionIsNotCreationOrder(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)

	olderRelease := fixture.createRelease("owner-order-c", "registry.example/live:c")
	older := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "owner-order-c", map[string]any{"release_id": olderRelease})
	olderDeployment := platform.DeploymentID(deploymentForOperation(t, fixture.pool, older.ID))
	newerRelease := fixture.createRelease("owner-order-d", "registry.example/live:d")
	newer := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "owner-order-d", map[string]any{"release_id": newerRelease})
	newerDeployment := platform.DeploymentID(deploymentForOperation(t, fixture.pool, newer.ID))

	for name, id := range map[string]platform.DeploymentID{
		"owner-order-c-candidate": olderDeployment,
		"owner-order-d-candidate": newerDeployment,
	} {
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
            SET state='running',instances=$2 WHERE id=$1`,
			id, fmt.Sprintf(`[{"name":%q,"process":"web","kind":"web","replica":"1","state":"running"}]`, name)); err != nil {
			t.Fatal(err)
		}
	}
	insertRunningRouteStep(t, fixture.pool, older.ID)
	insertRunningRouteStep(t, fixture.pool, newer.ID)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=ANY($1)`,
		[]string{string(older.ID), string(newer.ID)}); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
		EnvironmentID: env.ID, OperationID: older.ID, DeploymentID: olderDeployment, Attempt: 1,
		Phase: deploymentOwnerSwitching, RouteTarget: olderDeployment,
		RestoreReleaseID: env.ServingRelease, RestoreDeploymentID: env.ServingDeploymentID,
	})

	if got := routeNames(t, fixture); !reflect.DeepEqual(got, []string{"owner-order-c-candidate"}) {
		t.Fatalf("route selected %v, want the durable owner candidate", got)
	}
	err := runPlatformAttempt(fixture.ctx, fixture.service, newer.ID, 1)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("newer operation error = %v, want JobSnooze", err)
	}
	if got := routeNames(t, fixture); !reflect.DeepEqual(got, []string{"owner-order-c-candidate"}) {
		t.Fatalf("deferred operation changed the route to %v", got)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("deferred newer operation became terminal: %+v", current)
	}
}

func TestIntegrationDeploymentInterruptionAfterApplyBeforeAckRestoresExactTarget(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	servingRelease, servingDeployment := env.ServingRelease, env.ServingDeploymentID
	servingInstances, err := routeInstancesForTest(fixture.ctx, fixture.service, "retry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "interrupt-ack", "registry.example/live:interrupt", "running")
	if got := routeNames(t, fixture); !reflect.DeepEqual(got, []string{"interrupt-ack-candidate"}) {
		t.Fatalf("pre-recovery route = %v, want owner intent candidate", got)
	}
	transport.events = nil
	fixture.service.routeSync = func(ctx context.Context) error {
		selected, err := routeInstancesForTest(ctx, fixture.service, "retry.example.com")
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(selected, servingInstances) {
			return fmt.Errorf("restore wrote %+v, want exact serving %+v", selected, servingInstances)
		}
		transport.events = append(transport.events, "restore:ok")
		return nil
	}

	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("interrupted operation attempt: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Failed || final.ErrorCode != contract.Unavailable {
		t.Fatalf("interrupted operation = %+v, want failed/unavailable", final)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after safe terminal recovery: %+v", owner)
	}
	restoreIndex := transport.eventIndex("restore:")
	cleanupIndex := transport.eventIndex("run:" + cleanupStepID("interrupt-ack-candidate"))
	if restoreIndex < 0 || cleanupIndex < 0 || restoreIndex > cleanupIndex {
		t.Fatalf("restore/cleanup order events=%v", transport.events)
	}
	if got, want := routeNames(t, fixture), instanceNames(servingInstances); !reflect.DeepEqual(got, want) {
		// The owner is released, so the query now follows the serving pointer.
		t.Fatalf("route after release = %v, want previous serving route %v", got, want)
	}
	if release, deployment := fixture.serving(t); release != servingRelease || deployment != servingDeployment {
		t.Fatalf("serving after recovery = %s/%s, want %s/%s", release, deployment, servingRelease, servingDeployment)
	}
}

func TestIntegrationDeploymentCancellationRestoresBeforeCancel(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	servingRelease, servingDeployment := env.ServingRelease, env.ServingDeploymentID
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "cancel-route", "registry.example/live:cancel", "running")
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	transport.events = nil
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}

	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("cancel operation attempt: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Canceled {
		t.Fatalf("cancelled operation = %+v", final)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after cancellation: %+v", owner)
	}
	restoreIndex := transport.eventIndex("restore:")
	cleanupIndex := transport.eventIndex("run:" + cleanupStepID("cancel-route-candidate"))
	if restoreIndex < 0 || cleanupIndex < 0 || restoreIndex > cleanupIndex {
		t.Fatalf("cancel restore/cleanup order events=%v", transport.events)
	}
	if release, deployment := fixture.serving(t); release != servingRelease || deployment != servingDeployment {
		t.Fatalf("serving after cancellation = %s/%s", release, deployment)
	}
}

func TestIntegrationDeploymentFailedRestoreRetainsOwnerAndCandidates(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "failed-restore", "registry.example/live:failed-restore", "running")
	restoreFails := true
	fixture.service.routeSync = func(context.Context) error {
		if restoreFails {
			transport.events = append(transport.events, "restore:failed")
			return errors.New("route writer unavailable")
		}
		transport.events = append(transport.events, "restore:ok")
		return nil
	}

	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("attempt succeeded while the route restore failed")
	}
	owner, has := ownerForFixture(t, fixture, env)
	if !has || owner.Phase != deploymentOwnerRestoring || owner.RestoreDeploymentID != env.ServingDeploymentID {
		t.Fatalf("owner after failed restore = %+v, has=%t", owner, has)
	}
	if transport.eventIndex("run:"+cleanupStepID("failed-restore-candidate")) >= 0 {
		t.Fatalf("cleanup ran despite failed restore: %v", transport.events)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("operation terminalized while ownership was unresolved: %+v", current)
	}

	restoreFails = false
	transport.events = nil
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("restore retry: %v", err)
	}
	current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contract.Failed || current.ErrorCode != contract.Unavailable {
		t.Fatalf("restored operation = %+v, want failed/unavailable", current)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after successful restore: %+v", owner)
	}
	restoreIndex := transport.eventIndex("restore:")
	cleanupIndex := transport.eventIndex("run:" + cleanupStepID("failed-restore-candidate"))
	if restoreIndex < 0 || cleanupIndex < 0 || restoreIndex > cleanupIndex {
		t.Fatalf("restore retry order events=%v", transport.events)
	}
}

func TestIntegrationDeploymentStaleAttemptCannotChangeOwnerRoute(t *testing.T) {
	fixture := newDeploymentRetryFixture(t)
	fixture.deploy(t)
	env := fixture.environment(t)
	op, _, _, owner := fixture.prepareOwnedCandidate(t, "stale-owner", "registry.example/live:stale", "running")
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners SET owner_attempt=2 WHERE environment_id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET attempt=2 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	owner.Attempt = 2
	if _, err := fixture.service.setDeploymentOwnerRoute(fixture.ctx, owner, owner.RestoreDeploymentID, deploymentOwnerRestoring, 1); !errors.Is(err, errNotOwned) {
		t.Fatalf("stale owner update error = %v, want errNotOwned", err)
	}
	current, has, err := fixture.service.deploymentOwnerByEnvironment(fixture.ctx, fixture.pool, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !has || current.RouteTarget != owner.DeploymentID || current.Phase != deploymentOwnerSwitching {
		t.Fatalf("stale attempt changed owner: %+v", current)
	}
	dep, _, err := fixture.service.deploymentByOperation(fixture.ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := fixture.service.acquireDeploymentOwner(fixture.ctx, dep, env, 1); !errors.Is(err, errNotOwned) || acquired {
		t.Fatalf("stale acquire = acquired %t, err %v; want errNotOwned", acquired, err)
	}
}

func TestIntegrationDeploymentDifferentEnvironmentIsNotBlocked(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	_, _, _, _ = fixture.prepareOwnedCandidate(t, "env-owner", "registry.example/live:held", "running")

	fixture.run(platformSubmit(t, fixture.service, platform.KindEnvironmentCreate, "app/live", "staging-env", map[string]any{
		"name": "staging", "node": "local", "container_port": 80,
		"health": map[string]any{"timeout_seconds": 2},
		"domain": "staging.example.com", "domain_primary": true,
	}))
	fixture.run(platformSubmit(t, fixture.service, platform.KindReleaseCreate, "env/live/staging", "staging-release", map[string]any{
		"source": map[string]any{"type": "image", "image": "registry.example/live:staging"},
	}))
	stagingRelease := platform.ReleaseID(latestRelease(t, fixture.pool))
	fixture.run(platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/staging", "staging-deploy", map[string]any{"release_id": stagingRelease}))
	staging, err := fixture.service.environment(fixture.ctx, "live", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if staging.ServingRelease != stagingRelease {
		t.Fatalf("staging serving = %q, want %q", staging.ServingRelease, stagingRelease)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.DeploymentID == "" {
		t.Fatalf("production owner was lost while staging deployed: %+v", owner)
	}
}

func TestIntegrationContainerCleanupDefersServingAndOwnerTarget(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	serving := env.ServingDeploymentID
	_, targetDeployment, _, _ := fixture.prepareOwnedCandidate(t, "cleanup-owner", "registry.example/live:cleanup", "running")

	makeRow := func(name string, deployment platform.DeploymentID) containerCleanupRow {
		if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_container_cleanup
            (container_name,environment_id,node,deployment_id,reason)
            VALUES($1,$2,'local',$3,'candidate')`, name, env.ID, deployment); err != nil {
			t.Fatal(err)
		}
		return containerCleanupRow{ContainerName: name, EnvironmentID: env.ID, App: "live", Environment: "production", Node: "local", DeploymentID: deployment}
	}
	servingRow := makeRow("cleanup-serving", serving)
	ownerRow := makeRow("cleanup-owner-candidate", targetDeployment)
	transport.events = nil
	if err := fixture.service.retryContainerCleanup(fixture.ctx, servingRow); !errors.Is(err, errCleanupDeferred) {
		t.Fatalf("serving cleanup error = %v, want deferred", err)
	}
	if err := fixture.service.retryContainerCleanup(fixture.ctx, ownerRow); !errors.Is(err, errCleanupDeferred) {
		t.Fatalf("owner-target cleanup error = %v, want deferred", err)
	}
	if len(transport.events) != 0 {
		t.Fatalf("guarded cleanup ran: %v", transport.events)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM eregion_platform_deployment_owners WHERE environment_id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.retryContainerCleanup(fixture.ctx, ownerRow); err != nil {
		t.Fatalf("candidate cleanup after owner release: %v", err)
	}
	if transport.eventIndex("run:"+cleanupStepID("cleanup-owner-candidate")) < 0 {
		t.Fatalf("candidate cleanup did not run: %v", transport.events)
	}
	transport.events = nil
	if err := fixture.service.retryContainerCleanup(fixture.ctx, servingRow); !errors.Is(err, errCleanupDeferred) {
		t.Fatalf("serving cleanup after owner release = %v, want deferred", err)
	}
	if len(transport.events) != 0 {
		t.Fatalf("serving cleanup ran: %v", transport.events)
	}
}

func TestIntegrationDeploymentLegacyAmbiguousOwnerRecoversConservatively(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	firstRelease, firstDeployment := env.ServingRelease, env.ServingDeploymentID
	servingInstances, err := routeInstancesForTest(fixture.ctx, fixture.service, "retry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	var ops []contract.Operation
	for _, key := range []string{"legacy-c", "legacy-d"} {
		release := fixture.createRelease(key, "registry.example/live:"+key)
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key, map[string]any{"release_id": release})
		dep := deploymentForOperation(t, fixture.pool, op.ID)
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
            SET state='running',instances=$2 WHERE id=$1`,
			dep, fmt.Sprintf(`[{"name":%q,"process":"web","kind":"web","replica":"1","state":"running"}]`, key+"-candidate")); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_deployment_owners
        (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id)
        VALUES($1,'ambiguous',$2,$3,$4)`, env.ID, firstDeployment, firstRelease, firstDeployment); err != nil {
		t.Fatal(err)
	}
	if got, want := routeNames(t, fixture), instanceNames(servingInstances); !reflect.DeepEqual(got, want) {
		t.Fatalf("ambiguous owner routed %v, want previous serving %v", got, want)
	}
	if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("ambiguous owner remained: %+v", owner)
	}
	for _, op := range ops {
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != contract.Failed || current.ErrorCode != contract.Conflict {
			t.Fatalf("legacy operation %s = %+v, want failed/conflict", op.ID, current)
		}
	}
	release, deployment := fixture.serving(t)
	if release != firstRelease || deployment != firstDeployment {
		t.Fatalf("legacy recovery changed serving to %s/%s", release, deployment)
	}
}

func TestIntegrationDeploymentExhaustionRestoresBeforeTerminalFailure(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	op, _, env, _ := fixture.prepareOwnedCandidate(t, "exhausted", "registry.example/live:exhausted", "running")
	var jobID int64
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT job_id FROM eregion_operations WHERE id=$1`, op.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE river_job
        SET state='running',attempt=1,max_attempts=1,attempted_by=ARRAY['test-client']::text[]
        WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	transport.events = nil
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	requeued, discarded, err := fixture.service.recoverOrphanedPlatformJob(fixture.ctx, orphanedPlatformJob{
		ID: jobID, OperationID: op.ID, Attempt: 1, MaxAttempts: 1, LastClient: "test-client",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requeued || !discarded {
		t.Fatalf("orphan recovery requeued=%t discarded=%t, want discarded", requeued, discarded)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("exhaustion failed the operation before ownership recovery: %+v", current)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Failed {
		t.Fatalf("exhausted operation = %+v, want failed", final)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("exhaustion left owner: %+v", owner)
	}
	restoreIndex := transport.eventIndex("restore:")
	cleanupIndex := transport.eventIndex("run:" + cleanupStepID("exhausted-candidate"))
	if restoreIndex < 0 || cleanupIndex < 0 || restoreIndex > cleanupIndex {
		t.Fatalf("exhaustion restore/cleanup order events=%v", transport.events)
	}
}

func TestIntegrationDeploymentScaleSubmissionFencedByOwner(t *testing.T) {
	fixture := newDeploymentRetryFixture(t)
	fixture.deploy(t)
	env := fixture.environment(t)
	var servingOperation contract.OperationID
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT operation_id FROM eregion_platform_deployments WHERE id=$1`, env.ServingDeploymentID).Scan(&servingOperation); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
		EnvironmentID: env.ID, OperationID: servingOperation,
		DeploymentID: env.ServingDeploymentID, Attempt: 1, Phase: deploymentOwnerPrepared,
		RouteTarget: env.ServingDeploymentID, RestoreReleaseID: env.ServingRelease, RestoreDeploymentID: env.ServingDeploymentID,
	})
	_, err := fixture.service.Submit(fixture.ctx, platformSubmission(platform.KindProcessScale, "env/live/production", "scale-fenced", map[string]any{
		"process": "web", "replicas": 2,
	}))
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("scale submission error = %v, want conflict", err)
	}
}

func TestIntegrationDeploymentVerifiedCancellationRecoveryRestores(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	servingRelease, servingDeployment := env.ServingRelease, env.ServingDeploymentID
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "verified-cancel", "registry.example/live:verified-cancel", "completed")
	insertCompletedStep(t, fixture.pool, op.ID, deploymentPublicStep, strings.Repeat("b", 64))
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	transport.events = nil
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Canceled {
		t.Fatalf("verified cancellation recovery = %+v, want cancelled", final)
	}
	if got := promotionServingRelease(t, fixture.pool, "live", "production"); got != string(servingRelease) {
		t.Fatalf("serving = %q, want %q", got, servingRelease)
	}
	if release, deployment := fixture.serving(t); release != servingRelease || deployment != servingDeployment {
		t.Fatalf("serving pair = %s/%s, want %s/%s", release, deployment, servingRelease, servingDeployment)
	}
	restoreIndex := transport.eventIndex("restore:")
	cleanupIndex := transport.eventIndex("run:" + cleanupStepID("verified-cancel-candidate"))
	if restoreIndex < 0 || cleanupIndex < 0 || restoreIndex > cleanupIndex {
		t.Fatalf("verified cancellation restore/cleanup order events=%v", transport.events)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after verified cancellation: %+v", owner)
	}
}

func TestIntegrationDeploymentCancellationDuringPublicVerificationRestores(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	servingRelease, servingDeployment := env.ServingRelease, env.ServingDeploymentID
	release := fixture.createRelease("public-cancel", "registry.example/live:public-cancel")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "public-cancel", map[string]any{"release_id": release})
	fixture.service.publicVerify = func(ctx context.Context, _ platform.Environment, _ string) error {
		_, err := fixture.pool.Exec(ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID)
		return err
	}
	transport.events = nil
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("public-cancel attempt: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Canceled {
		t.Fatalf("public cancellation = %+v, want cancelled", final)
	}
	if release, deployment := fixture.serving(t); release != servingRelease || deployment != servingDeployment {
		t.Fatalf("serving after public cancellation = %s/%s, want %s/%s", release, deployment, servingRelease, servingDeployment)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after public cancellation: %+v", owner)
	}
	restoreIndex := transport.eventIndex("restore:")
	if restoreIndex < 0 {
		t.Fatalf("public cancellation did not restore: %v", transport.events)
	}
}

func TestIntegrationDeploymentCancellationAtFinalizeRestoresBeforeCleanup(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	servingRelease, servingDeployment := env.ServingRelease, env.ServingDeploymentID
	previous, err := fixture.service.deploymentByID(fixture.ctx, servingDeployment)
	if err != nil {
		t.Fatal(err)
	}
	release := fixture.createRelease("finalize-cancel", "registry.example/live:finalize-cancel")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "finalize-cancel", map[string]any{"release_id": release})
	fixture.service.beforeDeploymentFinalize = func() {
		_, _ = fixture.pool.Exec(context.Background(), `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID)
	}
	transport.events = nil
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("finalize-cancel attempt: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Canceled {
		t.Fatalf("finalize cancellation = %+v, want cancelled", final)
	}
	if release, deployment := fixture.serving(t); release != servingRelease || deployment != servingDeployment {
		t.Fatalf("serving after finalize cancellation = %s/%s, want %s/%s", release, deployment, servingRelease, servingDeployment)
	}
	for _, instance := range previous.Instances {
		if transport.eventIndex("run:"+cleanupStepID(instance.Name)) >= 0 {
			t.Fatalf("previous serving container %s was cleaned before final cancellation: %v", instance.Name, transport.events)
		}
	}
	restoreIndex := transport.eventIndex("restore:")
	if restoreIndex < 0 {
		t.Fatalf("finalize cancellation did not restore: %v", transport.events)
	}
}

func TestIntegrationDeploymentSuccessfulCleanupUsesCurrentServing(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	previous, err := fixture.service.deploymentByID(fixture.ctx, env.ServingDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	previousNames := make([]string, 0, len(previous.Instances))
	for _, instance := range previous.Instances {
		previousNames = append(previousNames, instance.Name)
	}
	transport.events = nil
	fixture.deploy(t)
	serving := fixture.environment(t).ServingDeploymentID
	if serving == previous.ID {
		t.Fatal("new deployment did not become serving")
	}
	for _, name := range previousNames {
		if transport.eventIndex("run:"+cleanupStepID(name)) >= 0 {
			continue
		}
		var rows int
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
            WHERE container_name=$1 AND deployment_id=$2 AND reason='previous'`,
			name, previous.ID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("previous container %s was neither removed nor durably queued; events=%v", name, transport.events)
		}
	}
}

func TestIntegrationDeploymentCleanupGuardUsesCurrentServing(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	staleEnv := fixture.environment(t)
	previous, err := fixture.service.deploymentByID(fixture.ctx, staleEnv.ServingDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.deploy(t)
	protected, err := fixture.service.deploymentCleanupProtected(fixture.ctx, staleEnv, previous.ID)
	if err != nil {
		t.Fatal(err)
	}
	if protected {
		t.Fatalf("stale environment snapshot still protected previous deployment %s", previous.ID)
	}
	for _, instance := range previous.Instances {
		row := containerCleanupRow{
			ContainerName: instance.Name, EnvironmentID: staleEnv.ID,
			App: "live", Environment: "production", Node: staleEnv.Node,
			DeploymentID: previous.ID, Reason: "previous", Attempts: 1, CreatedAt: time.Now().UTC(),
		}
		if err := fixture.service.retryContainerCleanup(fixture.ctx, row); err != nil {
			t.Fatalf("maintenance cleanup of %s: %v", instance.Name, err)
		}
		if transport.eventIndex("run:"+cleanupStepID(instance.Name)) < 0 {
			t.Fatalf("stale environment snapshot skipped cleanup of %s; events=%v", instance.Name, transport.events)
		}
	}
}

func TestIntegrationDeploymentServingCleanupIsDurable(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	serving, err := fixture.service.deploymentByID(fixture.ctx, env.ServingDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	transport.events = nil
	if err := fixture.service.removeDeploymentContainers(fixture.ctx, transport, serving.OperationID, 1, env,
		serving.ID, env.Node, "previous", serving.Instances); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE environment_id=$1`, env.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(serving.Instances) {
		t.Fatalf("durable cleanup rows = %d, want %d", rows, len(serving.Instances))
	}
	if len(transport.events) != 0 {
		t.Fatalf("serving cleanup ran: %v", transport.events)
	}
}

func TestIntegrationDeploymentCleanupRecordFailureRetainsOwnership(t *testing.T) {
	for name, ambiguous := range map[string]bool{"terminal": false, "ambiguous": true} {
		t.Run(name, func(t *testing.T) {
			transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
			fixture := newDeploymentRetryFixtureWithTransport(t, transport)
			fixture.deploy(t)
			env := fixture.environment(t)
			fixture.service.routeSync = func(context.Context) error {
				transport.events = append(transport.events, "restore:ok")
				return nil
			}
			if !ambiguous {
				op, _, _, _ := fixture.prepareOwnedCandidate(t, "record-failure", "registry.example/live:record-failure", "running")
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='failed' WHERE id=$1`, op.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				var ops []contract.Operation
				for _, key := range []string{"ambiguous-a", "ambiguous-b"} {
					release := fixture.createRelease(key, "registry.example/live:"+key)
					op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key, map[string]any{"release_id": release})
					dep := deploymentForOperation(t, fixture.pool, op.ID)
					if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
                        SET state='running',instances=$2 WHERE id=$1`,
						dep, fmt.Sprintf(`[{"name":%q,"process":"web","kind":"web","replica":"1","state":"running"}]`, key+"-candidate")); err != nil {
						t.Fatal(err)
					}
					ops = append(ops, op)
				}
				if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_deployment_owners
                    (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id)
                    VALUES($1,'ambiguous',$2,$3,$4)`, env.ID, env.ServingDeploymentID, env.ServingRelease, env.ServingDeploymentID); err != nil {
					t.Fatal(err)
				}
				defer func() {
					for _, op := range ops {
						current, _ := fixture.service.Get(fixture.ctx, testActor, op.ID)
						if current.State.Terminal() {
							t.Fatalf("ambiguous operation %s became terminal before cleanup was durable", op.ID)
						}
					}
				}()
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `DROP TABLE eregion_platform_container_cleanup`); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err == nil {
				t.Fatal("owner reconciliation succeeded while cleanup rows could not be recorded")
			}
			if owner, has := ownerForFixture(t, fixture, env); !has {
				t.Fatalf("ownership was released despite cleanup record failure: %+v", owner)
			}
		})
	}
}

func TestIntegrationContainerCleanupDeferredRowsDoNotStarveQueue(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	for index := 0; index < containerCleanupPageSize; index++ {
		name := fmt.Sprintf("aaa-protected-%02d", index)
		if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_container_cleanup
            (container_name,environment_id,node,deployment_id,reason,attempts,next_attempt_at)
            VALUES($1,$2,'local',$3,'previous',1,clock_timestamp()-interval '1 hour')`,
			name, env.ID, env.ServingDeploymentID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_container_cleanup
        (container_name,environment_id,node,deployment_id,reason,attempts,next_attempt_at)
        VALUES('zzz-unprotected',$1,'local',NULL,'previous',1,clock_timestamp()-interval '1 hour')`, env.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var future bool
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT attempts,next_attempt_at > clock_timestamp()
        FROM eregion_platform_container_cleanup WHERE container_name='aaa-protected-00'`).Scan(&attempts, &future); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !future {
		t.Fatalf("deferred protected row attempts=%d future=%t, want 1/true", attempts, future)
	}
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE container_name='zzz-unprotected'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("unprotected cleanup row was starved by protected rows")
	}
	if transport.eventIndex("run:"+cleanupStepID("zzz-unprotected")) < 0 {
		t.Fatalf("unprotected cleanup did not run: %v", transport.events)
	}
}

func TestIntegrationScaleAdmissionContentionIsNonblocking(t *testing.T) {
	fixture := newDeploymentRetryFixture(t)
	fixture.deploy(t)
	env := fixture.environment(t)
	conn, err := pgx.Connect(fixture.ctx, fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	key := targetLockKey("env/live/production")
	if _, err := conn.Exec(fixture.ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) }()

	const contenders = 16
	var wg sync.WaitGroup
	errs := make(chan error, contenders)
	for index := 0; index < contenders; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := fixture.service.Submit(ctx, platformSubmission(platform.KindProcessScale, "env/live/production",
				fmt.Sprintf("scale-contention-%d", index), map[string]any{"process": "web", "replicas": 2}))
			errs <- err
		}(index)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if contract.Classify(err).Code != contract.Conflict {
			t.Fatalf("scale contention error = %v, want conflict", err)
		}
	}
	var scaleOps int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_operations
        WHERE kind=$1 AND target='env/live/production'`, platform.KindProcessScale).Scan(&scaleOps); err != nil {
		t.Fatal(err)
	}
	if scaleOps != 0 {
		t.Fatalf("contended scale submissions created %d operations", scaleOps)
	}
	_ = env
}

func TestIntegrationDeployWorkerLockContentionSnoozes(t *testing.T) {
	fixture := newDeploymentRetryFixture(t)
	fixture.deploy(t)
	release := fixture.environment(t).ServingRelease
	const contenders = 14
	var ops []contract.Operation
	for index := 0; index < contenders; index++ {
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production",
			fmt.Sprintf("worker-contention-%d", index), map[string]any{"release_id": release})
		ops = append(ops, op)
	}
	conn, err := pgx.Connect(fixture.ctx, fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	key := targetLockKey("env/live/production")
	if _, err := conn.Exec(fixture.ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) }()

	var wg sync.WaitGroup
	errs := make(chan error, len(ops))
	for _, op := range ops {
		wg.Add(1)
		go func(op contract.Operation) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errs <- runPlatformAttempt(ctx, fixture.service, op.ID, 1)
		}(op)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var snooze *river.JobSnoozeError
		if !errors.As(err, &snooze) {
			t.Fatalf("worker contention error = %v, want JobSnooze", err)
		}
	}
}

func TestIntegrationDeploymentCleanupProtectsFrozenRestoreTarget(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	previous, err := fixture.service.deploymentByID(fixture.ctx, env.ServingDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	op, candidateDeployment, _, owner := fixture.prepareOwnedCandidate(t, "restore-protect", "registry.example/live:restore-protect", "running")
	// Simulate the pre-finalization state: the candidate is recorded serving,
	// while the owner still freezes A as its restore target.
	var candidateRelease string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT release_id FROM eregion_platform_deployments WHERE id=$1`, candidateDeployment).Scan(&candidateRelease); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environments
        SET serving_release_id=$2,serving_deployment_id=$3 WHERE id=$1`, env.ID, candidateRelease, candidateDeployment); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
        SET phase='serving',route_target_deployment_id=$2 WHERE environment_id=$1`, env.ID, candidateDeployment); err != nil {
		t.Fatal(err)
	}
	protected, err := fixture.service.deploymentCleanupProtected(fixture.ctx, env, previous.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !protected {
		t.Fatalf("frozen restore target %s was not protected", previous.ID)
	}
	transport.events = nil
	row := containerCleanupRow{
		ContainerName: previous.Instances[0].Name, EnvironmentID: env.ID,
		App: "live", Environment: "production", Node: env.Node,
		DeploymentID: previous.ID, Reason: "previous", Attempts: 1, CreatedAt: time.Now().UTC(),
	}
	if err := fixture.service.retryContainerCleanup(fixture.ctx, row); !errors.Is(err, errCleanupDeferred) {
		t.Fatalf("restore-target cleanup error = %v, want deferred", err)
	}
	if len(transport.events) != 0 {
		t.Fatalf("restore target was removed: %v", transport.events)
	}
	// Cancellation recovery must still find the predecessor it is about to
	// restore.
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Canceled {
		t.Fatalf("cancellation recovery = %+v, want cancelled", final)
	}
	if release, deployment := fixture.serving(t); deployment != previous.ID {
		t.Fatalf("restored serving = %s/%s, want deployment %s", release, deployment, previous.ID)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after restored cancellation: %+v", owner)
	}
	_ = owner
}

func TestIntegrationDeploymentPreRecordFailureRetainsOwner(t *testing.T) {
	for name, failLookup := range map[string]bool{"insert": false, "lookup": true} {
		t.Run(name, func(t *testing.T) {
			transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
			fixture := newDeploymentRetryFixtureWithTransport(t, transport)
			fixture.deploy(t)
			env := fixture.environment(t)
			previous, err := fixture.service.deploymentByID(fixture.ctx, env.ServingDeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			var previousInstances []byte
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT instances FROM eregion_platform_deployments WHERE id=$1`, previous.ID).Scan(&previousInstances); err != nil {
				t.Fatal(err)
			}
			release := fixture.createRelease("pre-record", "registry.example/live:pre-record")
			op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "pre-record", map[string]any{"release_id": release})
			if failLookup {
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET instances='{"invalid":true}'::jsonb WHERE id=$1`, previous.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := fixture.pool.Exec(fixture.ctx, fmt.Sprintf(`ALTER TABLE eregion_platform_container_cleanup
                    ADD CONSTRAINT pre_record_failure CHECK (container_name <> '%s')`, previous.Instances[0].Name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
				t.Fatal("deployment succeeded without durable previous cleanup")
			}
			current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State.Terminal() {
				t.Fatalf("operation terminalized without durable cleanup: %+v", current)
			}
			owner, has := ownerForFixture(t, fixture, env)
			if !has {
				t.Fatal("owner was released without durable cleanup")
			}
			if owner.Phase != deploymentOwnerServing {
				t.Fatalf("owner phase = %s, want serving", owner.Phase)
			}
			if failLookup {
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET instances=$2 WHERE id=$1`, previous.ID, previousInstances); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_container_cleanup
                    DROP CONSTRAINT pre_record_failure`); err != nil {
					t.Fatal(err)
				}
			}
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
				t.Fatalf("retry after pre-record repair: %v", err)
			}
			current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != contract.Succeeded {
				t.Fatalf("repaired deployment = %+v, want succeeded", current)
			}
			if _, has := ownerForFixture(t, fixture, env); has {
				t.Fatal("owner remained after successful finalization")
			}
		})
	}
}

func TestIntegrationTerminalOwnerCleanupUsesPostRestoreState(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
	op, candidateDeployment, _, _ := fixture.prepareOwnedCandidate(t, "post-restore", "registry.example/live:post-restore", "running")
	var candidateRelease string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT release_id FROM eregion_platform_deployments WHERE id=$1`, candidateDeployment).Scan(&candidateRelease); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environments
        SET serving_release_id=$2,serving_deployment_id=$3 WHERE id=$1`, env.ID, candidateRelease, candidateDeployment); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='failed' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if release, deployment := fixture.serving(t); release != previousRelease || deployment != previousDeployment {
		t.Fatalf("serving after terminal sweep = %s/%s, want %s/%s", release, deployment, previousRelease, previousDeployment)
	}
	var rows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, candidateDeployment).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("terminal recovery skipped candidate cleanup after restoring serving")
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("terminal owner remained: %+v", owner)
	}
}

func TestIntegrationDeploymentPostSwitchNonretryableSyncFailureRestores(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	servingRelease, servingDeployment := env.ServingRelease, env.ServingDeploymentID
	var switches int
	fixture.service.routeSwitch = func(context.Context, platform.Environment, []string) error {
		switches++
		if switches == 2 {
			return contract.NewFault(contract.PreconditionFailed, "post-switch resync failed", "test")
		}
		return nil
	}
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	release := fixture.createRelease("post-sync", "registry.example/live:post-sync")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "post-sync", map[string]any{"release_id": release})
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("post-switch sync attempt: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Failed || final.ErrorCode != contract.PreconditionFailed {
		t.Fatalf("post-switch operation = %+v, want failed/precondition", final)
	}
	if release, deployment := fixture.serving(t); release != servingRelease || deployment != servingDeployment {
		t.Fatalf("serving after post-switch failure = %s/%s, want %s/%s", release, deployment, servingRelease, servingDeployment)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after restored failure: %+v", owner)
	}
	if transport.eventIndex("restore:") < 0 {
		t.Fatalf("post-switch failure did not restore: %v", transport.events)
	}
}

func TestIntegrationDeploymentPostSwitchRestoreFailureRetainsOwner(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	var switches int
	fixture.service.routeSwitch = func(context.Context, platform.Environment, []string) error {
		switches++
		if switches == 2 {
			return contract.NewFault(contract.PreconditionFailed, "post-switch resync failed", "test")
		}
		return nil
	}
	fixture.service.routeSync = func(context.Context) error {
		return contract.NewFault(contract.Unavailable, "restore writer unavailable", "test")
	}
	release := fixture.createRelease("post-sync-fail", "registry.example/live:post-sync-fail")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "post-sync-fail", map[string]any{"release_id": release})
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("attempt succeeded while post-switch restoration failed")
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("operation terminalized with unresolved post-switch restore: %+v", current)
	}
	owner, has := ownerForFixture(t, fixture, env)
	if !has || owner.Phase != deploymentOwnerRestoring || owner.RestoreDeploymentID != env.ServingDeploymentID {
		t.Fatalf("owner after post-switch restore failure = %+v, has=%t", owner, has)
	}
}

func TestIntegrationDeploymentPostSwitchRetryableSyncFailureRetainsOwner(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	var switches int
	fixture.service.routeSwitch = func(context.Context, platform.Environment, []string) error {
		switches++
		if switches == 2 {
			return contract.NewFault(contract.Unavailable, "post-switch resync unavailable", "test")
		}
		return nil
	}
	release := fixture.createRelease("post-sync-retry", "registry.example/live:post-sync-retry")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "post-sync-retry", map[string]any{"release_id": release})
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("retryable post-switch sync failure was not returned")
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("retryable post-switch failure terminalized: %+v", current)
	}
	owner, has := ownerForFixture(t, fixture, env)
	if !has || owner.Phase != deploymentOwnerServing {
		t.Fatalf("owner after retryable post-switch failure = %+v, has=%t", owner, has)
	}
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("post-switch retry: %v", err)
	}
	current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contract.Succeeded {
		t.Fatalf("retried post-switch deployment = %+v, want succeeded", current)
	}
	if _, has := ownerForFixture(t, fixture, env); has {
		t.Fatal("owner remained after successful retry")
	}
}

func TestIntegrationAmbiguousCleanupUsesPostRestoreState(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
	var candidateDeployment platform.DeploymentID
	var candidateRelease platform.ReleaseID
	for index, key := range []string{"ambig-state-a", "ambig-state-b"} {
		release := fixture.createRelease(key, "registry.example/live:"+key)
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key, map[string]any{"release_id": release})
		dep := deploymentForOperation(t, fixture.pool, op.ID)
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
            SET state='running',instances=$2 WHERE id=$1`,
			dep, fmt.Sprintf(`[{"name":%q,"process":"web","kind":"web","replica":"1","state":"running"}]`, key+"-candidate")); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			candidateDeployment = platform.DeploymentID(dep)
			candidateRelease = release
		}
	}
	// Simulate a stale environment snapshot that already names the first
	// candidate. Recovery must record cleanup based on the post-restore target.
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environments
        SET serving_release_id=$2,serving_deployment_id=$3 WHERE id=$1`, env.ID, candidateRelease, candidateDeployment); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_deployment_owners
        (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id)
        VALUES($1,'ambiguous',$2,$3,$4)`, env.ID, previousDeployment, previousRelease, previousDeployment); err != nil {
		t.Fatal(err)
	}
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if release, deployment := fixture.serving(t); release != previousRelease || deployment != previousDeployment {
		t.Fatalf("serving after ambiguous recovery = %s/%s, want %s/%s", release, deployment, previousRelease, previousDeployment)
	}
	var rows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, candidateDeployment).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("ambiguous recovery skipped the stale-serving candidate cleanup")
	}
}

func TestIntegrationDeploymentRestorePhaseAuthoritative(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "restore-phase", "registry.example/live:restore-phase", "completed")
	insertCompletedStep(t, fixture.pool, op.ID, deploymentPublicStep, strings.Repeat("b", 64))
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
        SET phase='restoring',route_target_deployment_id=$2 WHERE environment_id=$1`, env.ID, previousDeployment); err != nil {
		t.Fatal(err)
	}
	transport.events = nil
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State == contract.Succeeded {
		t.Fatalf("restoring owner was finalized as success: %+v", final)
	}
	if final.State != contract.Failed {
		t.Fatalf("restoring owner recovery = %+v, want failed", final)
	}
	if release, deployment := fixture.serving(t); release != previousRelease || deployment != previousDeployment {
		t.Fatalf("serving after restore-phase recovery = %s/%s, want %s/%s", release, deployment, previousRelease, previousDeployment)
	}
	var restoreStep string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM eregion_operation_steps
        WHERE operation_id=$1 AND step=$2`, op.ID, deploymentRestoreRouteStep).Scan(&restoreStep); err != nil {
		t.Fatal(err)
	}
	if restoreStep != "completed" {
		t.Fatalf("restore step = %q, want completed", restoreStep)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after restored failure: %+v", owner)
	}
}

func TestIntegrationRecoveryPostSwitchSyncFailureUsesRestoration(t *testing.T) {
	for name, restoreFails := range map[string]bool{"restore succeeds": false, "restore fails": true} {
		t.Run(name, func(t *testing.T) {
			transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
			fixture := newDeploymentRetryFixtureWithTransport(t, transport)
			fixture.deploy(t)
			env := fixture.environment(t)
			previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
			op, _, _, _ := fixture.prepareOwnedCandidate(t, "recovery-sync", "registry.example/live:recovery-sync", "completed")
			insertCompletedStep(t, fixture.pool, op.ID, deploymentPublicStep, strings.Repeat("b", 64))
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
                SET phase='serving',route_target_deployment_id=$2 WHERE environment_id=$1`, env.ID, deploymentForOperation(t, fixture.pool, op.ID)); err != nil {
				t.Fatal(err)
			}
			calls := 0
			fixture.service.routeSync = func(context.Context) error {
				calls++
				if calls == 1 {
					return contract.NewFault(contract.PreconditionFailed, "recovery resync failed", "test")
				}
				if restoreFails {
					return contract.NewFault(contract.Unavailable, "restore writer unavailable", "test")
				}
				transport.events = append(transport.events, "restore:ok")
				return nil
			}
			current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = fixture.service.reconcileDeployment(fixture.ctx, current)
			if restoreFails {
				if err == nil {
					t.Fatal("reconciliation succeeded while recovery restoration failed")
				}
				current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.State.Terminal() {
					t.Fatalf("operation terminalized before recovery restoration: %+v", current)
				}
				owner, has := ownerForFixture(t, fixture, env)
				if !has || owner.Phase != deploymentOwnerRestoring {
					t.Fatalf("owner after failed recovery restore = %+v, has=%t", owner, has)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if final.State != contract.Failed {
				t.Fatalf("recovery sync failure = %+v, want failed", final)
			}
			if release, deployment := fixture.serving(t); release != previousRelease || deployment != previousDeployment {
				t.Fatalf("serving after recovery sync failure = %s/%s, want %s/%s", release, deployment, previousRelease, previousDeployment)
			}
			if owner, has := ownerForFixture(t, fixture, env); has {
				t.Fatalf("owner remained after restored recovery failure: %+v", owner)
			}
		})
	}
}

func TestIntegrationCancellationCleanupIntentIsDurable(t *testing.T) {
	for name, blockInsert := range map[string]bool{"insert failure": true, "resolver unavailable": false} {
		t.Run(name, func(t *testing.T) {
			transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
			fixture := newDeploymentRetryFixtureWithTransport(t, transport)
			fixture.deploy(t)
			env := fixture.environment(t)
			op, dep, _, _ := fixture.prepareOwnedCandidate(t, "cancel-durable", "registry.example/live:cancel-durable", "running")
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
				t.Fatal(err)
			}
			candidateName := dep + "-candidate"
			_ = candidateName
			if blockInsert {
				if _, err := fixture.pool.Exec(fixture.ctx, fmt.Sprintf(`ALTER TABLE eregion_platform_container_cleanup
                    ADD CONSTRAINT cancel_cleanup_failure CHECK (container_name <> '%s')`, "cancel-durable-candidate")); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture.service.resolver = nil
			}
			fixture.service.routeSync = func(context.Context) error { return nil }
			current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if blockInsert {
				if err := fixture.service.reconcileDeployment(fixture.ctx, current); err == nil {
					t.Fatal("cancellation terminalized without durable cleanup intent")
				}
				current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.State.Terminal() {
					t.Fatalf("cancellation terminalized on cleanup insert failure: %+v", current)
				}
				if owner, has := ownerForFixture(t, fixture, env); !has {
					t.Fatalf("owner released on cleanup insert failure: %+v", owner)
				}
				if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_container_cleanup DROP CONSTRAINT cancel_cleanup_failure`); err != nil {
					t.Fatal(err)
				}
				current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
					t.Fatal(err)
				}
				var rows int
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
                    WHERE deployment_id=$1`, dep).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows == 0 {
					t.Fatal("cancellation cleanup intent was not durable when the resolver was unavailable")
				}
			}
			final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if final.State != contract.Canceled {
				t.Fatalf("cancellation = %+v, want cancelled", final)
			}
		})
	}
}

func TestIntegrationPreviousCleanupPinnedRevisionNode(t *testing.T) {
	t.Run("uses pinned node after environment move", func(t *testing.T) {
		transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		env := fixture.environment(t)
		previous, err := fixture.service.deploymentByID(fixture.ctx, env.ServingDeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		if previous.RevisionID == "" {
			t.Fatal("previous deployment has no pinned revision")
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions
            SET runtime=jsonb_set(runtime,'{node}','"old-node"') WHERE id=$1`, previous.RevisionID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		release := fixture.createRelease("pinned-node", "registry.example/live:pinned-node")
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "pinned-node", map[string]any{"release_id": release})
		runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1)
		var node string
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT node FROM eregion_platform_container_cleanup
            WHERE deployment_id=$1 LIMIT 1`, previous.ID).Scan(&node); err != nil {
			t.Fatal(err)
		}
		if node != "old-node" {
			t.Fatalf("cleanup node = %q, want pinned old-node", node)
		}
	})
	t.Run("lookup failure retains owner", func(t *testing.T) {
		transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		env := fixture.environment(t)
		previous, err := fixture.service.deploymentByID(fixture.ctx, env.ServingDeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		fixture.run(platformSubmit(t, fixture.service, platform.KindConfigSet, "env/live/production",
			"pinned-fail-config", map[string]any{"config": map[string]string{"PIN": "1"}}))
		release := fixture.createRelease("pinned-fail", "registry.example/live:pinned-fail")
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "pinned-fail", map[string]any{"release_id": release})
		var runtime []byte
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT runtime FROM eregion_platform_environment_revisions WHERE id=$1`, previous.RevisionID).Scan(&runtime); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions
            SET runtime='[]'::jsonb WHERE id=$1`, previous.RevisionID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
			t.Fatal("deployment succeeded with unreadable pinned previous revision")
		}
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State.Terminal() {
			t.Fatalf("operation terminalized on pinned revision lookup failure: %+v", current)
		}
		if owner, has := ownerForFixture(t, fixture, env); !has {
			t.Fatalf("owner released on pinned revision lookup failure: %+v", owner)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions SET runtime=$2 WHERE id=$1`, previous.RevisionID, runtime); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
			t.Fatalf("retry after pinned revision repair: %v", err)
		}
	})
}

func TestIntegrationAmbiguousDeploymentLookupFailureRetainsOwner(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
	var candidateDeployment platform.DeploymentID
	for index, key := range []string{"lookup-a", "lookup-b"} {
		release := fixture.createRelease(key, "registry.example/live:"+key)
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key, map[string]any{"release_id": release})
		dep := deploymentForOperation(t, fixture.pool, op.ID)
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
            SET state='running',instances=$2 WHERE id=$1`,
			dep, fmt.Sprintf(`[{"name":%q,"process":"web","kind":"web","replica":"1","state":"running"}]`, key+"-candidate")); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			candidateDeployment = platform.DeploymentID(dep)
		}
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
        SET instances='{"invalid":true}'::jsonb WHERE id=$1`, candidateDeployment); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_deployment_owners
        (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id)
        VALUES($1,'ambiguous',$2,$3,$4)`, env.ID, previousDeployment, previousRelease, previousDeployment); err != nil {
		t.Fatal(err)
	}
	fixture.service.routeSync = func(context.Context) error { return nil }
	if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err == nil {
		t.Fatal("ambiguous recovery succeeded with an unreadable deployment")
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.Phase != deploymentOwnerAmbiguous {
		t.Fatalf("ambiguous owner after lookup failure = %+v, has=%t", owner, has)
	}
	var active int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_operations o
        JOIN eregion_platform_deployments d ON d.operation_id=o.id
        WHERE d.environment_id=$1 AND o.state IN ('queued','running','cancelling')`, env.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Fatalf("active operations after lookup failure = %d, want 2", active)
	}
}

func blockContainerCleanupInserts(t *testing.T, pool *pgxpool.Pool) func() {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`ALTER TABLE eregion_platform_container_cleanup
         ADD CONSTRAINT live_cleanup_insert_block CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := pool.Exec(context.Background(),
			`ALTER TABLE eregion_platform_container_cleanup
             DROP CONSTRAINT IF EXISTS live_cleanup_insert_block`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestIntegrationDeploymentPreSaveCleanupReconstruction covers the interruption
// boundary between a started container and saveDeploymentInstances: container
// step evidence plus the pinned revision must reconstruct the cleanup names
// before cancellation or terminal failure settles ownership.
func TestIntegrationDeploymentPreSaveCleanupReconstruction(t *testing.T) {
	t.Run("cancellation uses planned names without a resolver", func(t *testing.T) {
		transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		env := fixture.environment(t)
		if env.Revision == nil {
			t.Fatal("environment has no current revision")
		}
		release := fixture.createRelease("pre-save-cancel", "registry.example/live:pre-save-cancel")
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "pre-save-cancel",
			map[string]any{"release_id": release})
		depID := platform.DeploymentID(deploymentForOperation(t, fixture.pool, op.ID))
		plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, release, depID)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan) == 0 {
			t.Fatal("candidate plan is empty")
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
            SET state='running',instances='[]'::jsonb,revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
			t.Fatal(err)
		}
		for _, planned := range plan {
			insertCompletedStep(t, fixture.pool, op.ID, containerStepID(planned.Process.Name, planned.Replica), strings.Repeat("c", 64))
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations
            SET state='cancelling',attempt=1 WHERE id=$1`, op.ID); err != nil {
			t.Fatal(err)
		}
		insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
			EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: depID, Attempt: 1,
			Phase: deploymentOwnerSwitching, RouteTarget: depID,
			RestoreReleaseID: env.ServingRelease, RestoreDeploymentID: env.ServingDeploymentID,
		})
		fixture.service.resolver = nil
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
			t.Fatal(err)
		}
		final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.State != contract.Canceled {
			t.Fatalf("cancellation = %+v, want cancelled", final)
		}
		if owner, has := ownerForFixture(t, fixture, env); has {
			t.Fatalf("owner remained after pre-save cancellation: %+v", owner)
		}
		for _, planned := range plan {
			var count int
			var nodeName string
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*),coalesce(min(node),'')
                FROM eregion_platform_container_cleanup
                WHERE deployment_id=$1 AND container_name=$2`, depID, planned.Name).Scan(&count, &nodeName); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("cleanup rows for pre-save container %s = %d, want 1", planned.Name, count)
			}
			if nodeName != env.Node {
				t.Fatalf("cleanup node for %s = %q, want %q", planned.Name, nodeName, env.Node)
			}
		}
	})

	t.Run("unresolvable pinned node retains ownership", func(t *testing.T) {
		transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		env := fixture.environment(t)
		if env.Revision == nil {
			t.Fatal("environment has no current revision")
		}
		release := fixture.createRelease("pre-save-node", "registry.example/live:pre-save-node")
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "pre-save-node",
			map[string]any{"release_id": release})
		depID := platform.DeploymentID(deploymentForOperation(t, fixture.pool, op.ID))
		plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, release, depID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
            SET state='running',instances='[]'::jsonb,revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
			t.Fatal(err)
		}
		for _, planned := range plan {
			insertCompletedStep(t, fixture.pool, op.ID, containerStepID(planned.Process.Name, planned.Replica), strings.Repeat("d", 64))
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions
            SET runtime='{}'::jsonb WHERE id=$1`, env.RevisionID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations
            SET state='cancelling',attempt=1 WHERE id=$1`, op.ID); err != nil {
			t.Fatal(err)
		}
		insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
			EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: depID, Attempt: 1,
			Phase: deploymentOwnerSwitching, RouteTarget: depID,
			RestoreReleaseID: env.ServingRelease, RestoreDeploymentID: env.ServingDeploymentID,
		})
		fixture.service.resolver = nil
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.service.reconcileDeployment(fixture.ctx, current); err == nil {
			t.Fatal("cancellation settled with an unresolvable pinned cleanup node")
		}
		current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State.Terminal() {
			t.Fatalf("cancellation terminalized with an unresolvable pinned cleanup node: %+v", current)
		}
		if owner, has := ownerForFixture(t, fixture, env); !has {
			t.Fatalf("owner released with an unresolvable pinned cleanup node: %+v", owner)
		}
		var count int
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
            WHERE deployment_id=$1`, depID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("cleanup rows were invented for an unresolvable pinned node: %d", count)
		}
	})
}

// TestIntegrationDeploymentRetryableFailureRetainsCandidates proves a retryable
// pre-switch failure does not remove containers whose completed step rows make
// the retry skip recreation.
func TestIntegrationDeploymentRetryableFailureRetainsCandidates(t *testing.T) {
	transport := &healthFaultTransport{
		recordingDeployTransport: &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()},
	}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	transport.failure = errors.New("health probe timed out")
	env := fixture.environment(t)
	if env.Revision == nil {
		t.Fatal("environment has no current revision")
	}
	previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
	release := fixture.createRelease("retry-retain", "registry.example/live:retry-retain")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "retry-retain",
		map[string]any{"release_id": release})
	depID := platform.DeploymentID(deploymentForOperation(t, fixture.pool, op.ID))
	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, release, depID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatal("candidate plan is empty")
	}
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("retryable pre-switch failure was not returned")
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("retryable pre-switch failure terminalized: %+v", current)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.Phase != deploymentOwnerPrepared {
		t.Fatalf("owner after retryable pre-switch failure = %+v, has=%t", owner, has)
	}
	for _, planned := range plan {
		step := containerStepID(planned.Process.Name, planned.Replica)
		if runs := transport.runCount(op.ID, step); runs != 1 {
			t.Fatalf("container step %s ran %d times before retry, want 1", step, runs)
		}
		var state string
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM eregion_operation_steps
            WHERE operation_id=$1 AND step=$2`, op.ID, step).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "completed" {
			t.Fatalf("container step %s after retryable failure = %q, want completed", step, state)
		}
	}
	if index := transport.eventIndex("run:platform-cleanup-"); index >= 0 {
		t.Fatalf("retryable pre-switch failure attempted cleanup: %v", transport.events)
	}
	var cleanupRows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, depID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("retryable pre-switch failure recorded %d cleanup rows, want 0", cleanupRows)
	}
	if releaseID, deploymentID := fixture.serving(t); releaseID != previousRelease || deploymentID != previousDeployment {
		t.Fatalf("serving changed on retryable failure = %s/%s, want %s/%s", releaseID, deploymentID, previousRelease, previousDeployment)
	}

	transport.failure = nil
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("retry after retryable failure: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Succeeded {
		t.Fatalf("retried deployment = %+v, want succeeded", final)
	}
	for _, planned := range plan {
		step := containerStepID(planned.Process.Name, planned.Replica)
		if runs := transport.runCount(op.ID, step); runs != 1 {
			t.Fatalf("container step %s ran %d times across retry, want 1", step, runs)
		}
	}
	if releaseID, deploymentID := fixture.serving(t); releaseID != release || deploymentID != depID {
		t.Fatalf("serving after retry = %s/%s, want %s/%s", releaseID, deploymentID, release, depID)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after successful retry: %+v", owner)
	}
}

// TestIntegrationLiveCleanupIntentFailureRetainsOwner covers both live terminal
// paths: a pre-switch terminal failure and a post-switch public failure must
// persist candidate cleanup intent before settling. An insert failure keeps the
// nonterminal operation and its owner.
func TestIntegrationLiveCleanupIntentFailureRetainsOwner(t *testing.T) {
	t.Run("pre-switch terminal failure", func(t *testing.T) {
		transport := &healthFaultTransport{
			recordingDeployTransport: &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()},
		}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		transport.failure = contract.NewFault(contract.PreconditionFailed, "health rejected", "test")
		env := fixture.environment(t)
		release := fixture.createRelease("live-cleanup-health", "registry.example/live:live-cleanup-health")
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "live-cleanup-health",
			map[string]any{"release_id": release})
		dropBlock := blockContainerCleanupInserts(t, fixture.pool)
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
			t.Fatal("terminal failure settled without durable cleanup intent")
		}
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State.Terminal() {
			t.Fatalf("terminal failure settled without durable cleanup intent: %+v", current)
		}
		if owner, has := ownerForFixture(t, fixture, env); !has {
			t.Fatalf("owner released without durable cleanup intent: %+v", owner)
		}
		if index := transport.eventIndex("run:platform-cleanup-"); index >= 0 {
			t.Fatalf("cleanup ran before intent was durable: %v", transport.events)
		}
		dropBlock()
		transport.failure = nil
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
			t.Fatalf("retry after cleanup intent repair: %v", err)
		}
		final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.State != contract.Succeeded {
			t.Fatalf("retried deployment = %+v, want succeeded", final)
		}
	})

	t.Run("post-switch public failure", func(t *testing.T) {
		transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		env := fixture.environment(t)
		previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
		fixture.service.publicVerify = func(context.Context, platform.Environment, string) error {
			return contract.NewFault(contract.PreconditionFailed, "public verification rejected", "test")
		}
		release := fixture.createRelease("live-cleanup-public", "registry.example/live:live-cleanup-public")
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "live-cleanup-public",
			map[string]any{"release_id": release})
		dropBlock := blockContainerCleanupInserts(t, fixture.pool)
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
			t.Fatal("public failure settled without durable cleanup intent")
		}
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State.Terminal() {
			t.Fatalf("public failure settled without durable cleanup intent: %+v", current)
		}
		if owner, has := ownerForFixture(t, fixture, env); !has || owner.Phase != deploymentOwnerRestored {
			t.Fatalf("owner after blocked public cleanup intent = %+v, has=%t", owner, has)
		}
		if releaseID, deploymentID := fixture.serving(t); releaseID != previousRelease || deploymentID != previousDeployment {
			t.Fatalf("serving after blocked public cleanup intent = %s/%s, want %s/%s", releaseID, deploymentID, previousRelease, previousDeployment)
		}
		dropBlock()
		fixture.service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
			t.Fatalf("recovery after cleanup intent repair: %v", err)
		}
		final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.State != contract.Failed {
			t.Fatalf("recovered public failure = %+v, want failed", final)
		}
		if owner, has := ownerForFixture(t, fixture, env); has {
			t.Fatalf("owner remained after recovered public failure: %+v", owner)
		}
		if releaseID, deploymentID := fixture.serving(t); releaseID != previousRelease || deploymentID != previousDeployment {
			t.Fatalf("serving after recovered public failure = %s/%s, want %s/%s", releaseID, deploymentID, previousRelease, previousDeployment)
		}
	})
}

// TestIntegrationMaintenanceCleanupProtectsOwnerCandidate covers a crash after
// partial cleanup intent was recorded but before terminal settlement. The
// prepared owner may still resume, so maintenance must not delete its candidate.
func TestIntegrationMaintenanceCleanupProtectsOwnerCandidate(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	key := "partial-crash"
	_, depID, _, _ := fixture.prepareOwnedCandidate(t, key, "registry.example/live:partial-crash", "running")
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
        SET phase='prepared',route_target_deployment_id=$2 WHERE environment_id=$1`,
		env.ID, env.ServingDeploymentID); err != nil {
		t.Fatal(err)
	}
	dep, err := fixture.service.deploymentByID(fixture.ctx, depID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.persistDeploymentCleanupInstances(fixture.ctx, env, dep, dep.Instances, "candidate"); err != nil {
		t.Fatal(err)
	}
	name := key + "-candidate"
	transport.events = nil
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup with prepared owner: %v", err)
	}
	if transport.eventIndex("run:"+cleanupStepID(name)) >= 0 {
		t.Fatalf("maintenance cleanup removed the resumable candidate: %v", transport.events)
	}
	var rows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE container_name=$1`, name).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("candidate cleanup row after deferred maintenance = %d, want 1", rows)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.Phase != deploymentOwnerPrepared {
		t.Fatalf("prepared owner changed during maintenance cleanup: %+v, has=%t", owner, has)
	}
	// Simulate the terminal settlement releasing ownership: only then may the
	// durable cleanup row remove the candidate.
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM eregion_platform_deployment_owners WHERE environment_id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_container_cleanup
        SET next_attempt_at=clock_timestamp() WHERE container_name=$1`, name); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup after owner release: %v", err)
	}
	if transport.eventIndex("run:"+cleanupStepID(name)) < 0 {
		t.Fatalf("candidate cleanup did not run after owner release: %v", transport.events)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE container_name=$1`, name).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("candidate cleanup row after successful removal = %d, want 0", rows)
	}
}

// TestIntegrationWorkerShutdownRetainsCandidates distinguishes worker context
// cancellation from a durable cancel request: shutdown must not remove
// candidates a retry still needs.
func TestIntegrationWorkerShutdownRetainsCandidates(t *testing.T) {
	transport := &healthFaultTransport{
		recordingDeployTransport: &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()},
	}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	if env.Revision == nil {
		t.Fatal("environment has no current revision")
	}
	release := fixture.createRelease("shutdown-retain", "registry.example/live:shutdown-retain")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "shutdown-retain",
		map[string]any{"release_id": release})
	depID := platform.DeploymentID(deploymentForOperation(t, fixture.pool, op.ID))
	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, release, depID)
	if err != nil {
		t.Fatal(err)
	}
	transport.failure = context.Canceled
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("worker shutdown did not return an error")
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("worker shutdown terminalized the operation: %+v", current)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.Phase != deploymentOwnerPrepared {
		t.Fatalf("owner after worker shutdown = %+v, has=%t", owner, has)
	}
	for _, planned := range plan {
		step := containerStepID(planned.Process.Name, planned.Replica)
		if runs := transport.runCount(op.ID, step); runs != 1 {
			t.Fatalf("container step %s ran %d times before retry, want 1", step, runs)
		}
		var state string
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM eregion_operation_steps
            WHERE operation_id=$1 AND step=$2`, op.ID, step).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "completed" {
			t.Fatalf("container step %s after worker shutdown = %q, want completed", step, state)
		}
	}
	if index := transport.eventIndex("run:platform-cleanup-"); index >= 0 {
		t.Fatalf("worker shutdown ran candidate cleanup: %v", transport.events)
	}
	var cleanupRows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, depID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("worker shutdown recorded %d cleanup rows, want 0", cleanupRows)
	}
	transport.failure = nil
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("retry after worker shutdown: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Succeeded {
		t.Fatalf("retried deployment after shutdown = %+v, want succeeded", final)
	}
	for _, planned := range plan {
		step := containerStepID(planned.Process.Name, planned.Replica)
		if runs := transport.runCount(op.ID, step); runs != 1 {
			t.Fatalf("container step %s ran %d times across shutdown retry, want 1", step, runs)
		}
	}
	if releaseID, deploymentID := fixture.serving(t); releaseID != release || deploymentID != depID {
		t.Fatalf("serving after shutdown retry = %s/%s, want %s/%s", releaseID, deploymentID, release, depID)
	}
}

// TestIntegrationCleanupEvidenceReadFailureRetainsOwner covers a disappearing
// container-step evidence read before a pre-save cancellation settles.
func TestIntegrationCleanupEvidenceReadFailureRetainsOwner(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	if env.Revision == nil {
		t.Fatal("environment has no current revision")
	}
	release := fixture.createRelease("evidence-read", "registry.example/live:evidence-read")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "evidence-read",
		map[string]any{"release_id": release})
	depID := platform.DeploymentID(deploymentForOperation(t, fixture.pool, op.ID))
	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, release, depID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatal("candidate plan is empty")
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
        SET state='running',instances='[]'::jsonb,revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	for _, planned := range plan {
		insertCompletedStep(t, fixture.pool, op.ID, containerStepID(planned.Process.Name, planned.Replica), strings.Repeat("e", 64))
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations
        SET state='cancelling',attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
		EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: depID, Attempt: 1,
		Phase: deploymentOwnerSwitching, RouteTarget: depID,
		RestoreReleaseID: env.ServingRelease, RestoreDeploymentID: env.ServingDeploymentID,
	})
	fixture.service.deploymentCleanupEvidence = func(context.Context, contract.OperationID, string) (bool, error) {
		return false, errors.New("step evidence read failed")
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err == nil {
		t.Fatal("cancellation settled without a readable cleanup evidence set")
	}
	current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("cancellation terminalized on cleanup evidence read failure: %+v", current)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has {
		t.Fatalf("owner released on cleanup evidence read failure: %+v", owner)
	}
	var rows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, depID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("cleanup rows recorded despite evidence read failure = %d", rows)
	}
	fixture.service.deploymentCleanupEvidence = nil
	current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
		t.Fatalf("cancellation retry after evidence repair: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Canceled {
		t.Fatalf("cancellation after evidence repair = %+v, want cancelled", final)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("owner remained after cancellation: %+v", owner)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, depID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("durable cleanup rows after successful post-settlement removal = %d, want 0", rows)
	}
	if index := transport.eventIndex("run:" + cleanupStepID(plan[0].Name)); index < 0 {
		t.Fatalf("post-settlement cleanup did not run: %v", transport.events)
	}
}

// TestIntegrationDeploymentTerminalRetryWithRetainedCandidates covers a retry
// that turns terminal before any new transport work. Cleanup intent for the
// containers retained by the first attempt must be durable before the prepared
// owner is released, even when no resolver is configured.
func TestIntegrationDeploymentTerminalRetryWithRetainedCandidates(t *testing.T) {
	run := func(t *testing.T) (contract.Operation, []platform.PlannedContainer, *deploymentRetryFixture, *healthFaultTransport) {
		t.Helper()
		transport := &healthFaultTransport{
			recordingDeployTransport: &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()},
		}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		env := fixture.environment(t)
		if env.Revision == nil {
			t.Fatal("environment has no current revision")
		}
		release := fixture.createRelease("terminal-retry", "registry.example/live:terminal-retry")
		op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "terminal-retry",
			map[string]any{"release_id": release})
		depID := platform.DeploymentID(deploymentForOperation(t, fixture.pool, op.ID))
		plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, release, depID)
		if err != nil {
			t.Fatal(err)
		}
		transport.failure = errors.New("health probe timed out")
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
			t.Fatal("retryable failure was not returned")
		}
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State.Terminal() {
			t.Fatalf("retryable failure terminalized: %+v", current)
		}
		if owner, has := ownerForFixture(t, fixture, env); !has || owner.Phase != deploymentOwnerPrepared {
			t.Fatalf("owner after retryable failure = %+v, has=%t", owner, has)
		}
		transport.failure = nil
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_nodes
            SET capabilities=ARRAY['buildpacks'] WHERE name=$1`, env.Node); err != nil {
			t.Fatal(err)
		}
		fixture.service.resolver = nil
		transport.events = nil
		return op, plan, fixture, transport
	}

	t.Run("placement failure records durable cleanup", func(t *testing.T) {
		op, plan, fixture, transport := run(t)
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
			t.Fatalf("terminal placement attempt: %v", err)
		}
		final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.State != contract.Failed || final.ErrorCode != contract.PreconditionFailed {
			t.Fatalf("placement retry = %+v, want failed/precondition_failed", final)
		}
		env := fixture.environment(t)
		if owner, has := ownerForFixture(t, fixture, env); has {
			t.Fatalf("owner remained after terminal placement failure: %+v", owner)
		}
		if index := transport.eventIndex("run:" + cleanupStepID(plan[0].Name)); index >= 0 {
			t.Fatalf("cleanup ran without a resolver: %v", transport.events)
		}
		for _, planned := range plan {
			var count int
			var nodeName, deploymentID string
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*),coalesce(min(node),''),coalesce(min(deployment_id),'')
                FROM eregion_platform_container_cleanup WHERE container_name=$1`, planned.Name).
				Scan(&count, &nodeName, &deploymentID); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("cleanup rows for retained container %s = %d, want 1", planned.Name, count)
			}
			if nodeName != env.Node {
				t.Fatalf("cleanup node for %s = %q, want %q", planned.Name, nodeName, env.Node)
			}
			if deploymentID == "" {
				t.Fatalf("cleanup row for %s lost its deployment identity", planned.Name)
			}
		}
	})

	t.Run("cleanup record failure retains owner", func(t *testing.T) {
		op, _, fixture, _ := run(t)
		dropBlock := blockContainerCleanupInserts(t, fixture.pool)
		defer dropBlock()
		if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
			t.Fatal("terminal placement failure settled without durable cleanup intent")
		}
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State.Terminal() {
			t.Fatalf("terminal placement failure settled without durable cleanup intent: %+v", current)
		}
		env := fixture.environment(t)
		if owner, has := ownerForFixture(t, fixture, env); !has {
			t.Fatalf("owner released without durable cleanup intent: %+v", owner)
		}
	})
}

// TestIntegrationOwnerRestoreTargetCandidateFailsClosed keeps the unsafe
// invariant nonterminal: a live retry and terminal owner recovery both retain
// the owner instead of restoring or cleaning the candidate as its own
// predecessor.
func TestIntegrationOwnerRestoreTargetCandidateFailsClosed(t *testing.T) {
	for _, phase := range []deploymentOwnerPhase{deploymentOwnerSwitching, deploymentOwnerRestoring} {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
			fixture := newDeploymentRetryFixtureWithTransport(t, transport)
			fixture.deploy(t)
			env := fixture.environment(t)
			previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
			op, depID, _, _ := fixture.prepareOwnedCandidate(t, "unsafe-"+string(phase), "registry.example/live:unsafe", "running")
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
                SET phase=$2,restore_deployment_id=$3 WHERE environment_id=$1`, env.ID, string(phase), depID); err != nil {
				t.Fatal(err)
			}
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
				t.Fatal("unsafe restore target was accepted as a live retry")
			}
			current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State.Terminal() {
				t.Fatalf("unsafe live invariant terminalized the operation: %+v", current)
			}
			owner, has := ownerForFixture(t, fixture, env)
			if !has || owner.RestoreDeploymentID != depID {
				t.Fatalf("owner after unsafe live retry = %+v, has=%t", owner, has)
			}
			// The terminal owner sweep must enforce the same invariant instead
			// of bypassing the live guard.
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations
                SET state='failed',error_code='unavailable' WHERE id=$1`, op.ID); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err == nil {
				t.Fatal("terminal owner sweep settled an unsafe restore target")
			}
			owner, has = ownerForFixture(t, fixture, env)
			if !has || owner.RestoreDeploymentID != depID {
				t.Fatalf("owner after unsafe terminal sweep = %+v, has=%t", owner, has)
			}
			if release, deployment := fixture.serving(t); release != previousRelease || deployment != previousDeployment {
				t.Fatalf("serving after unsafe owner recovery = %s/%s, want %s/%s", release, deployment, previousRelease, previousDeployment)
			}
		})
	}
}

// TestIntegrationDeploymentRenewalRepairsCrashGapPredecessor recreates the old
// split-transaction crash gap: an owner frozen to the actual predecessor while
// the deployment row still names a stale one. Renewal must repair the deployment
// binding from the owner before recovery effects, so cleanup targets the real
// predecessor.
func TestIntegrationDeploymentRenewalRepairsCrashGapPredecessor(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	staleRelease, staleDeployment := env.ServingRelease, env.ServingDeploymentID
	staleDeploymentRow, err := fixture.service.deploymentByID(fixture.ctx, staleDeployment)
	if err != nil {
		t.Fatal(err)
	}

	releaseB := fixture.createRelease("gap-b", "registry.example/live:gap-b")
	opB := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "gap-b", map[string]any{"release_id": releaseB})
	releaseC := fixture.createRelease("gap-c", "registry.example/live:gap-c")
	opC := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "gap-c", map[string]any{"release_id": releaseC})

	// Two queued deploys: B runs first and becomes the true predecessor of C.
	fixture.run(opB)
	env = fixture.environment(t)
	predecessorRelease, predecessorDeployment := env.ServingRelease, env.ServingDeploymentID
	predecessorRow, err := fixture.service.deploymentByID(fixture.ctx, predecessorDeployment)
	if err != nil {
		t.Fatal(err)
	}
	if predecessorDeployment == staleDeployment {
		t.Fatal("B did not become serving before C")
	}

	// Simulate the crash gap for C: its owner froze B, but its deployment row
	// still names the older A predecessor.
	candidateDeployment := platform.DeploymentID(deploymentForOperation(t, fixture.pool, opC.ID))
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
        SET state='running',previous_release_id=$2,previous_deployment_id=$3 WHERE id=$1`,
		candidateDeployment, staleRelease, staleDeployment); err != nil {
		t.Fatal(err)
	}
	insertCompletedStep(t, fixture.pool, opC.ID, deploymentRouteStep, strings.Repeat("a", 64))
	insertCompletedStep(t, fixture.pool, opC.ID, deploymentPublicStep, strings.Repeat("b", 64))
	insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
		EnvironmentID: env.ID, OperationID: opC.ID, DeploymentID: candidateDeployment, Attempt: 1,
		Phase: deploymentOwnerServing, RouteTarget: candidateDeployment,
		RestoreReleaseID: predecessorRelease, RestoreDeploymentID: predecessorDeployment,
	})
	var beforeRelease, beforeDeployment string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
        FROM eregion_platform_deployments WHERE id=$1`, candidateDeployment).Scan(&beforeRelease, &beforeDeployment); err != nil {
		t.Fatal(err)
	}
	if beforeRelease != string(staleRelease) || beforeDeployment != string(staleDeployment) {
		t.Fatalf("crash-gap fixture previous = %s/%s, want stale %s/%s", beforeRelease, beforeDeployment, staleRelease, staleDeployment)
	}

	transport.events = nil
	if err := runPlatformAttempt(fixture.ctx, fixture.service, opC.ID, 1); err != nil {
		t.Fatalf("C attempt after crash gap: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, opC.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != contract.Succeeded {
		t.Fatalf("C after crash-gap recovery = %+v, want succeeded", final)
	}
	var afterRelease, afterDeployment string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
        FROM eregion_platform_deployments WHERE id=$1`, candidateDeployment).Scan(&afterRelease, &afterDeployment); err != nil {
		t.Fatal(err)
	}
	if afterRelease != string(predecessorRelease) || afterDeployment != string(predecessorDeployment) {
		t.Fatalf("renewal bound C previous = %s/%s, want owner predecessor %s/%s", afterRelease, afterDeployment, predecessorRelease, predecessorDeployment)
	}
	if release, deployment := fixture.serving(t); release != releaseC || deployment != candidateDeployment {
		t.Fatalf("serving after C = %s/%s, want %s/%s", release, deployment, releaseC, candidateDeployment)
	}
	for _, instance := range predecessorRow.Instances {
		if instance.Name == "" {
			continue
		}
		if transport.eventIndex("run:"+cleanupStepID(instance.Name)) < 0 {
			t.Fatalf("cleanup did not target true predecessor %s; events=%v", instance.Name, transport.events)
		}
	}
	for _, instance := range staleDeploymentRow.Instances {
		if instance.Name == "" {
			continue
		}
		if transport.eventIndex("run:"+cleanupStepID(instance.Name)) >= 0 {
			t.Fatalf("cleanup targeted stale predecessor %s; events=%v", instance.Name, transport.events)
		}
	}
}

// TestIntegrationWaitingOwnerReloadsAfterStaleSettlement covers a terminal
// failed owner whose recovery restores the real predecessor O while the waiting
// operation loaded an environment snapshot that still names the failed candidate
// A. Every reacquisition path must reload the environment and freeze O.
func TestIntegrationWaitingOwnerReloadsAfterStaleSettlement(t *testing.T) {
	type staleFixture struct {
		fixture     *deploymentRetryFixture
		env         platform.Environment
		op          contract.Operation
		dep         platform.DeploymentID
		oRelease    platform.ReleaseID
		oDeployment platform.DeploymentID
	}
	setup := func(t *testing.T, key string) staleFixture {
		t.Helper()
		transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
		fixture := newDeploymentRetryFixtureWithTransport(t, transport)
		fixture.deploy(t)
		env := fixture.environment(t)
		oRelease, oDeployment := env.ServingRelease, env.ServingDeploymentID

		releaseA := fixture.createRelease(key+"-a", "registry.example/live:"+key+"-a")
		opA := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key+"-a",
			map[string]any{"release_id": releaseA})
		deploymentA := platform.DeploymentID(deploymentForOperation(t, fixture.pool, opA.ID))
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
            SET state='failed' WHERE id=$1`, deploymentA); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations
            SET state='failed',attempt=1 WHERE id=$1`, opA.ID); err != nil {
			t.Fatal(err)
		}
		// The database serving pointer still names the failed candidate A while
		// the terminal owner's frozen restore target is O.
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,serving_deployment_id=$3 WHERE id=$1`, env.ID, releaseA, deploymentA); err != nil {
			t.Fatal(err)
		}
		insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
			EnvironmentID: env.ID, OperationID: opA.ID, DeploymentID: deploymentA, Attempt: 1,
			Phase: deploymentOwnerPrepared, RouteTarget: oDeployment,
			RestoreReleaseID: oRelease, RestoreDeploymentID: oDeployment,
		})
		releaseB := fixture.createRelease(key+"-b", "registry.example/live:"+key+"-b")
		opB := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key+"-b",
			map[string]any{"release_id": releaseB})
		return staleFixture{
			fixture: fixture, env: env, op: opB,
			dep:      platform.DeploymentID(deploymentForOperation(t, fixture.pool, opB.ID)),
			oRelease: oRelease, oDeployment: oDeployment,
		}
	}
	assertPredecessor := func(t *testing.T, fixture *deploymentRetryFixture, dep platform.DeploymentID, release platform.ReleaseID, deployment platform.DeploymentID) {
		t.Helper()
		var previousRelease, previousDeployment string
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
            FROM eregion_platform_deployments WHERE id=$1`, dep).Scan(&previousRelease, &previousDeployment); err != nil {
			t.Fatal(err)
		}
		if previousRelease != string(release) || previousDeployment != string(deployment) {
			t.Fatalf("%s previous = %s/%s, want reloaded serving predecessor %s/%s", dep, previousRelease, previousDeployment, release, deployment)
		}
	}

	t.Run("runDeployment", func(t *testing.T) {
		state := setup(t, "stale-run")
		if err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 1); err != nil {
			t.Fatalf("B attempt: %v", err)
		}
		current, err := state.fixture.service.Get(state.fixture.ctx, testActor, state.op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != contract.Succeeded {
			t.Fatalf("B = %+v, want succeeded", current)
		}
		assertPredecessor(t, state.fixture, state.dep, state.oRelease, state.oDeployment)
	})

	t.Run("cancellation", func(t *testing.T) {
		state := setup(t, "stale-cancel")
		if _, err := state.fixture.pool.Exec(state.fixture.ctx, `UPDATE eregion_operations
            SET state='cancelling',attempt=1 WHERE id=$1`, state.op.ID); err != nil {
			t.Fatal(err)
		}
		if err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 1); err != nil {
			t.Fatalf("B cancellation: %v", err)
		}
		current, err := state.fixture.service.Get(state.fixture.ctx, testActor, state.op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != contract.Canceled {
			t.Fatalf("B cancellation = %+v, want cancelled", current)
		}
		assertPredecessor(t, state.fixture, state.dep, state.oRelease, state.oDeployment)
		if release, deployment := state.fixture.serving(t); release != state.oRelease || deployment != state.oDeployment {
			t.Fatalf("serving after cancellation = %s/%s, want restored %s/%s", release, deployment, state.oRelease, state.oDeployment)
		}
	})

	t.Run("reconciliation", func(t *testing.T) {
		state := setup(t, "stale-reconcile")
		if _, err := state.fixture.pool.Exec(state.fixture.ctx, `UPDATE eregion_operations
            SET state='running',attempt=1 WHERE id=$1`, state.op.ID); err != nil {
			t.Fatal(err)
		}
		current, err := state.fixture.service.Get(state.fixture.ctx, testActor, state.op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.fixture.service.reconcileDeployment(state.fixture.ctx, current); err != nil {
			t.Fatalf("B reconciliation: %v", err)
		}
		current, err = state.fixture.service.Get(state.fixture.ctx, testActor, state.op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != contract.Failed {
			t.Fatalf("B reconciliation = %+v, want failed without a lease", current)
		}
		assertPredecessor(t, state.fixture, state.dep, state.oRelease, state.oDeployment)
		if release, deployment := state.fixture.serving(t); release != state.oRelease || deployment != state.oDeployment {
			t.Fatalf("serving after reconciliation = %s/%s, want restored %s/%s", release, deployment, state.oRelease, state.oDeployment)
		}
	})
}
