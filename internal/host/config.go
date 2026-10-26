package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// Layout holds every host path bootstrap may write. Tests replace it with
// temporary directories; production uses DefaultLayout. Ownership names are
// resolved only when bootstrap runs as root.
type Layout struct {
	StateDir     string `json:"state_dir"`
	NodeStateDir string `json:"node_state_dir"`
	ConfigDir    string `json:"config_dir"`
	DataDir      string `json:"data_dir"`
	BinDir       string `json:"bin_dir"`
	SystemdDir   string `json:"systemd_dir"`
	RegistryDir  string `json:"registry_dir"`
	User         string `json:"user"`
	Group        string `json:"group"`
	NodeUser     string `json:"node_user"`
	NodeGroup    string `json:"node_group"`
}

// DefaultLayout is the production filesystem layout. Bootstrap state is kept
// apart from the node execution state so pre-database progress never depends
// on the database or on a running worker.
func DefaultLayout() Layout {
	return Layout{
		StateDir:     "/var/lib/eregion/bootstrap",
		NodeStateDir: "/var/lib/eregion/node",
		ConfigDir:    "/etc/eregion",
		DataDir:      "/var/lib/eregion",
		BinDir:       "/usr/local/bin",
		SystemdDir:   "/etc/systemd/system",
		RegistryDir:  "/var/lib/eregion/registry",
		User:         "eregion",
		Group:        "eregion",
		NodeUser:     "eregion-node",
		NodeGroup:    "eregion-node",
	}
}

// Validate rejects relative or dangerous paths before any host mutation.
func (l Layout) Validate() error {
	paths := map[string]string{
		"state directory":         l.StateDir,
		"node state directory":    l.NodeStateDir,
		"configuration directory": l.ConfigDir,
		"data directory":          l.DataDir,
		"binary directory":        l.BinDir,
		"systemd directory":       l.SystemdDir,
		"registry directory":      l.RegistryDir,
	}
	for name, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("the %s must be an absolute path", name)
		}
		clean := filepath.Clean(path)
		if clean == "/" || clean == "." {
			return fmt.Errorf("the %s must not be the filesystem root", name)
		}
		if strings.ContainsAny(clean, "\n\r\x00") {
			return fmt.Errorf("the %s contains control characters", name)
		}
	}
	for name, value := range map[string]string{"user": l.User, "group": l.Group, "node user": l.NodeUser, "node group": l.NodeGroup} {
		if value != "" && !plainAccount(value) {
			return fmt.Errorf("the %s %q is not a plain account name", name, value)
		}
	}
	return nil
}

// Config is one role-aware bootstrap intent. Secret values are never part of
// its canonical inputs; they are hashed when they reach a step.
type Config struct {
	Node string `json:"node"`
	Role Role   `json:"role"`

	Layout Layout `json:"layout"`

	// PlatformPath is /etc/os-release in production and a fixture in tests.
	PlatformPath string `json:"platform_path"`
	Arch         string `json:"arch"`

	// DatabaseURL is the external PostgreSQL DSN for the main role. It is
	// loaded from a private file or EREGION_DATABASE_URL, never a flag.
	DatabaseURL string `json:"-"`

	OperatorTokenFile string `json:"-"`
	OperatorTokenOut  string `json:"operator_token_out,omitempty"`

	AutomationSubject  string   `json:"automation_subject,omitempty"`
	AutomationScopes   []string `json:"automation_scopes,omitempty"`
	AutomationTokenOut string   `json:"automation_token_out,omitempty"`

	APIListen         string `json:"api_listen"`
	APITLSCert        string `json:"api_tls_cert,omitempty"`
	APITLSKey         string `json:"api_tls_key,omitempty"`
	WorkerConcurrency int    `json:"worker_concurrency"`
	// Hostname is the main node's public name, <node>.<domain>. Caddy serves
	// it with an ACME certificate and exposes only the CI endpoint.
	Hostname       string `json:"hostname,omitempty"`
	RegistryListen string `json:"registry_listen"`
	// CommitConfirmSeconds bounds how long an access-changing apply may leave
	// the new access state live before a detached watchdog reverts it.
	CommitConfirmSeconds int `json:"commit_confirm_seconds,omitempty"`

	// BinaryPath is the eregion binary installed on every role. It defaults to
	// the running executable.
	BinaryPath string `json:"binary_path,omitempty"`

	// NodeAuthorizedKeysFile optionally installs forced-command SSH keys for
	// the node account. Key distribution and enrollment identity remain the
	// caller's responsibility.
	NodeAuthorizedKeysFile string `json:"node_authorized_keys_file,omitempty"`
}

