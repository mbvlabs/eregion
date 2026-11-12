package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type independentFileStep struct{ *fileStep }

func (*independentFileStep) IndependentOfHostLock() bool { return true }

// countingStep records how often Check and Apply ran, so a wait for the host
// lock can prove neither started.
type countingStep struct {
	*fileStep
	mu      sync.Mutex
	checks  int
	applies int
}

func (s *countingStep) Check(ctx context.Context, rt Runtime) (CheckResult, error) {
	s.mu.Lock()
	s.checks++
	s.mu.Unlock()
	return s.fileStep.Check(ctx, rt)
}

func (s *countingStep) Apply(ctx context.Context, rt Runtime, report Reporter) (ApplyResult, error) {
	s.mu.Lock()
	s.applies++
	s.mu.Unlock()
	return s.fileStep.Apply(ctx, rt, report)
}

func (s *countingStep) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks, s.applies
}

// phasedStep blocks in Check and Apply so status can observe the typed phase.
type phasedStep struct {
	exec      Execution
	checking  chan struct{}
	checkHold chan struct{}
	applying  chan struct{}
	applyHold chan struct{}
}

func (s *phasedStep) ID() string       { return s.exec.Step }
func (s *phasedStep) Describe() string { return "phased step" }
func (s *phasedStep) Inputs() ([]byte, error) {
	return []byte(`{"step":"` + s.exec.Step + `"}`), nil
}
func (s *phasedStep) Check(context.Context, Runtime) (CheckResult, error) {
	close(s.checking)
	<-s.checkHold
	return CheckResult{Detail: "needs apply"}, nil
}
func (s *phasedStep) Apply(context.Context, Runtime, Reporter) (ApplyResult, error) {
	close(s.applying)
	<-s.applyHold
	return ApplyResult{}, nil
}
func (*phasedStep) CancelBoundary() Boundary { return BoundaryAfterApply }

func TestHostLockWaitIsDeferredNotLiveAndResumesOnce(t *testing.T) {
	ctx := context.Background()
	eng := newTestEngine(t)
	eng.LockTimeout = 30 * time.Millisecond

	// A prior host mutation (the build) completes before the network-like
	// step asks for the same host lock.
	buildExec := testExecution(t, "build-step-v1", 1)
	if status, err := eng.Run(ctx, testStep(t, buildExec, "", false), buildExec, nil); err != nil || status.Status != StatusCompleted {
		t.Fatalf("build step did not complete: %+v %v", status, err)
	}

	// The next step waits behind a real held host lock. It exits at its bounded
	// timeout without running Check or Apply.
	release, err := Acquire(ctx, eng.hostLockPath(), LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	networkExec := testExecution(t, "network-step-v1", 1)
	network := &countingStep{fileStep: testStep(t, networkExec, "", false)}
	status, err := eng.Run(ctx, network, networkExec, nil)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("host-lock timeout error = %v, want ErrLockHeld", err)
	}
	if status.Status != StatusDeferred {
		t.Fatalf("host-lock timeout status = %+v, want deferred", status)
	}
	if status.Phase != PhaseWaitingHostLock {
		t.Fatalf("host-lock timeout phase = %q, want %q", status.Phase, PhaseWaitingHostLock)
	}
	if status.WaitMS < 0 {
		t.Fatalf("negative wait %d", status.WaitMS)
	}
	if checks, applies := network.counts(); checks != 0 || applies != 0 {
		t.Fatalf("a deferred waiter ran Check/Apply: checks=%d applies=%d", checks, applies)
	}
	observed, err := eng.Status(ctx, networkExec)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status != StatusDeferred {
		t.Fatalf("persisted status = %+v, want deferred (not running/stale)", observed)
	}
	if observed.Phase != PhaseWaitingHostLock || observed.WaitMS < 0 {
		t.Fatalf("persisted phase/wait = %+v", observed)
	}
	journal, err := LoadJournal(eng.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	if task := journal.Tasks[networkExec.EffectKey()]; task.Status != TaskDeferred || task.WaitMS < 0 {
		t.Fatalf("journal deferral = %+v", task)
	}

	release()
	status, err = eng.Run(ctx, network, networkExec, nil)
	if err != nil || status.Status != StatusCompleted || !status.Executed {
		t.Fatalf("resume after host lock release: %+v %v", status, err)
	}
	if checks, applies := network.counts(); checks != 1 || applies != 1 {
		t.Fatalf("resume counts: checks=%d applies=%d, want 1/1", checks, applies)
	}
	// A replay must not apply a second time.
	status, err = eng.Run(ctx, network, networkExec, nil)
	if err != nil || status.Status != StatusCompleted || status.Executed {
		t.Fatalf("replay after resume: %+v %v", status, err)
	}
	if checks, applies := network.counts(); checks != 1 || applies != 1 {
		t.Fatalf("replay counts: checks=%d applies=%d, want 1/1", checks, applies)
	}
}

func TestStatusReportsCheckingAndApplyingPhases(t *testing.T) {
	ctx := context.Background()
	eng := newTestEngine(t)
	exec := testExecution(t, "phased-step-v1", 1)
	step := &phasedStep{
		exec: exec, checking: make(chan struct{}), checkHold: make(chan struct{}),
		applying: make(chan struct{}), applyHold: make(chan struct{}),
	}
	done := make(chan TaskStatus, 1)
	runErr := make(chan error, 1)
	go func() {
		status, err := eng.Run(ctx, step, exec, nil)
		done <- status
		runErr <- err
	}()
	<-step.checking
	observed, err := eng.Status(ctx, exec)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status != StatusRunning || observed.Phase != PhaseChecking {
		t.Fatalf("checking observation = %+v", observed)
	}
	close(step.checkHold)
	<-step.applying
	observed, err = eng.Status(ctx, exec)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status != StatusRunning || observed.Phase != PhaseApplying {
		t.Fatalf("applying observation = %+v", observed)
	}
	close(step.applyHold)
	if status := <-done; status.Status != StatusCompleted {
		t.Fatalf("phased run = %+v", status)
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
}

func TestOnlyIndependentSignalsRunWhileHostMutationIsLocked(t *testing.T) {
	ctx := context.Background()
	eng := newTestEngine(t)
	eng.LockTimeout = 20 * time.Millisecond
	release, err := Acquire(ctx, eng.hostLockPath(), LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	normalExec := testExecution(t, "normal-v1", 1)
	if _, err := eng.Run(ctx, testStep(t, normalExec, "", false), normalExec, nil); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("ordinary host mutation did not respect lock: %v", err)
	}
	signalExec := testExecution(t, "signal-v1", 1)
	signal := &independentFileStep{testStep(t, signalExec, "", false)}
	status, err := eng.Run(ctx, signal, signalExec, nil)
	if err != nil || status.Status != StatusCompleted || !status.Executed {
		t.Fatalf("signal blocked behind host lock: %+v %v", status, err)
	}
	status, err = eng.Run(ctx, signal, signalExec, nil)
	if err != nil || status.Status != StatusCompleted || status.Executed {
		t.Fatalf("signal replay was not idempotent: %+v %v", status, err)
	}
}
