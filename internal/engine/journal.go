package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mbvlabs/eregion/internal/secrets"
)

// JournalSchema is the node journal format version.
const JournalSchema = 1

// Task states recorded in the journal.
const (
	TaskRunning   = "running"
	TaskCompleted = "completed"
	TaskFailed    = "failed"
)

// Journal records node task attempts keyed by effect key (operation + step).
type Journal struct {
	Schema    int             `json:"schema"`
	Node      string          `json:"node"`
	UpdatedAt string          `json:"updated_at"`
	Tasks     map[string]Task `json:"tasks"`
}

// Task is one effect's latest execution record.
type Task struct {
	OperationID    string          `json:"operation_id"`
	Step           string          `json:"step"`
	Attempt        uint32          `json:"attempt"`
	ExecutionID    string          `json:"execution_id"`
	RequestID      string          `json:"request_id,omitempty"`
	InputsHash     string          `json:"inputs_hash"`
	Status         string          `json:"status"`
	Owner          Owner           `json:"owner"`
	StartedAt      string          `json:"started_at,omitempty"`
	CompletedAt    string          `json:"completed_at,omitempty"`
	AppliedAttempt uint32          `json:"applied_attempt,omitempty"`
	Error          string          `json:"error,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
}

// Owner identifies one claim. PID and boot ID are diagnostics; the effect
// flock is the actual liveness fence.
type Owner struct {
	PID    int    `json:"pid"`
	BootID string `json:"boot_id,omitempty"`
	Token  string `json:"token,omitempty"`
}

// EffectRecord is the durable record a completed step writes beside the
// journal. It lets status recover a crash between Apply and the journal update.
type EffectRecord struct {
	OperationID    string          `json:"operation_id"`
	Step           string          `json:"step"`
	InputsHash     string          `json:"inputs_hash"`
	AppliedAttempt uint32          `json:"applied_attempt"`
	ExecutionID    string          `json:"execution_id"`
	RequestID      string          `json:"request_id,omitempty"`
	CompletedAt    string          `json:"completed_at"`
	Result         json.RawMessage `json:"result,omitempty"`
}

// JournalPath is the journal file inside a node state directory.
func JournalPath(stateDir string) string { return filepath.Join(stateDir, "journal.json") }

// NewJournal returns an empty journal for node.
func NewJournal(node string) *Journal {
	return &Journal{Schema: JournalSchema, Node: node, Tasks: map[string]Task{}}
}

// LoadJournal reads path. A missing journal is empty. A journal that does not
// decode returns an explicit precondition error and is never reset silently.
func LoadJournal(path string) (*Journal, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: caller-provided node state path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Journal{Schema: JournalSchema, Tasks: map[string]Task{}}, nil
		}
		return nil, err
	}
	var journal Journal
	if err := json.Unmarshal(raw, &journal); err != nil || journal.Schema != JournalSchema {
		return nil, journalCorrupt(path)
	}
	if journal.Tasks == nil {
		journal.Tasks = map[string]Task{}
	}
	return &journal, nil
}

func journalCorrupt(path string) error {
	return fmt.Errorf("%w: step journal %s is corrupt or has an unsupported schema", ErrJournalCorrupt, path)
}

// ErrJournalCorrupt is a precondition failure, never an empty journal.
var ErrJournalCorrupt = errors.New("node journal corrupt")

// Save writes the journal atomically with mode 0600. Error text is redacted
// before it reaches disk.
func (j *Journal) Save(path string, registry *secrets.Registry) error {
	j.Schema = JournalSchema
	j.UpdatedAt = now()
	if j.Tasks == nil {
		j.Tasks = map[string]Task{}
	}
	for key, task := range j.Tasks {
		task.Error = registry.Redact(task.Error)
		j.Tasks[key] = task
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(raw, '\n'), 0o600)
}

// updateJournal merges fn into the latest journal under the journal lock. It
// preserves concurrent updates to other effects.
func updateJournal(path string, registry *secrets.Registry, fn func(*Journal)) error {
	return WithFileLock(path+".lock", func() error {
		journal, err := LoadJournal(path)
		if err != nil {
			return err
		}
		fn(journal)
		return journal.Save(path, registry)
	})
}

// LoadEffectRecord reads a step's durable effect record. A missing record is
// (EffectRecord{}, false, nil).
func LoadEffectRecord(path string) (EffectRecord, bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: caller-provided node state path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return EffectRecord{}, false, nil
		}
		return EffectRecord{}, false, err
	}
	var record EffectRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return EffectRecord{}, false, fmt.Errorf("effect record %s is corrupt: %w", path, err)
	}
	return record, true, nil
}

// SaveEffectRecord writes a durable effect record atomically. The first writer
// wins: an existing record is never replaced, so a duplicate application cannot
// rewrite history.
func SaveEffectRecord(path string, record EffectRecord) (existed bool, err error) {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return false, err
	}
	return WriteFileOnce(path, append(raw, '\n'), 0o600)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
