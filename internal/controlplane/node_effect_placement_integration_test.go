package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// fixedStatusTransport answers status observations with a fixed state.
type fixedStatusTransport struct {
	status engine.Status
	mu     sync.Mutex
	calls  int
}

func (*fixedStatusTransport) Name() string { return "fixed-status" }

func (t *fixedStatusTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if request.Method != node.MethodStatus {
		return node.Response{}, errors.New("unexpected node method")
	}
	return node.Response{Status: t.status, ExecutionID: request.Execution.ExecutionID()}, nil
}

func (t *fixedStatusTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// unknownStatusTransport loses the SSH response while observing the effect.
// The guard must treat that as an unknown outcome and retain ownership.
type unknownStatusTransport struct {
	mu    sync.Mutex
	calls int
}

func (*unknownStatusTransport) Name() string { return "unknown-status" }

func (t *unknownStatusTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if request.Method != node.MethodStatus {
		return node.Response{}, errors.New("unexpected node method")
	}
	return node.Response{}, fmt.Errorf("%w: simulated lost status response", node.ErrStreamLost)
}

func (t *unknownStatusTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// newTestNodeResolver wires fixed transports into the real resolver cache, so
// placement lookup and node-name resolution still run.
func newTestNodeResolver(t *testing.T, pool *pgxpool.Pool, transports map[string]node.Transport) *NodeResolver {
	t.Helper()
	resolver := &NodeResolver{cache: map[string]*cachedTransport{}}
	for name, transport := range transports {
		if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_platform_nodes
            (name,roles,capabilities,enrollment_state,transport,ssh_address,ssh_user,ssh_host_key)
            VALUES($1,ARRAY['application'],ARRAY['docker'],'enrolled','ssh','127.0.0.1:1','test','test')
            ON CONFLICT (name) DO UPDATE SET enrollment_state='enrolled',transport='ssh',
                ssh_address='127.0.0.1:1',ssh_user='test',ssh_host_key='test'`, name); err != nil {
			t.Fatal(err)
		}
		rec, err := loadNodeConn(context.Background(), pool, name)
		if err != nil {
			t.Fatal(err)
		}
		resolver.cache[name] = &cachedTransport{
			key: connectionKey(rec, false), transport: transport, closer: func() {},
		}
	}
	return resolver
}

func insertRunningNodeStep(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID, step string) {
	t.Helper()
	execution := engine.Execution{OperationID: operationID, Attempt: 1, Step: step}
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_operation_steps
        (operation_id,step,inputs_hash,state,attempt,execution_id,started_at)
        VALUES($1,$2,$3,'running',1,$4,clock_timestamp())`,
		operationID, step, strings.Repeat("a", 64), execution.ExecutionID()); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationDeploymentEffectGuardUsesPinnedNodeAfterEnvironmentMove pins
// a deployment to node-a, moves the environment to node-b, and proves the
// cancellation guard observes the live effect on node-a. Observing the current
// environment node alone would read an absent effect on node-b and cancel.
func TestIntegrationDeploymentEffectGuardUsesPinnedNodeAfterEnvironmentMove(t *testing.T) {
	fixture := newDeploymentRetryFixtureWithTransport(t, &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()})
	fixture.deploy(t)
	env := fixture.environment(t)
	if env.RevisionID == "" {
		t.Fatal("fixture environment has no pinned revision")
	}
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, "pinned-guard", "registry.example/live:pinned-guard", "running")
	insertRunningNodeStep(t, fixture.pool, op.ID, deploymentNetworkStep)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions SET runtime='{"node":"node-a"}'::jsonb WHERE id=$1`, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities)
        VALUES('node-b',ARRAY['application'],ARRAY['docker']) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environments SET node_name='node-b' WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	liveA := &fixedStatusTransport{status: engine.StatusRunning}
	absentB := &fixedStatusTransport{status: engine.StatusAbsent}
	fixture.service.resolver = newTestNodeResolver(t, fixture.pool, map[string]node.Transport{
		"node-a": liveA,
		"node-b": absentB,
	})
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.service.reconcileDeployment(fixture.ctx, current)
	if err == nil {
		t.Fatal("cancellation settled while the pinned node had a live effect")
	}
	if code := contract.Classify(err).Code; code != contract.Unavailable {
		t.Fatalf("guard error code = %s, want unavailable: %v", code, err)
	}
	if liveA.callCount() == 0 {
		t.Fatal("the guard did not observe the pinned node")
	}
	if absentB.callCount() != 0 {
		t.Fatal("the guard observed the moved-to environment node instead of the pinned node")
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State.Terminal() {
		t.Fatalf("pinned live effect terminalized cancellation: %+v", final)
	}
	if _, has := ownerForFixture(t, fixture, env); !has {
		t.Fatal("pinned live effect released the deployment owner")
	}
}

// TestIntegrationDeploymentReadyImageGuardUsesPinnedNodeAfterEnvironmentMove
// covers the registry materialization effect added after the original guard
// table: a detached docker pull for a ready release must be observed on the
// deployment's pinned node before cancellation settles.
func TestIntegrationDeploymentReadyImageGuardUsesPinnedNodeAfterEnvironmentMove(t *testing.T) {
	fixture := newDeploymentRetryFixtureWithTransport(t, &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()})
	fixture.deploy(t)
	env := fixture.environment(t)
	if env.RevisionID == "" {
		t.Fatal("fixture environment has no pinned revision")
	}
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, "ready-image-pinned", "registry.example/live:ready-image-pinned", "running")
	insertRunningNodeStep(t, fixture.pool, op.ID, deploymentReadyImageStep)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environment_revisions SET runtime='{"node":"node-a"}'::jsonb WHERE id=$1`, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities)
        VALUES('node-b',ARRAY['application'],ARRAY['docker']) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_environments SET node_name='node-b' WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	liveA := &fixedStatusTransport{status: engine.StatusRunning}
	absentB := &fixedStatusTransport{status: engine.StatusAbsent}
	fixture.service.resolver = newTestNodeResolver(t, fixture.pool, map[string]node.Transport{
		"node-a": liveA,
		"node-b": absentB,
	})
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.service.reconcileDeployment(fixture.ctx, current)
	if err == nil {
		t.Fatal("cancellation settled while the pinned node had a live ready-image effect")
	}
	if code := contract.Classify(err).Code; code != contract.Unavailable {
		t.Fatalf("ready-image guard error code = %s, want unavailable: %v", code, err)
	}
	if liveA.callCount() == 0 {
		t.Fatal("the ready-image guard did not observe the pinned node")
	}
	if absentB.callCount() != 0 {
		t.Fatal("the ready-image guard observed the moved-to environment node instead of the pinned node")
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State.Terminal() {
		t.Fatalf("pinned live ready-image effect terminalized cancellation: %+v", final)
	}
	if _, has := ownerForFixture(t, fixture, env); !has {
		t.Fatal("pinned live ready-image effect released the deployment owner")
	}
}

// TestIntegrationDeploymentReadyImageUnknownStatusRetainsOwnership proves a
// lost status response for the registry materialization step is an unknown
// outcome: cancellation must not terminalize the operation or release the
// deployment owner until the effect is safely observed.
func TestIntegrationDeploymentReadyImageUnknownStatusRetainsOwnership(t *testing.T) {
	fixture := newDeploymentRetryFixtureWithTransport(t, &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()})
	fixture.deploy(t)
	env := fixture.environment(t)
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, "ready-image-unknown", "registry.example/live:ready-image-unknown", "running")
	insertRunningNodeStep(t, fixture.pool, op.ID, deploymentReadyImageStep)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	unknown := &unknownStatusTransport{}
	fixture.service.resolver = newTestNodeResolver(t, fixture.pool, map[string]node.Transport{env.Node: unknown})
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileDeployment(fixture.ctx, current); err == nil {
		t.Fatal("cancellation settled with an unknown ready-image outcome")
	}
	if unknown.callCount() == 0 {
		t.Fatal("the ready-image guard did not observe the pinned node")
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State.Terminal() {
		t.Fatalf("unknown ready-image outcome terminalized cancellation: %+v", final)
	}
	if _, has := ownerForFixture(t, fixture, env); !has {
		t.Fatal("unknown ready-image outcome released the deployment owner")
	}
}

