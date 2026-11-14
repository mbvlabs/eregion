package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/viewer"
)

type actionResultNode struct {
	status    engine.Status
	attempt   uint32
	hash      string
	result    json.RawMessage
	retryable bool
	runs      int
}

func TestIntegrationLateEvidenceProjectionDoesNotRewriteFailure(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "late-evidence", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := run.Service.failPlatformOperation(ctx, op.ID, 1, contract.Unavailable, "original failure"); err != nil {
		t.Fatal(err)
	}
	before, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || before.LatestResult == nil || before.LatestResult.Evidence != "not_collected" {
		t.Fatalf("before: %+v %v", before, err)
	}
	_, err = run.Service.mutate(ctx, op.ID, func(tx pgx.Tx, current *contract.Operation) error {
		for _, event := range []contract.Event{
			{Kind: "warning", Stage: "candidate", Message: "Candidate diagnostic capture started.",
				CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "fixture", Status: "unavailable"}},
			{Kind: "log", Stage: "candidate", Message: "retained original output",
				CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "fixture", Status: "captured", RedactionContext: "original"}},
			{Kind: "warning", Stage: "candidate", Message: "Candidate diagnostic capture: complete.",
				CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "fixture", Status: "complete"}},
		} {
			if err := appendEvent(ctx, tx, current, event); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app := api.AppService{Operations: run.Service}
	after, err := app.GetOperation(ctx, testActor, op.ID)
	if err != nil || after.State != contract.Failed || after.LatestResult == nil ||
		after.LatestResult.Evidence != "not_collected" || after.LatestResult.Message != "original failure" ||
		after.Evidence == nil || after.Evidence.Availability != "full" || after.Evidence.Excerpt != "retained original output" {
		t.Fatalf("after: %+v %v", after, err)
	}
	details := viewer.FaultDetails(viewer.Summary{OperationID: op.ID, State: after.State, ErrorCode: after.ErrorCode, Operation: after}, "operation follow")
	raw, err := json.Marshal(details)
	if err != nil || !strings.Contains(string(raw), "retained original output") || !strings.Contains(string(raw), "original failure") {
		t.Fatalf("failure envelope: %s %v", raw, err)
	}
}

