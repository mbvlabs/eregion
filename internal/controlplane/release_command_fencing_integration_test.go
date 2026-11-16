package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

type releaseFenceTransport struct {
	status        engine.Status
	retryable     bool
	wrongID       bool
	wrongTry      bool
	missingHash   bool
	inputMismatch bool
	result        json.RawMessage
	hash          string
	runs          int
}

func (*releaseFenceTransport) Name() string         { return "release-fence" }
func (*releaseFenceTransport) NodeIdentity() string { return "main" }

func (f *releaseFenceTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	if request.Method != node.MethodStatus {
		f.runs++
		return node.Response{}, errors.New("unexpected release command dispatch")
	}
	exec := request.Execution
	response := node.Response{Status: f.status, Attempt: exec.Attempt, ExecutionID: exec.ExecutionID(), InputsHash: f.hash, Retryable: f.retryable}
	response.InputsMismatch = f.inputMismatch
	response.Result = f.result
	if f.wrongID {
		response.ExecutionID = (engine.Execution{OperationID: exec.OperationID, Attempt: exec.Attempt + 1, Step: exec.Step}).ExecutionID()
	}
	if f.wrongTry {
		response.Attempt++
	}
	if f.missingHash {
		response.InputsHash = ""
	}
	return response, nil
}

type releaseCommandFenceFixture struct {
	pool       *pgxpool.Pool
	run        *Runtime
	ctx        context.Context
	appID      platform.AppID
	envID      platform.EnvironmentID
	revisionID platform.RevisionID
	hash       string
	digest     string
	env        platform.Environment
}

func TestIntegrationReleaseCommandRecoveryAndCleanupFences(t *testing.T) {
	f := newReleaseCommandFenceFixture(t)
	t.Run("recovery", func(t *testing.T) {
		for _, tc := range []releaseCommandFenceRecoveryCase{
			{"stale", engine.StatusStale, false, false, false, false, false},
			{"absent", engine.StatusAbsent, false, false, false, false, false},
			{"retryable-failed", engine.StatusFailed, true, false, false, false, false},
			{"failed-wrong-id", engine.StatusFailed, false, true, false, false, false},
			{"failed-wrong-attempt", engine.StatusFailed, false, false, true, false, false},
			{"failed-no-inputs", engine.StatusFailed, false, false, false, true, false},
			{"failed-input-mismatch", engine.StatusFailed, false, false, false, false, false},
			{"confirmed-failed", engine.StatusFailed, false, false, false, false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				runReleaseCommandFenceRecoveryCase(t, f, tc)
			})
		}
	})
	t.Run("cancellation-cleanup", func(t *testing.T) {
		for _, tc := range []releaseCommandCancellationCase{
			{"absent-after-intent", engine.StatusAbsent, false, false, false, false, true},
			{"stale-after-intent", engine.StatusStale, false, false, false, false, true},
			{"retryable-failed", engine.StatusFailed, true, false, false, false, true},
			{"failed-wrong-id", engine.StatusFailed, false, true, false, false, true},
			{"failed-wrong-attempt", engine.StatusFailed, false, false, true, false, true},
			{"failed-no-inputs", engine.StatusFailed, false, false, false, true, true},
			{"failed-input-mismatch", engine.StatusFailed, false, false, false, false, true},
			{"completed-no-inputs", engine.StatusCompleted, false, false, false, true, true},
			{"confirmed-failed", engine.StatusFailed, false, false, false, false, false},
			{"completed", engine.StatusCompleted, false, false, false, false, false},
			{"deferred-before-apply", engine.StatusDeferred, false, false, false, false, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				runReleaseCommandCancellationCase(t, f, tc)
			})
		}
	})
}

