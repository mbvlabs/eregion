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
        (operation_id,step,inputs_hash,state,attempt,execution_id,started_at,completed_at,error,error_code)
        VALUES($1,$2,$3,$4,1,$5,clock_timestamp(),` + completedAt + `,$6,'')`
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
        (operation_id,step,inputs_hash,state,attempt,execution_id,started_at,completed_at,error,error_code)
        VALUES($1,$2,$3,'completed',1,$4,clock_timestamp(),clock_timestamp(),'','')`,
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
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: transport, NodeStateDir: t.TempDir(),
		LocalNodeName: "local", RegistryEndpoint: "127.0.0.1:5000",
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
		platformAwait(t, s, op, contract.Succeeded) //nolint:contextcheck // The assertion helper owns its bounded test wait, independently of this setup context.
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
		createRelease: func(_, image string) platform.ReleaseID {
			return seedArtifactRelease(t, pool, s, "live", "production", image).ID
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

func (f *deploymentRetryFixture) deploy(t *testing.T) {
	t.Helper()
	release := f.createRelease("deploy-"+fmt.Sprint(time.Now().UnixNano()), "registry.example/live:base")
	f.run(platformSubmit(t, f.service, platform.KindDeploy, "env/live/production", "deploy-"+string(release), map[string]any{"release_id": release}))
}

func insertDeploymentOwner(t *testing.T, pool *pgxpool.Pool, env platform.Environment, owner deploymentOwner) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_platform_deployment_owners
        (environment_id,operation_id,deployment_id,owner_attempt,phase,route_target_deployment_id,restore_release_id,restore_deployment_id,created_at,updated_at)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp(),clock_timestamp())`,
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
	instances, err := routeInstancesForTest(fixture.ctx, fixture.service.entities, "retry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return instanceNames(instances)
}

func ownerForFixture(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment) (deploymentOwner, bool) {
	t.Helper()
	owner, has, err := fixture.service.deploymentOwnerByEnvironment(fixture.ctx, fixture.service.entities.Platform().Owners(), env.ID)
	if err != nil {
		t.Fatal(err)
	}
	return owner, has
}

func newDeployedRecordingFixture(t *testing.T) (*deploymentRetryFixture, *recordingDeployTransport, platform.Environment) {
	t.Helper()
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	return fixture, transport, fixture.environment(t)
}

func previousDeploymentFor(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment) platform.Deployment {
	t.Helper()
	previous, err := fixture.service.deploymentByID(fixture.ctx, env.ServingDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	return previous
}

func assertOperationNotTerminal(t *testing.T, fixture *deploymentRetryFixture, opID contract.OperationID, context string) {
	t.Helper()
	current, err := fixture.service.Get(fixture.ctx, testActor, opID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("%s: operation became terminal: %+v", context, current)
	}
}

func assertOperationState(t *testing.T, fixture *deploymentRetryFixture, opID contract.OperationID, want contract.State, context string) {
	t.Helper()
	current, err := fixture.service.Get(fixture.ctx, testActor, opID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != want {
		t.Fatalf("%s: operation = %+v, want %s", context, current, want)
	}
}

func assertOwnerPhase(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, want deploymentOwnerPhase, context string) {
	t.Helper()
	owner, has := ownerForFixture(t, fixture, env)
	if !has || owner.Phase != want {
		t.Fatalf("%s: owner = %+v, has=%t, want phase %s", context, owner, has, want)
	}
}

func assertOwnerRetained(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, context string) {
	t.Helper()
	if owner, has := ownerForFixture(t, fixture, env); !has {
		t.Fatalf("%s: owner was released: %+v", context, owner)
	}
}

func assertOwnerReleased(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, context string) {
	t.Helper()
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("%s: owner remained: %+v", context, owner)
	}
}

func assertServingDeployment(t *testing.T, fixture *deploymentRetryFixture, release platform.ReleaseID, deployment platform.DeploymentID, context string) {
	t.Helper()
	if gotRelease, gotDeployment := fixture.serving(t); gotRelease != release || gotDeployment != deployment {
		t.Fatalf("%s: serving = %s/%s, want %s/%s", context, gotRelease, gotDeployment, release, deployment)
	}
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

func TestIntegrationDeploymentInterruptionAfterApplyBeforeAckRetainsOwner(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	servingRelease, servingDeployment := env.ServingRelease, env.ServingDeploymentID
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "interrupt-ack", "registry.example/live:interrupt", "running")
	if got := routeNames(t, fixture); !reflect.DeepEqual(got, []string{"interrupt-ack-candidate"}) {
		t.Fatalf("pre-recovery route = %v, want owner intent candidate", got)
	}
	transport.events = nil
	fixture.service.routeSync = func(context.Context) error {
		transport.events = append(transport.events, "restore:ok")
		return nil
	}

	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unacknowledged route accepted: %v", err)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State.Terminal() {
		t.Fatalf("unacknowledged route allowed terminal state: %+v", final)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.OperationID != op.ID {
		t.Fatalf("unacknowledged route lost ownership: %+v", owner)
	}
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup: %v", err)
	}
	if transport.eventIndex("restore:") >= 0 || transport.eventIndex("run:"+cleanupStepID("interrupt-ack-candidate")) >= 0 {
		t.Fatalf("restore or cleanup dispatched with an unacknowledged route: %v", transport.events)
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
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "cancel-route", "registry.example/live:cancel", "completed")
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
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup: %v", err)
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
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "failed-restore", "registry.example/live:failed-restore", "completed")
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
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unacknowledged restore retry: %v", err)
	}
	current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("unacknowledged restore allowed terminal state: %+v", current)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.OperationID != op.ID {
		t.Fatalf("unacknowledged restore lost ownership: %+v", owner)
	}
	if transport.eventIndex("restore:") >= 0 || transport.eventIndex("run:"+cleanupStepID("failed-restore-candidate")) >= 0 {
		t.Fatalf("resent restore or ran cleanup before proof: %v", transport.events)
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
	current, has, err := fixture.service.deploymentOwnerByEnvironment(fixture.ctx, fixture.service.entities.Platform().Owners(), env.ID)
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
	stagingRelease := seedArtifactRelease(t, fixture.pool, fixture.service, "live", "staging", "registry.example/live:staging").ID
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
            (container_name,environment_id,node,deployment_id,reason,attempts,last_error,created_at,next_attempt_at)
            VALUES($1,$2,'local',$3,'candidate',0,'',clock_timestamp(),clock_timestamp())`, name, env.ID, deployment); err != nil {
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

func TestIntegrationContainerCleanupImmediateHandoffAfterLegacyLock(t *testing.T) {
	fixture, transport, env := newDeployedRecordingFixture(t)
	releaseID := fixture.createRelease("handoff", "registry.example/live:handoff")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "handoff", map[string]any{"release_id": releaseID})
	deploymentID := platform.DeploymentID(deploymentForOperation(t, fixture.pool, op.ID))
	if err := fixture.service.ensureContainerCleanup(fixture.ctx, "handoff-candidate", env.ID, env.Node, deploymentID, "candidate", nil); err != nil {
		t.Fatal(err)
	}
	release, err := fixture.service.lockTargets(fixture.ctx, []string{"env/live/production"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.removeDeploymentCleanupRowsNow(fixture.ctx, env, deploymentID)
	release()
	if transport.eventIndex("run:"+cleanupStepID("handoff-candidate")) >= 0 {
		t.Fatal("cleanup ran while the legacy lock was held")
	}
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup: %v", err)
	}
	if transport.eventIndex("run:"+cleanupStepID("handoff-candidate")) < 0 {
		t.Fatal("maintenance did not remove the deferred container")
	}
	var remaining int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup WHERE container_name='handoff-candidate'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("cleanup row remains after successful removal: %d", remaining)
	}
	if err := fixture.service.ensureContainerCleanup(fixture.ctx, "immediate-candidate", env.ID, env.Node, deploymentID, "candidate", nil); err != nil {
		t.Fatal(err)
	}
	fixture.service.removeDeploymentCleanupRowsNow(fixture.ctx, env, deploymentID)
	if transport.eventIndex("run:"+cleanupStepID("immediate-candidate")) < 0 {
		t.Fatal("uncontended immediate cleanup did not remove the container")
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup WHERE container_name='immediate-candidate'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("immediate cleanup row remains after successful removal: %d", remaining)
	}
}

