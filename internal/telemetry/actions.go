package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const (
	// KindInstall installs the dedicated telemetry stack on node/<node>.
	KindInstall = "platform.telemetry.install"
	// KindAgent installs the exporter collector on an application/builder node.
	KindAgent = "platform.telemetry.agent"
	// InventoryCollection exposes configured telemetry status.
	InventoryCollection = "telemetry"
)

// Placement is the authoritative node fact set supplied by the control plane.
// Action input is never allowed to invent placement.
type Placement struct {
	Name           string
	PrivateAddress string
	HostID         string
	Roles          []string
	IsMain         bool
	IsControlPlane bool
}

type PlacementResolver interface {
	ResolveNode(context.Context, string) (Placement, error)
}

type RuntimeStatus struct {
	Configured     bool              `json:"configured"`
	Node           string            `json:"node,omitempty"`
	PrivateAddress string            `json:"private_address,omitempty"`
	IngestEndpoint string            `json:"otlp_grpc_endpoint,omitempty"`
	HTTPEndpoint   string            `json:"otlp_http_endpoint,omitempty"`
	Endpoints      map[string]string `json:"endpoints,omitempty"`
	Retention      Retention         `json:"retention"`
	Traces         bool              `json:"traces"`
}

// StatusProvider lets the orchestrator report telemetry availability without
// making normal control-plane startup depend on telemetry.
type StatusProvider interface {
	TelemetryStatus(context.Context) (RuntimeStatus, error)
}

// RuntimeConfig wires durable telemetry actions. Every field is optional: when
// an action executes without its runtime, it fails honestly with unavailable.
// Configure is never called from init, so ordinary operations start without a
// telemetry node.
type RuntimeConfig struct {
	Placement PlacementResolver
	// Dispatch is required by both actions. It opens a filesystem+command
	// executor bound to the target's pinned host ID. There is no local fallback
	// in a platform action: installing on the worker would violate placement.
	Dispatch Dispatch
	Status   StatusProvider
}

type installInput struct {
	target              platform.Target
	PrivateAddress      string    `json:"private_address,omitempty"`
	RootDir             string    `json:"root_dir,omitempty"`
	Retention           Retention `json:"retention"`
	Versions            Versions  `json:"versions,omitempty"`
	EnableTraces        bool      `json:"enable_traces"`
	CredentialsFile     string    `json:"credentials_file"`
	GrafanaPasswordFile string    `json:"grafana_password_file"`
}

func (i installInput) Target() platform.Target { return i.target }
func (i installInput) Preview() any {
	return map[string]any{
		"node": i.target.Node, "private_address": i.PrivateAddress, "root_dir": i.RootDir,
		"retention": i.Retention, "enable_traces": i.EnableTraces,
		"credentials_file": i.CredentialsFile, "grafana_password_file": i.GrafanaPasswordFile,
		"note": "File paths are references; credential contents are never part of action input.",
	}
}

type agentInput struct {
	target           platform.Target
	PrivateAddress   string   `json:"private_address,omitempty"`
	TelemetryAddress string   `json:"telemetry_address"`
	TokenFile        string   `json:"token_file"`
	TLSCAFile        string   `json:"tls_ca_file"`
	RootDir          string   `json:"root_dir,omitempty"`
	LogPaths         []string `json:"log_paths,omitempty"`
	Versions         Versions `json:"versions,omitempty"`
}

func (i agentInput) Target() platform.Target { return i.target }
func (i agentInput) Preview() any {
	return map[string]any{
		"node": i.target.Node, "private_address": i.PrivateAddress,
		"telemetry_address": i.TelemetryAddress, "token_file": i.TokenFile,
		"tls_ca_file": i.TLSCAFile, "root_dir": i.RootDir, "log_paths": i.LogPaths,
		"note": "token_file is a path reference; its contents are never part of action input.",
	}
}

