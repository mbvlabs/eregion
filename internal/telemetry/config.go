// Package telemetry provisions a dedicated, role-restricted telemetry node and
// exposes a small read interface for logs and metrics. It deliberately does not
// own operation history: durable deployment history stays in PostgreSQL.
package telemetry

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	// RoleTelemetry is the only node role this installer accepts.
	RoleTelemetry = "telemetry"
	// RoleApplication and RoleBuilder are the only roles for a shipping agent.
	RoleApplication = "application"
	RoleBuilder     = "builder"

	DefaultRootDir = "/var/lib/eregion/telemetry"
	AgentRootDir   = "/var/lib/eregion/otel-agent"
)

// Retention is expressed in days so the values map directly to every backend's
// retention flag. Zero fields receive the defaults; values above the hard caps
// are rejected rather than silently clamped.
type Retention struct {
	MetricsDays int `json:"metrics_days"`
	LogsDays    int `json:"logs_days"`
	TracesDays  int `json:"traces_days"`
}

func DefaultRetention() Retention {
	return Retention{MetricsDays: 15, LogsDays: 14, TracesDays: 7}
}

// Validate applies defaults and enforces the bounded retention policy.
func (r Retention) Validate() (Retention, error) {
	if r == (Retention{}) {
		r = DefaultRetention()
	}
	switch {
	case r.MetricsDays < 1 || r.MetricsDays > 90:
		return r, contract.NewFault(contract.ValidationFailed, "metrics retention is out of range", "Use 1–90 days; the default is 15.")
	case r.LogsDays < 1 || r.LogsDays > 90:
		return r, contract.NewFault(contract.ValidationFailed, "logs retention is out of range", "Use 1–90 days; the default is 14.")
	case r.TracesDays < 1 || r.TracesDays > 30:
		return r, contract.NewFault(contract.ValidationFailed, "traces retention is out of range", "Use 1–30 days; the default is 7.")
	}
	return r, nil
}

// Versions pins every upstream image. The zero value uses the released defaults
// below. A mutable tag such as "latest" is rejected.
type Versions struct {
	Collector       string `json:"collector"`
	VictoriaMetrics string `json:"victoria_metrics"`
	VictoriaLogs    string `json:"victoria_logs"`
	Tempo           string `json:"tempo"`
	Grafana         string `json:"grafana"`
}

func DefaultVersions() Versions {
	return Versions{
		Collector:       "0.157.0",
		VictoriaMetrics: "v1.113.0",
		VictoriaLogs:    "v1.8.0",
		Tempo:           "2.7.1",
		Grafana:         "11.5.1",
	}
}

var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (v Versions) Validate() (Versions, error) {
	if v == (Versions{}) {
		v = DefaultVersions()
	}
	for name, value := range map[string]string{
		"collector": v.Collector, "VictoriaMetrics": v.VictoriaMetrics,
		"VictoriaLogs": v.VictoriaLogs, "tempo": v.Tempo, "Grafana": v.Grafana,
	} {
		if value == "latest" || !imageTagPattern.MatchString(value) {
			return v, contract.NewFault(contract.ValidationFailed, "invalid pinned image version for "+name, "Pin a released tag; mutable or empty tags are rejected.")
		}
	}
	return v, nil
}

// Node identifies the explicit placement of the telemetry stack. The main and
// control-plane flags are recorded by enrollment; both must be false here. This
// prevents a telemetry install from being pointed at the machine that also runs
// the control plane.
type Node struct {
	Name           string
	Role           string
	PrivateAddress string
	IsMain         bool
	IsControlPlane bool
}

var nodeNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (n Node) Validate() (Node, error) {
	if !nodeNamePattern.MatchString(n.Name) {
		return n, contract.NewFault(contract.ValidationFailed, "invalid telemetry node name", "Use a lowercase DNS label of at most 63 characters.")
	}
	if n.Role != RoleTelemetry {
		return n, contract.NewFault(contract.ValidationFailed, "telemetry install requires the telemetry node role", "Enroll a dedicated node with role telemetry; application and builder nodes receive only the exporter agent.")
	}
	if n.IsMain || n.IsControlPlane {
		return n, contract.NewFault(contract.ValidationFailed, "telemetry cannot be installed on the main or control-plane node", "Choose a dedicated telemetry node that is neither main nor control-plane.")
	}
	ip := net.ParseIP(strings.TrimSpace(n.PrivateAddress))
	if ip == nil {
		return n, contract.NewFault(contract.ValidationFailed, "invalid telemetry private address", "Use a literal private IPv4 or IPv6 address assigned to the node.")
	}
	if !privateAddress(ip) {
		return n, contract.NewFault(contract.ValidationFailed, "telemetry binds must use a private address", "Use RFC 1918 or IPv6 ULA; loopback, link-local, public, unspecified, and multicast addresses are rejected.")
	}
	n.PrivateAddress = ip.String()
	return n, nil
}