func TestIntegrationDeploymentLegacyAmbiguousOwnerRecoversConservatively(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	firstRelease, firstDeployment := env.ServingRelease, env.ServingDeploymentID
	servingInstances, err := routeInstancesForTest(fixture.ctx, fixture.service.entities, "retry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	ops := make([]contract.Operation, 0, 2)
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
        (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id,created_at,updated_at)
        VALUES($1,'ambiguous',$2,$3,$4,clock_timestamp(),clock_timestamp())`, env.ID, firstDeployment, firstRelease, firstDeployment); err != nil {
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
	op, _, env, _ := fixture.prepareOwnedCandidate(t, "exhausted", "registry.example/live:exhausted", "completed")
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
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup: %v", err)
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
	if err := fixture.service.retryDueContainerCleanups(fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup: %v", err)
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
			fixture, transport, env := newDeployedRecordingFixture(t)
			fixture.service.routeSync = func(context.Context) error {
				transport.events = append(transport.events, "restore:ok")
				return nil
			}
			ops := seedCleanupRecordFailure(t, fixture, env, ambiguous)
			if _, err := fixture.pool.Exec(fixture.ctx, `DROP TABLE eregion_platform_container_cleanup`); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err == nil {
				t.Fatal("owner reconciliation succeeded while cleanup rows could not be recorded")
			}
			assertCleanupRecordFailureRetained(t, fixture, env, ops)
		})
	}
}

// seedCleanupRecordFailure records the terminal or ambiguous owner state whose
// cleanup row insertion is about to fail.
func seedCleanupRecordFailure(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, ambiguous bool) []contract.Operation {
	t.Helper()
	if !ambiguous {
		op, _, _, _ := fixture.prepareOwnedCandidate(t, "record-failure", "registry.example/live:record-failure", "running")
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='failed' WHERE id=$1`, op.ID); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	ops := make([]contract.Operation, 0, 2)
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
        (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id,created_at,updated_at)
        VALUES($1,'ambiguous',$2,$3,$4,clock_timestamp(),clock_timestamp())`, env.ID, env.ServingDeploymentID, env.ServingRelease, env.ServingDeploymentID); err != nil {
		t.Fatal(err)
	}
	return ops
}

func assertCleanupRecordFailureRetained(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, ops []contract.Operation) {
	t.Helper()
	assertOwnerRetained(t, fixture, env, "cleanup record failure")
	for _, op := range ops {
		current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State.Terminal() {
			t.Fatalf("ambiguous operation %s became terminal before cleanup was durable", op.ID)
		}
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
            (container_name,environment_id,node,deployment_id,reason,attempts,last_error,created_at,next_attempt_at)
            VALUES($1,$2,'local',$3,'previous',1,'',clock_timestamp(),clock_timestamp()-interval '1 hour')`,
			name, env.ID, env.ServingDeploymentID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_container_cleanup
        (container_name,environment_id,node,deployment_id,reason,attempts,last_error,created_at,next_attempt_at)
        VALUES('zzz-unprotected',$1,'local',NULL,'previous',1,'',clock_timestamp(),clock_timestamp()-interval '1 hour')`, env.ID); err != nil {
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
	defer func() { _ = conn.Close(context.Background()) }()
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
	defer func() { _ = conn.Close(context.Background()) }()
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
	op, candidateDeployment, _, owner := fixture.prepareOwnedCandidate(t, "restore-protect", "registry.example/live:restore-protect", "completed")
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
			fixture, _, env := newDeployedRecordingFixture(t)
			previous := previousDeploymentFor(t, fixture, env)
			previousInstances, op := seedPreRecordFailure(t, fixture, previous, failLookup)
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
				t.Fatal("deployment succeeded without durable previous cleanup")
			}
			assertOperationNotTerminal(t, fixture, op.ID, "pre-record failure")
			assertOwnerPhase(t, fixture, env, deploymentOwnerServing, "pre-record failure")
			repairPreRecordFailure(t, fixture, previous, previousInstances, failLookup)
			err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1)
			var snooze *river.JobSnoozeError
			if !errors.As(err, &snooze) {
				t.Fatalf("expected verified handoff before retry: %v", err)
			}
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
				t.Fatalf("retry after pre-record repair: %v", err)
			}
			assertOperationState(t, fixture, op.ID, contract.Succeeded, "pre-record retry")
			assertOwnerReleased(t, fixture, env, "pre-record retry")
		})
	}
}

func seedPreRecordFailure(t *testing.T, fixture *deploymentRetryFixture, previous platform.Deployment, failLookup bool) ([]byte, contract.Operation) {
	t.Helper()
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
		return previousInstances, op
	}
	if _, err := fixture.pool.Exec(fixture.ctx, fmt.Sprintf(`ALTER TABLE eregion_platform_container_cleanup
        ADD CONSTRAINT pre_record_failure CHECK (container_name <> '%s')`, previous.Instances[0].Name)); err != nil {
		t.Fatal(err)
	}
	return previousInstances, op
}

func repairPreRecordFailure(t *testing.T, fixture *deploymentRetryFixture, previous platform.Deployment, previousInstances []byte, failLookup bool) {
	t.Helper()
	if failLookup {
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET instances=$2 WHERE id=$1`, previous.ID, string(previousInstances)); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_container_cleanup
        DROP CONSTRAINT pre_record_failure`); err != nil {
		t.Fatal(err)
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

func TestIntegrationDeploymentPostSwitchNonretryableErrorDoesNotProveQuiescence(t *testing.T) {
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
		transport.events = append(transport.events, "restore:ok")
		return nil
	}
	release := fixture.createRelease("post-sync", "registry.example/live:post-sync")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "post-sync", map[string]any{"release_id": release})
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("post-switch attempt completed without a route acknowledgement")
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State.Terminal() {
		t.Fatalf("unacknowledged reassertion terminalized: %+v", final)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.OperationID != op.ID {
		t.Fatalf("owner was lost after unacknowledged reassertion: %+v", owner)
	}
	if transport.eventIndex("restore:") >= 0 {
		t.Fatalf("post-switch failure attempted another load: %v", transport.events)
	}
}

func TestIntegrationDeploymentRecoveredReassertionCancellationRetainsOwner(t *testing.T) {
	transport := &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	env := fixture.environment(t)
	op, dep, _, _ := fixture.prepareOwnedCandidate(t, "retry-cancel", "registry.example/live:retry-cancel", "completed")
	insertCompletedStep(t, fixture.pool, op.ID, deploymentPublicStep, strings.Repeat("b", 64))
	var publishes int
	fixture.service.routeSync = func(ctx context.Context) error {
		publishes++
		if publishes == 1 {
			if _, err := fixture.service.RequestCancel(ctx, testActor, op.ID); err != nil {
				t.Fatal(err)
			}
		}
		return contract.NewFault(contract.Unavailable, "serving reassertion lost its reply", "test")
	}
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("attempt settled while the serving route load was unacknowledged")
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contract.Cancelling || publishes != 1 {
		t.Fatalf("unacknowledged reload: state=%s publishes=%d", current.State, publishes)
	}
	owner, has := ownerForFixture(t, fixture, env)
	if !has || owner.Phase != deploymentOwnerServing || owner.RouteTarget != dep {
		t.Fatalf("owner after unacknowledged reassertion = %+v, has=%t", owner, has)
	}
	claims := claimFixture{t: t, ctx: fixture.ctx, a: fixture.service.entities, b: fixture.service.entities}
	if claims.acquire(claims.b, claimID("reassertion-competitor", "reassertion-competitor"), op.Target) {
		t.Fatal("unacknowledged reassertion released the environment")
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
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("post-switch retry without reload proof: %v", err)
	}
	current, err = fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() || switches != 2 {
		t.Fatalf("unknown reload was treated as settled: state=%s switches=%d", current.State, switches)
	}
	if owner, has := ownerForFixture(t, fixture, env); !has || owner.OperationID != op.ID {
		t.Fatal("owner released without reload proof")
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
        (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id,created_at,updated_at)
        VALUES($1,'ambiguous',$2,$3,$4,clock_timestamp(),clock_timestamp())`, env.ID, previousDeployment, previousRelease, previousDeployment); err != nil {
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
			fixture, transport, env, op, previousRelease, previousDeployment := setupRecoverySyncFailure(t, "recovery-sync")
			installRecoverySyncRoute(t, fixture, transport, restoreFails)
			current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = fixture.service.reconcileDeployment(fixture.ctx, current)
			if restoreFails {
				assertRecoveryRestoreFailureRetained(t, fixture, env, op, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertOperationState(t, fixture, op.ID, contract.Failed, "recovery sync failure")
			assertServingDeployment(t, fixture, previousRelease, previousDeployment, "recovery sync failure")
			assertOwnerReleased(t, fixture, env, "recovery sync failure")
		})
	}
}