func TestIntegrationActionOutputSurvivesRetryWithoutOutcomeMetric(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "durable-output", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	run.Service.observeActionAttempt = func(_ string, outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	for range 2 {
		_, owned, err := run.Service.mutateOwned(ctx, op.ID, 1, func(tx pgx.Tx, current *contract.Operation) error {
			return run.Service.appendActionOutputTx(ctx, tx, current, map[string]any{"domain": "example.test", "removed": true})
		})
		if err != nil || !owned {
			t.Fatalf("snapshot: %v %v", owned, err)
		}
	}
	if len(outcomes) != 0 {
		t.Fatalf("snapshot emitted outcome: %v", outcomes)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 2, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := run.Service.finishPlatformOperation(ctx, op.ID, 2, func(pgx.Tx, *contract.Operation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || got.LatestResult == nil || got.LatestResult.DataStatus != "available" ||
		got.LatestResult.DataCursor == nil || !strings.Contains(string(got.LatestResult.Data), "example.test") {
		t.Fatalf("result: %+v %v", got, err)
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	var outputs int
	for _, event := range page.Events {
		if event.ActionOutputStatus != "" {
			outputs++
		}
	}
	if outputs != 1 || len(outcomes) != 1 || outcomes[0] != "succeeded" {
		t.Fatalf("outputs=%d outcomes=%v", outputs, outcomes)
	}
}

func TestIntegrationActionResultFollowsDeploymentReleaseIdentity(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	submit := func(key string) contract.Operation {
		t.Helper()
		op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
			Actor: testActor, IdempotencyKey: key, Input: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	releaseOp, deployOp := submit("artifact-source"), submit("artifact-use")
	appID, _ := platform.NewAppID()
	envID, _ := platform.NewEnvironmentID()
	releaseID, _ := platform.NewReleaseID()
	deploymentID, _ := platform.NewDeploymentID()
	if err := insertFixtureNode(ctx, pool, "fixture", []string{"application"}, []string{"docker"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureApp(ctx, pool, string(appID), "artifact", "artifact"); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, string(envID), string(appID), "production", "fixture", 20111, 30111, 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_releases
		(id,operation_id,app_id,environment_id,image,image_id,source,status,error_code,error,created_at,commit_sha)
		VALUES($1,$2,$3,$4,'registry.example/artifact@sha256:abc','sha256:config','{}','ready','','',clock_timestamp(),'commit-123')`,
		releaseID, releaseOp.ID, appID, envID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployments
		(id,operation_id,app_id,environment_id,action,release_id,state,error_code,message,created_at,updated_at,secret_revisions,instances)
		VALUES($1,$2,$3,$4,'restart',$5,'running','','',clock_timestamp(),clock_timestamp(),'{}','[]')`,
		deploymentID, deployOp.ID, appID, envID, releaseID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, deployOp.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := run.Service.recordRetryResult(ctx, deployOp.ID, 1, contract.Unavailable, "failed"); err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, deployOp.ID)
	if err != nil || got.LatestResult == nil || got.LatestResult.Identities.ReleaseID != string(releaseID) ||
		got.LatestResult.Identities.ImageID != "sha256:config" || got.LatestResult.Identities.CommitSHA != "commit-123" {
		t.Fatalf("linked artifact: %+v %v", got.LatestResult, err)
	}
}

func (*actionResultNode) Name() string { return "result-fixture" }
func (n *actionResultNode) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	execution := request.Execution
	if request.Method == node.MethodRun {
		n.runs++
		return node.Response{Status: engine.StatusCompleted, Attempt: execution.Attempt, ExecutionID: execution.ExecutionID(),
			InputsHash: n.hash, Result: json.RawMessage(`{}`)}, nil
	}
	if n.attempt != 0 {
		execution.Attempt = n.attempt
	}
	return node.Response{Status: n.status, Attempt: execution.Attempt, ExecutionID: execution.ExecutionID(),
		InputsHash: n.hash, Result: n.result, Retryable: n.retryable, ErrorCode: string(contract.Unavailable),
		Detail: "original node failure"}, nil
}

func TestIntegrationReleaseCommandAmbiguousStatusNeverRedispatches(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	tests := []struct {
		name                             string
		status                           engine.Status
		prior, oldFailure, fresh, cancel bool
	}{
		{"stale", engine.StatusStale, false, false, false, false},
		{"absent-after-intent", engine.StatusAbsent, true, false, false, false},
		{"old-retryable-failure", engine.StatusFailed, true, true, false, false},
		{"cancel-after-old-failure", engine.StatusFailed, true, true, false, true},
		{"fresh", engine.StatusAbsent, false, false, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
				Actor: testActor, IdempotencyKey: "release-guard-" + tc.name, Input: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			payload := platform.ReleaseCommandPayload{Step: deploymentCommandStep, Name: "release-command", Network: "app-network",
				Image: "example.org/app:one", Command: []string{"/bin/migrate"}, TimeoutSeconds: 30}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentCommandStep}
			step, err := node.DefaultRegistry().Build(exec, raw)
			if err != nil {
				t.Fatal(err)
			}
			inputs, err := step.Inputs()
			if err != nil {
				t.Fatal(err)
			}
			hash := engine.InputsHash(step.ID(), inputs)
			if tc.prior {
				if _, err := run.Service.beginStep(ctx, op.ID, 1, deploymentCommandStep, hash, exec.ExecutionID()); err != nil {
					t.Fatal(err)
				}
			}
			attempt := uint32(1)
			if tc.oldFailure {
				attempt = 2
				if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 2, "test"); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			}
			if tc.cancel {
				if _, err := run.Service.RequestCancel(ctx, testActor, op.ID); err != nil {
					t.Fatal(err)
				}
			}
			tr := &actionResultNode{status: tc.status, hash: hash, retryable: tc.oldFailure}
			if tc.oldFailure {
				tr.attempt = 1
			}
			_, err = (&platformWorker{service: run.Service}).runStepOn(ctx, tr, op.ID, attempt, deploymentCommandStep, payload, nil)
			if tc.fresh {
				if err != nil || tr.runs != 1 {
					t.Fatalf("fresh: runs=%d err=%v", tr.runs, err)
				}
				record, found, err := run.Service.stepRecord(ctx, op.ID, deploymentCommandStep)
				if err != nil || !found || record.State != "completed" {
					t.Fatalf("record: %+v %v", record, err)
				}
			} else if !errors.Is(err, errNodeEffectOutcomeUnknown) || tr.runs != 0 {
				t.Fatalf("ambiguous: runs=%d err=%v", tr.runs, err)
			}
		})
	}
}

func TestIntegrationNodeFailureEvidenceOriginalAttemptAndDedup(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	for _, old := range []bool{false, true} {
		name := "current"
		if old {
			name = "old"
		}
		t.Run(name, func(t *testing.T) {
			op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
				Actor: testActor, IdempotencyKey: "node-evidence-" + name, Input: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			payload := platform.ReleaseCommandPayload{Step: deploymentCommandStep, Name: "release-command", Network: "app-network",
				Image: "example.org/app:one", Command: []string{"/bin/migrate"}, TimeoutSeconds: 30}
			raw, _ := json.Marshal(payload)
			exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: deploymentCommandStep}
			step, err := node.DefaultRegistry().Build(exec, raw)
			if err != nil {
				t.Fatal(err)
			}
			inputs, err := step.Inputs()
			if err != nil {
				t.Fatal(err)
			}
			hash := engine.InputsHash(step.ID(), inputs)
			if _, err := run.Service.beginStep(ctx, op.ID, 1, deploymentCommandStep, hash, exec.ExecutionID()); err != nil {
				t.Fatal(err)
			}
			attempt := uint32(1)
			if old {
				attempt = 2
				if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 2, "test"); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			}
			tr := &actionResultNode{status: engine.StatusFailed, attempt: 1, hash: hash, retryable: true,
				result: json.RawMessage(`{"stage":"release-command-create","output_availability":"bounded","redaction_context":"original","stderr":"original stderr","stderr_truncated":false}`)}
			for range 2 {
				if _, err := (&platformWorker{service: run.Service}).runStepOn(ctx, tr, op.ID, attempt, deploymentCommandStep, payload, nil); err == nil {
					t.Fatal("failure was lost")
				}
			}
			page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, event := range page.Events {
				if event.NodeEvidence != nil {
					count++
					if event.NodeEvidence.Attempt != 1 || event.NodeEvidence.ExecutionID != exec.ExecutionID() ||
						event.Attempt != attempt || event.NodeEvidence.Stderr != "original stderr" {
						t.Fatalf("evidence: %+v", event)
					}
				}
			}
			if count != 1 || tr.runs != 0 {
				t.Fatalf("evidence count=%d runs=%d", count, tr.runs)
			}
			if !old {
				if err := run.Service.recordRetryResult(ctx, op.ID, 1, contract.Unavailable, "failed"); err != nil {
					t.Fatal(err)
				}
				got, err := run.Service.Get(ctx, testActor, op.ID)
				if err != nil || got.State != contract.Running || got.LatestResult == nil ||
					got.LatestResult.Stage != "release-command-create" || got.LatestResult.Excerpt != "original stderr" ||
					got.LatestResult.ExcerptCursor == nil || got.LatestResult.Evidence != "partial" {
					t.Fatalf("result: %+v %v", got, err)
				}
			}
		})
	}
}

func TestIntegrationEvidenceOmitsTextWithoutOriginalRedactionContext(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "missing-redaction-context", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: "build-v1"}
	response := node.Response{Status: engine.StatusFailed, Attempt: 1, ExecutionID: exec.ExecutionID(),
		Result: json.RawMessage(`{"stage":"build-v1","output_availability":"bounded","stderr":"unverified-private-output","container":{"availability":"partial","logs":"unverified-container-output"}}`)}
	if err := run.Service.retainNodeResponse(ctx, exec, response); err != nil {
		t.Fatal(err)
	}
	_, _, err = run.Service.mutateOwned(ctx, op.ID, 1, func(tx pgx.Tx, current *contract.Operation) error {
		return appendEvent(ctx, tx, current, contract.Event{Kind: "log", Stage: "candidate", Message: "legacy-private-output",
			CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "fixture", Status: "captured"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Service.recordRetryResult(ctx, op.ID, 1, contract.Unavailable, "failed"); err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || got.LatestResult == nil || got.LatestResult.Excerpt != "" ||
		got.LatestResult.ExcerptOmitted != "original redaction context unavailable" || got.Evidence == nil ||
		got.Evidence.Excerpt != "" || got.Evidence.ExcerptOmitted == "" {
		t.Fatalf("result: %+v %v", got, err)
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.NodeEvidence != nil {
			raw, err := json.Marshal(event.NodeEvidence)
			if err != nil || strings.Contains(string(raw), "unverified-") {
				t.Fatalf("node evidence: %s %v", raw, err)
			}
		}
	}
}

func TestIntegrationActionResultsSurviveRetryAndPaging(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "action-results", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	run.Service.observeActionAttempt = func(_ string, outcome string, _ time.Duration) {
		outcomes = append(outcomes, outcome)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	for range 2 {
		if err := run.Service.recordRetryResult(ctx, op.ID, 1, contract.Unavailable, "temporary failure"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || first.State != contract.Running || first.LatestResult == nil || first.LatestResult.Status != contract.Failed {
		t.Fatalf("first result: %+v %v", first, err)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 2, "test"); err != nil || !ok {
		t.Fatalf("second claim: %v %v", ok, err)
	}
	if err := run.Service.recordRetryResult(ctx, op.ID, 1, contract.Unavailable, "stale"); err != errNotOwned {
		t.Fatalf("stale attempt: %v", err)
	}
	if err := run.Service.finishPlatformOperation(ctx, op.ID, 2, func(pgx.Tx, *contract.Operation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	final, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || final.State != contract.Succeeded || final.LatestResult == nil || final.LatestResult.Attempt != 2 {
		t.Fatalf("final result: %+v %v", final, err)
	}
	if len(outcomes) != 2 || outcomes[0] != "retrying" || outcomes[1] != "succeeded" {
		t.Fatalf("outcomes: %v", outcomes)
	}
	cursor := contract.Cursor{OperationID: op.ID}
	var results []contract.ActionResult
	for cursor.After < final.LastSequence {
		page, err := run.Service.Events(ctx, testActor, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			if event.Result != nil {
				results = append(results, *event.Result)
			}
		}
		cursor = page.Cursor
	}
	if len(results) != 2 || results[0].Attempt != 1 || results[1].Attempt != 2 {
		t.Fatalf("history: %+v", results)
	}
}

func TestIntegrationActionResultTerminalAfterRetryKeepsOriginalStage(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "same-attempt", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	run.Service.observeActionAttempt = func(_ string, outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	first := string(op.ID) + ":1:build-v1"
	if _, err := run.Service.beginStep(ctx, op.ID, 1, "build-v1", "build", first); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.failStep(ctx, op.ID, 1, "build-v1", contract.Unavailable, "original failure"); err != nil {
		t.Fatal(err)
	}
	cleanup := string(op.ID) + ":1:cleanup-v1"
	if _, err := run.Service.beginStep(ctx, op.ID, 1, "cleanup-v1", "cleanup", cleanup); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Service.ackStep(ctx, op.ID, 1, "cleanup-v1", "cleanup", cleanup, nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.recordRetryResult(ctx, op.ID, 1, contract.Unavailable, "original failure"); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.failPlatformOperation(ctx, op.ID, 1, contract.Unavailable, "terminal failure"); err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || got.State != contract.Failed || got.LatestResult == nil ||
		got.LatestResult.Stage != "build-v1" || !got.LatestResult.Final || got.LatestResult.Message != "terminal failure" {
		t.Fatalf("terminal: %+v %v", got, err)
	}
	if len(outcomes) != 2 || outcomes[0] != "retrying" || outcomes[1] != "failed" {
		t.Fatalf("metrics: %v", outcomes)
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	var results []contract.ActionResult
	for _, event := range page.Events {
		if event.Result != nil {
			results = append(results, *event.Result)
		}
	}
	if len(results) != 2 || results[0].Final || !results[1].Final {
		t.Fatalf("results: %+v", results)
	}
}

func TestIntegrationExtensionResultPreservesNumbersAndReportsOmission(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	tests := []struct {
		name   string
		data   any
		status string
	}{
		{"number", map[string]any{"value": json.Number("9007199254740993")}, "available"},
		{"large", strings.Repeat("a", 4097), "omitted_too_large"},
		{"invalid", make(chan int), "omitted_unserializable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
				Actor: testActor, IdempotencyKey: "extension-" + tc.name, Input: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "extension"); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			executor := platform.ExecutorFunc(func(context.Context, platform.Execution) (platform.Result, error) {
				return platform.Result{Message: "extension completed", Data: tc.data}, nil
			})
			if err := (&platformWorker{service: run.Service}).runExtension(ctx, op.ID, 1,
				platform.Normalized{Kind: op.Kind, Target: platform.PlatformTarget(), Summary: "extension"}, executor); err != nil {
				t.Fatal(err)
			}
			got, err := run.Service.Get(ctx, testActor, op.ID)
			if err != nil || got.State != contract.Succeeded || got.LatestResult == nil ||
				got.LatestResult.DataStatus != tc.status || got.LatestResult.Message != "extension completed" {
				t.Fatalf("result: %+v %v", got, err)
			}
			if tc.name == "number" && !strings.Contains(string(got.LatestResult.Data), "9007199254740993") {
				t.Fatalf("number lost: %s", got.LatestResult.Data)
			}
		})
	}
}

func TestIntegrationActionResultReadFailurePreservesExecutionState(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "unreadable-result", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, err := run.Service.finishSucceeded(ctx, op.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Add a malformed later history record without changing any prior event.
	_, err = pool.Exec(ctx, `WITH updated AS (
		UPDATE eregion_operations SET last_sequence=last_sequence+1 WHERE id=$1 RETURNING last_sequence)
		INSERT INTO eregion_operation_events(operation_id,sequence,event)
		SELECT $1,last_sequence,jsonb_build_object('result','malformed') FROM updated`, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || got.State != contract.Succeeded || got.ResultReadError == "" || got.LatestResult != nil {
		t.Fatalf("snapshot: %+v %v", got, err)
	}
}

func TestIntegrationActionResultEvidenceAttemptScope(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	cases := []struct {
		name, summary, expected string
		claim, capture, old     bool
	}{
		{"none", "", "not_collected", false, false, false},
		{"old", "complete", "not_collected", true, true, true},
		{"interrupted", "", "interrupted", true, false, false},
		{"unavailable", "unavailable", "unavailable", true, false, false},
		{"partial", "unavailable", "partial", true, true, false},
		{"truncated", "truncated", "truncated", true, true, false},
		{"full", "complete", "full", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
				Actor: testActor, IdempotencyKey: "evidence-" + tc.name, Input: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			if tc.claim {
				_, _, err := run.Service.mutateOwned(ctx, op.ID, 1, func(tx pgx.Tx, current *contract.Operation) error {
					diagnostic := func(status, message string) error {
						return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Stage: "candidate", Message: message,
							CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: "fixture", Status: status, RedactionContext: "original"}})
					}
					if err := diagnostic("unavailable", "Candidate diagnostic capture started."); err != nil {
						return err
					}
					if tc.capture {
						if err := diagnostic("captured", "original safe output"); err != nil {
							return err
						}
					}
					if tc.summary != "" {
						return diagnostic(tc.summary, "Candidate diagnostic capture: "+tc.summary+".")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			attempt := uint32(1)
			if tc.old {
				attempt = 2
				if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, attempt, "test"); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			}
			if err := run.Service.recordRetryResult(ctx, op.ID, attempt, contract.Unavailable, "failed"); err != nil {
				t.Fatal(err)
			}
			got, err := run.Service.Get(ctx, testActor, op.ID)
			if err != nil || got.LatestResult == nil || got.LatestResult.Evidence != tc.expected || got.LatestResult.Stage != "action" {
				t.Fatalf("result: %+v %v", got.LatestResult, err)
			}
			if tc.capture && !tc.old && (got.LatestResult.Excerpt != "original safe output" || got.LatestResult.History.After != 0 || got.LatestResult.ExcerptCursor == nil) {
				t.Fatalf("excerpt and cursor: %+v", got.LatestResult)
			}
		})
	}
}