func privateAddress(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	return ip.IsPrivate()
}

// IngestCredential is one node's OTLP bearer credential. Credentials are sent
// to the collector through a root-owned token file, never through process
// arguments or a shell.
type IngestCredential struct {
	Node  string `json:"node"`
	Token string `json:"token"`
}

func (c IngestCredential) Validate() error {
	if !nodeNamePattern.MatchString(c.Node) {
		return contract.NewFault(contract.ValidationFailed, "invalid ingest credential node name", "Use a lowercase DNS label.")
	}
	if len(c.Token) < 32 || len(c.Token) > 512 || strings.TrimSpace(c.Token) != c.Token {
		return contract.NewFault(contract.ValidationFailed, "invalid ingest credential for "+c.Node, "Generate a 32–512 byte token without whitespace.")
	}
	for _, r := range c.Token {
		if r <= ' ' || r == 0x7f || r == '/' || r == '\\' {
			return contract.NewFault(contract.ValidationFailed, "invalid ingest credential for "+c.Node, "Use a base64url token; whitespace and path separators are rejected.")
		}
	}
	return nil
}

// GenerateIngestToken returns a 256-bit random base64url token for one node.
func GenerateIngestToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

// Config is the complete input to a telemetry stack render and install.
type Config struct {
	Node        Node
	Retention   Retention
	Versions    Versions
	Credentials []IngestCredential
	RootDir     string
	// GrafanaAdminPassword is placed in a 0600 env file, never in a command.
	GrafanaAdminPassword string
	// EnableTraces adds Tempo and its OTLP export pipeline.
	EnableTraces bool
}

// Prepared is a validated, defaulted config with a canonical root directory.
type Prepared struct {
	Config
}

func Prepare(cfg Config) (Prepared, error) {
	node, err := cfg.Node.Validate()
	if err != nil {
		return Prepared{}, err
	}
	retention, err := cfg.Retention.Validate()
	if err != nil {
		return Prepared{}, err
	}
	versions, err := cfg.Versions.Validate()
	if err != nil {
		return Prepared{}, err
	}
	if len(cfg.Credentials) == 0 {
		return Prepared{}, contract.NewFault(contract.ValidationFailed, "at least one node ingest credential is required", "Enroll each application or builder node with its own token before installing telemetry.")
	}
	seenNodes := map[string]bool{}
	seenTokens := map[string]bool{}
	for _, credential := range cfg.Credentials {
		if err := credential.Validate(); err != nil {
			return Prepared{}, err
		}
		if credential.Node == node.Name {
			return Prepared{}, contract.NewFault(contract.ValidationFailed, "telemetry node cannot authenticate to itself as an ingest client", "Issue credentials only to application and builder nodes.")
		}
		if seenNodes[credential.Node] {
			return Prepared{}, contract.NewFault(contract.ValidationFailed, "duplicate ingest credential for "+credential.Node, "Keep exactly one current credential per node.")
		}
		if seenTokens[credential.Token] {
			return Prepared{}, contract.NewFault(contract.ValidationFailed, "duplicate ingest token", "Every node needs a distinct token so one can be revoked without affecting another.")
		}
		seenNodes[credential.Node] = true
		seenTokens[credential.Token] = true
	}
	if strings.TrimSpace(cfg.GrafanaAdminPassword) == "" || len(cfg.GrafanaAdminPassword) > 256 || strings.ContainsAny(cfg.GrafanaAdminPassword, "\r\n") {
		return Prepared{}, contract.NewFault(contract.ValidationFailed, "a Grafana admin password is required", "Provide a single-line password of at most 256 bytes; it is written to a private env file.")
	}
	root := cfg.RootDir
	if root == "" {
		root = DefaultRootDir
	}
	absolute, err := filepath.Abs(root)
	if err != nil || !filepath.IsAbs(absolute) {
		return Prepared{}, contract.NewFault(contract.ValidationFailed, "invalid telemetry root directory", "Use an absolute directory owned by root.")
	}
	cfg.Node = node
	cfg.Retention = retention
	cfg.Versions = versions
	cfg.RootDir = filepath.Clean(absolute)
	return Prepared{Config: cfg}, nil
}

// IngestEndpoint is the private OTLP/gRPC endpoint applications and node agents
// configure. Ports are constants of the rendered stack.
func (p Prepared) IngestEndpoint() string {
	return net.JoinHostPort(p.Node.PrivateAddress, "4317")
}

// HTTPIngestEndpoint is the private OTLP/HTTP endpoint.
func (p Prepared) HTTPIngestEndpoint() string {
	return net.JoinHostPort(p.Node.PrivateAddress, "4318")
}