func setupRecoverySyncFailure(t *testing.T, key string) (*deploymentRetryFixture, *recordingDeployTransport, platform.Environment, contract.Operation, platform.ReleaseID, platform.DeploymentID) {
	t.Helper()
	fixture, transport, env := newDeployedRecordingFixture(t)
	previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, key, "registry.example/live:"+key, "completed")
	insertCompletedStep(t, fixture.pool, op.ID, deploymentPublicStep, strings.Repeat("b", 64))
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
        SET phase='serving',route_target_deployment_id=$2 WHERE environment_id=$1`, env.ID, depID); err != nil {
		t.Fatal(err)
	}
	return fixture, transport, env, op, previousRelease, previousDeployment
}

func installRecoverySyncRoute(t *testing.T, fixture *deploymentRetryFixture, transport *recordingDeployTransport, restoreFails bool) {
	t.Helper()
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
}

func assertRecoveryRestoreFailureRetained(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, op contract.Operation, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("reconciliation succeeded while recovery restoration failed")
	}
	assertOperationNotTerminal(t, fixture, op.ID, "recovery restore failure")
	assertOwnerPhase(t, fixture, env, deploymentOwnerRestoring, "recovery restore failure")
}

func TestIntegrationCancellationCleanupIntentIsDurable(t *testing.T) {
	for name, blockInsert := range map[string]bool{"insert failure": true, "resolver unavailable": false} {
		t.Run(name, func(t *testing.T) {
			fixture, _, env, op, dep := setupCancellationCleanupIntent(t, "cancel-durable", blockInsert)
			current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if blockInsert {
				assertBlockedCancellationCleanupIntent(t, fixture, env, op, current)
				return
			}
			if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
				t.Fatal(err)
			}
			assertResolverUnavailableCleanupIntent(t, fixture, op, dep)
		})
	}
}

func setupCancellationCleanupIntent(t *testing.T, key string, blockInsert bool) (*deploymentRetryFixture, *recordingDeployTransport, platform.Environment, contract.Operation, platform.DeploymentID) {
	t.Helper()
	fixture, transport, env := newDeployedRecordingFixture(t)
	op, dep, _, _ := fixture.prepareOwnedCandidate(t, key, "registry.example/live:"+key, "running")
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if blockInsert {
		if _, err := fixture.pool.Exec(fixture.ctx, fmt.Sprintf(`ALTER TABLE eregion_platform_container_cleanup
            ADD CONSTRAINT cancel_cleanup_failure CHECK (container_name <> '%s')`, key+"-candidate")); err != nil {
			t.Fatal(err)
		}
	} else {
		fixture.service.resolver = nil
	}
	fixture.service.routeSync = func(context.Context) error { return nil }
	return fixture, transport, env, op, dep
}

func assertBlockedCancellationCleanupIntent(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, op contract.Operation, current contract.Operation) {
	t.Helper()
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err == nil {
		t.Fatal("cancellation terminalized without durable cleanup intent")
	}
	assertOperationNotTerminal(t, fixture, op.ID, "cancellation cleanup insert failure")
	assertOwnerRetained(t, fixture, env, "cancellation cleanup insert failure")
	if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_container_cleanup DROP CONSTRAINT cancel_cleanup_failure`); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}
	assertOperationState(t, fixture, op.ID, contract.Canceled, "cancellation after cleanup repair")
}

func assertResolverUnavailableCleanupIntent(t *testing.T, fixture *deploymentRetryFixture, op contract.Operation, dep platform.DeploymentID) {
	t.Helper()
	var rows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, dep).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("cancellation cleanup intent was not durable when the resolver was unavailable")
	}
	assertOperationState(t, fixture, op.ID, contract.Canceled, "cancellation with unavailable resolver")
}

