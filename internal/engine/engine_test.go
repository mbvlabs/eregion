package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func testExecution(t *testing.T, step string, attempt uint32) Execution {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return Execution{OperationID: id, Attempt: attempt, Step: step, RequestID: "req"}
}

func testStep(t *testing.T, exec Execution, secret string, once bool) *fileStep {
	t.Helper()
	return &fileStep{exec: exec, secret: secret, once: once}
}

type fileStep struct {
	exec   Execution
	secret string
	once   bool
}

func (s *fileStep) ID() string { return s.exec.Step }

func (s *fileStep) Describe() string { return "test step" }

func (s *fileStep) Inputs() ([]byte, error) {
	return json.Marshal(struct {
		Step   string `json:"step"`
		Secret string `json:"secret,omitempty"`
	}{Step: s.exec.Step, Secret: HashSecret(s.secret)})
}

func (s *fileStep) file(rt Runtime) string {
	return filepath.Join(rt.Root, "effects", string(s.exec.OperationID), s.exec.Step+".applied")
}

func (s *fileStep) Check(_ context.Context, rt Runtime) (CheckResult, error) {
	_, err := os.Stat(s.file(rt))
	if err == nil {
		return CheckResult{Complete: true}, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return CheckResult{Complete: false}, nil
	}
	return CheckResult{}, err
}

func (s *fileStep) Apply(_ context.Context, rt Runtime, _ Reporter) (ApplyResult, error) {
	path := s.file(rt)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ApplyResult{}, err
	}
	payload := []byte(s.exec.ExecutionID())
	if s.once {
		existed, err := WriteFileOnce(path, payload, 0o600)
		if err != nil {
			return ApplyResult{}, err
		}
		if existed {
			return ApplyResult{Result: map[string]bool{"applied": false}}, nil
		}
	} else if err := WriteFileAtomic(path, payload, 0o600); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Result: map[string]bool{"applied": true}}, nil
}

func (*fileStep) CancelBoundary() Boundary { return BoundaryAfterApply }

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	eng, err := New(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	eng.LockTimeout = 2 * time.Second
	return eng
}