// Configure registers the durable telemetry actions and inventory collection on
// reg (or platform.Default when nil). Call once before the API or worker starts.
func Configure(reg *platform.Registry, cfg RuntimeConfig) error {
	if reg == nil {
		reg = platform.Default()
	}
	executor := &actionExecutor{runtime: cfg}
	if err := reg.Register(platform.Definition{
		Kind: KindInstall, Summary: "Install the dedicated telemetry node stack",
		Normalize: normalizeInstall, Executor: executor,
	}); err != nil {
		return err
	}
	if err := reg.Register(platform.Definition{
		Kind: KindAgent, Summary: "Install the OTLP exporter agent on an application or builder node",
		Normalize: normalizeAgent, Executor: executor,
	}); err != nil {
		return err
	}
	return reg.RegisterInventory(inventoryProvider{status: cfg.Status})
}

func normalizeInstall(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	if target.Kind != platform.TargetNode {
		return nil, contract.NewFault(contract.ValidationFailed, "telemetry install target must be node/<node>", "Use the dedicated telemetry node as the target.")
	}
	var input installInput
	input.target = target
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.PrivateAddress != "" && !privateAddressText(input.PrivateAddress) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid telemetry private address", "Use the private address recorded for the node.")
	}
	if input.RootDir != "" && !filepath.IsAbs(input.RootDir) {
		return nil, contract.NewFault(contract.ValidationFailed, "telemetry root directory must be absolute", "Use a root-owned absolute directory.")
	}
	if err := validateSecretReference(input.CredentialsFile); err != nil {
		return nil, err
	}
	if err := validateSecretReference(input.GrafanaPasswordFile); err != nil {
		return nil, err
	}
	retention, err := input.Retention.Validate()
	if err != nil {
		return nil, err
	}
	input.Retention = retention
	versions, err := input.Versions.Validate()
	if err != nil {
		return nil, err
	}
	input.Versions = versions
	return input, nil
}

func normalizeAgent(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	if target.Kind != platform.TargetNode {
		return nil, contract.NewFault(contract.ValidationFailed, "agent target must be node/<node>", "Use the application or builder node as the target.")
	}
	var input agentInput
	input.target = target
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.PrivateAddress != "" && !privateAddressText(input.PrivateAddress) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid node private address", "Use the private address recorded for the node.")
	}
	if err := validateSecretReference(input.TokenFile); err != nil {
		return nil, err
	}
	if err := validateFileReference(input.TLSCAFile); err != nil {
		return nil, err
	}
	if input.RootDir != "" && !filepath.IsAbs(input.RootDir) {
		return nil, contract.NewFault(contract.ValidationFailed, "agent root directory must be absolute", "Use a root-owned absolute directory.")
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(input.TelemetryAddress))
	if err != nil || port != "4317" {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid telemetry OTLP endpoint", "Use the telemetry node's private address with port 4317.")
	}
	ip := net.ParseIP(host)
	if ip == nil || !privateAddress(ip) {
		return nil, contract.NewFault(contract.ValidationFailed, "telemetry endpoint must be a private address", "The exporter accepts only a private telemetry node address.")
	}
	if len(input.LogPaths) > 16 {
		return nil, contract.NewFault(contract.ValidationFailed, "too many log paths", "Use at most 16 explicit container or application log paths.")
	}
	for _, path := range input.LogPaths {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") || strings.Contains(path, "..") {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid agent log path", "Use absolute paths or globs without newlines or parent traversal.")
		}
	}
	versions, err := input.Versions.Validate()
	if err != nil {
		return nil, err
	}
	input.Versions = versions
	return input, nil
}

func privateAddressText(value string) bool {
	node := Node{Name: "node", Role: RoleTelemetry, PrivateAddress: value}
	_, err := node.Validate()
	return err == nil
}

func validateSecretReference(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || strings.Contains(path, "..") {
		return contract.NewFault(contract.ValidationFailed, "a safe absolute credential-file reference is required", "Reference a root-owned 0600 file; never put credential contents in action input.")
	}
	return nil
}

func strictDecode(raw json.RawMessage, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid telemetry action input", "Use the documented fields; unknown fields are rejected.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return contract.NewFault(contract.ValidationFailed, "telemetry action input has trailing data", "Provide exactly one JSON object.")
	}
	return nil
}