func TestIntegrationPreviousCleanupPinnedRevisionNode(t *testing.T) {
	t.Run("uses pinned node after environment move", func(t *testing.T) {
		assertPinnedRevisionCleanupUsesPinnedNode(t)
	})
	t.Run("lookup failure retains owner", func(t *testing.T) {
		assertPinnedRevisionLookupFailureRetainsOwner(t)
	})
}

func disableRevisionTriggers(t *testing.T, fixture *deploymentRetryFixture) {
	t.Helper()
	if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func enableRevisionTriggers(t *testing.T, fixture *deploymentRetryFixture) {
	t.Helper()
	if _, err := fixture.pool.Exec(fixture.ctx, `ALTER TABLE eregion_platform_environment_revisions ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func assertPinnedRevisionCleanupUsesPinnedNode(t *testing.T) {
	t.Helper()
	fixture, _, env := newDeployedRecordingFixture(t)
	previous := previousDeploymentFor(t, fixture, env)
	if previous.RevisionID == "" {
		t.Fatal("previous deployment has no pinned revision")
	}
	disableRevisionTriggers(t, fixture)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions
        SET runtime=jsonb_set(runtime,'{node}','"old-node"') WHERE id=$1`, previous.RevisionID); err != nil {
		t.Fatal(err)
	}
	enableRevisionTriggers(t, fixture)
	release := fixture.createRelease("pinned-node", "registry.example/live:pinned-node")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "pinned-node", map[string]any{"release_id": release})
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("deploy with pinned cleanup node: %v", err)
	}
	var node string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT node FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1 LIMIT 1`, previous.ID).Scan(&node); err != nil {
		t.Fatal(err)
	}
	if node != "old-node" {
		t.Fatalf("cleanup node = %q, want pinned old-node", node)
	}
}

func assertPinnedRevisionLookupFailureRetainsOwner(t *testing.T) {
	t.Helper()
	fixture, _, env := newDeployedRecordingFixture(t)
	previous := previousDeploymentFor(t, fixture, env)
	fixture.run(platformSubmit(t, fixture.service, platform.KindConfigSet, "env/live/production",
		"pinned-fail-config", map[string]any{"config": map[string]string{"PIN": "1"}}))
	release := fixture.createRelease("pinned-fail", "registry.example/live:pinned-fail")
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "pinned-fail", map[string]any{"release_id": release})
	var runtime []byte
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT runtime FROM eregion_platform_environment_revisions WHERE id=$1`, previous.RevisionID).Scan(&runtime); err != nil {
		t.Fatal(err)
	}
	disableRevisionTriggers(t, fixture)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions
        SET runtime='[]'::jsonb WHERE id=$1`, previous.RevisionID); err != nil {
		t.Fatal(err)
	}
	enableRevisionTriggers(t, fixture)
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("deployment succeeded with unreadable pinned previous revision")
	}
	assertOperationNotTerminal(t, fixture, op.ID, "pinned revision lookup failure")
	assertOwnerRetained(t, fixture, env, "pinned revision lookup failure")
	disableRevisionTriggers(t, fixture)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions SET runtime=$2 WHERE id=$1`, previous.RevisionID, string(runtime)); err != nil {
		t.Fatal(err)
	}
	enableRevisionTriggers(t, fixture)
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
		t.Fatalf("retry after pinned revision repair: %v", err)
	}
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
        (environment_id,phase,route_target_deployment_id,restore_release_id,restore_deployment_id,created_at,updated_at)
        VALUES($1,'ambiguous',$2,$3,$4,clock_timestamp(),clock_timestamp())`, env.ID, previousDeployment, previousRelease, previousDeployment); err != nil {
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
		scenario := newPreSaveScenario(t, "pre-save-cancel", "c")
		assertPreSaveCancellationCleanup(t, scenario)
	})

	t.Run("unresolvable pinned node retains ownership", func(t *testing.T) {
		scenario := newPreSaveScenario(t, "pre-save-node", "d")
		disableRevisionTriggers(t, scenario.fixture)
		if _, err := scenario.fixture.pool.Exec(scenario.fixture.ctx, `UPDATE eregion_platform_environment_revisions
            SET runtime='{}'::jsonb WHERE id=$1`, scenario.env.RevisionID); err != nil {
			t.Fatal(err)
		}
		enableRevisionTriggers(t, scenario.fixture)
		assertUnresolvablePinnedCleanupNodeRetainsOwnership(t, scenario)
	})
}

type preSaveScenario struct {
	fixture *deploymentRetryFixture
	env     platform.Environment
	op      contract.Operation
	depID   platform.DeploymentID
	plan    []platform.PlannedContainer
}

func newPreSaveScenario(t *testing.T, key, hashChar string) *preSaveScenario {
	t.Helper()
	fixture, _, env := newDeployedRecordingFixture(t)
	if env.Revision == nil {
		t.Fatal("environment has no current revision")
	}
	release := fixture.createRelease(key, "registry.example/live:"+key)
	op := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key,
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
		insertCompletedStep(t, fixture.pool, op.ID, containerStepID(planned.Process.Name, planned.Replica), strings.Repeat(hashChar, 64))
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
	return &preSaveScenario{fixture: fixture, env: env, op: op, depID: depID, plan: plan}
}

func assertPreSaveCancellationCleanup(t *testing.T, scenario *preSaveScenario) {
	t.Helper()
	current, err := scenario.fixture.service.Get(scenario.fixture.ctx, testActor, scenario.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := scenario.fixture.service.reconcileDeployment(scenario.fixture.ctx, current); err != nil {
		t.Fatal(err)
	}
	assertOperationState(t, scenario.fixture, scenario.op.ID, contract.Canceled, "pre-save cancellation")
	assertOwnerReleased(t, scenario.fixture, scenario.env, "pre-save cancellation")
	for _, planned := range scenario.plan {
		var count int
		var nodeName string
		if err := scenario.fixture.pool.QueryRow(scenario.fixture.ctx, `SELECT count(*),coalesce(min(node),'')
            FROM eregion_platform_container_cleanup
            WHERE deployment_id=$1 AND container_name=$2`, scenario.depID, planned.Name).Scan(&count, &nodeName); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("cleanup rows for pre-save container %s = %d, want 1", planned.Name, count)
		}
		if nodeName != scenario.env.Node {
			t.Fatalf("cleanup node for %s = %q, want %q", planned.Name, nodeName, scenario.env.Node)
		}
	}
}

func assertUnresolvablePinnedCleanupNodeRetainsOwnership(t *testing.T, scenario *preSaveScenario) {
	t.Helper()
	current, err := scenario.fixture.service.Get(scenario.fixture.ctx, testActor, scenario.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := scenario.fixture.service.reconcileDeployment(scenario.fixture.ctx, current); err == nil {
		t.Fatal("cancellation settled with an unresolvable pinned cleanup node")
	}
	assertOperationNotTerminal(t, scenario.fixture, scenario.op.ID, "unresolvable pinned cleanup node")
	assertOwnerRetained(t, scenario.fixture, scenario.env, "unresolvable pinned cleanup node")
	var count int
	if err := scenario.fixture.pool.QueryRow(scenario.fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, scenario.depID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cleanup rows were invented for an unresolvable pinned node: %d", count)
	}
}

// TestIntegrationDeploymentRetryableFailureRetainsCandidates proves that a
// retry does not replace or clean up candidates while the admitted health
// check has no durable acknowledgement.
func TestIntegrationDeploymentRetryableFailureRetainsCandidates(t *testing.T) {
	state := newRetryableCandidateTest(t)
	assertRetryableFailureRetained(t, state)
	assertUnacknowledgedHealthRetryRetained(t, state)
}

type retryableCandidateTest struct {
	fixture          *deploymentRetryFixture
	transport        *healthFaultTransport
	env              platform.Environment
	release          platform.ReleaseID
	previousRelease  platform.ReleaseID
	previousDeployID platform.DeploymentID
	op               contract.Operation
	depID            platform.DeploymentID
	plan             []platform.PlannedContainer
}

func newRetryableCandidateTest(t *testing.T) *retryableCandidateTest {
	t.Helper()
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
	state := &retryableCandidateTest{
		fixture: fixture, transport: transport, env: env,
		previousRelease: env.ServingRelease, previousDeployID: env.ServingDeploymentID,
	}
	state.release = fixture.createRelease("retry-retain", "registry.example/live:retry-retain")
	state.op = platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "retry-retain",
		map[string]any{"release_id": state.release})
	state.depID = platform.DeploymentID(deploymentForOperation(t, fixture.pool, state.op.ID))
	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, state.release, state.depID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatal("candidate plan is empty")
	}
	state.plan = plan
	if err := runPlatformAttempt(fixture.ctx, fixture.service, state.op.ID, 1); err == nil {
		t.Fatal("retryable pre-switch failure was not returned")
	}
	assertOperationNotTerminal(t, fixture, state.op.ID, "retryable pre-switch failure")
	assertOwnerPhase(t, fixture, env, deploymentOwnerPrepared, "retryable pre-switch failure")
	return state
}

func assertRetryableFailureRetained(t *testing.T, state *retryableCandidateTest) {
	t.Helper()
	for _, planned := range state.plan {
		step := containerStepID(planned.Process.Name, planned.Replica)
		if runs := state.transport.runCount(state.op.ID, step); runs != 1 {
			t.Fatalf("container step %s ran %d times before retry, want 1", step, runs)
		}
		var stepState string
		if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT state FROM eregion_operation_steps
            WHERE operation_id=$1 AND step=$2`, state.op.ID, step).Scan(&stepState); err != nil {
			t.Fatal(err)
		}
		if stepState != "completed" {
			t.Fatalf("container step %s after retryable failure = %q, want completed", step, stepState)
		}
	}
	if index := state.transport.eventIndex("run:platform-cleanup-"); index >= 0 {
		t.Fatalf("retryable pre-switch failure attempted cleanup: %v", state.transport.events)
	}
	var cleanupRows int
	if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, state.depID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("retryable pre-switch failure recorded %d cleanup rows, want 0", cleanupRows)
	}
	assertServingDeployment(t, state.fixture, state.previousRelease, state.previousDeployID, "retryable pre-switch failure")
}

