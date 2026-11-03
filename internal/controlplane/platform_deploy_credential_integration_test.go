package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	gitReadSecretName = "GIT_READ"
	gitReadRevision   = uint64(7)
	gitReadValue      = "private-repository-read-credential"
)

type secretSnapshotCall struct {
	names []string
	refs  map[string]string
}

// snapshotSecretResolver stands in for the vault resolver: it returns only the
// git read credential and records how the deployment asked for it, so the test
// can tell a current-name resolution from a pinned-revision resolution.
type snapshotSecretResolver struct {
	mu    sync.Mutex
	calls []secretSnapshotCall
}

func (*snapshotSecretResolver) Resolve(context.Context, map[string]string) (map[string]string, error) {
	return nil, errors.New("namespace-free secret resolution is refused")
}

func (r *snapshotSecretResolver) ResolveEnvironmentSnapshot(_ context.Context, app, environment string, names []string, refs map[string]string) (map[string]string, map[string]vault.RevisionRef, error) {
	r.mu.Lock()
	call := secretSnapshotCall{names: append([]string(nil), names...), refs: map[string]string{}}
	for key, ref := range refs {
		call.refs[key] = ref
	}
	r.calls = append(r.calls, call)
	r.mu.Unlock()

	values := map[string]string{}
	revisions := map[string]vault.RevisionRef{}
	add := func(key, pinned string) error {
		if key != gitReadSecretName {
			return nil
		}
		if pinned != "" {
			want := fmt.Sprintf("vault:%s/%s/%s@%d", app, environment, gitReadSecretName, gitReadRevision)
			if pinned != want {
				return fmt.Errorf("credential pin %q, want %q", pinned, want)
			}
		}
		values[key] = gitReadValue
		revisions[key] = vault.RevisionRef{
			Revision: gitReadRevision, Hash: strings.Repeat("a", 64), KeyID: "test-key", CreatedAt: time.Unix(1, 0).UTC(),
		}
		return nil
	}
	for _, name := range names {
		if err := add(name, ""); err != nil {
			return nil, nil, err
		}
	}
	for key, ref := range refs {
		if err := add(key, ref); err != nil {
			return nil, nil, err
		}
	}
	return values, revisions, nil
}

func (r *snapshotSecretResolver) snapshot() []secretSnapshotCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]secretSnapshotCall(nil), r.calls...)
}

// gitReadTransport blocks the first attempt's pack status call until the test
// cancels it, which models a worker that died after recording its secret
// snapshot but before it could build. The next attempt resolves the source and
// records the credential the node would receive.
type gitReadTransport struct {
	*scriptedDeployTransport
	commit      string
	blocked     chan struct{}
	packSecrets chan map[string]string
}

func (t *gitReadTransport) Call(ctx context.Context, request node.Request, onEvent func(node.Event)) (node.Response, error) {
	if request.Execution.Step != deploymentPackStep {
		return t.scriptedDeployTransport.Call(ctx, request, onEvent)
	}
	switch request.Method {
	case node.MethodStatus:
		if request.Execution.Attempt == 1 {
			select {
			case <-t.blocked:
			default:
				close(t.blocked)
			}
			<-ctx.Done()
			return node.Response{}, ctx.Err()
		}
		return node.Response{Status: engine.StatusAbsent, ExecutionID: request.Execution.ExecutionID()}, nil
	case node.MethodRun:
		select {
		case t.packSecrets <- request.Secrets:
		default:
		}
		result, _ := json.Marshal(map[string]string{"commit": t.commit})
		return node.Response{
			Status: engine.StatusCompleted, ExecutionID: request.Execution.ExecutionID(),
			Executed: true, Result: result,
		}, nil
	default:
		return node.Response{}, fmt.Errorf("unexpected method %q", request.Method)
	}
}

// runPlatformAttempt dispatches one platform operation the way the River
// worker does, without needing a real River job to be claimed and retried.
func runPlatformAttempt(ctx context.Context, s *Service, id contract.OperationID, attempt int) error {
	worker := &platformWorker{service: s}
	return worker.Work(ctx, &river.Job[platformArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt},
		Args:   platformArgs{OperationID: id},
	})
}

