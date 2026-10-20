package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func execution(t *testing.T, step string, attempt uint32) engine.Execution {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return engine.Execution{OperationID: id, Attempt: attempt, Step: step, RequestID: "request"}
}

func runRequest(t *testing.T, exec engine.Execution, delay int) Request {
	t.Helper()
	raw, err := json.Marshal(DiagnosticPayload{Index: 1, Total: 1, StepDelayMS: delay})
	if err != nil {
		t.Fatal(err)
	}
	return Request{Method: MethodRun, RequestID: "request", Execution: exec, Payload: raw}
}

func TestLocalRunCreatesOneEffectAndReportsHonestly(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	response, err := local.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err != nil || response.Status != engine.StatusCompleted || !response.Executed || response.AppliedAttempt != 1 {
		t.Fatalf("first run: %+v %v", response, err)
	}
	rawMarker, err := os.ReadFile(MarkerPath(local.dispatcher.engine.StateDir, exec))
	if err != nil {
		t.Fatal(err)
	}
	var value marker
	if err := json.Unmarshal(rawMarker, &value); err != nil || value.AppliedAttempt != 1 {
		t.Fatalf("marker: %s %v", rawMarker, err)
	}
	status, err := local.Call(context.Background(), Request{Method: MethodStatus, RequestID: "status", Execution: exec}, nil)
	if err != nil || status.Status != engine.StatusCompleted || status.InputsHash == "" {
		t.Fatalf("status: %+v %v", status, err)
	}
	second, err := local.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err != nil || second.Status != engine.StatusCompleted || second.Executed {
		t.Fatalf("second run: %+v %v", second, err)
	}
	if got := countEffects(t, local.dispatcher.engine.StateDir, exec); got != 1 {
		t.Fatalf("duplicate effect: %d", got)
	}
}

