package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
	"golang.org/x/crypto/curve25519"
)

// Host-level node steps. They are the enrollment and reconciliation effects:
// an identity attestation, a capability probe, role-specific host
// configuration, and WireGuard key and peer management. Each is a versioned
// step the control plane builds through the same registry, so both sides agree
// on its canonical inputs.
var (
	identityStepPattern  = regexp.MustCompile(`^node-identity-[0-9]{2}-v1$`)
	probeStepPattern     = regexp.MustCompile(`^node-probe-[0-9]{2}-v1$`)
	hostcfgStepPattern   = regexp.MustCompile(`^node-hostcfg-[0-9]{2}-v1$`)
	wireguardStepPattern = regexp.MustCompile(`^node-wireguard-[0-9]{2}-v1$`)
	userNamePattern      = regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`)
	wgKeyPattern         = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
	hostPortPattern      = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]{1,255}$`)
	cidrPattern          = regexp.MustCompile(`^[0-9.]{7,15}/[0-9]{1,2}$`)
)

// HostSettings are the node operator's own host-level settings. They come from
// the node configuration file and are never derived from a request.
type HostSettings struct {
	// WireGuardApply is an operator-installed privileged helper that loads the
	// rendered WireGuard configuration (for example a sudo rule on one fixed
	// script). The node account is unprivileged, so without it a peer
	// configuration is rendered but not applied, and the step says so.
	WireGuardApply []string
}

// NewStepRegistryWithHost returns the default registry plus the host steps.
func NewStepRegistryWithHost(settings HostSettings) *StepRegistry {
	registry := newBaseRegistry()
	registry.Register(identityStepPattern.MatchString, buildIdentityStep)
	registry.Register(probeStepPattern.MatchString, buildProbeStep)
	registry.Register(hostcfgStepPattern.MatchString, buildHostcfgStep)
	registry.Register(wireguardStepPattern.MatchString, func(exec engine.Execution, payload json.RawMessage) (engine.Step, error) {
		return buildWireGuardStep(exec, payload, settings)
	})
	return registry
}

func decodeStrict(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("step payload is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid step payload: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ identity

// IdentityPayload asks the node to sign a challenge with its identity key.
type IdentityPayload struct {
	Step  string `json:"step"`
	Node  string `json:"node"`
	Nonce string `json:"nonce"`
}

type identityStep struct{ payload IdentityPayload }

func buildIdentityStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload IdentityPayload
	if err := decodeStrict(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if err := platform.ValidateName(payload.Node); err != nil {
		return nil, errors.New("invalid node name in the identity challenge")
	}
	if len(payload.Nonce) < 16 || len(payload.Nonce) > 128 {
		return nil, errors.New("the identity challenge nonce has an invalid length")
	}
	return &identityStep{payload: payload}, nil
}

func (s *identityStep) ID() string                      { return s.payload.Step }
func (s *identityStep) Describe() string                { return "Attest the node identity" }
func (s *identityStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *identityStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

// Check never reports completion: the attestation is the step's result, and a
// result is only recorded by Apply.
func (s *identityStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Detail: "identity attestation is produced on demand"}, nil
}

func (s *identityStep) Apply(_ context.Context, rt engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	attestation, err := Attest(rt.Root, s.payload.Node, s.payload.Nonce)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Result: attestation}, nil
}

// --------------------------------------------------------------------- probe

// ProbePayload asks the node to report its host facts.
type ProbePayload struct {
	Step string `json:"step"`
}

// ToolProbe is one tool the probe looked for.
type ToolProbe struct {
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
}

// OSRelease is the node's distribution release.
type OSRelease struct {
	ID         string `json:"id"`
	VersionID  string `json:"version_id"`
	PrettyName string `json:"pretty_name,omitempty"`
}

// ProbeResult is the real capability probe. Capabilities are what the host
// proved, never what an operator declared.
type ProbeResult struct {
	OS           OSRelease `json:"os"`
	Arch         string    `json:"arch"`
	Supported    bool      `json:"supported"`
	Unsupported  string    `json:"unsupported,omitempty"`
	Docker       ToolProbe `json:"docker"`
	Pack         ToolProbe `json:"pack"`
	WireGuard    ToolProbe `json:"wireguard"`
	User         string    `json:"user,omitempty"`
	Capabilities []string  `json:"capabilities"`
}

// Commander runs a bounded command and returns its standard output. A test
// replaces it; production runs the real program.
type Commander func(ctx context.Context, argv ...string) (string, error)

// ExecCommander runs a program with a bounded timeout and a fixed environment.
func ExecCommander(ctx context.Context, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed probe commands, never request values.
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	if err := command.Run(); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// ParseOSRelease reads the fields of an os-release file that matter.
func ParseOSRelease(text string) OSRelease {
	var release OSRelease
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			release.ID = strings.ToLower(value)
		case "VERSION_ID":
			release.VersionID = value
		case "PRETTY_NAME":
			release.PrettyName = value
		}
	}
	return release
}