type actionExecutor struct {
	runtime RuntimeConfig
}

func (e *actionExecutor) Execute(ctx context.Context, exec platform.Execution) (platform.Result, error) {
	switch exec.Kind {
	case KindInstall:
		return e.install(ctx, exec)
	case KindAgent:
		return e.agent(ctx, exec)
	default:
		return platform.Result{}, contract.NewFault(contract.ValidationFailed, "unsupported telemetry action", "Use platform.telemetry.install or platform.telemetry.agent.")
	}
}

func (e *actionExecutor) install(ctx context.Context, exec platform.Execution) (platform.Result, error) {
	if e.runtime.Placement == nil || e.runtime.Dispatch == nil {
		return platform.Result{}, contract.NewFault(contract.Unavailable, "telemetry dispatch is not configured", "Wire authoritative placement and a pinned node filesystem/command executor before submitting; there is no local-install fallback.")
	}
	var input installInput
	if err := strictDecode(exec.Input, &input); err != nil {
		return platform.Result{}, err
	}
	target, err := platform.ParseTarget(exec.Target)
	if err != nil || target.Kind != platform.TargetNode {
		return platform.Result{}, contract.NewFault(contract.ValidationFailed, "invalid telemetry target", "Use node/<node>.")
	}
	input.target = target
	report(exec, "preflight", "Resolving authoritative node placement.")
	placement, err := e.runtime.Placement.ResolveNode(ctx, target.Node)
	if err != nil {
		return platform.Result{}, err
	}
	if err := validateTelemetryPlacement(target.Node, placement, input.PrivateAddress); err != nil {
		return platform.Result{}, err
	}
	identity := TargetIdentity{Node: placement.Name, PrivateAddress: placement.PrivateAddress, HostID: placement.HostID}
	dispatched, err := e.runtime.Dispatch.Open(ctx, identity)
	if err != nil {
		return platform.Result{}, err
	}
	if !dispatched.Identity().Equal(identity) {
		return platform.Result{}, contract.NewFault(contract.PreconditionFailed, "node dispatch identity does not match authoritative placement", "Refuse the install; the executor is not bound to the intended node and host key.")
	}
	report(exec, "preflight", "Reading credential references.")
	credentials, err := readCredentialFile(input.CredentialsFile)
	if err != nil {
		return platform.Result{}, err
	}
	password, err := readPasswordFile(input.GrafanaPasswordFile)
	if err != nil {
		return platform.Result{}, err
	}
	cfg := Config{
		Node: Node{
			Name: placement.Name, Role: RoleTelemetry, PrivateAddress: placement.PrivateAddress,
			IsMain: placement.IsMain, IsControlPlane: placement.IsControlPlane,
		},
		Retention: input.Retention, Versions: input.Versions,
		Credentials: credentials, RootDir: input.RootDir,
		GrafanaAdminPassword: password, EnableTraces: input.EnableTraces,
	}
	prepared, err := Prepare(cfg)
	if err != nil {
		return platform.Result{}, err
	}
	report(exec, "render", "Rendering the pinned telemetry stack.")
	installer := NewInstallerWithExecutor(prepared, dispatched)
	result, err := installer.Apply(ctx)
	if err != nil {
		return platform.Result{}, err
	}
	report(exec, "apply", "Telemetry stack converged.")
	return platform.Result{
		Message: "Telemetry stack installed on the dedicated node.",
		Data: map[string]any{
			"node": placement.Name, "ingest_endpoint": prepared.IngestEndpoint(),
			"http_endpoint": prepared.HTTPIngestEndpoint(), "endpoints": prepared.DataSourceEndpoints(),
			"retention": prepared.Retention, "traces": prepared.EnableTraces,
			"files_written": len(result.Files), "commands_run": len(result.Commands),
		},
	}, nil
}

