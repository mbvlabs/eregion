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

// RetireBuildStopMarker fences only the synchronous build-stop marker effect.
// The caller must first durably abandon the original dispatch under its
// operation and intent fence. Completion does not establish that Pack stopped,
// or that the marker was not written before this fence.
func (e *Engine) RetireBuildStopMarker(ctx context.Context, exec Execution, inputsHash string) (TaskStatus, error) {
	if err := exec.Validate(); err != nil {
		return TaskStatus{}, err
	}
	hash, err := hex.DecodeString(inputsHash)
	if exec.Step != "platform-build-stop-01-v1" || exec.Attempt == 0 ||
		err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != inputsHash {
		return TaskStatus{}, fmt.Errorf("original build-stop execution and canonical input hash are required")
	}
	release, err := Acquire(ctx, e.effectLockPath(exec), LockExclusive, 0)
	if errors.Is(err, ErrLockHeld) {
		return TaskStatus{Status: StatusRunning, Detail: "build-stop effect lock is held"}, nil
	}
	if err != nil {
		return TaskStatus{}, err
	}
	defer release()
	fence := e.fencePath(exec, "retired-")
	identity := exec.ExecutionID() + "\n" + inputsHash + "\n"
	existing, err := os.ReadFile(fence)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return TaskStatus{}, err
	}
	if err == nil && string(existing) != identity {
		return TaskStatus{Status: StatusStale, Detail: "retirement does not match original execution"}, nil
	}
	record, found, err := LoadEffectRecord(e.EffectRecordPath(exec))
	if err != nil {
		return TaskStatus{}, err
	}
	if found && !matchingProvenEffect(record, exec, inputsHash) {
		return TaskStatus{Status: StatusStale, Detail: "effect record does not match original execution"}, nil
	}
	journal, err := LoadJournal(e.JournalPath())
	if err != nil {
		return TaskStatus{}, err
	}
	if task, found := journal.Tasks[exec.EffectKey()]; found && !matchingProvenTask(task, exec, inputsHash) {
		return TaskStatus{Status: StatusStale, Detail: "journal task does not match original execution"}, nil
	}
	if err := WriteFileAtomic(fence, []byte(identity), 0o600); err != nil {
		return TaskStatus{}, err
	}
	return TaskStatus{Status: StatusCompleted, ExecutionID: exec.ExecutionID(), Attempt: exec.Attempt, InputsHash: inputsHash, Detail: "build-stop marker fenced; prior marker outcome is not established"}, nil
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

// RetireProvenBuild is the stricter local recovery path for an unverified
// legacy claim. The caller must supply the original execution and input hash
// from independent durable dispatch history, not a newly constructed attempt.
// Only StatusCompleted permits releasing the claim.
func (e *Engine) RetireProvenBuild(ctx context.Context, exec Execution, inputsHash string) (TaskStatus, error) {
	if err := exec.Validate(); err != nil {
		return TaskStatus{}, err
	}
	hash, err := hex.DecodeString(inputsHash)
	if exec.Attempt == 0 || err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != inputsHash {
		return TaskStatus{}, fmt.Errorf("original execution attempt and canonical input hash are required")
	}
	release, err := Acquire(ctx, e.effectLockPath(exec), LockExclusive, 0)
	if errors.Is(err, ErrLockHeld) {
		return TaskStatus{Status: StatusRunning, Detail: "build effect lock is held"}, nil
	}
	if err != nil {
		return TaskStatus{}, err
	}
	defer release()

	status, err := e.provenBuildStatus(exec, inputsHash)
	if err != nil || status.Status != StatusCompleted {
		return status, err
	}
	// Fence only a proven execution, under the same lock that serializes Run.
	if err := WriteFileAtomic(e.fencePath(exec, "retired-"), []byte(exec.EffectKey()+"\n"), 0o600); err != nil {
		return TaskStatus{}, err
	}
	return status, nil
}

// provenBuildStatus is called with the effect lock held.
func (e *Engine) provenBuildStatus(exec Execution, inputsHash string) (TaskStatus, error) {
	uncertain, err := e.applyUncertain(exec)
	if err != nil {
		return TaskStatus{}, err
	}
	if uncertain {
		return TaskStatus{Status: StatusStale, Detail: "build apply may still be active"}, nil
	}
	record, hasRecord, err := LoadEffectRecord(e.EffectRecordPath(exec))
	if err != nil {
		return TaskStatus{}, err
	}
	journal, err := LoadJournal(e.JournalPath())
	if err != nil {
		return TaskStatus{}, err
	}
	task, hasTask := journal.Tasks[exec.EffectKey()]
	if hasRecord && !matchingProvenEffect(record, exec, inputsHash) {
		return TaskStatus{Status: StatusStale, Detail: "effect record does not match original execution"}, nil
	}
	if hasTask && !matchingProvenTask(task, exec, inputsHash) {
		return TaskStatus{Status: StatusStale, Detail: "journal task does not match original execution"}, nil
	}
	if hasTask && task.Status != TaskRunning && task.Status != TaskDeferred && task.Status != TaskFailed && task.Status != TaskCompleted {
		return TaskStatus{Status: StatusStale, Detail: "journal task has unknown status"}, nil
	}
	// Neither completion nor a phase before Apply excludes an older orphaned
	// apply unless uncertainty tracking was enabled for the whole history.
	trusted, err := e.provenBuildHistory()
	if err != nil {
		return TaskStatus{}, err
	}
	if !trusted {
		return TaskStatus{Status: StatusStale, Detail: "build history cannot exclude an earlier interrupted apply"}, nil
	}
	detail := ""
	if hasRecord {
		detail = "matching effect retired and quiescent"
	} else if hasTask &&
		(task.Status == TaskRunning || task.Status == TaskDeferred || task.Status == TaskFailed) &&
		(task.Phase == PhaseWaitingHostLock || task.Phase == PhaseChecking) {
		detail = "matching pre-apply task retired and quiescent"
	}
	if detail == "" {
		return TaskStatus{Status: StatusStale, Detail: "no matching durable proof of build quiescence"}, nil
	}
	return TaskStatus{Status: StatusCompleted, ExecutionID: exec.ExecutionID(), InputsHash: inputsHash, Detail: detail}, nil
}

func matchingProvenEffect(record EffectRecord, exec Execution, inputsHash string) bool {
	_, validCompletionTime := parseTaskTime(record.CompletedAt)
	return record.OperationID == string(exec.OperationID) && record.Step == exec.Step &&
		record.ExecutionID == exec.ExecutionID() && record.InputsHash == inputsHash && validCompletionTime
}

func matchingProvenTask(task Task, exec Execution, inputsHash string) bool {
	return task.OperationID == string(exec.OperationID) && task.Step == exec.Step &&
		task.Attempt == exec.Attempt && task.ExecutionID == exec.ExecutionID() && task.InputsHash == inputsHash
}

func (e *Engine) provenBuildHistory() (bool, error) {
	history, err := os.ReadFile(filepath.Join(e.StateDir, "build-history-v1"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return string(history) == "1\n", err
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