// Probe gathers the node's host facts. Missing tools are facts, not errors.
func Probe(ctx context.Context, osReleasePath, arch string, run Commander) ProbeResult {
	result := ProbeResult{Arch: arch, Capabilities: []string{}}
	if raw, err := os.ReadFile(osReleasePath); err == nil { //nolint:gosec // G304: fixed os-release path.
		result.OS = ParseOSRelease(string(raw))
	}
	switch {
	case !platform.SupportedRelease(result.OS.ID, result.OS.VersionID):
		result.Unsupported = fmt.Sprintf("unsupported distribution %q %q: supported are Debian 12/13 and Ubuntu 22.04/24.04", result.OS.ID, result.OS.VersionID)
	case !platform.SupportedArch(arch):
		result.Unsupported = fmt.Sprintf("unsupported architecture %q: supported are amd64 and arm64", arch)
	default:
		result.Supported = true
	}
	if out, err := run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err == nil {
		result.Docker = ToolProbe{Present: true, Version: firstLine(out)}
		result.Capabilities = append(result.Capabilities, platform.CapabilityDocker)
	}
	if out, err := run(ctx, "pack", "version"); err == nil {
		result.Pack = ToolProbe{Present: true, Version: firstLine(out)}
		result.Capabilities = append(result.Capabilities, platform.CapabilityBuildpacks)
	}
	if out, err := run(ctx, "wg", "--version"); err == nil {
		result.WireGuard = ToolProbe{Present: true, Version: firstLine(out)}
	}
	if out, err := run(ctx, "id", "-un"); err == nil {
		result.User = firstLine(out)
	}
	sort.Strings(result.Capabilities)
	return result
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 120 {
		line = line[:120]
	}
	return line
}

type probeStep struct{ payload ProbePayload }

func buildProbeStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ProbePayload
	if err := decodeStrict(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	return &probeStep{payload: payload}, nil
}

func (s *probeStep) ID() string                      { return s.payload.Step }
func (s *probeStep) Describe() string                { return "Probe host capabilities" }
func (s *probeStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *probeStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }
func (s *probeStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Detail: "the probe observes the host on demand"}, nil
}

func (s *probeStep) Apply(ctx context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	return engine.ApplyResult{Result: Probe(ctx, "/etc/os-release", runtime.GOARCH, ExecCommander)}, nil
}

// ------------------------------------------------------------ role host config

// HostcfgPayload is the role-specific host configuration.
type HostcfgPayload struct {
	Step  string   `json:"step"`
	Roles []string `json:"roles"`
	// BuildConcurrency is the builder node's concurrent-build limit.
	BuildConcurrency int `json:"build_concurrency,omitempty"`
	// BuildUser is an optional dedicated build account that must exist.
	BuildUser string `json:"build_user,omitempty"`
}

type hostcfgStep struct{ payload HostcfgPayload }

func buildHostcfgStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload HostcfgPayload
	if err := decodeStrict(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if len(payload.Roles) == 0 || len(payload.Roles) > 8 {
		return nil, errors.New("host configuration needs 1–8 roles")
	}
	sort.Strings(payload.Roles)
	for _, role := range payload.Roles {
		switch role {
		case platform.RoleApplication, platform.RoleBuilder, platform.RoleEdge, platform.RoleRegistry, platform.RoleTelemetry:
		default:
			return nil, fmt.Errorf("role %q has no host configuration", role)
		}
	}
	if payload.BuildConcurrency < 0 || payload.BuildConcurrency > platform.MaxBuildConcurrency {
		return nil, errors.New("build concurrency out of range")
	}
	if payload.BuildUser != "" && !userNamePattern.MatchString(payload.BuildUser) {
		return nil, errors.New("invalid build user")
	}
	return &hostcfgStep{payload: payload}, nil
}

func (s *hostcfgStep) ID() string                      { return s.payload.Step }
func (s *hostcfgStep) Describe() string                { return "Configure the host for its roles" }
func (s *hostcfgStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *hostcfgStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *hostcfgStep) has(role string) bool {
	for _, candidate := range s.payload.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

// BuildDirs are the isolated build directories of a builder node. The cache,
// scratch and home directories are private to builds: a runtime workload never
// shares them, and they live beneath the node's own private state directory.
func BuildDirs(root string) []string {
	base := filepath.Join(root, "build")
	return []string{base, filepath.Join(base, "cache"), filepath.Join(base, "tmp"), filepath.Join(base, "home"), filepath.Join(base, "docker-config")}
}

func (s *hostcfgStep) dirs(root string) []string {
	var dirs []string
	if s.has(platform.RoleApplication) {
		dirs = append(dirs, filepath.Join(root, "runtime"))
	}
	if s.has(platform.RoleBuilder) {
		dirs = append(dirs, BuildDirs(root)...)
	}
	return dirs
}

func (s *hostcfgStep) marker(root string) string { return filepath.Join(root, "hostcfg.json") }

type hostcfgMarker struct {
	Roles            []string `json:"roles"`
	BuildConcurrency int      `json:"build_concurrency,omitempty"`
	BuildUser        string   `json:"build_user,omitempty"`
}

func (s *hostcfgStep) desired() []byte {
	raw, _ := json.Marshal(hostcfgMarker{Roles: s.payload.Roles, BuildConcurrency: s.payload.BuildConcurrency, BuildUser: s.payload.BuildUser})
	return append(raw, '\n')
}

func (s *hostcfgStep) Check(_ context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	for _, dir := range s.dirs(rt.Root) {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return engine.CheckResult{Detail: "role directories are not configured"}, nil
		}
	}
	current, err := os.ReadFile(s.marker(rt.Root)) //nolint:gosec // G304: fixed path beneath the node state directory.
	if err != nil || !bytes.Equal(current, s.desired()) {
		return engine.CheckResult{Detail: "role configuration marker differs"}, nil
	}
	if s.payload.BuildUser != "" {
		if _, err := ExecCommander(context.Background(), "id", "-u", s.payload.BuildUser); err != nil {
			return engine.CheckResult{Detail: "the dedicated build user is missing"}, nil
		}
	}
	return engine.CheckResult{Complete: true, Detail: "role configuration already applied"}, nil
}

func (s *hostcfgStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if s.payload.BuildUser != "" {
		// An unprivileged node account cannot create a user. The operator
		// provisions it; enrollment verifies it, and fails closed if it is
		// missing rather than building under the node account by accident.
		if _, err := ExecCommander(ctx, "id", "-u", s.payload.BuildUser); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w: the dedicated build user %q does not exist; provision it, then retry the enrollment", engine.ErrPermanent, s.payload.BuildUser)
		}
	}
	for _, dir := range s.dirs(rt.Root) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if err := writeFileAtomic(s.marker(rt.Root), s.desired(), 0o600); err != nil {
		return engine.ApplyResult{}, err
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.ID(), Message: "configured roles " + strings.Join(s.payload.Roles, ",")})
	}
	return engine.ApplyResult{Result: map[string]any{"roles": s.payload.Roles, "directories": len(s.dirs(rt.Root))}}, nil
}

// writeFileAtomic replaces a file by renaming a complete temporary file.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ----------------------------------------------------------------- wireguard

// WireGuardPeer is one peer of the node's interface.
type WireGuardPeer struct {
	PublicKey  string   `json:"public_key"`
	AllowedIPs []string `json:"allowed_ips"`
	Endpoint   string   `json:"endpoint,omitempty"`
	Keepalive  int      `json:"keepalive,omitempty"`
}