func (e *actionExecutor) agent(ctx context.Context, exec platform.Execution) (platform.Result, error) {
	if e.runtime.Placement == nil || e.runtime.Dispatch == nil {
		return platform.Result{}, contract.NewFault(contract.Unavailable, "telemetry dispatch is not configured", "Wire authoritative placement and a pinned node filesystem/command executor before submitting; there is no local-install fallback.")
	}
	var input agentInput
	if err := strictDecode(exec.Input, &input); err != nil {
		return platform.Result{}, err
	}
	target, err := platform.ParseTarget(exec.Target)
	if err != nil || target.Kind != platform.TargetNode {
		return platform.Result{}, contract.NewFault(contract.ValidationFailed, "invalid agent target", "Use node/<node>.")
	}
	input.target = target
	placement, err := e.runtime.Placement.ResolveNode(ctx, target.Node)
	if err != nil {
		return platform.Result{}, err
	}
	if placement.Name != target.Node || placement.IsMain || placement.IsControlPlane || !hasRole(placement.Roles, RoleApplication) && !hasRole(placement.Roles, RoleBuilder) {
		return platform.Result{}, contract.NewFault(contract.PreconditionFailed, "agent target is not an application or builder node", "Install the stack only on a dedicated telemetry node and the agent only on application/build nodes.")
	}
	if input.PrivateAddress != "" && input.PrivateAddress != placement.PrivateAddress {
		return platform.Result{}, contract.NewFault(contract.PreconditionFailed, "agent private address does not match platform placement", "Use the address recorded by node registration.")
	}
	identity := TargetIdentity{Node: placement.Name, PrivateAddress: placement.PrivateAddress, HostID: placement.HostID}
	if err := identity.Validate(); err != nil {
		return platform.Result{}, contract.NewFault(contract.PreconditionFailed, "agent target has no pinned host identity", "Record the host-key fingerprint during enrollment before dispatching an agent.")
	}
	dispatched, err := e.runtime.Dispatch.Open(ctx, identity)
	if err != nil {
		return platform.Result{}, err
	}
	if !dispatched.Identity().Equal(identity) {
		return platform.Result{}, contract.NewFault(contract.PreconditionFailed, "node dispatch identity does not match authoritative placement", "Refuse the install; the executor is not bound to the intended node and host key.")
	}
	raw, err := readSecretFile(input.TokenFile, 1024)
	if err != nil {
		return platform.Result{}, err
	}
	token := strings.TrimRight(string(raw), "\r\n")
	caPEM, err := readConfigFile(input.TLSCAFile, 64*1024)
	if err != nil {
		return platform.Result{}, err
	}
	cfg := AgentConfig{
		Node:             Node{Name: placement.Name, Role: roleForAgent(placement.Roles), PrivateAddress: placement.PrivateAddress, IsMain: placement.IsMain, IsControlPlane: placement.IsControlPlane},
		TelemetryAddress: input.TelemetryAddress, Token: token, TLSCAPEM: caPEM,
		RootDir: input.RootDir, LogPaths: input.LogPaths, Versions: input.Versions,
	}
	prepared, err := PrepareAgent(cfg)
	if err != nil {
		return platform.Result{}, err
	}
	report(exec, "apply", "Installing the node exporter agent.")
	installer := NewAgentInstallerWithExecutor(prepared, dispatched)
	result, err := installer.Apply(ctx)
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{
		Message: "Telemetry exporter agent installed.",
		Data: map[string]any{
			"node": placement.Name, "telemetry_endpoint": prepared.TelemetryAddress,
			"files_written": len(result.Files), "commands_run": len(result.Commands),
		},
	}, nil
}

func validateTelemetryPlacement(name string, placement Placement, requestedAddress string) error {
	if placement.Name != name || !hasRole(placement.Roles, RoleTelemetry) {
		return contract.NewFault(contract.PreconditionFailed, "target node does not have the telemetry role", "Register a dedicated telemetry node and use node/<node> as the target.")
	}
	if placement.IsMain || placement.IsControlPlane {
		return contract.NewFault(contract.PreconditionFailed, "telemetry cannot be installed on the main or control-plane node", "Choose a dedicated telemetry node.")
	}
	if placement.PrivateAddress == "" || !privateAddressText(placement.PrivateAddress) {
		return contract.NewFault(contract.PreconditionFailed, "telemetry node has no private address", "Record a private address during node registration.")
	}
	if requestedAddress != "" && requestedAddress != placement.PrivateAddress {
		return contract.NewFault(contract.PreconditionFailed, "requested private address does not match platform placement", "Use the address recorded by node registration.")
	}
	identity := TargetIdentity{Node: placement.Name, PrivateAddress: placement.PrivateAddress, HostID: placement.HostID}
	if err := identity.Validate(); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "telemetry node has no pinned host identity", "Record the host-key fingerprint during enrollment before installing telemetry.")
	}
	return nil
}

