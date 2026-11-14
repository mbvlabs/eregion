package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Uses the real node dispatcher and the isolated PostgreSQL operation fixture.
// No Docker or other host fixture is started.
func TestIntegrationNodeFailureResultSurvivesStatus(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	dispatcher, err := node.NewDispatcher(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	step := "platform-argv-evidence-v1"
	execution := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
	// Git includes this nonexistent path in its real stderr.
	path := filepath.Join(t.TempDir(), "synthetic\x1bvalue")
	raw, err := json.Marshal(platform.ArgvPayload{Step: step, Argv: []string{"git", "-C", path, "rev-parse", "--verify", "HEAD"}})
	if err != nil {
		t.Fatal(err)
	}
	request := node.Request{Method: node.MethodRun, Execution: execution, Payload: raw, Secrets: map[string]string{"TEST_SECRET": "known-multiline\nsecret-value", "TEST_CONTROL": "synthetic?value"}}
	response, err := dispatcher.Call(ctx, request, nil)
	if err != nil || response.Status != engine.StatusFailed || len(response.Result) == 0 {
		t.Fatalf("run: %+v err=%v", response, err)
	}
	var evidence platform.StepExecutionEvidence
	if err := json.Unmarshal(response.Result, &evidence); err != nil || evidence.Stage != step || !strings.Contains(evidence.Stderr, secrets.Placeholder) {
		t.Fatalf("failure evidence: %+v err=%v", evidence, err)
	}
	status, err := dispatcher.Call(ctx, node.Request{Method: node.MethodStatus, Execution: execution}, nil)
	var recovered platform.StepExecutionEvidence
	decodeErr := json.Unmarshal(status.Result, &recovered)
	if err != nil || decodeErr != nil || status.Status != engine.StatusFailed || recovered.Stage != evidence.Stage || recovered.Stderr != evidence.Stderr || strings.Contains(recovered.Stderr, "synthetic?value") || strings.Contains(string(status.Result), "known-multiline") {
		t.Fatalf("status: %+v err=%v", status, err)
	}
}

func TestIntegrationCandidateObservationAttemptsHaveDistinctNodeEffects(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	dispatcher, err := node.NewDispatcher(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := uint32(1); attempt <= 2; attempt++ {
		step := fmt.Sprintf("platform-candidate-diagnostic-%d-v1", attempt)
		execution := engine.Execution{OperationID: f.op.ID, Attempt: attempt, Step: step}
		payload := platform.CandidateDiagnosticPayload{Step: step, App: f.env.App, Environment: f.env.Name, ReleaseID: f.release, DeploymentID: f.dep, Deadline: time.Now().Add(time.Minute), Containers: []platform.CandidateDiagnosticContainer{}, MetadataOnly: true}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		response, err := dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw}, nil)
		if err != nil || response.Status != engine.StatusCompleted || response.InputsMismatch {
			t.Fatalf("attempt %d: %+v err=%v", attempt, response, err)
		}
		var result platform.CandidateDiagnosticResult
		if err := json.Unmarshal(response.Result, &result); err != nil || !result.Unavailable {
			t.Fatalf("metadata-only attempt %d: %+v err=%v", attempt, result, err)
		}
	}
}

func TestIntegrationCandidateCaptureRejectsNewerOwner(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, f.op.ID); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, f.pool, f.env, deploymentOwner{
		EnvironmentID: f.env.ID, OperationID: f.op.ID, DeploymentID: f.dep,
		Attempt: 2, Phase: deploymentOwnerPrepared,
	})
	dep, err := f.service.deploymentByID(ctx, f.dep)
	if err != nil {
		t.Fatal(err)
	}
	f.service.captureCandidateDiagnostics(ctx, f.env, dep, 1)
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2`, f.op.ID, platform.CandidateDiagnosticStage).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale attempt wrote %d capture events", count)
	}
}