// MetricsReadToken returns the bearer token enforced by the metrics read
// gateway. It is derived from the install credential, so retries and re-renders
// keep the same token without storing it in action input.
func (p Prepared) MetricsReadToken() string { return readToken(p.GrafanaAdminPassword, "metrics") }

// LogsReadToken returns the bearer token enforced by the logs read gateway.
func (p Prepared) LogsReadToken() string { return readToken(p.GrafanaAdminPassword, "logs") }

func readToken(password, kind string) string {
	mac := hmac.New(sha256.New, []byte(password))
	_, _ = mac.Write([]byte("eregion/telemetry/read/" + kind))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TLSCAPath is the public CA certificate rendered on the telemetry node. Copy
// it to application and builder nodes before installing an agent.
func (p Prepared) TLSCAPath() string { return filepath.Join(p.RootDir, "tls", "ca.crt") }

// MetricsReadTokenPath and LogsReadTokenPath are private files on the telemetry
// node. A caller that reads telemetry must load the appropriate token from a
// safe 0600 copy; never put the token in an action input.
func (p Prepared) MetricsReadTokenPath() string {
	return filepath.Join(p.RootDir, "read-tokens", "metrics")
}
func (p Prepared) LogsReadTokenPath() string {
	return filepath.Join(p.RootDir, "read-tokens", "logs")
}

// AgentConfig is the exporter collector installed on an application or builder
// node. It never contains a credential: the token is mounted from a 0600 file.
type AgentConfig struct {
	Node             Node
	TelemetryAddress string
	Token            string
	TLSCAPEM         []byte
	RootDir          string
	LogPaths         []string
	Versions         Versions
}

type PreparedAgent struct {
	AgentConfig
}

func PrepareAgent(cfg AgentConfig) (PreparedAgent, error) {
	if cfg.Node.Name == "" || !nodeNamePattern.MatchString(cfg.Node.Name) {
		return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "invalid node name", "Use a lowercase DNS label.")
	}
	if cfg.Node.Role != RoleApplication && cfg.Node.Role != RoleBuilder {
		return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "agent install requires an application or builder role", "Telemetry nodes run the stack installer; control-plane nodes do not run a shipping agent.")
	}
	if cfg.Node.IsMain || cfg.Node.IsControlPlane {
		return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "agent cannot be installed on the main or control-plane node", "Export telemetry from application and builder nodes only.")
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(cfg.TelemetryAddress))
	if err != nil || port != "4317" {
		return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "invalid telemetry OTLP endpoint", "Use the telemetry node's private address with port 4317.")
	}
	ip := net.ParseIP(host)
	if ip == nil || !privateAddress(ip) {
		return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "telemetry endpoint must be a private address", "The exporter accepts only a private telemetry node address.")
	}
	credential := IngestCredential{Node: cfg.Node.Name, Token: cfg.Token}
	if err := credential.Validate(); err != nil {
		return PreparedAgent{}, err
	}
	if err := validateCAFile(cfg.TLSCAPEM); err != nil {
		return PreparedAgent{}, err
	}
	if len(cfg.LogPaths) == 0 {
		cfg.LogPaths = []string{"/var/log/containers/*.log", "/var/log/eregion/*.log"}
	}
	if len(cfg.LogPaths) > 16 {
		return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "too many log paths", "Use at most 16 explicit container or application log paths.")
	}
	for _, path := range cfg.LogPaths {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") || strings.Contains(path, "..") {
			return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "invalid agent log path", "Use absolute paths or globs without newlines or parent traversal.")
		}
	}
	versions, err := cfg.Versions.Validate()
	if err != nil {
		return PreparedAgent{}, err
	}
	root := cfg.RootDir
	if root == "" {
		root = AgentRootDir
	}
	absolute, err := filepath.Abs(root)
	if err != nil || !filepath.IsAbs(absolute) {
		return PreparedAgent{}, contract.NewFault(contract.ValidationFailed, "invalid agent root directory", "Use an absolute directory owned by root.")
	}
	cfg.TelemetryAddress = net.JoinHostPort(ip.String(), port)
	cfg.Versions = versions
	cfg.RootDir = filepath.Clean(absolute)
	return PreparedAgent{AgentConfig: cfg}, nil
}

// ErrNoTelemetryNode is returned by a configured data source when the caller
// knows no telemetry node exists. Callers must surface this as an absent state,
// not as zero samples.
var ErrNoTelemetryNode = errors.New("no telemetry node is configured")

// BuildStack installs the stack for a prepared config. It is a convenience for
// callers that already validated Node/Role placement.
func BuildStack(cfg Config) (*Installer, error) {
	prepared, err := Prepare(cfg)
	if err != nil {
		return nil, err
	}
	return NewInstaller(prepared, ExecRunner{}), nil
}

// AddressOf strips brackets from an already validated endpoint for diagnostics.
func AddressOf(endpoint string) string {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return endpoint
	}
	return fmt.Sprintf("%s:%s", host, port)
}