func hasRole(roles []string, role string) bool {
	for _, value := range roles {
		if value == role {
			return true
		}
	}
	return false
}

func roleForAgent(roles []string) string {
	if hasRole(roles, RoleBuilder) {
		return RoleBuilder
	}
	return RoleApplication
}

func report(exec platform.Execution, stage, message string) {
	if exec.Report != nil {
		exec.Report(platform.Event{Level: "info", Stage: stage, Message: message})
	}
}

func readCredentialFile(path string) ([]IngestCredential, error) {
	raw, err := readSecretFile(path, 64*1024)
	if err != nil {
		return nil, err
	}
	var values []struct {
		Node  string `json:"node"`
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "credential file is invalid", "Use a JSON array of node and token entries in a root-owned 0600 file.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, contract.NewFault(contract.ValidationFailed, "credential file has trailing data", "Use exactly one JSON array.")
	}
	if len(values) == 0 {
		return nil, contract.NewFault(contract.ValidationFailed, "credential file is empty", "Issue at least one per-node ingest credential.")
	}
	result := make([]IngestCredential, 0, len(values))
	for _, value := range values {
		result = append(result, IngestCredential{Node: value.Node, Token: value.Token})
	}
	return result, nil
}

func readPasswordFile(path string) (string, error) {
	raw, err := readSecretFile(path, 1024)
	if err != nil {
		return "", err
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(password) == "" {
		return "", contract.NewFault(contract.ValidationFailed, "Grafana password file is empty", "Write the admin password to the referenced 0600 file.")
	}
	return password, nil
}

func validateFileReference(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || strings.Contains(path, "..") {
		return contract.NewFault(contract.ValidationFailed, "a safe absolute file reference is required", "Reference a regular public CA file; never put file contents in action input.")
	}
	return nil
}

func readConfigFile(path string, max int64) ([]byte, error) {
	if err := validateFileReference(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "configuration file is unavailable", "Create the referenced public CA file before submitting the action.")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "configuration file is not a safe regular file", "Use a regular public file no larger than the documented bound; symlinks are rejected.")
	}
	if info.Mode().Perm()&0002 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "configuration file is world-writable", "Remove world-write permission from the CA file.")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "configuration file could not be opened", "Check the file permissions.")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "configuration file could not be read safely", "Check the file size and permissions.")
	}
	return raw, nil
}

func readSecretFile(path string, max int64) ([]byte, error) {
	if err := validateSecretReference(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "credential file is unavailable", "Create the referenced root-owned file before submitting the action.")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file is not a safe regular file", "Use a regular 0600 file no larger than the documented bound; symlinks are rejected.")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file is readable by group or others", "Set mode 0600 on the credential file.")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "credential file could not be opened", "Check ownership and permissions.")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file could not be read safely", "Check the file size and permissions.")
	}
	return raw, nil
}

type inventoryProvider struct {
	status StatusProvider
}

func (inventoryProvider) Collection() string { return InventoryCollection }

func (p inventoryProvider) Read(ctx context.Context, query platform.InventoryQuery) (any, error) {
	if p.status == nil {
		return []RuntimeStatus{{Configured: false, Retention: DefaultRetention()}}, nil
	}
	status, err := p.status.TelemetryStatus(ctx)
	if err != nil {
		return nil, err
	}
	if query.Node != "" && status.Node != query.Node {
		return []RuntimeStatus{}, nil
	}
	return []RuntimeStatus{status}, nil
}