// DefaultConfig starts from safe defaults. The caller must still set Node and
// the role-specific inputs.
func DefaultConfig(role Role, node string) Config {
	if node == "" {
		node = "local"
	}
	return Config{
		Node:                 node,
		Role:                 role,
		Layout:               DefaultLayout(),
		PlatformPath:         "/etc/os-release",
		APIListen:            "127.0.0.1:8080",
		WorkerConcurrency:    2,
		RegistryListen:       "127.0.0.1:5000",
		CommitConfirmSeconds: int(DefaultCommitConfirmWindow / time.Second),
	}
}

func plainAccount(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			if i == 0 && r >= '0' && r <= '9' {
				return false
			}
			continue
		}
		return false
	}
	return true
}

// Validate checks the complete intent before it can reach a step. It does not
// require the host filesystem to be writable; Plan uses the same validation so
// an unsupported mode is visible before apply.
func (c Config) Validate() error {
	if err := engine.ValidateNodeName(c.Node); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid node name", "Use 1–64 lowercase letters, digits, hyphens, or underscores.")
	}
	role, err := ConfigForRole(c.Role)
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, err.Error(), "Use --role main, application, builder, or telemetry.")
	}
	if err := c.Layout.Validate(); err != nil {
		return contract.NewFault(contract.ValidationFailed, err.Error(), "Use absolute, non-root host paths.")
	}
	if role.ControlPlane {
		if strings.TrimSpace(c.DatabaseURL) == "" {
			return contract.NewFault(contract.InputRequired, "the external database URL is required for the main role", "Set EREGION_DATABASE_URL or --database-url-file to a private file; the URL is never accepted as a command argument.")
		}
		if strings.TrimSpace(c.OperatorTokenOut) == "" {
			return contract.NewFault(contract.InputRequired, "--operator-token-out is required for the main role", "Choose a protected file so the first operator credential is not lost when bootstrap completes.")
		}
		if !filepath.IsAbs(c.OperatorTokenOut) {
			return contract.NewFault(contract.ValidationFailed, "--operator-token-out must be an absolute path", "Use a path under a private directory, for example /root/eregion-operator.token.")
		}
		if err := validateListen(c.APIListen, "API"); err != nil {
			return err
		}
		host, _, _ := net.SplitHostPort(c.APIListen)
		ip := net.ParseIP(host)
		loopback := ip != nil && ip.IsLoopback()
		if !loopback && (c.APITLSCert == "" || c.APITLSKey == "") {
			return contract.NewFault(contract.ValidationFailed, "a non-loopback API listener requires TLS", "Supply --api-tls-cert and --api-tls-key, or keep the API on a loopback address behind a reverse proxy.")
		}
		if (c.APITLSCert == "") != (c.APITLSKey == "") {
			return contract.NewFault(contract.ValidationFailed, "API TLS requires both a certificate and a key", "Supply --api-tls-cert and --api-tls-key together.")
		}
		if c.Hostname != "" {
			if err := ValidateHostname(c.Hostname); err != nil {
				return contract.NewFault(contract.ValidationFailed, "invalid hostname "+c.Hostname, err.Error())
			}
		}
		if c.WorkerConcurrency < 1 || c.WorkerConcurrency > 16 {
			return contract.NewFault(contract.ValidationFailed, "worker concurrency out of range", "Use --worker-concurrency 1–16.")
		}
		if c.CommitConfirmSeconds != 0 && (c.CommitConfirmSeconds < 30 || c.CommitConfirmSeconds > 900) {
			return contract.NewFault(contract.ValidationFailed, "commit-confirm window out of range", "Use --commit-confirm-seconds 30–900.")
		}
		if c.AutomationTokenOut != "" && len(c.AutomationScopes) == 0 {
			return contract.NewFault(contract.ValidationFailed, "an automation token needs at least one scope", "Use --automation-scope operations:submit and other explicit scopes.")
		}
		if len(c.AutomationScopes) > 0 && c.AutomationTokenOut == "" {
			return contract.NewFault(contract.InputRequired, "an automation token needs an output file", "Supply --automation-token-out so the generated credential can be handed off securely.")
		}
		if len(c.AutomationScopes) > 0 {
			if strings.TrimSpace(c.AutomationSubject) == "" {
				return contract.NewFault(contract.ValidationFailed, "an automation credential needs a subject", "Use --automation-subject with the CI identity.")
			}
			for _, scope := range c.AutomationScopes {
				if !knownScope(scope) {
					return contract.NewFault(contract.ValidationFailed, "unknown automation scope", "Use operations:submit, operations:read, operations:cancel, or system:read.")
				}
			}
			if !filepath.IsAbs(c.AutomationTokenOut) {
				return contract.NewFault(contract.ValidationFailed, "--automation-token-out must be an absolute path", "Use a path under a private directory.")
			}
		}
	}
	if role.Registry {
		if err := validateListen(c.RegistryListen, "registry"); err != nil {
			return err
		}
		host, _, _ := net.SplitHostPort(c.RegistryListen)
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return contract.NewFault(contract.ValidationFailed, "the registry must listen on a loopback address", "This increment installs a local registry without TLS or registry authentication; expose it through an authenticated proxy before changing the bind address.")
		}
	}
	if role.NodeAgent {
		if c.Layout.NodeUser == "" || c.Layout.NodeGroup == "" {
			return contract.NewFault(contract.ValidationFailed, "node roles require a node account and group", "Use the default eregion-node execution identity.")
		}
	}
	if role.ControlPlane {
		if _, err := c.EffectiveBinaryPath(); err != nil {
			return contract.NewFault(contract.ValidationFailed, "cannot locate the eregion binary", "Supply --binary explicitly.")
		}
	}
	for name, path := range map[string]string{"operator token output": c.OperatorTokenOut, "automation token output": c.AutomationTokenOut} {
		if path != "" && !filepath.IsAbs(path) {
			return contract.NewFault(contract.ValidationFailed, "the "+name+" must be absolute", "Use an absolute path under a private directory.")
		}
	}
	if c.NodeAuthorizedKeysFile != "" && !filepath.IsAbs(c.NodeAuthorizedKeysFile) {
		return contract.NewFault(contract.ValidationFailed, "the node authorized keys file must be absolute", "Use an absolute path to a public key file.")
	}
	return nil
}

