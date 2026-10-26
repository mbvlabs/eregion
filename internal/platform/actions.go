package platform

import (
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	DefaultContainerPort = 8080
	DefaultHealthPath    = "/"
	DefaultHealthTimeout = 30
	DefaultBind          = "127.0.0.1"
	DefaultBuilder       = "paketobuildpacks/builder-jammy-base"
)

var (
	processNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	secretNamePattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

type AppCreateInput struct {
	Name string `json:"name"`
	// Environment optionally creates the first environment and its first
	// desired-state revision in the same submission transaction.
	Environment *EnvironmentSpec `json:"environment,omitempty"`
	// Deploy records the intent to build and deploy the first revision. Building
	// from a revision is implemented by the deploy work list row.
	Deploy bool `json:"deploy,omitempty"`
}

func (in *AppCreateInput) Target() Target { return PlatformTarget() }
func (in *AppCreateInput) Preview() any   { return in }

type HealthInput struct {
	Type           string `json:"type,omitempty"`
	Path           string `json:"path,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// EnvironmentSpec is the complete first-environment declaration used by
// platform.app.create. It intentionally has no target of its own.
type EnvironmentSpec struct {
	Name string `json:"name"`
	// Node runs application containers. It must have application + docker.
	Node string `json:"node"`
	// BuildNode optionally runs Buildpacks for releases of this environment. It
	// must have builder + buildpacks. When empty, Node is the build node.
	BuildNode     string            `json:"build_node,omitempty"`
	ContainerPort int               `json:"container_port,omitempty"`
	HostPort      int               `json:"host_port,omitempty"`
	Bind          string            `json:"bind,omitempty"`
	Health        HealthInput       `json:"health,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	// SecretRefs is the transitional pinned form. New revisions use SecretNames
	// and D18 resolution; it remains only until the deploy row replaces it.
	SecretRefs map[string]string `json:"secret_refs,omitempty"`

	Source            SourceBinding     `json:"source,omitempty"`
	AutoDeploy        bool              `json:"auto_deploy,omitempty"`
	WebhookSecretName string            `json:"webhook_secret_name,omitempty"`
	Runtime           RuntimeSpec       `json:"runtime,omitempty"`
	Processes         []ProcessSpec     `json:"processes,omitempty"`
	Config            map[string]string `json:"config,omitempty"`
	SecretNames       []string          `json:"secret_names,omitempty"`
}

type EnvironmentCreateInput struct {
	App string `json:"app,omitempty"`
	EnvironmentSpec
}

func (in *EnvironmentCreateInput) Target() Target {
	return Target{Kind: TargetApp, App: in.App}
}
func (in *EnvironmentCreateInput) Preview() any { return in }

// EnvironmentUpdateInput is a partial patch. Every supplied field replaces the
// corresponding part of the current revision; the result is a new full
// revision, never an edit of the old one.
type EnvironmentUpdateInput struct {
	App         string  `json:"app,omitempty"`
	Environment string  `json:"environment,omitempty"`
	Node        *string `json:"node,omitempty"`
	BuildNode   *string `json:"build_node,omitempty"`

	Source            *SourceBinding     `json:"source,omitempty"`
	AutoDeploy        *bool              `json:"auto_deploy,omitempty"`
	WebhookSecretName *string            `json:"webhook_secret_name,omitempty"`
	Runtime           *RuntimeSpec       `json:"runtime,omitempty"`
	Processes         *[]ProcessSpec     `json:"processes,omitempty"`
	Config            *map[string]string `json:"config,omitempty"`
	SecretNames       *[]string          `json:"secret_names,omitempty"`
}

func (in *EnvironmentUpdateInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *EnvironmentUpdateInput) Preview() any { return in }

type ProcessScaleInput struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Process     string `json:"process"`
	Replicas    int    `json:"replicas"`
}

func (in *ProcessScaleInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *ProcessScaleInput) Preview() any { return in }

