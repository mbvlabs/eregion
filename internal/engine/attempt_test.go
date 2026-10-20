package engine

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRecoveredEffectRecordsTheApplyingAttempt(t *testing.T) {
	eng := newTestEngine(t)
	exec := testExecution(t, "test-step-v1", 1)
	step := &attemptStep{exec: exec, applied: 1}
	status, err := eng.Run(context.Background(), step, exec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Executed {
		t.Fatal("a recovered effect must not be applied again")
	}
	if status.AppliedAttempt != 1 {
		t.Fatalf("applied attempt %d, want 1", status.AppliedAttempt)
	}
	record, ok, err := LoadEffectRecord(eng.EffectRecordPath(exec))
	if err != nil || !ok {
		t.Fatalf("effect record: %v %v", ok, err)
	}
	if record.AppliedAttempt != 1 {
		t.Fatalf("recorded applied attempt %d, want 1", record.AppliedAttempt)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatal("effect record is not valid JSON")
	}
}

// attemptStep reports a durable applying attempt through Check.
type attemptStep struct {
	exec    Execution
	applied uint32
}

func (s *attemptStep) ID() string       { return s.exec.Step }
func (s *attemptStep) Describe() string { return "attempt step" }
func (s *attemptStep) Inputs() ([]byte, error) {
	return json.Marshal(struct {
		Step string `json:"step"`
	}{Step: s.exec.Step})
}
func (s *attemptStep) Check(context.Context, Runtime) (CheckResult, error) {
	return CheckResult{Complete: true, Detail: "already applied", AppliedAttempt: s.applied}, nil
}
func (s *attemptStep) Apply(context.Context, Runtime, Reporter) (ApplyResult, error) {
	return ApplyResult{}, nil
}
func (s *attemptStep) CancelBoundary() Boundary { return BoundaryAfterApply }
