package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/contract"
)

const intentSchema = 1

type hostIntent struct {
	Schema int    `json:"schema"`
	Config Config `json:"config"`
}

// ErrNoIntent reports a node whose host intent has not been recorded yet.
var ErrNoIntent = errors.New("the node has no recorded host intent")

func intentPath(layout Layout) string {
	return filepath.Join(layout.StateDir, "intent.json")
}

func intentInputsDir(layout Layout) string {
	return filepath.Join(layout.StateDir, "inputs")
}

// saveIntent records the non-secret host intent after a successful apply. The
// operator's original --database-url-file, --binary and uploaded TLS/key paths
// may be temporary, so the intent points at owner-only snapshots instead. Every
// later plan/apply reconstructs the same desired state from this file.
func saveIntent(cfg Config) error {
	stored := cfg
	stored.DatabaseURL = ""
	if installed := filepath.Join(cfg.Layout.BinDir, "eregion"); fileExists(installed) {
		stored.BinaryPath = installed
	}
	var err error
	if stored.APITLSCert, err = snapshotIntentInput(cfg.Layout, cfg.APITLSCert, "api-tls-cert"); err != nil {
		return err
	}
	if stored.APITLSKey, err = snapshotIntentInput(cfg.Layout, cfg.APITLSKey, "api-tls-key"); err != nil {
		return err
	}
	if stored.NodeAuthorizedKeysFile, err = snapshotIntentInput(cfg.Layout, cfg.NodeAuthorizedKeysFile, "node-authorized-keys"); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(hostIntent{Schema: intentSchema, Config: stored}, "", "  ")
	if err != nil {
		return err
	}
	return writeReplacingSecret(intentPath(cfg.Layout), append(raw, '\n'), "", "")
}

func snapshotIntentInput(layout Layout, source, name string) (string, error) {
	if source == "" {
		return "", nil
	}
	dest := filepath.Join(intentInputsDir(layout), name)
	if filepath.Clean(source) == filepath.Clean(dest) {
		return dest, nil
	}
	raw, _, exists, err := fileState(source)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("intent input %s is missing", source)
	}
	if err := ensureDir(intentInputsDir(layout), 0o700); err != nil {
		return "", err
	}
	if err := writeFile(dest, raw, 0o600, "root", "root"); err != nil {
		return "", err
	}
	return dest, nil
}

// LoadIntent reads the desired host state recorded by an earlier apply. The
// database URL is deliberately absent; callers supply it from the node's
// service environment.
func LoadIntent(layout Layout) (Config, error) {
	raw, _, exists, err := fileState(intentPath(layout))
	if err != nil {
		return Config{}, err
	}
	if !exists {
		return Config{}, ErrNoIntent
	}
	var intent hostIntent
	if err := json.Unmarshal(raw, &intent); err != nil || intent.Schema != intentSchema {
		return Config{}, contract.NewFault(contract.PreconditionFailed, "the recorded host intent is corrupt", "Inspect "+intentPath(layout)+"; run server init again to record the node's desired state.")
	}
	return intent.Config, nil
}

// LoadNodeConfig is the node-side entry point for plan, apply, doctor and
// status. It combines the recorded intent with the node's own service
// environment, so a secret such as the control database URL never travels in a
// command or an operation input.
func LoadNodeConfig(layout Layout, databaseURL string) (Config, error) {
	cfg, err := LoadIntent(layout)
	if errors.Is(err, ErrNoIntent) {
		return Config{}, contract.NewFault(contract.PreconditionFailed, "the node has no recorded host intent", "Run server init (or bootstrap apply) on this node once to record its desired host state.")
	}
	if err != nil {
		return Config{}, err
	}
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = databaseURL
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
