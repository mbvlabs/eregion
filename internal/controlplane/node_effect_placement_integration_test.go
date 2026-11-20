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
	"github.com/mbvlabs/eregion/internal/entities"
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
	resolver := &NodeResolver{nodes: newTestEntities(t, pool).Nodes(), cache: map[string]*cachedTransport{}}
	for name, transport := range transports {
		if err := insertFixtureNode(context.Background(), pool, name, []string{"application"}, []string{"docker"}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE eregion_platform_nodes SET
            enrollment_state='enrolled',transport='ssh',ssh_address='127.0.0.1:1',ssh_user='test',ssh_host_key='test'
            WHERE name=$1`, name); err != nil {
			t.Fatal(err)
		}
		rec, err := loadNodeConn(context.Background(), newTestEntities(t, pool).Nodes(), name)
		if err != nil {
			t.Fatal(err)
		}
		resolver.cache[name] = &cachedTransport{
			key: connectionKey(rec, false), transport: transport, closer: func() {},
		}
	}
	return resolver
}

// appendEnvironmentNodeRevision appends a new immutable environment revision
// with the requested runtime node and advances the environment's current
// revision through the production revision writer. Tests that move placement
// after a deployment was pinned must use this instead of updating revision JSON.
func appendEnvironmentNodeRevision(t *testing.T, fixture *deploymentRetryFixture, node string) platform.RevisionID {
	t.Helper()
	env, err := fixture.service.environment(fixture.ctx, "live", "production")
	if err != nil {
		t.Fatal(err)
	}
	if env.Revision == nil {
		t.Fatal("environment has no current revision")
	}
	state := platform.CloneRevisionState(env.Revision.RevisionState)
	state.Runtime.Node = node
	var revisionID platform.RevisionID
	err = fixture.service.entities.WithinTx(fixture.ctx, func(tx *entities.StoreTx) error {
		var err error
		revisionID, err = publishEnvironmentRevisionTx(fixture.ctx, tx, env, "", testActor,
			"environment.updated", "move environment to "+node, state, nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return revisionID
}

func insertRunningNodeStep(t *testing.T, pool *pgxpool.Pool, operationID contract.OperationID, step string) {
	t.Helper()
	execution := engine.Execution{OperationID: operationID, Attempt: 1, Step: step}
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_operation_steps
        (operation_id,step,inputs_hash,state,attempt,execution_id,started_at,error,error_code)
        VALUES($1,$2,$3,'running',1,$4,clock_timestamp(),'','')`,
		operationID, step, strings.Repeat("a", 64), execution.ExecutionID()); err != nil {
		t.Fatal(err)
	}
}

// pinnedEffectGuardCase names one effect guarded on a deployment's pinned node.
type pinnedEffectGuardCase struct {
	key         string
	image       string
	step        string
	description string
}

// assertPinnedEffectGuard proves a cancellation guard observes the effect on
// the deployment's pinned node, not on the environment's current node.
func assertPinnedEffectGuard(t *testing.T, tc pinnedEffectGuardCase) {
	t.Helper()
	fixture := newDeploymentRetryFixtureWithTransport(t, &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()})
	fixture.deploy(t)
	env := fixture.environment(t)
	if env.RevisionID == "" {
		t.Fatal("fixture environment has no pinned revision")
	}
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, tc.key, tc.image, "running")
	insertRunningNodeStep(t, fixture.pool, op.ID, tc.step)
	// Register the placed nodes before the revision writer updates the
	// environment's node FK.
	liveA := &fixedStatusTransport{status: engine.StatusRunning}
	absentB := &fixedStatusTransport{status: engine.StatusAbsent}
	fixture.service.resolver = newTestNodeResolver(t, fixture.pool, map[string]node.Transport{
		"node-a": liveA,
		"node-b": absentB,
	})
	pinnedRevision := appendEnvironmentNodeRevision(t, fixture, "node-a")
	_ = appendEnvironmentNodeRevision(t, fixture, "node-b")
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_platform_deployments SET revision_id=$2 WHERE id=$1`, depID, pinnedRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.service.reconcileDeployment(fixture.ctx, current)
	if err == nil {
		t.Fatalf("cancellation settled while the pinned node had a live %s", tc.description)
	}
	if code := contract.Classify(err).Code; code != contract.Unavailable {
		t.Fatalf("%s guard error code = %s, want unavailable: %v", tc.description, code, err)
	}
	if liveA.callCount() == 0 {
		t.Fatalf("the %s guard did not observe the pinned node", tc.description)
	}
	if absentB.callCount() != 0 {
		t.Fatalf("the %s guard observed the moved-to environment node instead of the pinned node", tc.description)
	}
	final, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State.Terminal() {
		t.Fatalf("pinned live %s terminalized cancellation: %+v", tc.description, final)
	}
	if _, has := ownerForFixture(t, fixture, env); !has {
		t.Fatalf("pinned live %s released the deployment owner", tc.description)
	}
}

// failureCleanupCase names one live effect that must block terminal cleanup.
type failureCleanupCase struct {
	key         string
	image       string
	step        string
	description string
}

// assertFailureCleanupWaits proves terminal cleanup keeps ownership and writes
// no cleanup row while a live effect remains on the deployment's pinned node.
func assertFailureCleanupWaits(t *testing.T, tc failureCleanupCase) {
	t.Helper()
	fixture := newDeploymentRetryFixtureWithTransport(t, &recordingDeployTransport{scriptedDeployTransport: newScriptedDeployTransport()})
	fixture.deploy(t)
	env := fixture.environment(t)
	op, depID, _, _ := fixture.prepareOwnedCandidate(t, tc.key, tc.image, "running")
	insertRunningNodeStep(t, fixture.pool, op.ID, tc.step)
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
	err = worker.finishDeploymentFailure(fixture.ctx, op.ID, 1, env, dep, nil, errPlatformCancelled)
	if err == nil {
		t.Fatalf("terminal cleanup proceeded while a %s was live", tc.description)
	}
	if code := contract.Classify(err).Code; code != contract.Unavailable {
		t.Fatalf("%s cleanup guard code = %s, want unavailable: %v", tc.description, code, err)
	}
	var cleanupRows int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM eregion_platform_container_cleanup WHERE deployment_id=$1`, depID).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	if cleanupRows != 0 {
		t.Fatalf("live %s recorded %d cleanup row(s)", tc.description, cleanupRows)
	}
	current, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("live %s terminalized the operation: %+v", tc.description, current)
	}
	if _, has := ownerForFixture(t, fixture, env); !has {
		t.Fatalf("live %s released the deployment owner", tc.description)
	}
}