func assertUnacknowledgedHealthRetryRetained(t *testing.T, state *retryableCandidateTest) {
	t.Helper()
	state.transport.failure = nil
	if err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("retry without health completion proof: %v", err)
	}
	assertOperationNotTerminal(t, state.fixture, state.op.ID, "unacknowledged health retry")
	assertOwnerRetained(t, state.fixture, state.env, "unacknowledged health retry")
	assertRetryableFailureRetained(t, state)
}

// TestIntegrationLiveCleanupIntentFailureRetainsOwner covers both live terminal
// paths: a pre-switch terminal failure and a post-switch public failure must
// persist candidate cleanup intent before settling. An insert failure keeps the
// nonterminal operation and its owner.
func TestIntegrationLiveCleanupIntentFailureRetainsOwner(t *testing.T) {
	t.Run("pre-switch terminal failure", func(t *testing.T) {
		assertPreSwitchTerminalCleanupIntent(t)
	})
	t.Run("post-switch public failure", func(t *testing.T) {
		assertPostSwitchPublicCleanupIntent(t)
	})
}

func assertPreSwitchTerminalCleanupIntent(t *testing.T) {
	t.Helper()
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
	assertOperationNotTerminal(t, fixture, op.ID, "terminal cleanup intent failure")
	assertOwnerRetained(t, fixture, env, "terminal cleanup intent failure")
	if index := transport.eventIndex("run:platform-cleanup-"); index >= 0 {
		t.Fatalf("cleanup ran before intent was durable: %v", transport.events)
	}
	dropBlock()
	transport.failure = nil
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("cleanup repair is not proof of health completion: %v", err)
	}
	assertOperationNotTerminal(t, fixture, op.ID, "health evidence still absent after cleanup repair")
	assertOwnerRetained(t, fixture, env, "health evidence still absent after cleanup repair")
}

func assertPostSwitchPublicCleanupIntent(t *testing.T) {
	t.Helper()
	fixture, _, env := newDeployedRecordingFixture(t)
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
	assertOperationNotTerminal(t, fixture, op.ID, "public cleanup intent failure")
	assertOwnerPhase(t, fixture, env, deploymentOwnerRestored, "public cleanup intent failure")
	assertServingDeployment(t, fixture, previousRelease, previousDeployment, "public cleanup intent failure")
	dropBlock()
	fixture.service.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("cleanup repair is not proof that every original effect settled: %v", err)
	}
	assertOperationNotTerminal(t, fixture, op.ID, "public cleanup intent recovery awaiting proof")
	assertOwnerRetained(t, fixture, env, "public cleanup intent recovery awaiting proof")
	assertServingDeployment(t, fixture, previousRelease, previousDeployment, "public cleanup intent recovery")
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
	_, depID, _, owner := fixture.prepareOwnedCandidate(t, key, "registry.example/live:partial-crash", "running")
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
        SET phase='prepared',route_target_deployment_id=$2 WHERE environment_id=$1`,
		env.ID, env.ServingDeploymentID); err != nil {
		t.Fatal(err)
	}
	dep, err := fixture.service.deploymentByID(fixture.ctx, depID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.persistDeploymentCleanupInstances(fixture.ctx, env, dep, dep.Instances, owner.Attempt); err != nil {
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
	state := newShutdownCandidateTest(t)
	assertShutdownRetained(t, state)
	assertShutdownRetrySucceeded(t, state)
}

type shutdownCandidateTest struct {
	fixture   *deploymentRetryFixture
	transport *healthFaultTransport
	env       platform.Environment
	release   platform.ReleaseID
	op        contract.Operation
	depID     platform.DeploymentID
	plan      []platform.PlannedContainer
}

func newShutdownCandidateTest(t *testing.T) *shutdownCandidateTest {
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
	state := &shutdownCandidateTest{fixture: fixture, transport: transport, env: env}
	state.release = fixture.createRelease("shutdown-retain", "registry.example/live:shutdown-retain")
	state.op = platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "shutdown-retain",
		map[string]any{"release_id": state.release})
	state.depID = platform.DeploymentID(deploymentForOperation(t, fixture.pool, state.op.ID))
	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, state.release, state.depID)
	if err != nil {
		t.Fatal(err)
	}
	state.plan = plan
	transport.failure = context.Canceled
	if err := runPlatformAttempt(fixture.ctx, fixture.service, state.op.ID, 1); err == nil {
		t.Fatal("worker shutdown did not return an error")
	}
	assertOperationNotTerminal(t, fixture, state.op.ID, "worker shutdown")
	assertOwnerPhase(t, fixture, env, deploymentOwnerPrepared, "worker shutdown")
	return state
}

func assertShutdownRetained(t *testing.T, state *shutdownCandidateTest) {
	t.Helper()
	for _, planned := range state.plan {
		step := containerStepID(planned.Process.Name, planned.Replica)
		if runs := state.transport.runCount(state.op.ID, step); runs != 1 {
			t.Fatalf("container step %s ran %d times before retry, want 1", step, runs)
		}
		var stepState string
		if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT state FROM eregion_operation_steps
            WHERE operation_id=$1 AND step=$2`, state.op.ID, step).Scan(&stepState); err != nil {
			t.Fatal(err)
		}
		if stepState != "completed" {
			t.Fatalf("container step %s after worker shutdown = %q, want completed", step, stepState)
		}
	}
	if index := state.transport.eventIndex("run:platform-cleanup-"); index >= 0 {
		t.Fatalf("worker shutdown ran candidate cleanup: %v", state.transport.events)
	}
	var cleanupRows int
	if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, state.depID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("worker shutdown recorded %d cleanup rows, want 0", cleanupRows)
	}
}