// WireGuardPayload is a key-generation or peer-apply request.
type WireGuardPayload struct {
	Step   string `json:"step"`
	Action string `json:"action"` // keygen or apply
	// Address is the node's tunnel address, apply only.
	Address    string          `json:"address,omitempty"`
	ListenPort int             `json:"listen_port,omitempty"`
	Peers      []WireGuardPeer `json:"peers,omitempty"`
}

type wireguardStep struct {
	payload  WireGuardPayload
	settings HostSettings
}

func buildWireGuardStep(exec engine.Execution, raw json.RawMessage, settings HostSettings) (engine.Step, error) {
	var payload WireGuardPayload
	if err := decodeStrict(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	switch payload.Action {
	case "keygen":
		if payload.Address != "" || payload.ListenPort != 0 || len(payload.Peers) > 0 {
			return nil, errors.New("a key-generation request carries no peers")
		}
	case "apply":
		if payload.ListenPort < 1 || payload.ListenPort > 65535 {
			return nil, errors.New("invalid WireGuard listen port")
		}
		if !cidrPattern.MatchString(payload.Address) {
			return nil, errors.New("the tunnel address must be an IPv4 CIDR")
		}
		if len(payload.Peers) > 64 {
			return nil, errors.New("too many WireGuard peers")
		}
		seen := map[string]bool{}
		for i, peer := range payload.Peers {
			if !wgKeyPattern.MatchString(peer.PublicKey) || seen[peer.PublicKey] {
				return nil, errors.New("invalid or duplicate WireGuard peer key")
			}
			seen[peer.PublicKey] = true
			if len(peer.AllowedIPs) == 0 || len(peer.AllowedIPs) > 8 {
				return nil, errors.New("a peer needs 1–8 allowed prefixes")
			}
			for _, prefix := range peer.AllowedIPs {
				if !cidrPattern.MatchString(prefix) {
					return nil, errors.New("invalid allowed prefix")
				}
			}
			if peer.Endpoint != "" && !hostPortPattern.MatchString(peer.Endpoint) {
				return nil, errors.New("invalid peer endpoint")
			}
			if peer.Keepalive < 0 || peer.Keepalive > 3600 {
				return nil, errors.New("invalid peer keepalive")
			}
			payload.Peers[i] = peer
		}
		sort.Slice(payload.Peers, func(i, j int) bool { return payload.Peers[i].PublicKey < payload.Peers[j].PublicKey })
	default:
		return nil, fmt.Errorf("unknown WireGuard action %q", payload.Action)
	}
	return &wireguardStep{payload: payload, settings: settings}, nil
}

func (s *wireguardStep) ID() string                      { return s.payload.Step }
func (s *wireguardStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *wireguardStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }
func (s *wireguardStep) Describe() string {
	if s.payload.Action == "keygen" {
		return "Generate the node WireGuard key"
	}
	return "Reconcile WireGuard peers"
}

func wgDir(root string) string { return filepath.Join(root, "wireguard") }

// Check never reports completion: both actions return an observation (the
// public key, the render/apply state), which only Apply records. Apply itself
// is idempotent: it changes nothing when the node already matches.
func (s *wireguardStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Detail: "WireGuard state is observed on demand"}, nil
}

// WireGuardResult is what the step reports. It carries public values only.
type WireGuardResult struct {
	PublicKey string `json:"public_key,omitempty"`
	// State is "rendered" when the configuration is on the node but no
	// privileged helper loaded it, and "applied" when the helper did.
	State      string `json:"state,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`
	PeerCount  int    `json:"peer_count,omitempty"`
	Changed    bool   `json:"changed,omitempty"`
}