func apiExternallyReachable(cfg Config) bool {
	host, _, err := net.SplitHostPort(cfg.APIListen)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

func validateListen(address, name string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return contract.NewFault(contract.ValidationFailed, "invalid "+name+" listen address", "Use a literal host:port such as 127.0.0.1:8080.")
	}
	if net.ParseIP(host) == nil {
		return contract.NewFault(contract.ValidationFailed, "the "+name+" listen address must use a literal IP", "Use 127.0.0.1, ::1, or a specific host address; hostname resolution is not a listening policy.")
	}
	return nil
}

// RoleConfig returns the validated role description.
func (c Config) RoleConfig() (RoleConfig, error) { return ConfigForRole(c.Role) }

// EffectiveBinaryPath is the explicitly supplied CLI/API binary or the
// currently running executable.
func (c Config) EffectiveBinaryPath() (string, error) {
	if c.BinaryPath != "" {
		return c.BinaryPath, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return executable, nil
}

// AccessRisks lists changes in this intent that could change operator access.
// They require an additional explicit acknowledgement at apply time.
func (c Config) AccessRisks() []string {
	var risks []string
	host, _, _ := net.SplitHostPort(c.APIListen)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		risks = append(risks, "the API will listen on a non-loopback address; TLS is required and the operator must verify the path to it")
	}
	if c.NodeAuthorizedKeysFile != "" {
		risks = append(risks, "the node account SSH authorized_keys may be replaced; existing access must be reviewed")
	}
	return risks
}

