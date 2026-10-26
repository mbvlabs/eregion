package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

type nodeConfigStep struct {
	cfg Config
}

func (s *nodeConfigStep) ID() string { return "host-node-config-v1" }
func (s *nodeConfigStep) Describe() string {
	return "Install the node configuration and optional forced-command SSH keys"
}
func (s *nodeConfigStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *nodeConfigStep) nodeHome() string {
	return filepath.Join(filepath.Dir(s.cfg.Layout.DataDir), s.cfg.Layout.NodeUser)
}

func (s *nodeConfigStep) configPath() string {
	return filepath.Join(s.cfg.Layout.ConfigDir, "node.json")
}
func (s *nodeConfigStep) sshDir() string   { return filepath.Join(s.nodeHome(), ".ssh") }
func (s *nodeConfigStep) keysPath() string { return filepath.Join(s.sshDir(), "authorized_keys") }

func (s *nodeConfigStep) config() ([]byte, error) {
	// Preserve operator-owned keys in an existing node.json. Bootstrap owns the
	// node name and state directory; it must not erase a privileged
	// wireguard_apply helper, a task_command, or any future node setting merely
	// because this binary does not know it.
	values := map[string]json.RawMessage{}
	raw, _, exists, err := fileState(s.configPath())
	if err != nil {
		return nil, err
	}
	if exists {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("the existing node configuration %s is not valid JSON: %w", s.configPath(), err)
		}
	}
	node, err := json.Marshal(s.cfg.Node)
	if err != nil {
		return nil, err
	}
	stateDir, err := json.Marshal(s.cfg.Layout.NodeStateDir)
	if err != nil {
		return nil, err
	}
	values["node"], values["state_dir"] = node, stateDir
	if s.cfg.Hostname != "" {
		hostname, err := json.Marshal(s.cfg.Hostname)
		if err != nil {
			return nil, err
		}
		values["hostname"] = hostname
	}
	merged, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(merged, '\n'), nil
}

func (s *nodeConfigStep) authorizedKeys() ([]byte, bool, error) {
	if s.cfg.NodeAuthorizedKeysFile == "" {
		return nil, false, nil
	}
	raw, err := os.ReadFile(s.cfg.NodeAuthorizedKeysFile) //nolint:gosec // G304: operator-provided public key file.
	if err != nil {
		return nil, true, err
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !validPublicKey(line) {
			return nil, true, fmt.Errorf("%s contains a line that is not an ssh public key", s.cfg.NodeAuthorizedKeysFile)
		}
		lines = append(lines, `restrict,command="/usr/local/bin/eregion node-agent serve" `+line)
	}
	if len(lines) == 0 {
		return nil, true, fmt.Errorf("%s contains no public keys", s.cfg.NodeAuthorizedKeysFile)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), true, nil
}

func validPublicKey(line string) bool {
	for _, prefix := range []string{"ssh-ed25519 ", "ssh-rsa ", "ecdsa-sha2-", "sk-ssh-ed25519@openssh.com ", "sk-ecdsa-sha2-"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func (s *nodeConfigStep) Inputs() ([]byte, error) {
	config, err := s.config()
	if err != nil {
		return nil, err
	}
	keys, hasKeys, err := s.authorizedKeys()
	if err != nil {
		return nil, err
	}
	return canonicalInput(struct {
		ConfigHash string `json:"config_hash"`
		KeysHash   string `json:"keys_hash"`
		HasKeys    bool   `json:"has_keys"`
		ConfigPath string `json:"config_path"`
		KeysPath   string `json:"keys_path"`
		Home       string `json:"home"`
		Hostname   string `json:"hostname,omitempty"`
	}{contentSHA256(config), contentSHA256(keys), hasKeys, s.configPath(), s.keysPath(), s.nodeHome(), s.cfg.Hostname})
}

func (s *nodeConfigStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	config, err := s.config()
	if err != nil {
		return engine.CheckResult{}, err
	}
	ok, mode, err := sameFileContent(s.configPath(), config)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok || mode != 0o600 {
		return incomplete("node configuration is missing, differs, or is not mode 0600")
	}
	info, err := os.Lstat(s.configPath())
	if err != nil {
		return engine.CheckResult{}, err
	}
	owned, err := ownershipMatches(info, s.cfg.Layout.NodeUser, s.cfg.Layout.NodeGroup)
	if err != nil || !owned {
		return incomplete("node configuration has incorrect ownership")
	}
	keys, hasKeys, err := s.authorizedKeys()
	if err != nil {
		return engine.CheckResult{}, err
	}
	if hasKeys {
		ok, mode, err = sameFileContent(s.keysPath(), keys)
		if err != nil {
			return engine.CheckResult{}, err
		}
		if !ok || mode != 0o600 {
			return incomplete("node authorized_keys are missing, differ, or are not mode 0600")
		}
	}
	return complete("node configuration is installed" + map[bool]string{true: " with forced-command SSH keys", false: ""}[hasKeys])
}

func (s *nodeConfigStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	config, err := s.config()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeReplacingSecret(s.configPath(), config, s.cfg.Layout.NodeUser, s.cfg.Layout.NodeGroup); err != nil {
		return engine.ApplyResult{}, err
	}
	keys, hasKeys, err := s.authorizedKeys()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if hasKeys {
		if err := ensureDir(s.sshDir(), 0o700); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := chownIfRoot(s.sshDir(), s.cfg.Layout.NodeUser, s.cfg.Layout.NodeGroup); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := writeReplacingSecret(s.keysPath(), keys, s.cfg.Layout.NodeUser, s.cfg.Layout.NodeGroup); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := chownIfRoot(s.nodeHome(), s.cfg.Layout.NodeUser, s.cfg.Layout.NodeGroup); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	return engine.ApplyResult{Notes: []string{"node configuration installed"}, Result: map[string]any{"config": s.configPath(), "authorized_keys": hasKeys}}, nil
}

func (s *nodeConfigStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	config, err := s.config()
	if err != nil {
		return nil, err
	}
	out := simpleDiff(s.configPath(), nil, config)
	if s.cfg.NodeAuthorizedKeysFile != "" {
		out = append(out, "+ install forced-command keys from "+s.cfg.NodeAuthorizedKeysFile+" at "+s.keysPath())
	}
	return out, nil
}

func (s *nodeConfigStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