// TestIntegrationDeployRetryResolvesPinnedGitReadCredential reproduces the
// rescued-deploy defect: the first attempt records the secret snapshot and then
// disappears, and the second attempt must still get the git read credential
// from that snapshot. Before the fix the retry saw a non-empty snapshot and
// never resolved the credential, so the build failed with
// precondition_failed "git read credential is unavailable".
func TestIntegrationDeployRetryResolvesPinnedGitReadCredential(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	resolver := &snapshotSecretResolver{}
	const commit = submittedGitCommit
	transport := &gitReadTransport{
		scriptedDeployTransport: newScriptedDeployTransport(),
		commit:                  commit,
		blocked:                 make(chan struct{}),
		packSecrets:             make(chan map[string]string, 1),
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: transport, NodeStateDir: t.TempDir(),
		LocalNodeName: "local", SecretResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }

	// Setup operations are dispatched synchronously; no River workers run in
	// this test, so the deploy below cannot race the simulated attempts.
	for _, op := range []contract.Operation{
		platformSubmit(t, s, platform.KindNodeRegister, "platform", "credential-node", map[string]any{
			"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
			"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
		}),
		platformSubmit(t, s, platform.KindAppCreate, "platform", "credential-app", map[string]any{"name": "private"}),
		platformSubmit(t, s, platform.KindEnvironmentCreate, "app/private", "credential-env", map[string]any{
			"name": "production", "node": "local", "container_port": 80,
			"health": map[string]any{"timeout_seconds": 2},
			"domain": "private.example.com", "domain_primary": true,
			"source": map[string]any{
				"kind": "git", "url": "https://github.com/acme/private.git",
				"ref": commit, "builder": platform.DefaultBuilder,
				"credential_secret_name": gitReadSecretName,
			},
		}),
	} {
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatalf("%s setup attempt: %v", op.Kind, err)
		}
	}

	deploy := platformSubmit(t, s, platform.KindDeploy, "env/private/production", "credential-deploy", map[string]any{})
	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- runPlatformAttempt(firstCtx, s, deploy.ID, 1)
	}()
	select {
	case <-transport.blocked:
	case <-time.After(30 * time.Second):
		cancelFirst()
		t.Fatal("the first attempt never reached the build step")
	}
	cancelFirst()
	<-firstErr

	var snapshot map[string]platform.SecretRevisionRef
	var rawSnapshot []byte
	if err := pool.QueryRow(ctx, `SELECT secret_revisions FROM eregion_platform_deployments WHERE operation_id=$1`, deploy.ID).Scan(&rawSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawSnapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	ref, ok := snapshot[gitReadSecretName]
	if !ok || ref.Revision != gitReadRevision {
		t.Fatalf("first attempt snapshot = %s, want the git read credential at revision %d", rawSnapshot, gitReadRevision)
	}
	current, err := s.Get(ctx, testActor, deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() {
		t.Fatalf("simulated worker loss terminalized the operation: %+v", current)
	}

	// The second attempt is the rescued or River-retried job: the snapshot
	// exists, so the credential must come from it rather than from a fresh
	// name lookup.
	if err := runPlatformAttempt(ctx, s, deploy.ID, 2); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	current, err = s.Get(ctx, testActor, deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contract.Succeeded {
		t.Fatalf("second attempt state = %s, want succeeded", current.State)
	}

	select {
	case secrets := <-transport.packSecrets:
		if secrets[gitReadSecretName] != gitReadValue {
			t.Fatalf("build received credential %q, want the resolved value", secrets[gitReadSecretName])
		}
	default:
		t.Fatal("the second attempt never ran the build")
	}

	calls := resolver.snapshot()
	if len(calls) != 2 {
		t.Fatalf("secret resolution calls = %+v, want one name resolution and one pinned resolution", calls)
	}
	if len(calls[0].names) != 1 || calls[0].names[0] != gitReadSecretName || len(calls[0].refs) != 0 {
		t.Fatalf("first resolution = %+v, want the credential name at its active revision", calls[0])
	}
	wantPin := fmt.Sprintf("vault:private/production/%s@%d", gitReadSecretName, gitReadRevision)
	if len(calls[1].names) != 0 || calls[1].refs[gitReadSecretName] != wantPin {
		t.Fatalf("retry resolution = %+v, want pinned %q", calls[1], wantPin)
	}

	// The credential value is only ever handed to the node step: never in the
	// operation input, its durable events, or the secret snapshot.
	var input []byte
	if err := pool.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, deploy.ID).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(input), gitReadValue) {
		t.Fatal("the git read credential leaked into operation input")
	}
	var events string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(event::text, ''),'') FROM eregion_operation_events WHERE operation_id=$1`, deploy.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(events, gitReadValue) {
		t.Fatal("the git read credential leaked into operation events")
	}
	if strings.Contains(string(rawSnapshot), gitReadValue) {
		t.Fatal("the git read credential leaked into the secret snapshot")
	}
}