// CanonicalInputs is the non-secret intent used for plan bindings and the
// bootstrap state generation. Secrets contribute only through intentional
// hashes in individual steps.
func (c Config) CanonicalInputs() ([]byte, error) {
	type canonical struct {
		Node               string   `json:"node"`
		Role               Role     `json:"role"`
		Layout             Layout   `json:"layout"`
		PlatformPath       string   `json:"platform_path"`
		Arch               string   `json:"arch"`
		OperatorTokenOut   string   `json:"operator_token_out"`
		AutomationSubject  string   `json:"automation_subject"`
		AutomationScopes   []string `json:"automation_scopes"`
		AutomationTokenOut string   `json:"automation_token_out"`
		APIListen          string   `json:"api_listen"`
		APITLSCert         string   `json:"api_tls_cert"`
		APITLSKey          string   `json:"api_tls_key"`
		WorkerConcurrency  int      `json:"worker_concurrency"`
		RegistryListen     string   `json:"registry_listen"`
		CommitConfirm      int      `json:"commit_confirm_seconds"`
		BinaryPath         string   `json:"binary_path"`
		NodeAuthorizedKeys string   `json:"node_authorized_keys"`
	}
	scopes := append([]string(nil), c.AutomationScopes...)
	sort.Strings(scopes)
	value := canonical{
		Node: c.Node, Role: c.Role, Layout: c.Layout, PlatformPath: c.PlatformPath, Arch: c.Arch,
		OperatorTokenOut: c.OperatorTokenOut, AutomationSubject: c.AutomationSubject,
		AutomationScopes: scopes, AutomationTokenOut: c.AutomationTokenOut,
		APIListen: c.APIListen, APITLSCert: c.APITLSCert, APITLSKey: c.APITLSKey,
		WorkerConcurrency: c.WorkerConcurrency, RegistryListen: c.RegistryListen,
		CommitConfirm:      c.CommitConfirmSeconds,
		BinaryPath:         c.BinaryPath,
		NodeAuthorizedKeys: c.NodeAuthorizedKeysFile,
	}
	return json.Marshal(value)
}

// ConfigHash identifies the intent. It includes the binary digests because a
// new binary is a new convergence generation, not a resumed old one.
func (c Config) ConfigHash() (string, error) {
	raw, err := c.CanonicalInputs()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte("eregion/host/config/v1\x00"))
	hash.Write(raw)
	binaryPath := c.BinaryPath
	if binaryPath == "" {
		if executable, err := c.EffectiveBinaryPath(); err == nil {
			binaryPath = executable
		}
	}
	for _, path := range []string{binaryPath, c.APITLSCert, c.APITLSKey, c.NodeAuthorizedKeysFile, c.OperatorTokenFile} {
		if path == "" {
			continue
		}
		digest, err := fileSHA256(path)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", path, err)
		}
		hash.Write([]byte{0})
		hash.Write([]byte(digest))
	}
	if c.DatabaseURL != "" {
		hash.Write([]byte{0})
		hash.Write([]byte(engine.HashSecret(c.DatabaseURL)))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // G304: operator-provided binary or configuration path.
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
