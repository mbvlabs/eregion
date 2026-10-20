package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Status is an honest node-side observation for one effect.
type Status string

const (
	StatusAbsent    Status = "absent"
	StatusRunning   Status = "running"
	StatusStale     Status = "stale"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// TaskStatus is what status/run returns for one effect identity.
type TaskStatus struct {
	Status         Status          `json:"status"`
	ExecutionID    string          `json:"execution_id,omitempty"`
	Attempt        uint32          `json:"attempt,omitempty"`
	AppliedAttempt uint32          `json:"applied_attempt,omitempty"`
	InputsHash     string          `json:"inputs_hash,omitempty"`
	Executed       bool            `json:"executed"`
	Result         json.RawMessage `json:"result,omitempty"`
	Detail         string          `json:"detail,omitempty"`
}

func TaskStatusName(state string) Status {
	switch state {
	case TaskRunning:
		return StatusRunning
	case TaskCompleted:
		return StatusCompleted
	case TaskFailed:
		return StatusFailed
	default:
		return Status(state)
	}
}

// Execution identifies one node task invocation. EffectKey is stable across
// attempts; ExecutionID includes the attempt.
type Execution struct {
	OperationID contract.OperationID `json:"operation_id"`
	Attempt     uint32               `json:"attempt"`
	Step        string               `json:"step"`
	RequestID   string               `json:"request_id,omitempty"`
}

func (e Execution) ExecutionID() string {
	return fmt.Sprintf("%s:%d:%s", e.OperationID, e.Attempt, e.Step)
}

func (e Execution) EffectKey() string {
	return fmt.Sprintf("%s:%s", e.OperationID, e.Step)
}

func (e Execution) Validate() error {
	if err := e.OperationID.Validate(); err != nil {
		return err
	}
	if !IDPattern.MatchString(e.Step) {
		return fmt.Errorf("invalid step ID %q", e.Step)
	}
	return nil
}

// Engine owns one node-local state directory.
type Engine struct {
	StateDir    string
	Node        string
	Secrets     *secrets.Registry
	LockTimeout time.Duration
}

// New validates and creates the engine's private state directory.
func New(stateDir, node string, registry *secrets.Registry) (*Engine, error) {
	if stateDir == "" {
		return nil, errors.New("node state directory is required")
	}
	if err := ValidateNodeName(node); err != nil {
		return nil, err
	}
	if err := EnsureDir(stateDir); err != nil {
		return nil, err
	}
	return &Engine{StateDir: stateDir, Node: node, Secrets: registry, LockTimeout: 20 * time.Second}, nil
}

func (e *Engine) JournalPath() string { return filepath.Join(e.StateDir, "journal.json") }

func (e *Engine) hostLockPath() string { return filepath.Join(e.StateDir, "host.lock") }

func (e *Engine) effectLockPath(exec Execution) string {
	sum := sha256.Sum256([]byte(exec.EffectKey()))
	return filepath.Join(e.StateDir, "locks", hex.EncodeToString(sum[:])+".lock")
}

// EffectRecordPath is the durable effect location for operation+step.
func (e *Engine) EffectRecordPath(exec Execution) string {
	return filepath.Join(e.StateDir, "effects", string(exec.OperationID), exec.Step+".json")
}

func (e *Engine) saveTask(exec Execution, task Task) error {
	return updateJournal(e.JournalPath(), e.registry(), func(journal *Journal) {
		journal.Node = e.Node
		journal.Tasks[exec.EffectKey()] = task
	})
}

func (e *Engine) registry() *secrets.Registry {
	if e.Secrets == nil {
		return &secrets.Registry{}
	}
	return e.Secrets
}

func (e *Engine) lockTimeout() time.Duration {
	if e.LockTimeout <= 0 {
		return 20 * time.Second
	}
	return e.LockTimeout
}

// Status reports the durable state of one effect without mutating anything.
// A running journal entry is only "running" while another process holds the
// effect lock; the kernel releases that lock when the owner dies.
func (e *Engine) Status(ctx context.Context, exec Execution) (TaskStatus, error) {
	if err := exec.Validate(); err != nil {
		return TaskStatus{}, err
	}
	if record, ok, err := LoadEffectRecord(e.EffectRecordPath(exec)); err != nil {
		return TaskStatus{}, err
	} else if ok {
		return TaskStatus{
			Status:         StatusCompleted,
			ExecutionID:    record.ExecutionID,
			Attempt:        record.AppliedAttempt,
			AppliedAttempt: record.AppliedAttempt,
			InputsHash:     record.InputsHash,
			Result:         record.Result,
			Detail:         "durable effect record",
		}, nil
	}
	journal, err := LoadJournal(e.JournalPath())
	if err != nil {
		return TaskStatus{}, err
	}
	task, ok := journal.Tasks[exec.EffectKey()]
	if !ok {
		return TaskStatus{Status: StatusAbsent, ExecutionID: exec.ExecutionID()}, nil
	}
	status := TaskStatus{
		Status:         TaskStatusName(task.Status),
		ExecutionID:    task.ExecutionID,
		Attempt:        task.Attempt,
		AppliedAttempt: task.AppliedAttempt,
		InputsHash:     task.InputsHash,
		Result:         task.Result,
	}
	switch task.Status {
	case TaskCompleted, TaskFailed:
		return status, nil
	case TaskRunning:
		release, lockErr := Acquire(ctx, e.effectLockPath(exec), LockExclusive, 0)
		if lockErr == nil {
			release()
			status.Status = StatusStale
			status.Detail = "owner lock is free; the previous attempt was interrupted"
			return status, nil
		}
		if errors.Is(lockErr, ErrLockHeld) {
			status.Status = StatusRunning
			status.Detail = "owner holds the local effect lock"
			return status, nil
		}
		return TaskStatus{}, lockErr
	default:
		return TaskStatus{}, fmt.Errorf("journal task %s has unknown status %q", exec.EffectKey(), task.Status)
	}
}

// Run acquires the effect lock, reconciles an existing effect, and otherwise
// runs Check then Apply once. It returns StatusRunning with ErrLockHeld when a
// live local owner holds the effect longer than the lock timeout.
func (e *Engine) Run(ctx context.Context, step Step, exec Execution, report Reporter) (TaskStatus, error) {
	if err := exec.Validate(); err != nil {
		return TaskStatus{}, err
	}
	if step.ID() != exec.Step {
		return TaskStatus{}, fmt.Errorf("step ID %q does not match execution step %q", step.ID(), exec.Step)
	}
	inputs, err := step.Inputs()
	if err != nil {
		return TaskStatus{}, fmt.Errorf("canonical inputs for %s: %w", step.ID(), err)
	}
	inputsHash := InputsHash(step.ID(), inputs)
	effectLock, err := Acquire(ctx, e.effectLockPath(exec), LockExclusive, e.lockTimeout())
	if err != nil {
		if errors.Is(err, ErrLockHeld) {
			return TaskStatus{Status: StatusRunning, ExecutionID: exec.ExecutionID(), InputsHash: inputsHash, Detail: "another local execution holds the effect lock"}, ErrLockHeld
		}
		return TaskStatus{}, err
	}
	defer effectLock()

	// Re-read everything under the effect lock. This closes the status->run race.
	if record, ok, err := LoadEffectRecord(e.EffectRecordPath(exec)); err != nil {
		return TaskStatus{}, err
	} else if ok {
		if record.InputsHash != inputsHash {
			return TaskStatus{}, fmt.Errorf("effect %s completed with different inputs", exec.EffectKey())
		}
		if err := e.completeFromRecord(exec, record); err != nil {
			return TaskStatus{}, err
		}
		return TaskStatus{
			Status:         StatusCompleted,
			ExecutionID:    record.ExecutionID,
			Attempt:        record.AppliedAttempt,
			AppliedAttempt: record.AppliedAttempt,
			InputsHash:     record.InputsHash,
			Result:         record.Result,
			Detail:         "recovered durable effect record",
		}, nil
	}

	journal, err := LoadJournal(e.JournalPath())
	if err != nil {
		return TaskStatus{}, err
	}
	if task, ok := journal.Tasks[exec.EffectKey()]; ok {
		if task.InputsHash != "" && task.InputsHash != inputsHash {
			return TaskStatus{}, fmt.Errorf("effect %s has different inputs", exec.EffectKey())
		}
		if task.Status == TaskCompleted {
			return TaskStatus{
				Status:         StatusCompleted,
				ExecutionID:    task.ExecutionID,
				Attempt:        task.Attempt,
				AppliedAttempt: task.AppliedAttempt,
				InputsHash:     task.InputsHash,
				Result:         task.Result,
				Detail:         "journal already completed",
			}, nil
		}
	}

	token, err := claimToken()
	if err != nil {
		return TaskStatus{}, err
	}
	owner := Owner{PID: os.Getpid(), BootID: bootID(), Token: token}
	running := Task{
		OperationID: string(exec.OperationID),
		Step:        exec.Step,
		Attempt:     exec.Attempt,
		ExecutionID: exec.ExecutionID(),
		RequestID:   exec.RequestID,
		InputsHash:  inputsHash,
		Status:      string(StatusRunning),
		Owner:       owner,
		StartedAt:   now(),
	}
	if err := e.saveTask(exec, running); err != nil {
		return TaskStatus{}, err
	}

	hostLock, err := Acquire(ctx, e.hostLockPath(), LockExclusive, e.lockTimeout())
	if err != nil {
		if errors.Is(err, ErrLockHeld) {
			return TaskStatus{Status: StatusRunning, ExecutionID: exec.ExecutionID(), InputsHash: inputsHash, Detail: "another local host mutation holds the host lock"}, ErrLockHeld
		}
		return TaskStatus{}, err
	}
	defer hostLock()

	// Check again under the host lock: another effect may have changed shared state.
	check, err := step.Check(ctx, Runtime{DryRun: false, Secrets: e.registry(), Root: e.StateDir})
	if err != nil {
		return e.fail(exec, running, fmt.Errorf("check %s: %w", step.ID(), err))
	}
	executed := false
	result := ApplyResult{}
	if !check.Complete {
		report.emit(Event{Kind: "log", Step: step.ID(), Message: check.Detail})
		result, err = step.Apply(ctx, Runtime{DryRun: false, Secrets: e.registry(), Root: e.StateDir}, report)
		if err != nil {
			return e.fail(exec, running, err)
		}
		executed = true
	}

	encodedResult, err := json.Marshal(result.Result)
	if err != nil {
		// The effect already happened. Do not turn an unserializable diagnostic
		// result into a failed step that would be re-applied; keep the durable
		// record without it.
		encodedResult = nil
	}
	appliedAttempt := uint32(0)
	if executed {
		appliedAttempt = exec.Attempt
	} else if task, ok := journal.Tasks[exec.EffectKey()]; ok {
		appliedAttempt = task.AppliedAttempt
	}
	record := EffectRecord{
		OperationID:    string(exec.OperationID),
		Step:           exec.Step,
		InputsHash:     inputsHash,
		AppliedAttempt: appliedAttempt,
		ExecutionID:    exec.ExecutionID(),
		RequestID:      exec.RequestID,
		CompletedAt:    now(),
		Result:         encodedResult,
	}
	if err := EnsureDir(filepath.Dir(e.EffectRecordPath(exec))); err != nil {
		return e.fail(exec, running, err)
	}
	existed, err := SaveEffectRecord(e.EffectRecordPath(exec), record)
	if err != nil {
		return e.fail(exec, running, err)
	}
	if existed {
		// Another writer won the durable record. Never overwrite its history;
		// accept it only when it describes the same effect and inputs.
		existing, ok, err := LoadEffectRecord(e.EffectRecordPath(exec))
		if err != nil {
			return TaskStatus{}, err
		}
		if !ok || existing.InputsHash != inputsHash {
			return TaskStatus{}, fmt.Errorf("effect %s was published with different inputs", exec.EffectKey())
		}
		record = existing
		executed = false
		appliedAttempt = existing.AppliedAttempt
	}

	// Verify ownership before the terminal journal write. The effect lock already
	// serialises writers; this catches a claim that was replaced by an operator or
	// a buggy caller.
	current, err := LoadJournal(e.JournalPath())
	if err != nil {
		return TaskStatus{}, err
	}
	if task, ok := current.Tasks[exec.EffectKey()]; !ok || task.Owner.Token != token {
		return TaskStatus{}, fmt.Errorf("effect %s claim was replaced before completion", exec.EffectKey())
	}
	completed := running
	completed.Status = string(StatusCompleted)
	completed.CompletedAt = record.CompletedAt
	completed.AppliedAttempt = appliedAttempt
	completed.Result = record.Result
	completed.ExecutionID = record.ExecutionID
	if err := e.saveTask(exec, completed); err != nil {
		return TaskStatus{}, err
	}
	return TaskStatus{
		Status:         StatusCompleted,
		ExecutionID:    record.ExecutionID,
		Attempt:        exec.Attempt,
		AppliedAttempt: appliedAttempt,
		InputsHash:     inputsHash,
		Executed:       executed,
		Result:         record.Result,
	}, nil
}

func (e *Engine) completeFromRecord(exec Execution, record EffectRecord) error {
	journal, err := LoadJournal(e.JournalPath())
	if err != nil {
		return err
	}
	if task, ok := journal.Tasks[exec.EffectKey()]; ok && task.Status == TaskCompleted && task.InputsHash == record.InputsHash {
		return nil
	}
	task := Task{
		OperationID:    string(exec.OperationID),
		Step:           exec.Step,
		Attempt:        record.AppliedAttempt,
		ExecutionID:    record.ExecutionID,
		RequestID:      record.RequestID,
		InputsHash:     record.InputsHash,
		Status:         string(StatusCompleted),
		StartedAt:      record.CompletedAt,
		CompletedAt:    record.CompletedAt,
		AppliedAttempt: record.AppliedAttempt,
		Result:         record.Result,
	}
	return e.saveTask(exec, task)
}

func (e *Engine) fail(exec Execution, task Task, cause error) (TaskStatus, error) {
	message := e.registry().Redact(cause.Error())
	task.Status = TaskFailed
	task.Error = message
	task.CompletedAt = now()
	if err := updateJournal(e.JournalPath(), e.registry(), func(journal *Journal) {
		journal.Node = e.Node
		current := journal.Tasks[exec.EffectKey()]
		// Never overwrite a completion that won a race.
		if current.Status == TaskCompleted {
			return
		}
		current.Status = TaskFailed
		current.Error = message
		current.CompletedAt = task.CompletedAt
		journal.Tasks[exec.EffectKey()] = current
	}); err != nil {
		return TaskStatus{}, errors.Join(cause, err)
	}
	return TaskStatus{Status: StatusFailed, ExecutionID: exec.ExecutionID(), Attempt: exec.Attempt, InputsHash: task.InputsHash, Detail: message}, cause
}

func claimToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func bootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
