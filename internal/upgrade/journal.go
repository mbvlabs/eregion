package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// JournalSchema is the upgrade journal format version.
const JournalSchema = 1

// Journal statuses. A nonterminal status means a replacement was interrupted
// and Recover must reconcile the binary before another upgrade starts.
const (
	StatusInstalling  = "installing"
	StatusInstalled   = "installed"
	StatusRollingBack = "rolling_back"
	StatusRolledBack  = "rolled_back"
	StatusFailed      = "failed"
)

// Journal is the durable record of one binary replacement. It is written
// before the atomic rename and updated after, so a crash leaves enough
// information to finish or roll back deterministically.
type Journal struct {
	Schema       int    `json:"schema"`
	Target       Target `json:"target"`
	Status       string `json:"status"`
	OperationID  string `json:"operation_id"`
	BinaryPath   string `json:"binary_path"`
	FromVersion  string `json:"from_version"`
	FromSHA256   string `json:"from_sha256"`
	ToVersion    string `json:"to_version"`
	ToSHA256     string `json:"to_sha256"`
	BackupPath   string `json:"backup_path,omitempty"`
	BackupSHA256 string `json:"backup_sha256,omitempty"`
	SourcePath   string `json:"source_path,omitempty"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
	StagedPath   string `json:"staged_path,omitempty"`
	UpdatedAt    string `json:"updated_at"`
	Error        string `json:"error,omitempty"`
}

func journalPath(stateDir string, target Target) string {
	return filepath.Join(stateDir, string(target), "journal.json")
}

func loadJournal(path string) (*Journal, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: state-directory path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var journal Journal
	if err := json.Unmarshal(raw, &journal); err != nil || journal.Schema != JournalSchema {
		return nil, contractJournalCorrupt(path)
	}
	return &journal, nil
}

func contractJournalCorrupt(path string) error {
	return fmt.Errorf("upgrade journal %s is corrupt or has an unsupported schema", path)
}

func saveJournal(path string, journal Journal) error {
	journal.Schema = JournalSchema
	journal.UpdatedAt = nowRFC3339()
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'), 0o600)
}

func terminalStatus(status string) bool {
	return status == StatusInstalled || status == StatusRolledBack || status == StatusFailed
}
