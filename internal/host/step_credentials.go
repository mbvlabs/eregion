package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

type credentialsStep struct {
	cfg         Config
	credentials Credentials
}

func (s *credentialsStep) ID() string { return "host-credentials-v1" }
func (s *credentialsStep) Describe() string {
	return "Write owner-only API credentials and the control-plane environment"
}
func (s *credentialsStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *credentialsStep) authPath() string {
	return filepath.Join(s.cfg.Layout.ConfigDir, "auth.json")
}
func (s *credentialsStep) envPath() string {
	return filepath.Join(s.cfg.Layout.ConfigDir, "control-plane.env")
}

func (s *credentialsStep) Inputs() ([]byte, error) {
	hashes := s.credentials.TokenHashes()
	return canonicalInput(struct {
		TokenHashes     map[string]string `json:"token_hashes"`
		DatabaseURLHash string            `json:"database_url_hash"`
		AuthPath        string            `json:"auth_path"`
		EnvPath         string            `json:"env_path"`
		Node            string            `json:"node"`
		NodeStateDir    string            `json:"node_state_dir"`
	}{hashes, engine.HashSecret(s.cfg.DatabaseURL), s.authPath(), s.envPath(), s.cfg.Node, s.cfg.Layout.NodeStateDir})
}

func (s *credentialsStep) managedEnv() []string {
	return []string{
		envLine("EREGION_DATABASE_URL", s.cfg.DatabaseURL),
		envLine("EREGION_AUTH_FILE", s.authPath()),
		envLine("EREGION_NODE_STATE_DIR", s.cfg.Layout.NodeStateDir),
		envLine("EREGION_NODE_TRANSPORT", "local"),
		envLine("EREGION_NODE_NAME", s.cfg.Node),
		// The operator installs this identity and pins node host keys here.
		// Bootstrap records the paths so an enrolled fleet needs no ad-hoc
		// service edits; it never writes or invents a private key.
		envLine("EREGION_NODE_SSH_IDENTITY", filepath.Join(s.cfg.Layout.ConfigDir, "control-plane-key")),
		envLine("EREGION_NODE_SSH_KNOWN_HOSTS", filepath.Join(s.cfg.Layout.ConfigDir, "known_hosts")),
		envLine("EREGION_NODE_WG_KEY_DIR", filepath.Join(s.cfg.Layout.DataDir, "control-plane", "wireguard")),
		envLine("EREGION_NODE_WG_ADDRESS", "10.77.0.1"),
		envLine("EREGION_NODE_WG_SUBNET", "10.77.0.0/24"),
		// server init generates this keyring before the control plane starts;
		// the secrets service refuses rather than silently running without it.
		envLine("EREGION_VAULT_KEY_FILE", VaultKeyringPath(s.cfg)),
	}
}

// mergeEnv keeps operator-added lines and replaces only the keys bootstrap
// manages. It returns the keys it preserved so plan and check output can report
// the merge instead of presenting it as an exact rewrite.
func mergeEnv(existing []byte, managed []string) ([]byte, []string) {
	managedSet := map[string]bool{}
	for _, line := range managed {
		key, _, _ := strings.Cut(line, "=")
		managedSet[strings.TrimSpace(key)] = true
	}
	var kept []string
	preservedSet := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(existing), "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			kept = append(kept, line)
			continue
		}
		key = strings.TrimSpace(key)
		if managedSet[key] {
			continue
		}
		kept = append(kept, line)
		preservedSet[key] = true
	}
	lines := append(kept, managed...)
	preserved := make([]string, 0, len(preservedSet))
	for key := range preservedSet {
		preserved = append(preserved, key)
	}
	sort.Strings(preserved)
	return []byte(strings.Join(lines, "\n") + "\n"), preserved
}

func (s *credentialsStep) env() []byte {
	existing, _, _, _ := fileState(s.envPath())
	merged, _ := mergeEnv(existing, s.managedEnv())
	return merged
}

func (s *credentialsStep) envPreserved() []string {
	existing, _, _, err := fileState(s.envPath())
	if err != nil {
		return nil
	}
	_, preserved := mergeEnv(existing, s.managedEnv())
	return preserved
}

func envLine(key, value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", "")
	return key + "=\"" + escaped + "\""
}