func newReleaseCommandFenceFixture(t *testing.T) releaseCommandFenceFixture {
	t.Helper()
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	revisionID := platform.RevisionID("erv_" + strings.Repeat("a", 32))
	if err := insertFixtureNode(ctx, pool, "main", []string{"application"}, []string{"docker"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureApp(ctx, pool, string(appID), "fence", "fence"); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, string(envID), string(appID), "production", "main", 20331, 30331, 8080); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureRevision(ctx, pool, string(revisionID), string(envID), 1, `{"node":"main","health_path":"/"}`); err != nil {
		t.Fatal(err)
	}
	return releaseCommandFenceFixture{
		pool: pool, run: run, ctx: ctx, appID: appID, envID: envID, revisionID: revisionID,
		hash: strings.Repeat("a", 64), digest: platform.ReleaseCommandDigest(platform.CommandSpec{Argv: []string{"/bin/migrate"}, TimeoutSeconds: 30}),
		env: platform.Environment{Node: "main"},
	}
}

func (f releaseCommandFenceFixture) submit(t *testing.T, key string) contract.Operation {
	t.Helper()
	op, err := f.run.Service.Submit(f.ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: key, Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, claimed, err := f.run.Service.claimAttempt(f.ctx, op.ID, 1, "test"); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentCommandStep}
	if _, err := f.run.Service.beginStep(f.ctx, op.ID, 1, deploymentCommandStep, f.hash, exec.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	return op
}

func (f releaseCommandFenceFixture) insertDeployment(t *testing.T, op contract.Operation) (platform.ReleaseID, platform.Deployment) {
	t.Helper()
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	depID, err := platform.NewDeploymentID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO eregion_platform_releases
		(id,operation_id,app_id,environment_id,image,image_id,source,status,error_code,error,created_at,commit_sha)
		VALUES($1,$2,$3,$4,'example.org/fence:one','','{}','ready','','',clock_timestamp(),'')`,
		releaseID, op.ID, f.appID, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO eregion_platform_deployments
		(id,operation_id,app_id,environment_id,action,release_id,revision_id,state,error_code,message,created_at,updated_at,secret_revisions,instances)
		VALUES($1,$2,$3,$4,'deploy',$5,$6,'running','','',clock_timestamp(),clock_timestamp(),'{}','[]')`,
		depID, op.ID, f.appID, f.envID, releaseID, f.revisionID); err != nil {
		t.Fatal(err)
	}
	dep, ok, err := f.run.Service.deploymentByOperation(f.ctx, op.ID)
	if err != nil || !ok {
		t.Fatalf("deployment: %+v %v", dep, err)
	}
	return releaseID, dep
}

type releaseCommandFenceRecoveryCase struct {
	name                                                   string
	status                                                 engine.Status
	retryable, wrongID, wrongTry, missingHash, allowFailed bool
}

func runReleaseCommandFenceRecoveryCase(t *testing.T, f releaseCommandFenceFixture, tc releaseCommandFenceRecoveryCase) {
	t.Helper()
	op := f.submit(t, "recovery-"+tc.name)
	releaseID, _ := f.insertDeployment(t, op)
	if _, err := f.pool.Exec(f.ctx, `UPDATE eregion_operations SET state='failed' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO eregion_platform_release_command_runs
		(release_id,command_digest,operation_id,state,error_code,error,started_at)
		VALUES($1,$2,$3,'running','','',clock_timestamp())`, releaseID, f.digest, op.ID); err != nil {
		t.Fatal(err)
	}
	tr := newReleaseFenceTransport(f.hash, tc.name, tc.status, tc.retryable, tc.wrongID, tc.wrongTry, tc.missingHash)
	s := &Service{entities: newTestEntities(t, f.pool), resolver: NewNodeResolver(newTestEntities(t, f.pool).Nodes(), ResolverConfig{}, func() node.Transport { return tr })}
	current, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	decision, err := (&platformWorker{service: s}).recoverReleaseCommandRun(f.ctx, releaseID, f.digest,
		releaseCommandRun{State: "running", OperationID: op.ID}, current)
	row, rowErr := s.releaseCommandRun(f.ctx, releaseID, f.digest)
	if rowErr != nil || tr.runs != 0 || row.OperationID != op.ID {
		t.Fatalf("row=%+v runs=%d err=%v", row, tr.runs, rowErr)
	}
	if tc.allowFailed {
		if err != nil || row.State != "failed" || decision != releaseCommandRunNow {
			t.Fatalf("confirmed failure decision=%v row=%+v err=%v", decision, row, err)
		}
		return
	}
	if err == nil || row.State != "running" {
		t.Fatalf("unknown outcome released ownership: decision=%v row=%+v err=%v", decision, row, err)
	}
}

type releaseCommandCancellationCase struct {
	name                                                       string
	status                                                     engine.Status
	retryable, wrongID, wrongTry, missingHash, wantOutstanding bool
}

func runReleaseCommandCancellationCase(t *testing.T, f releaseCommandFenceFixture, tc releaseCommandCancellationCase) {
	t.Helper()
	op := f.submit(t, "cleanup-"+tc.name)
	_, dep := f.insertDeployment(t, op)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO eregion_platform_deployment_owners
		(environment_id,operation_id,deployment_id,owner_attempt,phase,created_at,updated_at)
		VALUES($1,$2,$3,1,'prepared',clock_timestamp(),clock_timestamp())
		ON CONFLICT(environment_id) DO UPDATE SET operation_id=$2,deployment_id=$3,
			owner_attempt=1,phase='prepared',updated_at=clock_timestamp()`, f.envID, op.ID, dep.ID); err != nil {
		t.Fatal(err)
	}
	tr := newReleaseFenceTransport(f.hash, tc.name, tc.status, tc.retryable, tc.wrongID, tc.wrongTry, tc.missingHash)
	f.run.Service.resolver = NewNodeResolver(newTestEntities(t, f.pool).Nodes(), ResolverConfig{}, func() node.Transport { return tr })
	outstanding, err := f.run.Service.deploymentNodeEffectOutstanding(f.ctx, dep, f.env, op.ID, 1)
	if err != nil || outstanding != tc.wantOutstanding || tr.runs != 0 {
		t.Fatalf("outstanding=%v want=%v runs=%d err=%v", outstanding, tc.wantOutstanding, tr.runs, err)
	}
	record, found, err := f.run.Service.stepRecord(f.ctx, op.ID, deploymentCommandStep)
	if err != nil || !found {
		t.Fatalf("step: %+v %v", record, err)
	}
	if tc.wantOutstanding && record.State != "running" {
		t.Fatalf("unknown outcome modified step: %+v", record)
	}
	if tc.wantOutstanding {
		assertCancellationCleanupRefused(t, f, op, dep)
	}
	if tc.status == engine.StatusCompleted && !tc.wantOutstanding && record.State != "completed" {
		t.Fatalf("completion not acknowledged: %+v", record)
	}
	if tc.name == "confirmed-failed" {
		assertFailedCommandObservation(t, f, op)
	}
}

func newReleaseFenceTransport(
	hash, name string,
	status engine.Status,
	retryable, wrongID, wrongTry, missingHash bool,
) *releaseFenceTransport {
	tr := &releaseFenceTransport{status: status, retryable: retryable, wrongID: wrongID, wrongTry: wrongTry, missingHash: missingHash, hash: hash}
	tr.inputMismatch = name == "failed-input-mismatch"
	if name == "confirmed-failed" {
		tr.result = json.RawMessage(`{"stage":"release-command","output_availability":"bounded","redaction_context":"not_required","container":{"availability":"partial","started_at":"2026-11-14T10:00:00Z","finished_at":"2026-11-14T10:00:01Z","exit_code":1}}`)
	}
	return tr
}

func assertCancellationCleanupRefused(t *testing.T, f releaseCommandFenceFixture, op contract.Operation, dep platform.Deployment) {
	t.Helper()
	if err := (&platformWorker{service: f.run.Service}).ensureNoOutstandingDeploymentEffect(f.ctx, op.ID, 1, dep, f.env, "cancellation"); err == nil {
		t.Fatal("unknown effect permitted cancellation cleanup")
	}
	var owner contract.OperationID
	if err := f.pool.QueryRow(f.ctx, `SELECT operation_id FROM eregion_platform_deployment_owners WHERE environment_id=$1`, f.envID).Scan(&owner); err != nil || owner != op.ID {
		t.Fatalf("unknown effect lost owner: %s %v", owner, err)
	}
}

func assertFailedCommandObservation(t *testing.T, f releaseCommandFenceFixture, op contract.Operation) {
	t.Helper()
	page, err := f.run.Service.Events(f.ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if evidence := event.NodeEvidence; evidence != nil && evidence.Container != nil && evidence.Container.ExitCode != nil &&
			*evidence.Container.ExitCode == 1 && evidence.Attempt == 1 {
			return
		}
	}
	t.Fatal("cleanup discarded the original failed command observation")
}