// TestIntegrationDeploymentEffectGuardUsesPinnedNodeAfterEnvironmentMove pins
// a deployment to node-a, moves the environment to node-b, and proves the
// cancellation guard observes the live effect on node-a. Observing the current
// environment node alone would read an absent effect on node-b and cancel.
func TestIntegrationDeploymentEffectGuardUsesPinnedNodeAfterEnvironmentMove(t *testing.T) {
	assertPinnedEffectGuard(t, pinnedEffectGuardCase{
		key: "pinned-guard", image: "registry.example/live:pinned-guard",
		step: deploymentNetworkStep, description: "effect",
	})
}

// TestIntegrationDeploymentReadyImageGuardUsesPinnedNodeAfterEnvironmentMove
// covers the registry materialization effect added after the original guard
// table: a detached docker pull for a ready release must be observed on the
// deployment's pinned node before cancellation settles.
func TestIntegrationDeploymentReadyImageGuardUsesPinnedNodeAfterEnvironmentMove(t *testing.T) {
	assertPinnedEffectGuard(t, pinnedEffectGuardCase{
		key: "ready-image-pinned", image: "registry.example/live:ready-image-pinned",
		step: deploymentReadyImageStep, description: "ready-image effect",
	})
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
	assertFailureCleanupWaits(t, failureCleanupCase{
		key: "ready-image-cleanup", image: "registry.example/live:ready-image-cleanup",
		step: deploymentReadyImageStep, description: "ready-image effect",
	})
}

// TestIntegrationDeploymentFailureCleanupWaitsForLivePinnedEffect exercises
// the terminalization path itself: a cancellation/terminal cleanup cause must
// not persist cleanup or settle while a live pinned effect remains.
func TestIntegrationDeploymentFailureCleanupWaitsForLivePinnedEffect(t *testing.T) {
	assertFailureCleanupWaits(t, failureCleanupCase{
		key: "live-failure", image: "registry.example/live:live-failure",
		step: deploymentNetworkStep, description: "pinned effect",
	})
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
	case node.MethodObserve, node.MethodObserveImage, node.MethodInventory, node.MethodRetireBuild:
		return node.Response{}, errors.New("unexpected node method")
	default:
		return node.Response{}, errors.New("unexpected node method")
	}
}

func newBuildCancellationFixture(t *testing.T, transport node.Transport) (*Service, *pgxpool.Pool, contract.Operation) {
	t.Helper()
	pool, _ := testPool(t, true)
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
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
				"url": "https://github.com/acme/private.git",
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
	var claims int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_build_capacity_claims WHERE operation_id=$1 AND node_name='local'`, deploy.ID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("lost build capacity claims = %d, %v; want one retained claim", claims, err)
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
	var claims int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_build_capacity_claims WHERE operation_id=$1`, deploy.ID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("failed build capacity claims = %d, %v; want one retained claim", claims, err)
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