func TestConcurrentRunsApplyOnce(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	var wait sync.WaitGroup
	var executed int
	var mu sync.Mutex
	for range 2 {
		wait.Go(func() {
			response, err := local.Call(context.Background(), runRequest(t, exec, 0), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if response.Executed {
				mu.Lock()
				executed++
				mu.Unlock()
			}
		})
	}
	wait.Wait()
	if executed != 1 {
		t.Fatalf("expected one Apply, got %d", executed)
	}
	if got := countEffects(t, local.dispatcher.engine.StateDir, exec); got != 1 {
		t.Fatalf("duplicate effect: %d", got)
	}
}

func TestStatusReportsLiveRun(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	done := make(chan Response, 1)
	go func() {
		response, callErr := local.Call(context.Background(), runRequest(t, exec, 250), nil)
		if callErr != nil {
			t.Error(callErr)
		}
		done <- response
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, statusErr := local.Call(context.Background(), Request{Method: MethodStatus, RequestID: "status", Execution: exec}, nil)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.Status == engine.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never observed running status: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	response := <-done
	if response.Status != engine.StatusCompleted {
		t.Fatalf("run did not complete: %+v", response)
	}
}

func TestStatusReportsStaleOwner(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	step, err := NewDiagnosticStep(exec, DiagnosticPayload{Index: 1, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	journal := engine.NewJournal("local")
	journal.Tasks[exec.EffectKey()] = engine.Task{
		OperationID: string(exec.OperationID), Step: exec.Step, Attempt: 1,
		ExecutionID: exec.ExecutionID(), InputsHash: engine.InputsHash(step.ID(), inputs),
		Status: engine.TaskRunning,
	}
	if err := journal.Save(local.dispatcher.engine.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	status, err := local.Call(context.Background(), Request{Method: MethodStatus, RequestID: "status", Execution: exec}, nil)
	if err != nil || status.Status != engine.StatusStale {
		t.Fatalf("stale status: %+v %v", status, err)
	}
	// Taking over runs Check and applies once.
	response, err := local.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err != nil || response.Status != engine.StatusCompleted || !response.Executed {
		t.Fatalf("takeover: %+v %v", response, err)
	}
}

func TestDiagnosticInputsAreStableAcrossAttempts(t *testing.T) {
	exec := execution(t, DiagnosticStepID(2), 1)
	first, err := NewDiagnosticStep(exec, DiagnosticPayload{Index: 2, Total: 3, StepDelayMS: 0})
	if err != nil {
		t.Fatal(err)
	}
	inputs, _ := first.Inputs()
	hash := engine.InputsHash(first.ID(), inputs)
	retry := exec
	retry.Attempt = 2
	second, err := NewDiagnosticStep(retry, DiagnosticPayload{Index: 2, Total: 3, StepDelayMS: 0})
	if err != nil {
		t.Fatal(err)
	}
	retryInputs, _ := second.Inputs()
	if engine.InputsHash(second.ID(), retryInputs) != hash {
		t.Fatal("retry changed the effect inputs hash")
	}
	if first.CancelBoundary() != engine.BoundaryAfterApply {
		t.Fatal("diagnostic step must finish before cancellation")
	}
}

func TestInvalidPayloadFailsClosed(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	raw, _ := json.Marshal(DiagnosticPayload{Index: 2, Total: 1})
	response, err := local.Call(context.Background(), Request{Method: MethodRun, RequestID: "bad", Execution: exec, Payload: raw}, nil)
	if err != nil || response.Status != engine.StatusFailed || response.Retryable {
		t.Fatalf("invalid payload: %+v %v", response, err)
	}
}

func TestRunClassifiesInputsMismatchAsPermanent(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	raw, _ := json.Marshal(DiagnosticPayload{Index: 1, Total: 2})
	if response, err := local.Call(context.Background(), Request{Method: MethodRun, RequestID: "first", Execution: exec, Payload: raw}, nil); err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("first run: %+v %v", response, err)
	}
	secondRaw, _ := json.Marshal(DiagnosticPayload{Index: 1, Total: 1})
	response, err := local.Call(context.Background(), Request{Method: MethodRun, RequestID: "second", Execution: exec, Payload: secondRaw}, nil)
	if err != nil || response.Status != engine.StatusFailed || response.Retryable || !response.InputsMismatch {
		t.Fatalf("inputs mismatch classification: %+v %v", response, err)
	}
	if got := countEffects(t, local.dispatcher.engine.StateDir, exec); got != 1 {
		t.Fatalf("inputs mismatch re-applied the effect: %d", got)
	}
}

func TestRunClassifiesCorruptMarkerAsPermanent(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	path := MarkerPath(local.dispatcher.engine.StateDir, exec)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	response, err := local.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err != nil || response.Status != engine.StatusFailed || response.Retryable || response.InputsMismatch {
		t.Fatalf("corrupt marker classification: %+v %v", response, err)
	}
}

func TestRecoveredEffectRecordsTheApplyingAttempt(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, DiagnosticStepID(1), 1)
	// A previous attempt applied the effect but died before the effect record
	// was published. The marker still names the attempt that applied it.
	path := MarkerPath(local.dispatcher.engine.StateDir, exec)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	inputs, err := NewDiagnosticStep(exec, DiagnosticPayload{Index: 1, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := inputs.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(marker{
		OperationID:    string(exec.OperationID),
		Step:           exec.Step,
		InputsHash:     engine.InputsHash(exec.Step, canonical),
		AppliedAttempt: 1,
		ExecutionID:    exec.ExecutionID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	retry := exec
	retry.Attempt = 2
	response, err := local.Call(context.Background(), runRequest(t, retry, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted || response.Executed {
		t.Fatalf("recovered effect: %+v", response)
	}
	if response.AppliedAttempt != 1 {
		t.Fatalf("applied attempt %d, want the attempt that applied the effect (1)", response.AppliedAttempt)
	}
}

func countEffects(t *testing.T, root string, exec engine.Execution) int {
	t.Helper()
	entries, err := os.ReadDir(root + "/effects/" + string(exec.OperationID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Name() == exec.Step+".applied" {
			count++
		}
	}
	return count
}
