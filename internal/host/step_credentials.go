package host

import (
	"context"
	"encoding/json"
	"fmt"
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
		TokenHashes        map[string]string `json:"token_hashes"`
		DatabaseURLHash    string            `json:"database_url_hash"`
		AuthPath           string            `json:"auth_path"`
		EnvPath            string            `json:"env_path"`
		OperatorTokenOut   string            `json:"operator_token_out"`
		AutomationTokenOut string            `json:"automation_token_out"`
		AutomationScopes   []string          `json:"automation_scopes"`
		AutomationSubject  string            `json:"automation_subject"`
		Node               string            `json:"node"`
		NodeStateDir       string            `json:"node_state_dir"`
	}{hashes, engine.HashSecret(s.cfg.DatabaseURL), s.authPath(), s.envPath(), s.cfg.OperatorTokenOut, s.cfg.AutomationTokenOut, s.cfg.AutomationScopes, s.cfg.AutomationSubject, s.cfg.Node, s.cfg.Layout.NodeStateDir})
}

func (s *credentialsStep) env() []byte {
	lines := []string{
		envLine("EREGION_DATABASE_URL", s.cfg.DatabaseURL),
		envLine("EREGION_AUTH_FILE", s.authPath()),
		envLine("EREGION_NODE_STATE_DIR", s.cfg.Layout.NodeStateDir),
		envLine("EREGION_NODE_TRANSPORT", "local"),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func envLine(key, value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", "")
	return key + "=\"" + escaped + "\""
}

func (s *credentialsStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	if s.credentials.Operator == "" {
		return incomplete("the operator credential will be generated during apply; its output file is " + s.cfg.OperatorTokenOut)
	}
	auth, err := s.credentials.AuthFile(s.cfg.Node)
	if err != nil {
		return engine.CheckResult{}, err
	}
	ok, mode, err := sameFileContent(s.authPath(), auth)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok || mode != 0o600 {
		return incomplete("the API auth file is missing, differs, or is not mode 0600")
	}
	ok, mode, err = sameFileContent(s.envPath(), s.env())
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok || mode != 0o600 {
		return incomplete("the control-plane environment file is missing, differs, or is not mode 0600")
	}
	if changed, err := tokenExportState(s.cfg.OperatorTokenOut, s.credentials.Operator); err != nil {
		return engine.CheckResult{}, err
	} else if changed {
		return incomplete("the operator credential export is missing, differs, or is not mode 0600")
	}
	if s.cfg.AutomationTokenOut != "" {
		if changed, err := tokenExportState(s.cfg.AutomationTokenOut, s.credentials.Automation); err != nil {
			return engine.CheckResult{}, err
		} else if changed {
			return incomplete("the automation credential export is missing, differs, or is not mode 0600")
		}
	}
	return complete("API auth, control-plane environment, and requested credential exports are owner-only")
}

func tokenExportState(path, token string) (bool, error) {
	if path == "" || token == "" {
		return true, nil
	}
	ok, mode, err := sameFileContent(path, []byte(token+"\n"))
	if err != nil {
		return true, err
	}
	return !ok || mode != 0o600, nil
}

func (s *credentialsStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	auth, err := s.credentials.AuthFile(s.cfg.Node)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeReplacingSecret(s.authPath(), auth, s.cfg.Layout.User, s.cfg.Layout.Group); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeReplacingSecret(s.envPath(), s.env(), s.cfg.Layout.User, s.cfg.Layout.Group); err != nil {
		return engine.ApplyResult{}, err
	}
	if _, err := exportToken(s.cfg.OperatorTokenOut, s.credentials.Operator, "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	if s.cfg.AutomationTokenOut != "" {
		if _, err := exportToken(s.cfg.AutomationTokenOut, s.credentials.Automation, "root", "root"); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	result := map[string]any{
		"auth_file":            s.authPath(),
		"env_file":             s.envPath(),
		"operator_token_out":   s.cfg.OperatorTokenOut,
		"operator_fingerprint": CredentialFingerprint(s.credentials.Operator),
	}
	if s.cfg.AutomationTokenOut != "" {
		result["automation_token_out"] = s.cfg.AutomationTokenOut
		result["automation_fingerprint"] = CredentialFingerprint(s.credentials.Automation)
	}
	return engine.ApplyResult{Notes: []string{"credentials written with owner-only permissions; tokens are not logged"}, Result: result}, nil
}

func (s *credentialsStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	out := []string{
		"+ write " + s.authPath() + " (0600; hashed principals only)",
		"+ write " + s.envPath() + " (0600; database URL and client paths)",
	}
	if s.cfg.OperatorTokenOut != "" {
		out = append(out, "+ export the operator credential to "+s.cfg.OperatorTokenOut+" (0600)")
	}
	if s.cfg.AutomationTokenOut != "" {
		out = append(out, "+ export a scoped automation credential to "+s.cfg.AutomationTokenOut+" (0600, scopes "+strings.Join(s.cfg.AutomationScopes, ",")+")")
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
