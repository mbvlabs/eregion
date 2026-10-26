package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"golang.org/x/crypto/ssh"
)

// NodeConfig is the non-secret part of /etc/eregion/node.json. Bootstrap owns
// the node name, state directory and main hostname; other keys are preserved.
type NodeConfig struct {
	Node     string `json:"node"`
	StateDir string `json:"state_dir"`
	Hostname string `json:"hostname,omitempty"`
}

// DefaultNodeConfigPath is the production node configuration file.
func DefaultNodeConfigPath() string {
	return filepath.Join(DefaultLayout().ConfigDir, "node.json")
}

// LoadNodeFile reads the node configuration. A missing file is an error: a
// node RPC must not invent the node identity or a public hostname.
func LoadNodeFile(path string) (NodeConfig, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the fixed node configuration path.
	if err != nil {
		return NodeConfig{}, contract.NewFault(contract.PreconditionFailed, "the node configuration is not readable", err.Error())
	}
	var config NodeConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return NodeConfig{}, contract.NewFault(contract.PreconditionFailed, "the node configuration does not parse", "Run eregion apply to converge it.")
	}
	return config, nil
}

// HostKeySummary is the public SSH host key type and fingerprint, as the
// handoff shows it. It reads only the public key file.
func HostKeySummary() (string, error) {
	matches, _ := filepath.Glob("/etc/ssh/ssh_host_*_key.pub")
	sort.Strings(matches)
	if len(matches) == 0 {
		return "", errors.New("no SSH host public key is installed")
	}
	raw, err := os.ReadFile(matches[0]) //nolint:gosec // G304: fixed /etc/ssh public key files.
	if err != nil {
		return "", err
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", matches[0], err)
	}
	return key.Type() + " " + ssh.FingerprintSHA256(key), nil
}

// PlatformSummary is the node's OS release name and architecture.
func PlatformSummary() string {
	name := runtime.GOOS
	if raw, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_NAME="); ok {
				if unquoted := strings.Trim(value, `"'`); unquoted != "" {
					name = unquoted
				}
				break
			}
		}
	}
	return name + ", " + runtime.GOARCH
}