func TestRunIsIdempotentAndJournaled(t *testing.T) {
	eng := newTestEngine(t)
	exec := testExecution(t, "test-step-v1", 1)
	step := testStep(t, exec, "", false)
	ctx := context.Background()

	status, err := eng.Run(ctx, step, exec, nil)
	if err != nil || status.Status != StatusCompleted || !status.Executed {
		t.Fatalf("first run: %+v %v", status, err)
	}
	status, err = eng.Run(ctx, step, exec, nil)
	if err != nil || status.Status != StatusCompleted || status.Executed {
		t.Fatalf("second run: %+v %v", status, err)
	}
	journal, err := LoadJournal(eng.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	task := journal.Tasks[exec.EffectKey()]
	if task.Status != TaskCompleted || task.AppliedAttempt != 1 || task.InputsHash == "" {
		t.Fatalf("journal task: %+v", task)
	}
	raw, err := os.ReadFile(step.file(Runtime{Root: eng.StateDir}))
	if err != nil || string(raw) != exec.ExecutionID() {
		t.Fatalf("effect content: %q %v", raw, err)
	}
}

func TestStatusTracksLockOwnership(t *testing.T) {
	eng := newTestEngine(t)
	exec := testExecution(t, "status-step-v1", 1)
	step := testStep(t, exec, "", false)
	release, err := Acquire(context.Background(), eng.effectLockPath(exec), LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	task := Task{
		OperationID: string(exec.OperationID), Step: exec.Step, Attempt: 1,
		ExecutionID: exec.ExecutionID(), InputsHash: InputsHash(step.ID(), inputs), Status: TaskRunning,
	}
	if err := updateJournal(eng.JournalPath(), eng.registry(), func(j *Journal) { j.Tasks[exec.EffectKey()] = task }); err != nil {
		t.Fatal(err)
	}
	status, err := eng.Status(context.Background(), exec)
	if err != nil || status.Status != StatusRunning {
		t.Fatalf("live status: %+v %v", status, err)
	}
	release()
	status, err = eng.Status(context.Background(), exec)
	if err != nil || status.Status != StatusStale {
		t.Fatalf("stale status: %+v %v", status, err)
	}
	// A stale running entry is safely taken over; Check finds no effect yet.
	status, err = eng.Run(context.Background(), step, exec, nil)
	if err != nil || status.Status != StatusCompleted || !status.Executed {
		t.Fatalf("takeover: %+v %v", status, err)
	}
}

func TestChangedInputsAndCreateOnce(t *testing.T) {
	eng := newTestEngine(t)
	exec := testExecution(t, "inputs-step-v1", 1)
	first := testStep(t, exec, "one", true)
	if _, err := eng.Run(context.Background(), first, exec, nil); err != nil {
		t.Fatal(err)
	}
	// A different secret changes the canonical inputs hash. The create-once step
	// refuses to overwrite its original effect.
	second := testStep(t, exec, "two", true)
	if _, err := eng.Run(context.Background(), second, exec, nil); err == nil {
		t.Fatal("create-once step accepted different inputs")
	}
}

func TestSecretsAreHashedAndRedacted(t *testing.T) {
	registry := &secrets.Registry{}
	registry.Add("super-secret-value")
	eng, err := New(t.TempDir(), "local", registry)
	if err != nil {
		t.Fatal(err)
	}
	exec := testExecution(t, "secret-step-v1", 1)
	step := testStep(t, exec, "super-secret-value", false)
	if _, err := eng.Run(context.Background(), step, exec, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(eng.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatal("journal leaked a plaintext secret")
	}
	inputs, _ := step.Inputs()
	if !strings.Contains(string(raw), InputsHash(step.ID(), inputs)) {
		t.Fatal("journal did not record the canonical inputs hash")
	}
}

func TestCorruptJournalFailsClosed(t *testing.T) {
	eng := newTestEngine(t)
	if err := os.MkdirAll(filepath.Dir(eng.JournalPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eng.JournalPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := testExecution(t, "corrupt-step-v1", 1)
	if _, err := eng.Status(context.Background(), exec); !errors.Is(err, ErrJournalCorrupt) {
		t.Fatalf("corrupt journal did not fail closed: %v", err)
	}
}

func TestConcurrentRunAppliesOnce(t *testing.T) {
	eng := newTestEngine(t)
	exec := testExecution(t, "concurrent-step-v1", 1)
	step := testStep(t, exec, "", true)
	var wait sync.WaitGroup
	executed := make(chan bool, 2)
	for range 2 {
		wait.Go(func() {
			status, err := eng.Run(context.Background(), step, exec, nil)
			if err != nil {
				t.Error(err)
				return
			}
			executed <- status.Executed
		})
	}
	wait.Wait()
	close(executed)
	total := 0
	for value := range executed {
		if value {
			total++
		}
	}
	if total != 1 {
		t.Fatalf("expected exactly one Apply, got %d", total)
	}
}

func TestWriteFileOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effect")
	existed, err := WriteFileOnce(path, []byte("first"), 0o600)
	if err != nil || existed {
		t.Fatalf("first write: %v %v", existed, err)
	}
	existed, err = WriteFileOnce(path, []byte("second"), 0o600)
	if err != nil || !existed {
		t.Fatalf("second write: %v %v", existed, err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "first" {
		t.Fatalf("create-once was overwritten: %q", raw)
	}
}

func TestLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	release, err := Acquire(context.Background(), path, LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), path, LockExclusive, 0); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	release, err = Acquire(context.Background(), path, LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestStatusRecoversEffectBeforeJournal(t *testing.T) {
	eng := newTestEngine(t)
	exec := testExecution(t, "effect-first-v1", 1)
	step := testStep(t, exec, "", false)
	if _, err := eng.Run(context.Background(), step, exec, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after publishing the effect record but before the final
	// journal write.
	journal := NewJournal("local")
	journal.Tasks[exec.EffectKey()] = Task{
		OperationID: string(exec.OperationID), Step: exec.Step, Attempt: 1,
		ExecutionID: exec.ExecutionID(), InputsHash: "stale", Status: TaskRunning,
	}
	if err := journal.Save(eng.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	status, err := eng.Status(context.Background(), exec)
	if err != nil || status.Status != StatusCompleted || status.ExecutionID != exec.ExecutionID() {
		t.Fatalf("effect record did not win: %+v %v", status, err)
	}
	if err := os.Remove(eng.JournalPath()); err != nil {
		t.Fatal(err)
	}
	status, err = eng.Status(context.Background(), exec)
	if err != nil || status.Status != StatusCompleted {
		t.Fatalf("missing journal hid the effect: %+v %v", status, err)
	}
}
