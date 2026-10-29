package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

type independentFileStep struct{ *fileStep }

func (*independentFileStep) IndependentOfHostLock() bool { return true }

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