func assertShutdownRetrySucceeded(t *testing.T, state *shutdownCandidateTest) {
	t.Helper()
	state.transport.failure = nil
	if err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 1); err != nil {
		t.Fatalf("retry after worker shutdown: %v", err)
	}
	assertOperationState(t, state.fixture, state.op.ID, contract.Succeeded, "retry after worker shutdown")
	for _, planned := range state.plan {
		step := containerStepID(planned.Process.Name, planned.Replica)
		if runs := state.transport.runCount(state.op.ID, step); runs != 1 {
			t.Fatalf("container step %s ran %d times across shutdown retry, want 1", step, runs)
		}
	}
	assertServingDeployment(t, state.fixture, state.release, state.depID, "retry after worker shutdown")
}

// TestIntegrationCleanupEvidenceReadFailureRetainsOwner covers a disappearing
// container-step evidence read before a pre-save cancellation settles.
func TestIntegrationCleanupEvidenceReadFailureRetainsOwner(t *testing.T) {
	scenario := newCleanupEvidenceReadScenario(t)
	current, err := scenario.fixture.service.Get(scenario.fixture.ctx, testActor, scenario.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := scenario.fixture.service.reconcileDeployment(scenario.fixture.ctx, current); err == nil {
		t.Fatal("cancellation settled without a readable cleanup evidence set")
	}
	assertOperationNotTerminal(t, scenario.fixture, scenario.op.ID, "cleanup evidence read failure")
	assertOwnerRetained(t, scenario.fixture, scenario.env, "cleanup evidence read failure")
	assertCleanupRowCount(t, scenario.fixture, scenario.depID, 0, "cleanup evidence read failure")

	scenario.fixture.service.deploymentCleanupEvidence = nil
	current, err = scenario.fixture.service.Get(scenario.fixture.ctx, testActor, scenario.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := scenario.fixture.service.reconcileDeployment(scenario.fixture.ctx, current); err != nil {
		t.Fatalf("cancellation retry after evidence repair: %v", err)
	}
	assertOperationState(t, scenario.fixture, scenario.op.ID, contract.Canceled, "cleanup evidence repaired")
	assertOwnerReleased(t, scenario.fixture, scenario.env, "cleanup evidence repaired")
	assertCleanupRowCount(t, scenario.fixture, scenario.depID, 0, "post-settlement cleanup removal")
	if index := scenario.transport.eventIndex("run:" + cleanupStepID(scenario.plan[0].Name)); index < 0 {
		t.Fatalf("post-settlement cleanup did not run: %v", scenario.transport.events)
	}
}

type cleanupEvidenceReadScenario struct {
	fixture   *deploymentRetryFixture
	transport *recordingDeployTransport
	env       platform.Environment
	op        contract.Operation
	depID     platform.DeploymentID
	plan      []platform.PlannedContainer
}

func newCleanupEvidenceReadScenario(t *testing.T) *cleanupEvidenceReadScenario {
	t.Helper()
	fixture, transport, env := newDeployedRecordingFixture(t)
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
	return &cleanupEvidenceReadScenario{fixture: fixture, transport: transport, env: env, op: op, depID: depID, plan: plan}
}

func assertCleanupRowCount(t *testing.T, fixture *deploymentRetryFixture, depID platform.DeploymentID, want int, context string) {
	t.Helper()
	var rows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
        WHERE deployment_id=$1`, depID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != want {
		t.Fatalf("%s: cleanup rows = %d, want %d", context, rows, want)
	}
}

// An unknown prior effect must retain the owner even when a subsequent
// transport would report no journal record.
func TestIntegrationDeploymentTerminalRetryWithRetainedCandidates(t *testing.T) {
	t.Run("placement failure retains owner while effect is unknown", func(t *testing.T) {
		assertPlacementFailureRetainsUnknownEffect(t, setupTerminalRetryCandidates(t))
	})
	t.Run("replacement transport cannot prove cleanup is safe", func(t *testing.T) {
		assertReplacementTransportRetainsOwner(t, setupTerminalRetryCandidates(t))
	})
}

type terminalRetryCandidateTest struct {
	fixture   *deploymentRetryFixture
	transport *healthFaultTransport
	op        contract.Operation
	plan      []platform.PlannedContainer
}

func setupTerminalRetryCandidates(t *testing.T) *terminalRetryCandidateTest {
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
	assertOperationNotTerminal(t, fixture, op.ID, "retryable failure before terminal retry")
	assertOwnerPhase(t, fixture, env, deploymentOwnerPrepared, "retryable failure before terminal retry")
	transport.failure = nil
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_nodes
        SET capabilities=ARRAY['buildpacks'] WHERE name=$1`, env.Node); err != nil {
		t.Fatal(err)
	}
	fixture.service.resolver = nil
	transport.events = nil
	return &terminalRetryCandidateTest{fixture: fixture, transport: transport, op: op, plan: plan}
}

func assertPlacementFailureRetainsUnknownEffect(t *testing.T, state *terminalRetryCandidateTest) {
	t.Helper()
	err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 1)
	if err == nil {
		t.Fatal("placement retry unexpectedly succeeded while a detached effect was unknown")
	}
	if fault := contract.Classify(err); fault.Code != contract.Unavailable {
		t.Fatalf("placement retry error = %s, %v; want unavailable", fault.Code, err)
	}
	assertOperationNotTerminal(t, state.fixture, state.op.ID, "placement retry")
	env := state.fixture.environment(t)
	assertOwnerRetained(t, state.fixture, env, "placement retry")
	if index := state.transport.eventIndex("run:" + cleanupStepID(state.plan[0].Name)); index >= 0 {
		t.Fatalf("cleanup ran while a detached effect was unknown: %v", state.transport.events)
	}
	for _, planned := range state.plan {
		var count int
		if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT count(*)
            FROM eregion_platform_container_cleanup WHERE container_name=$1`, planned.Name).
			Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("cleanup rows for retained container %s = %d, want 0 while the effect is unknown", planned.Name, count)
		}
	}
}

func assertReplacementTransportRetainsOwner(t *testing.T, state *terminalRetryCandidateTest) {
	t.Helper()
	env := state.fixture.environment(t)
	// Absence from a replacement transport cannot account for the original
	// dispatch. The live cleanup tests exercise the separate insert boundary.
	absent := &fixedStatusTransport{status: engine.StatusAbsent}
	state.fixture.service.resolver = newTestNodeResolver(t, state.fixture.pool, map[string]node.Transport{env.Node: absent})
	err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 2)
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("retry against unauthenticated replacement: %v", err)
	}
	if absent.callCount() != 0 {
		t.Fatal("retry asked the replacement node to prove the original effect")
	}
	assertOperationNotTerminal(t, state.fixture, state.op.ID, "replacement health transport")
	assertOwnerRetained(t, state.fixture, env, "replacement health transport")
}

// TestIntegrationOwnerRestoreTargetCandidateFailsClosed keeps the unsafe
// invariant nonterminal: a live retry and terminal owner recovery both retain
// the owner instead of restoring or cleaning the candidate as its own
// predecessor.
func TestIntegrationOwnerRestoreTargetCandidateFailsClosed(t *testing.T) {
	for _, phase := range []deploymentOwnerPhase{deploymentOwnerSwitching, deploymentOwnerRestoring} {
		t.Run(string(phase), func(t *testing.T) {
			assertUnsafeRestoreTargetFailsClosed(t, phase)
		})
	}
}

func assertUnsafeRestoreTargetFailsClosed(t *testing.T, phase deploymentOwnerPhase) {
	t.Helper()
	fixture, _, env := newDeployedRecordingFixture(t)
	previousRelease, previousDeployment := env.ServingRelease, env.ServingDeploymentID
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, "unsafe-"+string(phase), "registry.example/live:unsafe", "running")
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployment_owners
        SET phase=$2,restore_deployment_id=$3 WHERE environment_id=$1`, env.ID, string(phase), depID); err != nil {
		t.Fatal(err)
	}
	if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
		t.Fatal("unsafe restore target was accepted as a live retry")
	}
	assertOperationNotTerminal(t, fixture, op.ID, "unsafe live invariant")
	assertUnsafeOwnerTarget(t, fixture, env, depID, "unsafe live retry")
	// The terminal owner sweep must enforce the same invariant instead of
	// bypassing the live guard.
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations
        SET state='failed',error_code='unavailable' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeploymentOwners(fixture.ctx); err == nil {
		t.Fatal("terminal owner sweep settled an unsafe restore target")
	}
	assertUnsafeOwnerTarget(t, fixture, env, depID, "unsafe terminal sweep")
	assertServingDeployment(t, fixture, previousRelease, previousDeployment, "unsafe owner recovery")
}

