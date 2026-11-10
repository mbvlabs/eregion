package host

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type journalProbeStep struct {
	id      string
	applied bool
}

func (s *journalProbeStep) ID() string { return s.id }
func (s *journalProbeStep) Describe() string {
	return "test-only journal effect"
}
func (s *journalProbeStep) Inputs() ([]byte, error) { return []byte("test"), nil }
func (s *journalProbeStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return incomplete("probe is never pre-complete")
}
func (s *journalProbeStep) Apply(context.Context, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	s.applied = true
	return engine.ApplyResult{}, nil
}
func (s *journalProbeStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func TestCompletedOldJournalEntryDoesNotSkipTheNewHostStepVersion(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.New(dir, "main", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	operation := contract.OperationID(operationID("main", "fixture-hash", 3))
	old := engine.Execution{OperationID: operation, Attempt: 3, Step: "host-ingress-v2"}
	journal := engine.NewJournal("main")
	journal.Tasks[old.EffectKey()] = engine.Task{
		OperationID: string(operation), Step: old.Step, Attempt: 3, Status: engine.TaskCompleted, AppliedAttempt: 3,
	}
	if err := journal.Save(eng.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}

	oldStatus, err := eng.Status(context.Background(), old)
	if err != nil || oldStatus.Status != engine.StatusCompleted {
		t.Fatalf("old journal status = %+v err=%v", oldStatus, err)
	}
	newExecution := engine.Execution{OperationID: operation, Attempt: 3, Step: "host-ingress-v3"}
	status, err := eng.Status(context.Background(), newExecution)
	if err != nil || status.Status != engine.StatusAbsent {
		t.Fatalf("new step inherited the old completed entry: %+v err=%v", status, err)
	}
	step := &journalProbeStep{id: "host-ingress-v3"}
	status, err = eng.Run(context.Background(), step, newExecution, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Executed || !step.applied || status.Status != engine.StatusCompleted {
		t.Fatalf("new step was skipped by the completed old journal: status=%+v applied=%t", status, step.applied)
	}
}
