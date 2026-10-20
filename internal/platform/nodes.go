package platform

import (
	"encoding/json"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"golang.org/x/crypto/ssh"
)

// Node enrollment and reconciliation are durable actions. Enrollment replaces
// "declared facts only" registration for any node that is reached over SSH: the
// control plane pins the SSH host key the operator supplied, verifies a key the
// node generated itself, probes the host, and only then records the node as
// enrolled. A hostname or an address is never proof of identity.
const (
	KindNodeEnroll    = "platform.node.enroll"
	KindNodeReconcile = "platform.node.reconcile"
)

// EnrollmentState is where a node is in its enrollment lifecycle.
type EnrollmentState string

const (
	// EnrollmentRegistered is a declared-facts record. Nothing was verified.
	EnrollmentRegistered EnrollmentState = "registered"
	// EnrollmentPending is an accepted enrollment that has not finished.
	EnrollmentPending EnrollmentState = "pending"
	// EnrollmentEnrolled is a node whose host key and identity are pinned and
	// whose host was probed. Only an enrolled node receives remote work.
	EnrollmentEnrolled EnrollmentState = "enrolled"
	// EnrollmentFailed is an enrollment that failed for a reason a retry of the
	// same inputs cannot fix; the stored error says why.
	EnrollmentFailed EnrollmentState = "failed"
)

// WireGuardState is where a node's private-network configuration is.
type WireGuardState string

const (
	WireGuardNone    WireGuardState = "none"
	WireGuardKeyed   WireGuardState = "keyed"
	WireGuardRender  WireGuardState = "rendered"
	WireGuardApplied WireGuardState = "applied"
	WireGuardFailed  WireGuardState = "failed"
)

// DefaultSSHUser is the unprivileged node account the control plane uses.
const DefaultSSHUser = "eregion"

// MaxBuildConcurrency bounds a builder node's concurrent builds.
const MaxBuildConcurrency = 16

