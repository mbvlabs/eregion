package platform

import (
	"encoding/json"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

const (
	DefaultContainerPort = 8080
	DefaultHealthPath    = "/"
	DefaultHealthTimeout = 30
	DefaultBind          = "127.0.0.1"
	DefaultBuilder       = "paketobuildpacks/builder-jammy-base"
)

type AppCreateInput struct {
	Name string `json:"name"`
}

func (in *AppCreateInput) Target() Target { return PlatformTarget() }
func (in *AppCreateInput) Preview() any   { return in }

type HealthInput struct {
	Type           string `json:"type,omitempty"`
	Path           string `json:"path,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type EnvironmentCreateInput struct {
	App           string            `json:"app,omitempty"`
	Name          string            `json:"name"`
	Node          string            `json:"node"`
	ContainerPort int               `json:"container_port,omitempty"`
	HostPort      int               `json:"host_port,omitempty"`
	Bind          string            `json:"bind,omitempty"`
	Health        HealthInput       `json:"health,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	// SecretRefs maps environment variable names to opaque secret references.
	// The M3 vault agent resolves them; values never enter this input, the
	// canonical operation input, the node payload, or history.
	SecretRefs map[string]string `json:"secret_refs,omitempty"`
}

func (in *EnvironmentCreateInput) Target() Target {
	return Target{Kind: TargetApp, App: in.App}
}
func (in *EnvironmentCreateInput) Preview() any { return in }

type ReleaseCreateInput struct {
	App         string       `json:"app,omitempty"`
	Environment string       `json:"environment,omitempty"`
	Source      Source       `json:"source"`
	Build       *Build       `json:"build,omitempty"`
	Command     *CommandSpec `json:"command,omitempty"`
	Pull        bool         `json:"pull,omitempty"`
}

func (in *ReleaseCreateInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *ReleaseCreateInput) Preview() any { return in }

type DeployInput struct {
	App         string    `json:"app,omitempty"`
	Environment string    `json:"environment,omitempty"`
	ReleaseID   ReleaseID `json:"release_id"`
}

func (in *DeployInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *DeployInput) Preview() any { return in }

type RollbackInput struct {
	App         string    `json:"app,omitempty"`
	Environment string    `json:"environment,omitempty"`
	ToReleaseID ReleaseID `json:"to_release_id,omitempty"`
}

func (in *RollbackInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *RollbackInput) Preview() any { return in }

type NodeRegisterInput struct {
	Name         string   `json:"name"`
	Roles        []string `json:"roles"`
	Capabilities []string `json:"capabilities"`
	Address      string   `json:"address,omitempty"`
}

func (in *NodeRegisterInput) Target() Target { return PlatformTarget() }
func (in *NodeRegisterInput) Preview() any   { return in }

type NodeCheckInput struct {
	Node string `json:"node,omitempty"`
}

func (in *NodeCheckInput) Target() Target { return Target{Kind: TargetNode, Node: in.Node} }
func (in *NodeCheckInput) Preview() any   { return in }

// Deploy and rollback inputs carry their resolved app and environment, so the
// canonical input binds the same scope as the operation target.

func registerBuiltins(r *Registry) {
	r.registerBuiltin(Definition{Kind: KindAppCreate, Summary: "Create an application", Normalize: normalizeAppCreate})
	r.registerBuiltin(Definition{Kind: KindNodeRegister, Summary: "Register node placement facts", Normalize: normalizeNodeRegister})
	r.registerBuiltin(Definition{Kind: KindNodeCheck, Summary: "Check node runtime availability", Normalize: normalizeNodeCheck})
	r.registerBuiltin(Definition{Kind: KindNodeEnroll, Summary: "Enroll a node: pin its host key, verify its identity, probe it, and configure its roles", Normalize: normalizeNodeEnroll})
	r.registerBuiltin(Definition{Kind: KindNodeReconcile, Summary: "Reconcile a node's WireGuard peers and role configuration", Normalize: normalizeNodeReconcile})
	r.registerBuiltin(Definition{Kind: KindEnvironmentCreate, Summary: "Create an application environment", Normalize: normalizeEnvironmentCreate})
	r.registerBuiltin(Definition{Kind: KindReleaseCreate, Summary: "Record or build a release", Normalize: normalizeReleaseCreate})
	r.registerBuiltin(Definition{Kind: KindDeploy, Summary: "Deploy a release with health-first switching", Normalize: normalizeDeploy})
	r.registerBuiltin(Definition{Kind: KindRollback, Summary: "Deploy a previous release", Normalize: normalizeRollback})
}

func normalizeAppCreate(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetPlatform {
		return nil, invalid("target", "platform.app.create requires the canonical target platform.")
	}
	var input AppCreateInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if err := ValidateName(input.Name); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeNodeRegister(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetPlatform {
		return nil, invalid("target", "platform.node.register requires the canonical target platform.")
	}
	var input NodeRegisterInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if err := ValidateName(input.Name); err != nil {
		return nil, err
	}
	roles, err := validateRoles(input.Roles)
	if err != nil {
		return nil, err
	}
	caps, err := validateCapabilities(input.Capabilities)
	if err != nil {
		return nil, err
	}
	input.Roles, input.Capabilities = roles, caps
	if input.Address != "" {
		host, port, err := net.SplitHostPort(input.Address)
		if err != nil || host == "" || port == "" || strings.ContainsAny(input.Address, " \t\r\n/@") {
			return nil, invalid("node address", "Use host:port without credentials or whitespace.")
		}
	}
	return &input, nil
}

func normalizeNodeCheck(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetNode {
		return nil, invalid("target", "platform.node.check requires target node/<node>.")
	}
	var input NodeCheckInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.Node != "" && input.Node != target.Node {
		return nil, invalid("node", "The node field must match the target node, or be omitted.")
	}
	input.Node = target.Node
	return &input, nil
}

func normalizeEnvironmentCreate(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetApp {
		return nil, invalid("target", "platform.environment.create requires target app/<app>.")
	}
	var input EnvironmentCreateInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.App != "" && input.App != target.App {
		return nil, invalid("app", "The app field must match the target app, or be omitted.")
	}
	input.App = target.App
	if err := ValidateName(input.Name); err != nil {
		return nil, err
	}
	if err := ValidateName(input.Node); err != nil {
		return nil, err
	}
	if input.ContainerPort == 0 {
		input.ContainerPort = DefaultContainerPort
	}
	if err := validatePort(input.ContainerPort, 1); err != nil {
		return nil, err
	}
	if input.HostPort != 0 {
		if err := validatePort(input.HostPort, 1); err != nil {
			return nil, err
		}
	}
	if input.Bind == "" {
		input.Bind = DefaultBind
	}
	if net.ParseIP(input.Bind) == nil || strings.Contains(input.Bind, "/") {
		return nil, invalid("bind address", "Use a literal IPv4 or IPv6 address such as 127.0.0.1 or 0.0.0.0.")
	}
	health, err := normalizeHealth(input.Health)
	if err != nil {
		return nil, err
	}
	input.Health = health
	if input.Env == nil {
		input.Env = map[string]string{}
	}
	if err := validateEnv(input.Env, "environment configuration"); err != nil {
		return nil, err
	}
	if input.SecretRefs == nil {
		input.SecretRefs = map[string]string{}
	}
	if err := validateSecretRefs(input.SecretRefs); err != nil {
		return nil, err
	}
	return &input, nil
}

func validateSecretRefs(refs map[string]string) error {
	if len(refs) > 64 {
		return invalid("secret references", "Use at most 64 secret references.")
	}
	for key, ref := range refs {
		if !envKeyPattern.MatchString(key) {
			return invalid("secret reference key", "Use 1–128 ASCII letters, digits, and underscores; do not start with a digit.")
		}
		if len(ref) == 0 || len(ref) > 200 || strings.ContainsAny(ref, " \t\r\n\x00") || strings.HasPrefix(ref, "-") {
			return invalid("secret reference", "Use an opaque reference without whitespace, such as vault path/name.")
		}
	}
	return nil
}

func normalizeHealth(in HealthInput) (HealthInput, error) {
	if in.Type == "" {
		in.Type = "http"
	}
	if in.Type != "http" && in.Type != "tcp" {
		return in, invalid("health type", "Use http or tcp.")
	}
	if in.Path == "" {
		in.Path = DefaultHealthPath
	}
	if !strings.HasPrefix(in.Path, "/") || len(in.Path) > 256 || strings.ContainsAny(in.Path, " \t\r\n\x00?#") {
		return in, invalid("health path", "Use an absolute path without whitespace, controls, query, or fragment, up to 256 bytes.")
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = DefaultHealthTimeout
	}
	if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 300 {
		return in, invalid("health timeout", "Use 1–300 seconds.")
	}
	return in, nil
}

func normalizeReleaseCreate(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.release.create requires target env/<app>/<env>.")
	}
	var input ReleaseCreateInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.App != "" && input.App != target.App {
		return nil, invalid("app", "The app field must match the target app, or be omitted.")
	}
	if input.Environment != "" && input.Environment != target.Environment {
		return nil, invalid("environment", "The environment field must match the target environment, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if err := normalizeSource(&input); err != nil {
		return nil, err
	}
	if input.Command != nil {
		if err := validateCommand(input.Command); err != nil {
			return nil, err
		}
	}
	return &input, nil
}

func validateCommand(command *CommandSpec) error {
	if len(command.Argv) == 0 || len(command.Argv) > 32 {
		return invalid("release command", "Use 1–32 arguments; the command runs directly without a shell.")
	}
	for _, arg := range command.Argv {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
			return invalid("release command argument", "Arguments cannot contain NUL or line breaks.")
		}
	}
	if command.TimeoutSeconds == 0 {
		command.TimeoutSeconds = 120
	}
	if command.TimeoutSeconds < 1 || command.TimeoutSeconds > 600 {
		return invalid("release command timeout", "Use 1–600 seconds.")
	}
	return nil
}

func normalizeSource(input *ReleaseCreateInput) error {
	switch input.Source.Type {
	case "image":
		if input.Build != nil {
			return invalid("build", "An image release does not run Buildpacks; omit build.")
		}
		if err := ValidateImage(input.Source.Image); err != nil {
			return err
		}
		if input.Source.Path != "" || input.Source.URL != "" || input.Source.Ref != "" {
			return invalid("source", "An image source uses only source.image.")
		}
	case "directory":
		if input.Source.Image != "" || input.Source.URL != "" || input.Source.Ref != "" {
			return invalid("source", "A directory source uses only source.path.")
		}
		if !filepath.IsAbs(input.Source.Path) || strings.ContainsRune(input.Source.Path, 0) || strings.ContainsAny(input.Source.Path, "\r\n") {
			return invalid("source path", "Use an absolute path available on the target node.")
		}
		if err := normalizeBuild(input); err != nil {
			return err
		}
	case "git":
		if input.Source.Image != "" || input.Source.Path != "" {
			return invalid("source", "A git source uses source.url and optional source.ref.")
		}
		parsed, err := url.Parse(input.Source.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return invalid("source URL", "Use an https:// repository URL without credentials, query, or fragment.")
		}
		if input.Source.Ref != "" && !validGitRef(input.Source.Ref) {
			return invalid("source ref", "Use a branch or tag of 1–128 letters, digits, dot, underscore, slash, or hyphen; it cannot contain .. or start with -.")
		}
		if err := normalizeBuild(input); err != nil {
			return err
		}
	default:
		return invalid("source type", "Use image, directory, or git.")
	}
	return nil
}

func normalizeBuild(input *ReleaseCreateInput) error {
	if input.Build == nil {
		input.Build = &Build{}
	}
	if input.Build.Builder == "" {
		input.Build.Builder = DefaultBuilder
	}
	if err := ValidateImage(input.Build.Builder); err != nil {
		return err
	}
	if input.Build.Image != "" {
		if err := ValidateImage(input.Build.Image); err != nil {
			return err
		}
	}
	if input.Build.Env == nil {
		input.Build.Env = map[string]string{}
	}
	return validateEnv(input.Build.Env, "build environment")
}

func normalizeDeploy(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.deploy requires target env/<app>/<env>.")
	}
	var input DeployInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.App != "" && input.App != target.App {
		return nil, invalid("app", "The app field must match the target app, or be omitted.")
	}
	if input.Environment != "" && input.Environment != target.Environment {
		return nil, invalid("environment", "The environment field must match the target environment, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if err := input.ReleaseID.Validate(); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeRollback(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.rollback requires target env/<app>/<env>.")
	}
	var input RollbackInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.App != "" && input.App != target.App {
		return nil, invalid("app", "The app field must match the target app, or be omitted.")
	}
	if input.Environment != "" && input.Environment != target.Environment {
		return nil, invalid("environment", "The environment field must match the target environment, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if input.ToReleaseID != "" {
		if err := input.ToReleaseID.Validate(); err != nil {
			return nil, err
		}
	}
	return &input, nil
}

func validateRoles(roles []string) ([]string, error) {
	if len(roles) == 0 || len(roles) > 8 {
		return nil, invalid("node roles", "Use 1–8 roles from application, builder, edge, registry, telemetry, control-plane.")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		known := false
		for _, candidate := range knownRoles {
			if role == candidate {
				known = true
				break
			}
		}
		if !known || seen[role] {
			return nil, invalid("node role", "Use each supported role at most once: application, builder, edge, registry, telemetry, control-plane.")
		}
		seen[role] = true
		out = append(out, role)
	}
	return out, nil
}

func validateCapabilities(capabilities []string) ([]string, error) {
	if len(capabilities) == 0 || len(capabilities) > 16 {
		return nil, invalid("node capabilities", "Use 1–16 capabilities such as docker, buildpacks, registry, or http-ingress.")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.TrimSpace(capability)
		if !capabilityPattern.MatchString(capability) || seen[capability] {
			return nil, invalid("node capability", "Use 1–32 lowercase letters, digits, and inner hyphens for each capability, without duplicates.")
		}
		seen[capability] = true
		out = append(out, capability)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func validGitRef(ref string) bool {
	if ref == "" || len(ref) > 128 || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") {
		return false
	}
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '/', r == '-':
		default:
			return false
		}
	}
	return true
}
