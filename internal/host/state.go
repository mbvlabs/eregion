package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

const stateSchema = 1

// State is the bootstrap journal's index. The engine journal holds per-step
// attempts; this file says which operation generation those attempts belong
// to. An interrupted generation stays active until the same intent resumes it.
type State struct {
	Schema      int       `json:"schema"`
	Node        string    `json:"node"`
	Role        Role      `json:"role"`
	ConfigHash  string    `json:"config_hash"`
	Generation  uint32    `json:"generation"`
	OperationID string    `json:"operation_id"`
	Active      bool      `json:"active"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

func statePath(cfg Config) string { return filepath.Join(cfg.Layout.StateDir, "state.json") }

func loadState(cfg Config) (State, bool, error) {
	raw, _, exists, err := fileState(statePath(cfg))
	if err != nil || !exists {
		return State{}, exists, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil || state.Schema != stateSchema {
		return State{}, true, contract.NewFault(contract.PreconditionFailed, "the bootstrap state file is corrupt", "Inspect "+statePath(cfg)+" and the engine journal; never delete them while an apply may be running.")
	}
	if state.OperationID == "" || state.Generation == 0 {
		return State{}, true, contract.NewFault(contract.PreconditionFailed, "the bootstrap state file is incomplete", "Inspect "+statePath(cfg)+" and the engine journal.")
	}
	return state, true, nil
}

func operationID(node, configHash string, generation uint32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("eregion/host/bootstrap/v1\x00%s\x00%s\x00%d", node, configHash, generation)))
	return "op_" + hex.EncodeToString(sum[:16])
}

// projectState computes what apply would do without writing anything. A
// completed intent starts a new generation so drift is repaired; an
// interrupted matching intent resumes the same generation and operation.
func projectState(cfg Config, configHash string, now time.Time) (State, error) {
	state, exists, err := loadState(cfg)
	if err != nil {
		return State{}, err
	}
	if exists && state.Active && state.Node == cfg.Node && state.Role == cfg.Role && state.ConfigHash == configHash {
		return state, nil
	}
	generation := uint32(1)
	if exists {
		generation = state.Generation + 1
	}
	return State{
		Schema:      stateSchema,
		Node:        cfg.Node,
		Role:        cfg.Role,
		ConfigHash:  configHash,
		Generation:  generation,
		OperationID: operationID(cfg.Node, configHash, generation),
		Active:      true,
		StartedAt:   now.UTC(),
	}, nil
}

// beginState locks the state file and either resumes the active matching
// generation or starts the next one.
func beginState(cfg Config, configHash string, now time.Time) (State, error) {
	if err := ensureState(cfg); err != nil {
		return State{}, err
	}
	var selected State
	err := engine.WithFileLock(statePath(cfg)+".lock", func() error {
		projected, err := projectState(cfg, configHash, now)
		if err != nil {
			return err
		}
		selected = projected
		raw, err := json.MarshalIndent(selected, "", "  ")
		if err != nil {
			return err
		}
		return writeReplacingSecret(statePath(cfg), append(raw, '\n'), "", "")
	})
	if err != nil {
		return State{}, err
	}
	return selected, nil
}

func finishState(cfg Config, operationID string, now time.Time) error {
	return engine.WithFileLock(statePath(cfg)+".lock", func() error {
		state, exists, err := loadState(cfg)
		if err != nil {
			return err
		}
		if !exists || state.OperationID != operationID {
			return contract.NewFault(contract.PreconditionFailed, "the bootstrap state changed during apply", "Inspect the bootstrap state and journal before retrying.")
		}
		state.Active = false
		state.CompletedAt = now.UTC()
		raw, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		return writeReplacingSecret(statePath(cfg), append(raw, '\n'), "", "")
	})
}

// ReadState exposes the bootstrap state for status. A missing state is not an
// error: the host simply has not been bootstrapped.
func ReadState(cfg Config) (State, bool, error) { return loadState(cfg) }

// JournalPath is the engine journal used by bootstrap.
func JournalPath(cfg Config) string { return engine.JournalPath(cfg.Layout.StateDir) }
