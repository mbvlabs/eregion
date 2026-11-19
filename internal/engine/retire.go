package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrApplyQuiescent marks an Apply failure proven not to leave a build active.
var ErrApplyQuiescent = errors.New("build apply is quiescent")

func (e *Engine) fencePath(exec Execution, prefix string) string {
	sum := sha256.Sum256([]byte(exec.EffectKey()))
	return filepath.Join(e.StateDir, prefix+hex.EncodeToString(sum[:]))
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (e *Engine) retirementExists(exec Execution) (bool, error) {
	return fileExists(e.fencePath(exec, "retired-"))
}

// IsRetired is an advisory preflight. Run checks again under the effect lock.
func (e *Engine) IsRetired(exec Execution) (bool, error) {
	if err := exec.Validate(); err != nil {
		return false, err
	}
	return e.retirementExists(exec)
}

func (e *Engine) applyUncertain(exec Execution) (bool, error) {
	return fileExists(e.fencePath(exec, "build-applying-"))
}

func (e *Engine) markApplyUncertain(exec Execution) error {
	return WriteFileAtomic(e.fencePath(exec, "build-applying-"), []byte(exec.EffectKey()+"\n"), 0o600)
}

func (e *Engine) clearApplyUncertain(exec Execution) error {
	if err := os.Remove(e.fencePath(exec, "build-applying-")); err != nil {
		return err
	}
	return syncDir(e.StateDir)
}

// RetireBuild fences future runs even when the old apply cannot be proven
// quiescent. Only a completed result means both properties have been proven.
func (e *Engine) RetireBuild(ctx context.Context, exec Execution) (TaskStatus, error) {
	if err := exec.Validate(); err != nil {
		return TaskStatus{}, err
	}
	release, err := Acquire(ctx, e.effectLockPath(exec), LockExclusive, 0)
	if errors.Is(err, ErrLockHeld) {
		return TaskStatus{Status: StatusRunning, Detail: "build effect lock is held"}, nil
	}
	if err != nil {
		return TaskStatus{}, err
	}
	defer release()
	if err := WriteFileAtomic(e.fencePath(exec, "retired-"), []byte(exec.EffectKey()+"\n"), 0o600); err != nil {
		return TaskStatus{}, err
	}
	uncertain, err := e.applyUncertain(exec)
	if err != nil {
		return TaskStatus{}, err
	}
	if uncertain {
		return TaskStatus{Status: StatusStale, Detail: "build apply may still be active"}, nil
	}
	_, completed, err := LoadEffectRecord(e.EffectRecordPath(exec))
	if err != nil {
		return TaskStatus{}, err
	}
	if !completed {
		status, err := e.retirementJournalStatus(exec)
		if err != nil || status.Status != "" {
			return status, err
		}
	}
	return TaskStatus{Status: StatusCompleted, ExecutionID: exec.ExecutionID(), Detail: "build retired and quiescent"}, nil
}

// An empty status means the journal establishes quiescence.
func (e *Engine) retirementJournalStatus(exec Execution) (TaskStatus, error) {
	journal, err := LoadJournal(e.JournalPath())
	if err != nil {
		return TaskStatus{}, err
	}
	trusted, err := fileExists(filepath.Join(e.StateDir, "build-history-v1"))
	if err != nil {
		return TaskStatus{}, err
	}
	task, ok := journal.Tasks[exec.EffectKey()]
	if !ok {
		if !trusted {
			return TaskStatus{Status: StatusStale, Detail: "pre-upgrade journal history may have been compacted"}, nil
		}
		return TaskStatus{}, nil
	}
	if !trusted && task.Status != TaskCompleted {
		return TaskStatus{Status: StatusStale, Detail: "pre-upgrade build history may contain an interrupted apply"}, nil
	}
	switch {
	case task.Status == TaskCompleted:
		return TaskStatus{}, nil
	case (task.Status == TaskRunning || task.Status == TaskDeferred || task.Status == TaskFailed) &&
		(task.Phase == PhaseWaitingHostLock || task.Phase == PhaseChecking):
		return TaskStatus{}, nil
	default:
		return TaskStatus{Status: StatusStale, Detail: fmt.Sprintf("cannot prove build quiescent after phase %q", task.Phase)}, nil
	}
}