func assertUnsafeOwnerTarget(t *testing.T, fixture *deploymentRetryFixture, env platform.Environment, depID platform.DeploymentID, context string) {
	t.Helper()
	owner, has := ownerForFixture(t, fixture, env)
	if !has || owner.RestoreDeploymentID != depID {
		t.Fatalf("%s: owner = %+v, has=%t, want restore target %s", context, owner, has, depID)
	}
}

// TestIntegrationDeploymentRenewalRepairsCrashGapPredecessor recreates the old
// split-transaction crash gap: an owner frozen to the actual predecessor while
// the deployment row still names a stale one. Renewal must repair the deployment
// binding from the owner before recovery effects, so cleanup targets the real
// predecessor.
func TestIntegrationDeploymentRenewalRepairsCrashGapPredecessor(t *testing.T) {
	state := newCrashGapScenario(t)
	assertCrashGapRecovered(t, state)
}

type crashGapScenario struct {
	fixture             *deploymentRetryFixture
	transport           *recordingDeployTransport
	env                 platform.Environment
	staleRelease        platform.ReleaseID
	staleDeployment     platform.DeploymentID
	candidateDeployment platform.DeploymentID
	predecessorRelease  platform.ReleaseID
	predecessorDeploy   platform.DeploymentID
	predecessorRow      platform.Deployment
	opC                 contract.Operation
	releaseC            platform.ReleaseID
}

func newCrashGapScenario(t *testing.T) *crashGapScenario {
	t.Helper()
	fixture, transport, env := newDeployedRecordingFixture(t)
	state := &crashGapScenario{
		fixture: fixture, transport: transport, env: env,
		staleRelease: env.ServingRelease, staleDeployment: env.ServingDeploymentID,
	}
	releaseB := fixture.createRelease("gap-b", "registry.example/live:gap-b")
	opB := platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "gap-b", map[string]any{"release_id": releaseB})
	state.releaseC = fixture.createRelease("gap-c", "registry.example/live:gap-c")
	state.opC = platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", "gap-c", map[string]any{"release_id": state.releaseC})

	// Two queued deploys: B runs first and becomes the true predecessor of C.
	fixture.run(opB)
	env = fixture.environment(t)
	state.predecessorRelease, state.predecessorDeploy = env.ServingRelease, env.ServingDeploymentID
	state.predecessorRow = previousDeploymentFor(t, fixture, env)
	if state.predecessorDeploy == state.staleDeployment {
		t.Fatal("B did not become serving before C")
	}

	// Simulate the crash gap for C: its owner froze B, but its deployment row
	// still names the older A predecessor.
	state.candidateDeployment = platform.DeploymentID(deploymentForOperation(t, fixture.pool, state.opC.ID))
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments
        SET state='running',previous_release_id=$2,previous_deployment_id=$3 WHERE id=$1`,
		state.candidateDeployment, state.staleRelease, state.staleDeployment); err != nil {
		t.Fatal(err)
	}
	insertCompletedStep(t, fixture.pool, state.opC.ID, deploymentRouteStep, strings.Repeat("a", 64))
	insertCompletedStep(t, fixture.pool, state.opC.ID, deploymentPublicStep, strings.Repeat("b", 64))
	insertDeploymentOwner(t, fixture.pool, env, deploymentOwner{
		EnvironmentID: env.ID, OperationID: state.opC.ID, DeploymentID: state.candidateDeployment, Attempt: 1,
		Phase: deploymentOwnerServing, RouteTarget: state.candidateDeployment,
		RestoreReleaseID: state.predecessorRelease, RestoreDeploymentID: state.predecessorDeploy,
	})
	var beforeRelease, beforeDeployment string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
        FROM eregion_platform_deployments WHERE id=$1`, state.candidateDeployment).Scan(&beforeRelease, &beforeDeployment); err != nil {
		t.Fatal(err)
	}
	if beforeRelease != string(state.staleRelease) || beforeDeployment != string(state.staleDeployment) {
		t.Fatalf("crash-gap fixture previous = %s/%s, want stale %s/%s", beforeRelease, beforeDeployment, state.staleRelease, state.staleDeployment)
	}
	return state
}