func (s *wireguardStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	private, public, err := loadOrCreateWireGuardKey(rt.Root)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if s.payload.Action == "keygen" {
		return engine.ApplyResult{Result: WireGuardResult{PublicKey: public}}, nil
	}
	config := RenderWireGuardConfig(private, s.payload)
	sum := sha256.Sum256([]byte(config))
	hash := hex.EncodeToString(sum[:])
	confPath := filepath.Join(wgDir(rt.Root), "wg0.conf")
	appliedPath := filepath.Join(wgDir(rt.Root), "applied.sha256")
	result := WireGuardResult{PublicKey: public, ConfigHash: hash, PeerCount: len(s.payload.Peers)}
	if current, err := os.ReadFile(confPath); err != nil || string(current) != config { //nolint:gosec // G304: fixed path beneath the node state directory.
		if err := writeFileAtomic(confPath, []byte(config), 0o600); err != nil {
			return engine.ApplyResult{}, err
		}
		result.Changed = true
	}
	appliedHash, _ := os.ReadFile(appliedPath) //nolint:gosec // G304: fixed path beneath the node state directory.
	switch {
	case len(s.settings.WireGuardApply) == 0:
		result.State = "rendered"
	case strings.TrimSpace(string(appliedHash)) == hash:
		result.State = "applied"
	default:
		argv := append(append([]string(nil), s.settings.WireGuardApply...), confPath)
		if _, err := ExecCommander(ctx, argv...); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("the WireGuard apply helper failed: %w", err)
		}
		if err := writeFileAtomic(appliedPath, []byte(hash+"\n"), 0o600); err != nil {
			return engine.ApplyResult{}, err
		}
		result.State, result.Changed = "applied", true
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.ID(), Message: fmt.Sprintf("WireGuard %s with %d peers (%s)", result.State, result.PeerCount, hash[:12])})
	}
	return engine.ApplyResult{Result: result}, nil
}

// RenderWireGuardConfig renders the node's wg0 configuration. The private key
// is part of the file but the file is created 0600 on the node and the key is
// read from the node's own key file: the rendered text never leaves the node
// and is not part of any step input or result.
func RenderWireGuardConfig(privateKey string, payload WireGuardPayload) string {
	var out strings.Builder
	out.WriteString("# Rendered by eregion-node; edits are overwritten.\n[Interface]\n")
	out.WriteString("PrivateKey = " + privateKey + "\n")
	out.WriteString("Address = " + payload.Address + "\n")
	fmt.Fprintf(&out, "ListenPort = %d\n", payload.ListenPort)
	for _, peer := range payload.Peers {
		out.WriteString("\n[Peer]\nPublicKey = " + peer.PublicKey + "\n")
		out.WriteString("AllowedIPs = " + strings.Join(peer.AllowedIPs, ", ") + "\n")
		if peer.Endpoint != "" {
			out.WriteString("Endpoint = " + peer.Endpoint + "\n")
		}
		if peer.Keepalive > 0 {
			fmt.Fprintf(&out, "PersistentKeepalive = %d\n", peer.Keepalive)
		}
	}
	return out.String()
}

// loadOrCreateWireGuardKey returns the node's WireGuard key pair, generating
// it on the node the first time. The private key never leaves this file.
func loadOrCreateWireGuardKey(root string) (string, string, error) {
	path := filepath.Join(wgDir(root), "private.key")
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: fixed path beneath the node state directory.
		private := strings.TrimSpace(string(raw))
		public, err := wireGuardPublic(private)
		if err != nil {
			return "", "", fmt.Errorf("%w: the node WireGuard key is corrupt", engine.ErrPermanent)
		}
		return private, public, nil
	} else if !os.IsNotExist(err) {
		return "", "", err
	}
	if err := os.MkdirAll(wgDir(root), 0o700); err != nil {
		return "", "", err
	}
	var key [curve25519.ScalarSize]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", "", err
	}
	key[0] &= 248
	key[31] = (key[31] & 127) | 64
	private := base64.StdEncoding.EncodeToString(key[:])
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: fixed path beneath the node state directory.
	if err != nil {
		if os.IsExist(err) {
			return loadOrCreateWireGuardKey(root)
		}
		return "", "", err
	}
	if _, err := file.WriteString(private + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	public, err := wireGuardPublic(private)
	return private, public, err
}

func wireGuardPublic(private string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(private)
	if err != nil || len(raw) != curve25519.ScalarSize {
		return "", errors.New("not a WireGuard key")
	}
	public, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(public), nil
}