func (s *credentialsStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	path := s.authPath()
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return incomplete("the API auth file is missing at " + path)
		}
		return engine.CheckResult{}, err
	}
	if !info.Mode().IsRegular() {
		return engine.CheckResult{}, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return incomplete("the API auth file " + path + " is not mode 0600")
	}
	owned, err := ownershipMatches(info, s.cfg.Layout.User, s.cfg.Layout.Group)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !owned {
		return incomplete("the API auth file " + path + " is not owned by the control-plane account")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed host configuration path.
	if err != nil {
		return engine.CheckResult{}, err
	}
	if _, err := decodeAuthFile(raw); err != nil {
		// A malformed store is not drift: rewriting it would destroy tokens.
		return engine.CheckResult{}, err
	}
	ok, mode, err := sameFileContent(s.envPath(), s.env())
	if err != nil {
		return engine.CheckResult{}, err
	}
	preserved := s.envPreserved()
	if !ok || mode != 0o600 {
		detail := "the control-plane environment file is missing, differs, or is not mode 0600"
		if len(preserved) > 0 {
			detail += "; operator keys preserved: " + strings.Join(preserved, ", ")
		}
		return incomplete(detail)
	}
	if len(preserved) > 0 {
		return complete("API token store and control-plane environment are owner-only; preserved operator keys: " + strings.Join(preserved, ", "))
	}
	return complete("API token store and control-plane environment are owner-only")
}

func (s *credentialsStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	path := s.authPath()
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	authNote := "existing API token store preserved"
	if exists {
		// Validate before touching the file: a damaged store must fail closed,
		// never be replaced by bootstrap's empty list.
		if _, err := decodeAuthFile(raw); err != nil {
			return engine.ApplyResult{}, err
		}
		if mode != 0o600 {
			if err := os.Chmod(path, 0o600); err != nil {
				return engine.ApplyResult{}, err
			}
		}
		if err := chownIfRoot(path, s.cfg.Layout.User, s.cfg.Layout.Group); err != nil {
			return engine.ApplyResult{}, err
		}
	} else {
		auth, err := s.credentials.AuthFile(s.cfg.Node)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if err := writeReplacingSecret(path, auth, s.cfg.Layout.User, s.cfg.Layout.Group); err != nil {
			return engine.ApplyResult{}, err
		}
		authNote = "empty API token store created; setup mints no token"
	}
	if err := writeReplacingSecret(s.envPath(), s.env(), s.cfg.Layout.User, s.cfg.Layout.Group); err != nil {
		return engine.ApplyResult{}, err
	}
	result := map[string]any{
		"auth_file": path,
		"env_file":  s.envPath(),
	}
	return engine.ApplyResult{Notes: []string{authNote + "; credentials are owner-only and tokens are not logged"}, Result: result}, nil
}

func (s *credentialsStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	authLine := "+ create " + s.authPath() + " (0600; empty token store)"
	if _, _, exists, err := fileState(s.authPath()); err != nil {
		return nil, err
	} else if exists {
		authLine = "= verify the existing " + s.authPath() + " token store (0600; tokens preserved)"
	}
	out := []string{
		authLine,
		"+ merge " + s.envPath() + " (0600; managed keys replaced, operator lines preserved)",
	}
	for _, key := range s.envPreserved() {
		out = append(out, "= preserve operator environment key "+key)
	}
	return out, nil
}

func (s *credentialsStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}

// EnvKeys returns the keys (never values) of a control-plane environment file.
func EnvKeys(raw []byte) []string {
	var keys []string
	for _, line := range strings.Split(string(raw), "\n") {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// AuthFileSummary is a non-secret description for status output.
func AuthFileSummary(raw []byte) (string, error) {
	var principals []struct {
		Actor struct {
			Kind    string `json:"kind"`
			Subject string `json:"subject"`
		} `json:"actor"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &principals); err != nil {
		return "", fmt.Errorf("parse the auth file: %w", err)
	}
	var parts []string
	for _, principal := range principals {
		parts = append(parts, principal.Actor.Kind+":"+principal.Actor.Subject+"["+strings.Join(principal.Scopes, ",")+"]")
	}
	return strings.Join(parts, " "), nil
}