func assertCrashGapRecovered(t *testing.T, state *crashGapScenario) {
	t.Helper()
	state.transport.events = nil
	if err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.opC.ID, 1); err != nil {
		t.Fatalf("C attempt after crash gap: %v", err)
	}
	assertOperationState(t, state.fixture, state.opC.ID, contract.Succeeded, "crash-gap recovery")
	var afterRelease, afterDeployment string
	if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
        FROM eregion_platform_deployments WHERE id=$1`, state.candidateDeployment).Scan(&afterRelease, &afterDeployment); err != nil {
		t.Fatal(err)
	}
	if afterRelease != string(state.predecessorRelease) || afterDeployment != string(state.predecessorDeploy) {
		t.Fatalf("renewal bound C previous = %s/%s, want owner predecessor %s/%s", afterRelease, afterDeployment, state.predecessorRelease, state.predecessorDeploy)
	}
	assertServingDeployment(t, state.fixture, state.releaseC, state.candidateDeployment, "crash-gap recovery")
	assertCrashGapCleanup(t, state)
}

func assertCrashGapCleanup(t *testing.T, state *crashGapScenario) {
	t.Helper()
	if len(state.predecessorRow.Instances) == 0 {
		t.Fatal("true predecessor has no instances to clean up")
	}
	for _, instance := range state.predecessorRow.Instances {
		if instance.Name == "" {
			continue
		}
		var deployment, reason string
		if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT deployment_id,reason FROM eregion_platform_container_cleanup
            WHERE container_name=$1`, instance.Name).Scan(&deployment, &reason); err != nil {
			t.Fatalf("true predecessor cleanup intent %s: %v", instance.Name, err)
		}
		if deployment != string(state.predecessorDeploy) || reason != "previous" {
			t.Fatalf("cleanup intent for %s = %s/%s, want %s/previous", instance.Name, deployment, reason, state.predecessorDeploy)
		}
	}
	// Maintenance is global: B may already have queued A for removal. Only C's
	// exact previous linkage and B's cleanup row identify C's intended target.
	if err := state.fixture.service.retryDueContainerCleanups(state.fixture.ctx); err != nil {
		t.Fatalf("maintenance cleanup: %v", err)
	}
	for _, instance := range state.predecessorRow.Instances {
		if instance.Name == "" {
			continue
		}
		if state.transport.eventIndex("run:"+cleanupStepID(instance.Name)) < 0 {
			t.Fatalf("cleanup did not target true predecessor %s; events=%v", instance.Name, state.transport.events)
		}
		var remaining int
		if err := state.fixture.pool.QueryRow(state.fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup
            WHERE container_name=$1`, instance.Name).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("true predecessor cleanup row %s remains", instance.Name)
		}
	}
}

// TestIntegrationWaitingOwnerReloadsAfterStaleSettlement covers a terminal
// failed owner whose recovery restores the real predecessor O while the waiting
// operation loaded an environment snapshot that still names the failed candidate
// A. Every reacquisition path must reload the environment and freeze O.
func TestIntegrationWaitingOwnerReloadsAfterStaleSettlement(t *testing.T) {
	t.Run("runDeployment", func(t *testing.T) {
		state := setupStaleSettlement(t, "stale-run")
		if err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 1); err != nil {
			t.Fatalf("B attempt: %v", err)
		}
		assertOperationState(t, state.fixture, state.op.ID, contract.Succeeded, "B deployment after stale settlement")
		state.assertPredecessor(t)
	})

	t.Run("cancellation", func(t *testing.T) {
		state := setupStaleSettlement(t, "stale-cancel")
		if _, err := state.fixture.pool.Exec(state.fixture.ctx, `UPDATE eregion_operations
            SET state='cancelling',attempt=1 WHERE id=$1`, state.op.ID); err != nil {
			t.Fatal(err)
		}
		if err := runPlatformAttempt(state.fixture.ctx, state.fixture.service, state.op.ID, 1); err != nil {
			t.Fatalf("B cancellation: %v", err)
		}
		assertOperationState(t, state.fixture, state.op.ID, contract.Canceled, "B cancellation after stale settlement")
		state.assertPredecessor(t)
		assertServingDeployment(t, state.fixture, state.oRelease, state.oDeployment, "B cancellation after stale settlement")
	})

	t.Run("reconciliation", func(t *testing.T) {
		state := setupStaleSettlement(t, "stale-reconcile")
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
		assertOperationState(t, state.fixture, state.op.ID, contract.Failed, "B reconciliation without lease")
		state.assertPredecessor(t)
		assertServingDeployment(t, state.fixture, state.oRelease, state.oDeployment, "B reconciliation after stale settlement")
	})
}

type staleSettlementFixture struct {
	fixture     *deploymentRetryFixture
	env         platform.Environment
	op          contract.Operation
	dep         platform.DeploymentID
	oRelease    platform.ReleaseID
	oDeployment platform.DeploymentID
}

func setupStaleSettlement(t *testing.T, key string) *staleSettlementFixture {
	t.Helper()
	fixture, _, env := newDeployedRecordingFixture(t)
	state := &staleSettlementFixture{
		fixture: fixture, env: env,
		oRelease: env.ServingRelease, oDeployment: env.ServingDeploymentID,
	}

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
		Phase: deploymentOwnerPrepared, RouteTarget: state.oDeployment,
		RestoreReleaseID: state.oRelease, RestoreDeploymentID: state.oDeployment,
	})
	releaseB := fixture.createRelease(key+"-b", "registry.example/live:"+key+"-b")
	state.op = platformSubmit(t, fixture.service, platform.KindDeploy, "env/live/production", key+"-b",
		map[string]any{"release_id": releaseB})
	state.dep = platform.DeploymentID(deploymentForOperation(t, fixture.pool, state.op.ID))
	return state
}

func (s *staleSettlementFixture) assertPredecessor(t *testing.T) {
	t.Helper()
	var previousRelease, previousDeployment string
	if err := s.fixture.pool.QueryRow(s.fixture.ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
        FROM eregion_platform_deployments WHERE id=$1`, s.dep).Scan(&previousRelease, &previousDeployment); err != nil {
		t.Fatal(err)
	}
	if previousRelease != string(s.oRelease) || previousDeployment != string(s.oDeployment) {
		t.Fatalf("%s previous = %s/%s, want reloaded serving predecessor %s/%s", s.dep, previousRelease, previousDeployment, s.oRelease, s.oDeployment)
	}
}