var (
	sshUserPattern     = regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`)
	fingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

// NodeWireGuard is the public, secret-free private-network state of a node.
type NodeWireGuard struct {
	Enabled      bool           `json:"enabled"`
	Address      string         `json:"address,omitempty"`
	Endpoint     string         `json:"endpoint,omitempty"`
	PublicKey    string         `json:"public_key,omitempty"`
	State        WireGuardState `json:"state"`
	ReconciledAt *time.Time     `json:"reconciled_at,omitempty"`
}

// NodeEnrollInput enrolls (or re-verifies) one node. The canonical input holds
// only public material: a pinned public host key, optional expected
// fingerprints, and addressing facts. It never holds a credential.
type NodeEnrollInput struct {
	Node  string   `json:"node,omitempty"`
	Roles []string `json:"roles,omitempty"`
	// Address is the SSH host:port the control plane dials.
	Address string `json:"address,omitempty"`
	// User is the unprivileged node account; it defaults to eregion.
	User string `json:"user,omitempty"`
	// HostKey is the node's SSH public host key in authorized_keys form. It is
	// pinned: there is no trust on first use.
	HostKey string `json:"host_key,omitempty"`
	// HostKeyFingerprint optionally cross-checks HostKey (SHA256:...).
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	// IdentityFingerprint optionally pins the node-generated identity key. When
	// empty the key is pinned from the first verified attestation.
	IdentityFingerprint string `json:"identity_fingerprint,omitempty"`
	// BuildConcurrency limits concurrent builds on a builder node (default 1).
	BuildConcurrency int `json:"build_concurrency,omitempty"`
	// BuildUser optionally names a dedicated build account that must exist.
	BuildUser      string               `json:"build_user,omitempty"`
	PrivateNetwork *PrivateNetworkInput `json:"private_network,omitempty"`
	// Retry re-runs the enrollment recorded for the node without new facts.
	Retry bool `json:"retry,omitempty"`
}

// PrivateNetworkInput asks for the node to join the WireGuard private network.
type PrivateNetworkInput struct {
	// Endpoint is the node's public WireGuard host:port (UDP).
	Endpoint string `json:"endpoint"`
}

func (in *NodeEnrollInput) Target() Target { return Target{Kind: TargetNode, Node: in.Node} }
func (in *NodeEnrollInput) Preview() any   { return in }

// NodeReconcileInput reconciles host configuration and WireGuard peers.
type NodeReconcileInput struct {
	Node string `json:"node,omitempty"`
	// Via selects the path: empty uses the node's current transport; "direct"
	// forces the direct SSH address, which repairs a broken tunnel.
	Via string `json:"via,omitempty"`
}

func (in *NodeReconcileInput) Target() Target { return Target{Kind: TargetNode, Node: in.Node} }
func (in *NodeReconcileInput) Preview() any   { return in }

func normalizeNodeEnroll(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetNode {
		return nil, invalid("target", "platform.node.enroll requires target node/<node>.")
	}
	var input NodeEnrollInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.Node != "" && input.Node != target.Node {
		return nil, invalid("node", "The node field must match the target node, or be omitted.")
	}
	input.Node = target.Node
	if input.Retry {
		if len(input.Roles) > 0 || input.Address != "" || input.User != "" || input.HostKey != "" ||
			input.HostKeyFingerprint != "" || input.IdentityFingerprint != "" || input.BuildConcurrency != 0 ||
			input.BuildUser != "" || input.PrivateNetwork != nil {
			return nil, invalid("retry", "A retry re-runs the recorded enrollment; do not combine it with new facts.")
		}
		return &input, nil
	}
	roles, err := validateRoles(input.Roles)
	if err != nil {
		return nil, err
	}
	if err := ValidateEnrollRoles(roles); err != nil {
		return nil, err
	}
	input.Roles = roles
	host, port, err := net.SplitHostPort(input.Address)
	if err != nil || host == "" || port == "" || strings.ContainsAny(input.Address, " \t\r\n/@") {
		return nil, invalid("node address", "Use the SSH host:port without credentials or whitespace.")
	}
	if input.User == "" {
		input.User = DefaultSSHUser
	}
	if !sshUserPattern.MatchString(input.User) {
		return nil, invalid("ssh user", "Use a plain unprivileged account name.")
	}
	if input.User == "root" {
		return nil, invalid("ssh user", "The control plane never connects as root; use the unprivileged node account.")
	}
	key, fingerprint, err := ParseHostKey(input.HostKey)
	if err != nil {
		return nil, err
	}
	if input.HostKeyFingerprint != "" && input.HostKeyFingerprint != fingerprint {
		return nil, invalid("host key fingerprint", "The fingerprint does not match the supplied host key; verify the key out of band.")
	}
	input.HostKey, input.HostKeyFingerprint = key, fingerprint
	if input.IdentityFingerprint != "" && !fingerprintPattern.MatchString(input.IdentityFingerprint) {
		return nil, invalid("identity fingerprint", "Use the SHA256:<base64> fingerprint printed by eregion-node identity.")
	}
	hasBuilder := false
	for _, role := range roles {
		hasBuilder = hasBuilder || role == RoleBuilder
	}
	if input.BuildConcurrency == 0 {
		input.BuildConcurrency = 1
	}
	if input.BuildConcurrency < 1 || input.BuildConcurrency > MaxBuildConcurrency {
		return nil, invalid("build concurrency", "Use 1–16 concurrent builds.")
	}
	if !hasBuilder && (input.BuildConcurrency != 1 || input.BuildUser != "") {
		return nil, invalid("build settings", "Build concurrency and build user apply only to nodes with the builder role.")
	}
	if input.BuildUser != "" && !sshUserPattern.MatchString(input.BuildUser) {
		return nil, invalid("build user", "Use a plain account name.")
	}
	if input.PrivateNetwork != nil {
		if endpointHost, endpointPort, err := net.SplitHostPort(input.PrivateNetwork.Endpoint); err != nil || endpointHost == "" || endpointPort == "" || strings.ContainsAny(input.PrivateNetwork.Endpoint, " \t\r\n/@") {
			return nil, invalid("private network endpoint", "Use the node's public WireGuard host:port (UDP).")
		}
	}
	return &input, nil
}

func normalizeNodeReconcile(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetNode {
		return nil, invalid("target", "platform.node.reconcile requires target node/<node>.")
	}
	var input NodeReconcileInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.Node != "" && input.Node != target.Node {
		return nil, invalid("node", "The node field must match the target node, or be omitted.")
	}
	input.Node = target.Node
	if input.Via != "" && input.Via != "direct" {
		return nil, invalid("via", "Use \"direct\" or omit it.")
	}
	return &input, nil
}

// ParseHostKey parses an SSH public host key in authorized_keys form and
// returns its canonical text (type and base64 only) and SHA256 fingerprint.
func ParseHostKey(text string) (string, string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 2048 || strings.ContainsAny(text, "\r\n\x00") {
		return "", "", invalid("host key", "Supply the node's SSH host public key on one line, for example from ssh-keyscan; it is pinned, never trusted on first use.")
	}
	key, _, options, _, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil || len(options) > 0 {
		return "", "", invalid("host key", "Use a public key in authorized_keys form: \"<type> <base64>\".")
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	return canonical, ssh.FingerprintSHA256(key), nil
}

// ValidateEnrollRoles enforces the role-combination policy for an enrolled
// (additional) node. The main node keeps its own defaults; control-plane is
// never enrollable.
//
// Allowed combinations: application, edge, application+edge, builder,
// registry, builder+registry, telemetry. A builder never shares a node with an
// application workload, so source execution, build credentials, and caches
// stay away from runtime workloads. Telemetry is always alone.
func ValidateEnrollRoles(roles []string) error {
	set := map[string]bool{}
	for _, role := range roles {
		set[role] = true
	}
	if set[RoleControlPlane] {
		return invalid("node roles", "The control-plane role belongs to the main node and cannot be enrolled.")
	}
	allowed := [][]string{
		{RoleApplication}, {RoleEdge}, {RoleApplication, RoleEdge},
		{RoleBuilder}, {RoleRegistry}, {RoleBuilder, RoleRegistry},
		{RoleTelemetry},
	}
	for _, candidate := range allowed {
		if len(candidate) == len(set) {
			match := true
			for _, role := range candidate {
				match = match && set[role]
			}
			if match {
				return nil
			}
		}
	}
	sorted := append([]string(nil), roles...)
	sort.Strings(sorted)
	if set[RoleBuilder] && set[RoleApplication] {
		return contract.NewFault(contract.ValidationFailed, "builder and application roles cannot share an enrolled node",
			"Enroll two nodes: build credentials, caches, and source execution stay off nodes that run application workloads.")
	}
	return contract.NewFault(contract.ValidationFailed, "unsupported node role combination: "+strings.Join(sorted, "+"),
		"Allowed: application, edge, application+edge, builder, registry, builder+registry, telemetry.")
}

// RequiredCapabilities returns the capabilities a node must prove (by probe)
// for a role set. A declared role without its capabilities is refused.
func RequiredCapabilities(roles []string) []string {
	need := map[string]bool{}
	for _, role := range roles {
		switch role {
		case RoleApplication:
			need[CapabilityDocker] = true
		case RoleBuilder:
			need[CapabilityDocker] = true
			need[CapabilityBuildpacks] = true
		}
	}
	out := make([]string, 0, len(need))
	for capability := range need {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

// SupportedRelease reports whether a Linux distribution release is in the
// supported matrix: Debian 12/13 and Ubuntu 22.04/24.04.
func SupportedRelease(id, versionID string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "debian":
		return versionID == "12" || versionID == "13"
	case "ubuntu":
		return versionID == "22.04" || versionID == "24.04"
	}
	return false
}

// SupportedArch reports whether the node architecture is supported.
func SupportedArch(arch string) bool { return arch == "amd64" || arch == "arm64" }

// RegistryScope is what a registry credential may do.
type RegistryScope string

const (
	RegistryPush RegistryScope = "push"
	RegistryPull RegistryScope = "pull"
)

// RegistryScopeFor returns the only registry credential scope a node's roles
// justify: a builder pushes, a runtime node only pulls. A node with both would
// need two credentials; the enrollment policy does not allow that mix.
func RegistryScopeFor(roles []string) RegistryScope {
	for _, role := range roles {
		if role == RoleBuilder {
			return RegistryPush
		}
	}
	return RegistryPull
}