type ConfigSetInput struct {
	App         string            `json:"app,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Config      map[string]string `json:"config"`
}

func (in *ConfigSetInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *ConfigSetInput) Preview() any { return in }

type ConfigUnsetInput struct {
	App         string   `json:"app,omitempty"`
	Environment string   `json:"environment,omitempty"`
	Keys        []string `json:"keys"`
}

func (in *ConfigUnsetInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *ConfigUnsetInput) Preview() any { return in }

// EnvironmentSecretAttachInput adds an existing vault secret NAME to the
// environment desired state. The value is written by secret set and never
// enters this input.
type EnvironmentSecretAttachInput struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Name        string `json:"name"`
}

func (in *EnvironmentSecretAttachInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *EnvironmentSecretAttachInput) Preview() any { return in }

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
	r.registerBuiltin(Definition{Kind: KindEnvironmentUpdate, Summary: "Change an environment as a new desired-state revision", Normalize: normalizeEnvironmentUpdate})
	r.registerBuiltin(Definition{Kind: KindProcessScale, Summary: "Scale one environment process in a new revision", Normalize: normalizeProcessScale})
	r.registerBuiltin(Definition{Kind: KindConfigSet, Summary: "Set plain environment configuration in a new revision", Normalize: normalizeConfigSet})
	r.registerBuiltin(Definition{Kind: KindConfigUnset, Summary: "Unset plain environment configuration in a new revision", Normalize: normalizeConfigUnset})
	r.registerBuiltin(Definition{Kind: KindEnvironmentSecretAttach, Summary: "Attach a vault secret name to the environment's next revision", Normalize: normalizeEnvironmentSecretAttach})
	r.registerBuiltin(Definition{Kind: KindReleaseCreate, Summary: "Record or build a release", Normalize: normalizeReleaseCreate})
	r.registerBuiltin(Definition{Kind: KindDeploy, Summary: "Deploy a release with health-first switching", Normalize: normalizeDeploy})
	r.registerBuiltin(Definition{Kind: KindRollback, Summary: "Deploy a previous release", Normalize: normalizeRollback})
	r.registerBuiltin(Definition{Kind: KindDomainAdd, Summary: "Route a public hostname to an environment through Caddy", Normalize: normalizeDomain})
	r.registerBuiltin(Definition{Kind: KindDomainRemove, Summary: "Remove a public hostname's route", Normalize: normalizeDomain})
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
	if input.Environment != nil {
		if err := normalizeEnvironmentSpec(input.Environment); err != nil {
			return nil, err
		}
	} else if input.Deploy {
		return nil, invalid("deploy", "A first deploy needs an environment specification.")
	}
	return &input, nil
}

func normalizeEnvironmentSpec(spec *EnvironmentSpec) error {
	if err := ValidateName(spec.Name); err != nil {
		return err
	}
	if err := ValidateName(spec.Node); err != nil {
		return err
	}
	if spec.BuildNode != "" {
		if err := ValidateName(spec.BuildNode); err != nil {
			return err
		}
	}
	if spec.ContainerPort != 0 {
		if err := validatePort(spec.ContainerPort, 1); err != nil {
			return err
		}
	}
	if spec.HostPort != 0 {
		if err := validatePort(spec.HostPort, 1); err != nil {
			return err
		}
	}
	if spec.Bind == "" {
		spec.Bind = DefaultBind
	}
	if net.ParseIP(spec.Bind) == nil || strings.Contains(spec.Bind, "/") {
		return invalid("bind address", "Use a literal IPv4 or IPv6 address such as 127.0.0.1 or 0.0.0.0.")
	}
	health, err := normalizeHealth(spec.Health)
	if err != nil {
		return err
	}
	spec.Health = health
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if err := validateEnv(spec.Env, "environment configuration"); err != nil {
		return err
	}
	if spec.SecretRefs == nil {
		spec.SecretRefs = map[string]string{}
	}
	if err := validateSecretRefs(spec.SecretRefs); err != nil {
		return err
	}
	if spec.Config == nil {
		spec.Config = map[string]string{}
	}
	if spec.Processes == nil {
		spec.Processes = []ProcessSpec{}
	}
	state := RevisionState{
		Source:            spec.Source,
		AutoDeploy:        spec.AutoDeploy,
		WebhookSecretName: spec.WebhookSecretName,
		Runtime:           spec.Runtime,
		Processes:         spec.Processes,
		Config:            spec.Config,
		SecretNames:       spec.SecretNames,
	}
	if len(state.Config) == 0 && len(spec.Env) > 0 {
		state.Config = spec.Env
	}
	if len(state.SecretNames) > 0 {
		spec.SecretRefs = map[string]string{}
	}
	// The legacy health block remains the fallback for the older environment
	// create form; a revision runtime is the authoritative new form.
	if state.Runtime.Node != "" && state.Runtime.Node != spec.Node {
		return invalid("runtime node", "The runtime node must match the environment's node.")
	}
	state.Runtime.Node = spec.Node
	if state.Runtime.HealthType == "" {
		state.Runtime.HealthType = spec.Health.Type
	}
	if state.Runtime.HealthPath == "" {
		state.Runtime.HealthPath = spec.Health.Path
	}
	if state.Runtime.HealthTimeoutSeconds == 0 {
		state.Runtime.HealthTimeoutSeconds = spec.Health.TimeoutSeconds
	}
	if spec.ContainerPort == 0 && len(state.Processes) > 0 {
		for _, process := range state.Processes {
			if process.Kind == "web" && process.ContainerPort > 0 {
				spec.ContainerPort = process.ContainerPort
				break
			}
		}
	}
	if spec.ContainerPort == 0 {
		spec.ContainerPort = DefaultContainerPort
	}
	if err := normalizeRevisionState(&state); err != nil {
		return err
	}
	spec.Source = state.Source
	spec.AutoDeploy = state.AutoDeploy
	spec.WebhookSecretName = state.WebhookSecretName
	spec.Runtime = state.Runtime
	spec.Processes = state.Processes
	spec.Config = state.Config
	spec.SecretNames = state.SecretNames
	if len(state.Processes) == 0 {
		spec.Processes = []ProcessSpec{defaultWebProcess(spec.ContainerPort)}
	}
	return nil
}

func defaultWebProcess(port int) ProcessSpec {
	if port == 0 {
		port = DefaultContainerPort
	}
	return ProcessSpec{Name: "web", Kind: "web", Args: []string{}, Replicas: 1, ContainerPort: port}
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
	if err := normalizeEnvironmentSpec(&input.EnvironmentSpec); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeEnvironmentUpdate(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.environment.update requires target env/<app>/<env>.")
	}
	var input EnvironmentUpdateInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if (input.App != "" && input.App != target.App) || (input.Environment != "" && input.Environment != target.Environment) {
		return nil, invalid("environment", "The app and environment fields must match the target, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if input.Node != nil {
		if err := ValidateName(*input.Node); err != nil {
			return nil, err
		}
	}
	if input.BuildNode != nil && *input.BuildNode != "" {
		if err := ValidateName(*input.BuildNode); err != nil {
			return nil, err
		}
	}
	if input.Source != nil {
		if err := normalizeSourceBinding(input.Source); err != nil {
			return nil, err
		}
	}
	if input.Runtime != nil {
		if err := normalizeRuntime(input.Runtime); err != nil {
			return nil, err
		}
	}
	if input.Processes != nil {
		processes, err := normalizeProcessSpecs(*input.Processes)
		if err != nil {
			return nil, err
		}
		input.Processes = &processes
	}
	if input.Config != nil {
		if err := validateEnv(*input.Config, "environment configuration"); err != nil {
			return nil, err
		}
	}
	if input.SecretNames != nil {
		names, err := normalizeSecretNames(*input.SecretNames)
		if err != nil {
			return nil, err
		}
		input.SecretNames = &names
	}
	if input.WebhookSecretName != nil {
		name, err := normalizeOptionalSecretName(*input.WebhookSecretName)
		if err != nil {
			return nil, err
		}
		input.WebhookSecretName = &name
	}
	return &input, nil
}

func normalizeProcessScale(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.process.scale requires target env/<app>/<env>.")
	}
	var input ProcessScaleInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if (input.App != "" && input.App != target.App) || (input.Environment != "" && input.Environment != target.Environment) {
		return nil, invalid("environment", "The app and environment fields must match the target, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if !processNamePattern.MatchString(input.Process) {
		return nil, invalid("process", "Use a lowercase process name of 1–63 letters, digits, underscores, or hyphens.")
	}
	if input.Replicas < 1 || input.Replicas > 32 {
		return nil, invalid("replicas", "Use between 1 and 32 replicas.")
	}
	return &input, nil
}

func normalizeConfigSet(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.config.set requires target env/<app>/<env>.")
	}
	var input ConfigSetInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if (input.App != "" && input.App != target.App) || (input.Environment != "" && input.Environment != target.Environment) {
		return nil, invalid("environment", "The app and environment fields must match the target, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if len(input.Config) == 0 {
		return nil, invalid("config", "Provide at least one configuration key.")
	}
	if err := validateEnv(input.Config, "environment configuration"); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeConfigUnset(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.config.unset requires target env/<app>/<env>.")
	}
	var input ConfigUnsetInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if (input.App != "" && input.App != target.App) || (input.Environment != "" && input.Environment != target.Environment) {
		return nil, invalid("environment", "The app and environment fields must match the target, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if len(input.Keys) == 0 || len(input.Keys) > 64 {
		return nil, invalid("keys", "Provide 1–64 configuration keys.")
	}
	seen := map[string]bool{}
	for _, key := range input.Keys {
		if !envKeyPattern.MatchString(key) {
			return nil, invalid("configuration key", "Use 1–128 ASCII letters, digits, and underscores; do not start with a digit.")
		}
		if seen[key] {
			return nil, invalid("configuration key", "List each key once.")
		}
		seen[key] = true
	}
	return &input, nil
}

func normalizeEnvironmentSecretAttach(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.environment.secret.attach requires target env/<app>/<env>.")
	}
	var input EnvironmentSecretAttachInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if (input.App != "" && input.App != target.App) || (input.Environment != "" && input.Environment != target.Environment) {
		return nil, invalid("environment", "The app and environment fields must match the target, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if !secretNamePattern.MatchString(input.Name) {
		return nil, invalid("secret name", "Use 1–128 uppercase ASCII letters, digits, and underscores; start with a letter.")
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

// NormalizeRevisionState applies defaults and validates one complete desired
// state. It is used both for a new revision and after a patch is merged.
func NormalizeRevisionState(state *RevisionState) error {
	if err := normalizeSourceBinding(&state.Source); err != nil {
		return err
	}
	if err := normalizeRuntime(&state.Runtime); err != nil {
		return err
	}
	processes, err := normalizeProcessSpecs(state.Processes)
	if err != nil {
		return err
	}
	state.Processes = processes
	if state.Config == nil {
		state.Config = map[string]string{}
	}
	if err := validateEnv(state.Config, "environment configuration"); err != nil {
		return err
	}
	names, err := normalizeSecretNames(state.SecretNames)
	if err != nil {
		return err
	}
	state.SecretNames = names
	name, err := normalizeOptionalSecretName(state.WebhookSecretName)
	if err != nil {
		return err
	}
	state.WebhookSecretName = name
	if state.AutoDeploy && state.Source.Kind != "git" {
		return invalid("auto_deploy", "Automatic deployment requires a git source binding.")
	}
	return nil
}

func normalizeRevisionState(state *RevisionState) error {
	return NormalizeRevisionState(state)
}

func normalizeSourceBinding(source *SourceBinding) error {
	source.Kind = strings.ToLower(strings.TrimSpace(source.Kind))
	source.URL = strings.TrimSpace(source.URL)
	source.Ref = strings.TrimSpace(source.Ref)
	source.CredentialSecretName = strings.TrimSpace(source.CredentialSecretName)
	source.ContextPath = strings.TrimSpace(source.ContextPath)
	source.Builder = strings.TrimSpace(source.Builder)
	source.Image = strings.TrimSpace(source.Image)
	if source.BuildEnv == nil {
		source.BuildEnv = map[string]string{}
	}
	switch source.Kind {
	case "":
		if source.URL != "" || source.Ref != "" || source.CredentialSecretName != "" ||
			source.ContextPath != "" || source.Builder != "" || source.Image != "" || len(source.BuildEnv) > 0 {
			return invalid("source kind", "Use git or image, or omit the source to leave a deployability gap.")
		}
		return nil
	case "image":
		if err := ValidateImage(source.Image); err != nil {
			return err
		}
		if source.URL != "" || source.Ref != "" || source.CredentialSecretName != "" ||
			source.ContextPath != "" || source.Builder != "" || len(source.BuildEnv) > 0 {
			return invalid("source", "An image source uses only source.image.")
		}
		return nil
	case "git":
		if err := ValidateGitSourceURL(source.URL); err != nil {
			return invalid("source URL", err.Error())
		}
		if source.Ref != "" && !validGitRef(source.Ref) {
			return invalid("source ref", "Use a branch or tag of 1–128 letters, digits, dot, underscore, slash, or hyphen; it cannot contain .. or start with -.")
		}
		if source.Image != "" {
			return invalid("source", "A git source uses source.url and optional source.ref.")
		}
		if source.CredentialSecretName != "" {
			if !secretNamePattern.MatchString(source.CredentialSecretName) {
				return invalid("credential secret name", "Use a vault secret NAME such as GIT_READ_TOKEN; the value is never part of the revision.")
			}
		}
		if err := validateContextPath(source.ContextPath); err != nil {
			return err
		}
		if source.Builder == "" {
			source.Builder = DefaultBuilder
		}
		if err := ValidateImage(source.Builder); err != nil {
			return err
		}
		if err := validateEnv(source.BuildEnv, "build environment"); err != nil {
			return err
		}
		return nil
	default:
		return invalid("source kind", "Use git or image.")
	}
}

func validateContextPath(path string) error {
	if path == "" {
		return nil
	}
	if filepath.IsAbs(path) || strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") || len(path) > 512 {
		return invalid("source context path", "Use a relative path inside the repository.")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return invalid("source context path", "The context path cannot leave the repository.")
		}
	}
	return nil
}

func normalizeRuntime(runtime *RuntimeSpec) error {
	runtime.Node = strings.TrimSpace(runtime.Node)
	if runtime.Node != "" {
		if err := ValidateName(runtime.Node); err != nil {
			return err
		}
	}
	runtime.HealthType = strings.ToLower(strings.TrimSpace(runtime.HealthType))
	if runtime.HealthType == "" {
		runtime.HealthType = "http"
	}
	if runtime.HealthType != "http" && runtime.HealthType != "tcp" {
		return invalid("health type", "Use http or tcp.")
	}
	runtime.HealthPath = strings.TrimSpace(runtime.HealthPath)
	if runtime.HealthPath == "" {
		runtime.HealthPath = DefaultHealthPath
	}
	if !strings.HasPrefix(runtime.HealthPath, "/") || len(runtime.HealthPath) > 256 || strings.ContainsAny(runtime.HealthPath, " \t\r\n\x00?#") {
		return invalid("health path", "Use an absolute path without whitespace, controls, query, or fragment, up to 256 bytes.")
	}
	if runtime.HealthTimeoutSeconds == 0 {
		runtime.HealthTimeoutSeconds = DefaultHealthTimeout
	}
	if runtime.HealthTimeoutSeconds < 1 || runtime.HealthTimeoutSeconds > 300 {
		return invalid("health timeout", "Use 1–300 seconds.")
	}
	if runtime.ReleaseCommand != nil {
		if err := validateCommand(runtime.ReleaseCommand); err != nil {
			return err
		}
	}
	return nil
}

func normalizeProcessSpecs(processes []ProcessSpec) ([]ProcessSpec, error) {
	if len(processes) > 32 {
		return nil, invalid("processes", "Use at most 32 processes.")
	}
	out := make([]ProcessSpec, 0, len(processes))
	seen := map[string]bool{}
	web := 0
	for _, process := range processes {
		process.Name = strings.ToLower(strings.TrimSpace(process.Name))
		process.Kind = strings.ToLower(strings.TrimSpace(process.Kind))
		process.Command = strings.TrimSpace(process.Command)
		if process.Args == nil {
			process.Args = []string{}
		}
		if process.Replicas == 0 {
			process.Replicas = 1
		}
		if !processNamePattern.MatchString(process.Name) {
			return nil, invalid("process name", "Use a lowercase name of 1–63 letters, digits, underscores, or hyphens.")
		}
		if seen[process.Name] {
			return nil, invalid("process name", "Use each process name once.")
		}
		seen[process.Name] = true
		switch process.Kind {
		case "web", "worker", "service":
		default:
			return nil, invalid("process kind", "Use web, worker, or service.")
		}
		if process.Replicas < 1 || process.Replicas > 32 {
			return nil, invalid("process replicas", "Use between 1 and 32 replicas.")
		}
		if len(process.Command) > 1024 || strings.ContainsAny(process.Command, "\x00\r\n") {
			return nil, invalid("process command", "Use at most 1024 bytes without NUL or line breaks.")
		}
		if len(process.Args) > 128 {
			return nil, invalid("process arguments", "Use at most 128 arguments.")
		}
		for _, arg := range process.Args {
			if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
				return nil, invalid("process argument", "Arguments cannot contain NUL or line breaks, and are limited to 4096 bytes.")
			}
		}
		if process.Kind == "web" {
			web++
			if process.Name != "web" || process.ContainerPort < 1 || process.ContainerPort > 65535 {
				return nil, invalid("web process", "The web process must be named web and declare a valid container port.")
			}
		}
		if process.Kind == "worker" {
			if process.ContainerPort != 0 {
				return nil, invalid("process port", "A worker process cannot publish a container port.")
			}
			if process.Command == "" {
				return nil, invalid("process command", "A worker process needs a command.")
			}
		}
		if process.Kind == "service" {
			if process.Name == "web" {
				return nil, invalid("process name", "web is reserved for the web process.")
			}
			if process.Command == "" {
				return nil, invalid("process command", "A service process needs a command.")
			}
			if process.ContainerPort < 1 || process.ContainerPort > 65535 {
				return nil, invalid("process port", "A service process needs a valid container port.")
			}
		}
		out = append(out, process)
	}
	if web > 1 {
		return nil, invalid("web process", "Use at most one web process.")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func normalizeSecretNames(names []string) ([]string, error) {
	if len(names) > 128 {
		return nil, invalid("secret names", "Use at most 128 secret names.")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if !secretNamePattern.MatchString(name) {
			return nil, invalid("secret name", "Use 1–128 uppercase ASCII letters, digits, and underscores; start with a letter.")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeOptionalSecretName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if !secretNamePattern.MatchString(name) {
		return "", invalid("secret name", "Use 1–128 uppercase ASCII letters, digits, and underscores; start with a letter.")
	}
	return name, nil
}

// CloneRevisionState copies maps and slices so a new revision never aliases the
// previous revision's state.
func CloneRevisionState(state RevisionState) RevisionState {
	out := state
	out.Source.BuildEnv = cloneStringMap(state.Source.BuildEnv)
	out.Config = cloneStringMap(state.Config)
	out.SecretNames = append([]string(nil), state.SecretNames...)
	out.Processes = make([]ProcessSpec, len(state.Processes))
	for i, process := range state.Processes {
		process.Args = append([]string(nil), process.Args...)
		out.Processes[i] = process
	}
	if state.Runtime.ReleaseCommand != nil {
		command := *state.Runtime.ReleaseCommand
		command.Argv = append([]string(nil), command.Argv...)
		out.Runtime.ReleaseCommand = &command
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// ApplyEnvironmentPatch returns the next full desired state. The old revision is
// never mutated.
func ApplyEnvironmentPatch(base RevisionState, patch EnvironmentUpdateInput) RevisionState {
	out := CloneRevisionState(base)
	if patch.Node != nil {
		out.Runtime.Node = *patch.Node
	}
	if patch.Source != nil {
		out.Source = CloneRevisionState(RevisionState{Source: *patch.Source}).Source
	}
	if patch.AutoDeploy != nil {
		out.AutoDeploy = *patch.AutoDeploy
	}
	if patch.WebhookSecretName != nil {
		out.WebhookSecretName = *patch.WebhookSecretName
	}
	if patch.Runtime != nil {
		runtime := *patch.Runtime
		if runtime.Node == "" {
			runtime.Node = base.Runtime.Node
		}
		if runtime.ReleaseCommand != nil {
			command := *runtime.ReleaseCommand
			command.Argv = append([]string(nil), command.Argv...)
			runtime.ReleaseCommand = &command
		}
		out.Runtime = runtime
	}
	if patch.Processes != nil {
		out.Processes = make([]ProcessSpec, len(*patch.Processes))
		for i, process := range *patch.Processes {
			process.Args = append([]string(nil), process.Args...)
			out.Processes[i] = process
		}
	}
	if patch.Config != nil {
		out.Config = cloneStringMap(*patch.Config)
	}
	if patch.SecretNames != nil {
		out.SecretNames = append([]string(nil), (*patch.SecretNames)...)
	}
	return out
}

// ScaleProcess returns a new state with one process's replicas changed.
func ScaleProcess(base RevisionState, name string, replicas int) (RevisionState, error) {
	out := CloneRevisionState(base)
	for i := range out.Processes {
		if out.Processes[i].Name == name {
			if replicas < 1 || replicas > 32 {
				return base, invalid("replicas", "Use between 1 and 32 replicas.")
			}
			out.Processes[i].Replicas = replicas
			return out, nil
		}
	}
	return base, invalid("process", "The named process is not in the environment revision.")
}

// SetConfig merges plain configuration into a new state.
func SetConfig(base RevisionState, values map[string]string) RevisionState {
	out := CloneRevisionState(base)
	if out.Config == nil {
		out.Config = map[string]string{}
	}
	for key, value := range values {
		out.Config[key] = value
	}
	return out
}

// UnsetConfig removes plain configuration keys from a new state.
func UnsetConfig(base RevisionState, keys []string) RevisionState {
	out := CloneRevisionState(base)
	for _, key := range keys {
		delete(out.Config, key)
	}
	return out
}

// AttachSecret adds a vault secret name without accepting a value.
func AttachSecret(base RevisionState, name string) RevisionState {
	out := CloneRevisionState(base)
	for _, existing := range out.SecretNames {
		if existing == name {
			return out
		}
	}
	out.SecretNames = append(out.SecretNames, name)
	sort.Strings(out.SecretNames)
	return out
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
		if err := ValidateGitSourceURL(input.Source.URL); err != nil {
			return invalid("source URL", err.Error())
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

// DomainInput names a public hostname and the environment it routes to.
type DomainInput struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Domain      string `json:"domain"`
}

func (in *DomainInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *DomainInput) Preview() any { return in }

func normalizeDomain(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "domain actions require target env/<app>/<env>.")
	}
	var input DomainInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if (input.App != "" && input.App != target.App) || (input.Environment != "" && input.Environment != target.Environment) {
		return nil, invalid("environment", "The app and environment fields must match the target, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	input.Domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input.Domain), "."))
	if err := ValidateDomain(input.Domain); err != nil {
		return nil, invalid("domain", err.Error())
	}
	return &input, nil
}

// ValidateDomain checks a public DNS name such as app.example.com.
func ValidateDomain(name string) error {
	if len(name) > 253 || !strings.Contains(name, ".") {
		return errors.New("use a fully qualified name such as app.example.com")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("each label must be 1–63 letters, digits or hyphens")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("use lowercase letters, digits, hyphens and dots")
			}
		}
	}
	return nil
}