// TestIntegrationDeploymentFailureCleanupWaitsForReadyImageEffect exercises the
// terminalization path with the registry materialization step, so a lost pull
// cannot persist cleanup or settle while the effect may still be live.
func TestIntegrationDeploymentFailureCleanupWaitsForReadyImageEffect(t *testing.T) {
	fixture := newDeploymentRetryFixtureWithTransport(t, &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()})
	fixture.deploy(t)
	env := fixture.environment(t)
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, "ready-image-cleanup", "registry.example/live:ready-image-cleanup", "running")
	insertRunningNodeStep(t, fixture.pool, op.ID, deploymentReadyImageStep)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	live := &fixedStatusTransport{status: engine.StatusRunning}
	fixture.service.resolver = newTestNodeResolver(t, fixture.pool, map[string]node.Transport{env.Node: live})
	dep, err := fixture.service.deploymentByID(fixture.ctx, depID)
	if err != nil {
		t.Fatal(err)
	}
	worker := &platformWorker{service: fixture.service}
	err = worker.finishDeploymentFailure(fixture.ctx, nil, op.ID, 1, env, dep, nil, errPlatformCancelled)
	if err == nil {
		t.Fatal("terminal cleanup proceeded while a ready-image effect was live")
	}
	if code := contract.Classify(err).Code; code != contract.Unavailable {
		t.Fatalf("ready-image cleanup guard code = %s, want unavailable: %v", code, err)
	}
	var cleanupRows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup WHERE deployment_id=$1`, depID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("live ready-image effect recorded %d cleanup row(s)", cleanupRows)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("live ready-image effect terminalized the operation: %+v", current)
	}
	if _, has := ownerForFixture(t, fixture, env); !has {
		t.Fatal("live ready-image effect released the deployment owner")
	}
}

// TestIntegrationDeploymentFailureCleanupWaitsForLivePinnedEffect exercises
// the terminalization path itself: a cancellation/terminal cleanup cause must
// not persist cleanup or settle while a live pinned effect remains.
func TestIntegrationDeploymentFailureCleanupWaitsForLivePinnedEffect(t *testing.T) {
	fixture := newDeploymentRetryFixtureWithTransport(t, &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()})
	fixture.deploy(t)
	env := fixture.environment(t)
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, "live-failure", "registry.example/live:live-failure", "running")
	insertRunningNodeStep(t, fixture.pool, op.ID, deploymentNetworkStep)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET revision_id=$2 WHERE id=$1`, depID, env.RevisionID); err != nil {
		t.Fatal(err)
	}
	live := &fixedStatusTransport{status: engine.StatusRunning}
	fixture.service.resolver = newTestNodeResolver(t, fixture.pool, map[string]node.Transport{env.Node: live})
	dep, err := fixture.service.deploymentByID(fixture.ctx, depID)
	if err != nil {
		t.Fatal(err)
	}
	worker := &platformWorker{service: fixture.service}
	err = worker.finishDeploymentFailure(fixture.ctx, nil, op.ID, 1, env, dep, nil, errPlatformCancelled)
	if err == nil {
		t.Fatal("terminal cleanup proceeded while a pinned effect was live")
	}
	if code := contract.Classify(err).Code; code != contract.Unavailable {
		t.Fatalf("cleanup guard code = %s, want unavailable: %v", code, err)
	}
	var cleanupRows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup WHERE deployment_id=$1`, depID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("live effect recorded %d cleanup row(s)", cleanupRows)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("live effect terminalized the operation: %+v", current)
	}
	if _, has := ownerForFixture(t, fixture, env); !has {
		t.Fatal("live effect released the deployment owner")
	}
}

// cancelLostPackTransport starts the pack step, accepts cancellation, and then
// loses the SSH response. The node remains live and its later status proves it.
type cancelLostPackTransport struct {
	*scriptedDeployTransport
	pool       *pgxpool.Pool
	lost       bool
	mu         sync.Mutex
	dispatched bool
}

func (t *cancelLostPackTransport) Call(ctx context.Context, request node.Request, onEvent func(node.Event)) (node.Response, error) {
	if request.Execution.Step != deploymentPackStep {
		return t.scriptedDeployTransport.Call(ctx, request, onEvent)
	}
	t.mu.Lock()
	dispatched := t.dispatched
	t.mu.Unlock()
	switch request.Method {
	case node.MethodStatus:
		if dispatched {
			return node.Response{Status: engine.StatusRunning, ExecutionID: request.Execution.ExecutionID()}, nil
		}
		return node.Response{Status: engine.StatusAbsent, ExecutionID: request.Execution.ExecutionID()}, nil
	case node.MethodRun:
		t.mu.Lock()
		t.dispatched = true
		t.mu.Unlock()
		if _, err := t.pool.Exec(ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, request.Execution.OperationID); err != nil {
			return node.Response{}, err
		}
		if t.lost {
			return node.Response{}, fmt.Errorf("%w: simulated lost pack response", node.ErrStreamLost)
		}
		return node.Response{Status: engine.StatusFailed, Attempt: request.Execution.Attempt,
			ExecutionID: request.Execution.ExecutionID(), Detail: "known pack failure"}, nil
	default:
		return node.Response{}, errors.New("unexpected node method")
	}
}

func newBuildCancellationFixture(t *testing.T, transport node.Transport) (*Service, *pgxpool.Pool, contract.Operation) {
	t.Helper()
	pool, _ := testPool(t, true)
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: transport, NodeStateDir: t.TempDir(), LocalNodeName: "local",
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.routeSync = func(context.Context) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }
	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) { return submittedGitCommit, nil }
	ctx := context.Background()
	for _, op := range []contract.Operation{
		platformSubmit(t, s, platform.KindNodeRegister, "platform", "build-cancel-node", map[string]any{
			"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
			"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
		}),
		platformSubmit(t, s, platform.KindAppCreate, "platform", "build-cancel-app", map[string]any{"name": "cancel"}),
		platformSubmit(t, s, platform.KindEnvironmentCreate, "app/cancel", "build-cancel-env", map[string]any{
			"name": "production", "node": "local", "container_port": 80,
			"health": map[string]any{"timeout_seconds": 2},
			"domain": "cancel.example.com", "domain_primary": true,
			"source": map[string]any{
				"kind": "git", "url": "https://github.com/acme/private.git",
				"ref": submittedGitCommit, "builder": "paketobuildpacks/builder-jammy-base",
			},
		}),
	} {
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatalf("%s setup attempt: %v", op.Kind, err)
		}
	}
	s.resolver = newTestNodeResolver(t, pool, map[string]node.Transport{"local": transport})
	deploy := platformSubmit(t, s, platform.KindDeploy, "env/cancel/production", "build-cancel-deploy", map[string]any{})
	return s, pool, deploy
}

func TestIntegrationCancellationLostBuildResponsePreservesUnknown(t *testing.T) {
	transport := &cancelLostPackTransport{scriptedDeployTransport: newScriptedDeployTransport(), lost: true}
	s, pool, deploy := newBuildCancellationFixture(t, transport)
	transport.pool = pool
	if err := runPlatformAttempt(context.Background(), s, deploy.ID, 1); err == nil {
		t.Fatal("a lost build response was reported as a completed attempt")
	} else if errors.Is(err, errPlatformCancelled) {
		t.Fatalf("a lost build response was mislabeled as a cancellation boundary: %v", err)
	} else if !errors.Is(err, errNodeEffectOutcomeUnknown) {
		t.Fatalf("lost build response = %v, want unknown-outcome classification", err)
	}
	current, err := s.Get(context.Background(), testActor, deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("lost build response terminalized: %+v", current)
	}
	var deploymentState, buildState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM eregion_platform_deployments WHERE operation_id=$1`, deploy.ID).Scan(&deploymentState); err != nil {
		t.Fatal(err)
	}
	if deploymentState == "cancelled" || deploymentState == "failed" || deploymentState == "succeeded" {
		t.Fatalf("deployment state after lost response = %q", deploymentState)
	}
	if err := pool.QueryRow(context.Background(), `SELECT b.state FROM eregion_platform_builds b JOIN eregion_platform_releases r ON r.build_id=b.id
        WHERE r.operation_id=$1`, deploy.ID).Scan(&buildState); err != nil {
		t.Fatal(err)
	}
	if buildState == "failed" || buildState == "cancelled" {
		t.Fatalf("lost build response recorded %q; the node may still be building", buildState)
	}
}

func TestIntegrationCancellationKnownBuildFailureStillWaitsForLiveEffect(t *testing.T) {
	transport := &cancelLostPackTransport{scriptedDeployTransport: newScriptedDeployTransport()}
	s, pool, deploy := newBuildCancellationFixture(t, transport)
	transport.pool = pool
	if err := runPlatformAttempt(context.Background(), s, deploy.ID, 1); err == nil {
		t.Fatal("known build failure was reported as success")
	} else if code := contract.Classify(err).Code; code != contract.Unavailable {
		t.Fatalf("guard error code = %s, want unavailable: %v", code, err)
	}
	current, err := s.Get(context.Background(), testActor, deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("live pack effect terminalized cancellation: %+v", current)
	}
	var cleanupRows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_container_cleanup c
        JOIN eregion_platform_deployments d ON d.id=c.deployment_id WHERE d.operation_id=$1`, deploy.ID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("live pack effect recorded %d cleanup row(s)", cleanupRows)
	}
}
