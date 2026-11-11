// Package appdef parses and resolves the opt-in eregion.toml app definition.
//
// The file is input only. PostgreSQL environment revisions remain the
// authoritative desired state, and nothing here writes a store, reads secret
// values or talks to a node. Every supported field maps to the existing
// platform EnvironmentSpec/EnvironmentUpdateInput types so the CLI and node
// validate the same canonical action.
package appdef

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/buildpacks"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/pelletier/go-toml/v2"
)

// Version is the only accepted file schema version.
const Version = 1

// Definition is one strict eregion.toml document. Pointer fields distinguish an
// absent value (inherit/default) from an explicitly empty value (clear).
type Definition struct {
	Version      *int                   `toml:"version"`
	App          string                 `toml:"app"`
	Node         *string                `toml:"node"`
	BuildNode    *string                `toml:"build_node"`
	Source       *Source                `toml:"source"`
	Runtime      *Runtime               `toml:"runtime"`
	Processes    *[]Process             `toml:"processes"`
	Config       *map[string]string     `toml:"config"`
	SecretNames  *[]string              `toml:"secret_names"`
	Domains      *[]Domain              `toml:"domains"`
	Resources    *[]Resource            `toml:"resources"`
	Environments map[string]Environment `toml:"environments"`

	invalidResources error
}

// Environment is one named override over the shared document fields. It cannot
// change the app or environment identity.
type Environment struct {
	Node        *string            `toml:"node"`
	BuildNode   *string            `toml:"build_node"`
	Source      *Source            `toml:"source"`
	Runtime     *Runtime           `toml:"runtime"`
	Processes   *[]Process         `toml:"processes"`
	Config      *map[string]string `toml:"config"`
	SecretNames *[]string          `toml:"secret_names"`
	Domains     *[]Domain          `toml:"domains"`
	Resources   *[]Resource        `toml:"resources"`
}

// Source mirrors platform.SourceBinding. CredentialSecret is a vault NAME; a
// value is never part of the file.
type Source struct {
	Kind             *string                `toml:"kind"`
	URL              *string                `toml:"url"`
	Ref              *string                `toml:"ref"`
	CredentialSecret *string                `toml:"credential_secret"`
	ContextPath      *string                `toml:"context_path"`
	Builder          *string                `toml:"builder"`
	BuildEnv         *map[string]string     `toml:"build_env"`
	GoTargets        *[]buildpacks.GoTarget `toml:"go_targets"`
	Buildpacks       *BuildpackSettings     `toml:"buildpacks"`
	Image            *string                `toml:"image"`

	buildpacksSpecified bool
}

// The definition uses pointer-valued leaves so an environment override can
// distinguish an omitted setting from an explicit empty value.
type BuildpackSettings struct {
	Runtime  *string            `toml:"runtime"`
	Frontend *BuildpackFrontend `toml:"frontend"`
	Advanced *BuildpackAdvanced `toml:"advanced"`
}

type BuildpackFrontend struct {
	Runtime         *string   `toml:"runtime"`
	Directory       *string   `toml:"directory"`
	Scripts         *[]string `toml:"scripts"`
	KeepNodeRuntime *bool     `toml:"keep_node_runtime"`
}

type BuildpackAdvanced struct {
	GoVersion    *string `toml:"go_version"`
	GoBuildFlags *string `toml:"go_build_flags"`
	NodeVersion  *string `toml:"node_version"`
}

// Runtime mirrors platform.RuntimeSpec. ReleaseCommand is the direct argv of
// the one-off release command, never a shell line.
type Runtime struct {
	HealthType           *string   `toml:"health_type"`
	HealthPath           *string   `toml:"health_path"`
	HealthTimeoutSeconds *int      `toml:"health_timeout_seconds"`
	ReleaseCommand       *[]string `toml:"release_command"`
}

// Process is one process formation entry.
type Process struct {
	Name          string   `toml:"name"`
	Kind          string   `toml:"kind"`
	Command       string   `toml:"command"`
	Args          []string `toml:"args"`
	Replicas      int      `toml:"replicas"`
	ContainerPort int      `toml:"container_port"`
}

// Domain is one public hostname binding. CheckPath is an optional per-binding
// observation path; empty inherits the environment health path.
type Domain struct {
	Name      string `toml:"name"`
	Primary   bool   `toml:"primary"`
	CheckPath string `toml:"check_path"`
}

// Resource is one environment-owned generic Docker dependency. Values never
// enter the file: SecretEnv maps container variables to vault secret NAMES.
// Applying the file declares and provisions these resources explicitly.
type Resource struct {
	Name      string            `toml:"name"`
	Image     string            `toml:"image"`
	Port      int               `toml:"port"`
	Env       map[string]string `toml:"env"`
	SecretEnv map[string]string `toml:"secret_env"`
	Mounts    []ResourceMount   `toml:"mounts"`
	Health    ResourceHealth    `toml:"health"`
	Command   []string          `toml:"command"`
	Attach    *bool             `toml:"attach"`
}

type ResourceMount struct {
	Name        string `toml:"name"`
	Destination string `toml:"destination"`
	ReadOnly    bool   `toml:"read_only"`
}

type ResourceHealth struct {
	Type           string   `toml:"type"`
	Command        []string `toml:"command"`
	TimeoutSeconds int      `toml:"timeout_seconds"`
	IntervalMS     int      `toml:"interval_ms"`
}

// Resolved is the selected environment after shared defaults and one named
// override are applied. Absent collections remain unset so an update leaves
// the corresponding server state alone; an explicit empty collection is set
// and clears it.
type Resolved struct {
	App          string
	Environment  string
	Node         string
	NodeSet      bool
	BuildNode    string
	BuildNodeSet bool

	Source *Source

	Runtime *Runtime

	Processes    []platform.ProcessSpec
	ProcessesSet bool

	Config    map[string]string
	ConfigSet bool

	SecretNames    []string
	SecretNamesSet bool

	Domains    []Domain
	DomainsSet bool

	Resources    []Resource
	ResourcesSet bool
}

// Parse decodes a strict definition and validates its schema version.
func Parse(raw []byte) (Definition, error) {
	var present map[string]any
	if err := toml.Unmarshal(raw, &present); err != nil {
		return Definition{}, decodeError(err)
	}
	if _, ok := present["inspection_only"]; ok {
		return Definition{}, errors.New("inspection-only snapshot is not an applicable definition; create or export an applicable definition and configure unsupported settings separately")
	}
	var definition Definition
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&definition); err != nil {
		return Definition{}, decodeError(err)
	}
	markPresence(&definition, present)
	if definition.Version == nil {
		return Definition{}, errors.New("version is required")
	}
	if *definition.Version != Version {
		return Definition{}, fmt.Errorf("version: unsupported schema %d; use version = %d", *definition.Version, Version)
	}
	if definition.invalidResources != nil {
		return Definition{}, definition.invalidResources
	}
	return definition, nil
}

// markPresence distinguishes an explicitly empty collection from an absent one.
// The decoder may leave a pointer nil for `key = []` and an empty table, so the
// document's key set is consulted after the strict structural decode.
func markPresence(definition *Definition, present map[string]any) {
	if hasKey(present, "node") && definition.Node == nil {
		definition.Node = ptrValue("")
	}
	if hasKey(present, "build_node") && definition.BuildNode == nil {
		definition.BuildNode = ptrValue("")
	}
	if hasKey(present, "source") {
		source := tableValue(present, "source")
		if definition.Source == nil {
			definition.Source = &Source{}
		}
		markSourcePresence(definition.Source, source)
	}
	if hasKey(present, "runtime") {
		runtime := tableValue(present, "runtime")
		if definition.Runtime == nil {
			definition.Runtime = &Runtime{}
		}
		markRuntimePresence(definition.Runtime, runtime)
	}
	if hasKey(present, "processes") && definition.Processes == nil {
		definition.Processes = &[]Process{}
	}
	if hasKey(present, "config") && definition.Config == nil {
		definition.Config = &map[string]string{}
	}
	if hasKey(present, "secret_names") && definition.SecretNames == nil {
		definition.SecretNames = &[]string{}
	}
	if hasKey(present, "domains") && definition.Domains == nil {
		definition.Domains = &[]Domain{}
	}
	if hasKey(present, "resources") && definition.Resources == nil {
		definition.Resources = &[]Resource{}
	}
	if definition.Resources != nil {
		if _, err := convertResources(*definition.Resources); err != nil {
			// Parse remains a schema check; Resolve repeats validation with
			// field-aware diagnostics. Record the failure here so a direct
			// Parse caller cannot accept an invalid declaration.
			definition.invalidResources = err
		}
	}
	if hasKey(present, "environments") {
		environments := tableValue(present, "environments")
		for name, raw := range environments {
			override, ok := definition.Environments[name]
			if !ok {
				override = Environment{}
			}
			table, _ := raw.(map[string]any)
			if hasKey(table, "node") && override.Node == nil {
				override.Node = ptrValue("")
			}
			if hasKey(table, "build_node") && override.BuildNode == nil {
				override.BuildNode = ptrValue("")
			}
			if hasKey(table, "source") {
				if override.Source == nil {
					override.Source = &Source{}
				}
				markSourcePresence(override.Source, tableValue(table, "source"))
			}
			if hasKey(table, "runtime") {
				if override.Runtime == nil {
					override.Runtime = &Runtime{}
				}
				markRuntimePresence(override.Runtime, tableValue(table, "runtime"))
			}
			if hasKey(table, "processes") && override.Processes == nil {
				override.Processes = &[]Process{}
			}
			if hasKey(table, "config") && override.Config == nil {
				override.Config = &map[string]string{}
			}
			if hasKey(table, "secret_names") && override.SecretNames == nil {
				override.SecretNames = &[]string{}
			}
			if hasKey(table, "domains") && override.Domains == nil {
				override.Domains = &[]Domain{}
			}
			if hasKey(table, "resources") && override.Resources == nil {
				override.Resources = &[]Resource{}
			}
			if definition.Environments == nil {
				definition.Environments = map[string]Environment{}
			}
			definition.Environments[name] = override
		}
	}
}

func markSourcePresence(source *Source, present map[string]any) {
	for key, target := range map[string]**string{
		"kind": &source.Kind, "url": &source.URL, "ref": &source.Ref,
		"credential_secret": &source.CredentialSecret, "context_path": &source.ContextPath,
		"builder": &source.Builder, "image": &source.Image,
	} {
		if hasKey(present, key) && *target == nil {
			*target = ptrValue("")
		}
	}
	if hasKey(present, "build_env") && source.BuildEnv == nil {
		source.BuildEnv = &map[string]string{}
	}
	if hasKey(present, "go_targets") && source.GoTargets == nil {
		source.GoTargets = &[]buildpacks.GoTarget{}
	}
	if hasKey(present, "buildpacks") {
		if source.Buildpacks == nil {
			source.Buildpacks = &BuildpackSettings{}
		}
		table := tableValue(present, "buildpacks")
		settings := source.Buildpacks
		if hasKey(table, "runtime") && settings.Runtime == nil {
			settings.Runtime = ptrValue("")
		}
		if hasKey(table, "frontend") {
			if settings.Frontend == nil {
				settings.Frontend = &BuildpackFrontend{}
			}
			frontend, fields := settings.Frontend, tableValue(table, "frontend")
			if hasKey(fields, "runtime") && frontend.Runtime == nil {
				frontend.Runtime = ptrValue("")
			}
			if hasKey(fields, "directory") && frontend.Directory == nil {
				frontend.Directory = ptrValue("")
			}
			if hasKey(fields, "scripts") && frontend.Scripts == nil {
				frontend.Scripts = &[]string{}
			}
			if hasKey(fields, "keep_node_runtime") && frontend.KeepNodeRuntime == nil {
				frontend.KeepNodeRuntime = ptrValue(false)
			}
		}
		if hasKey(table, "advanced") {
			if settings.Advanced == nil {
				settings.Advanced = &BuildpackAdvanced{}
			}
			advanced, fields := settings.Advanced, tableValue(table, "advanced")
			if hasKey(fields, "go_version") && advanced.GoVersion == nil {
				advanced.GoVersion = ptrValue("")
			}
			if hasKey(fields, "go_build_flags") && advanced.GoBuildFlags == nil {
				advanced.GoBuildFlags = ptrValue("")
			}
			if hasKey(fields, "node_version") && advanced.NodeVersion == nil {
				advanced.NodeVersion = ptrValue("")
			}
		}
	}
}

func markRuntimePresence(runtime *Runtime, present map[string]any) {
	if hasKey(present, "health_type") && runtime.HealthType == nil {
		runtime.HealthType = ptrValue("")
	}
	if hasKey(present, "health_path") && runtime.HealthPath == nil {
		runtime.HealthPath = ptrValue("")
	}
	if hasKey(present, "health_timeout_seconds") && runtime.HealthTimeoutSeconds == nil {
		runtime.HealthTimeoutSeconds = ptrValue(0)
	}
	if hasKey(present, "release_command") && runtime.ReleaseCommand == nil {
		runtime.ReleaseCommand = &[]string{}
	}
}

func hasKey(table map[string]any, key string) bool {
	_, ok := table[key]
	return ok
}

func tableValue(table map[string]any, key string) map[string]any {
	value, _ := table[key].(map[string]any)
	return value
}

func ptrValue[T any](value T) *T { return &value }

// Resolve applies the shared recipe first and the selected environment
// override second. The selected environment is the command target. A named
// override is optional: a PR or any other environment can be created from the
// shared recipe without first adding its name to the document.
func (d Definition) Resolve(environment string) (*Resolved, error) {
	if d.App != "" {
		if err := platform.ValidateName(d.App); err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
	}
	if environment != "" {
		if err := platform.ValidateName(environment); err != nil {
			return nil, fmt.Errorf("environment: %w", err)
		}
	}
	for name := range d.Environments {
		if err := platform.ValidateName(name); err != nil {
			return nil, fmt.Errorf("environments.%s: %w", name, err)
		}
	}

	resolved := &Resolved{App: d.App, Environment: environment}
	if err := resolved.apply(FileLayer{
		Node: d.Node, BuildNode: d.BuildNode, Source: d.Source, Runtime: d.Runtime,
		Processes: d.Processes, Config: d.Config, SecretNames: d.SecretNames, Domains: d.Domains, Resources: d.Resources,
	}); err != nil {
		return nil, err
	}
	if override, ok := d.Environments[environment]; ok && environment != "" {
		if err := resolved.apply(FileLayer{
			Node: override.Node, BuildNode: override.BuildNode, Source: override.Source, Runtime: override.Runtime,
			Processes: override.Processes, Config: override.Config, SecretNames: override.SecretNames, Domains: override.Domains, Resources: override.Resources,
		}); err != nil {
			return nil, fmt.Errorf("environments.%s: %w", environment, err)
		}
	}
	if resolved.NodeSet {
		if err := platform.ValidateName(resolved.Node); err != nil {
			return nil, fmt.Errorf("node: %w", err)
		}
	}
	if resolved.BuildNodeSet && resolved.BuildNode != "" {
		if err := platform.ValidateName(resolved.BuildNode); err != nil {
			return nil, fmt.Errorf("build_node: %w", err)
		}
	}
	return resolved, nil
}

// FileLayer is one shared or environment-specific set of fields.
type FileLayer struct {
	Node        *string
	BuildNode   *string
	Source      *Source
	Runtime     *Runtime
	Processes   *[]Process
	Config      *map[string]string
	SecretNames *[]string
	Domains     *[]Domain
	Resources   *[]Resource
}

func (r *Resolved) apply(layer FileLayer) error {
	if layer.Node != nil {
		r.Node, r.NodeSet = *layer.Node, true
	}
	if layer.BuildNode != nil {
		r.BuildNode, r.BuildNodeSet = *layer.BuildNode, true
	}
	if layer.Source != nil {
		if r.Source == nil {
			r.Source = &Source{}
		} else if layer.Source.Kind != nil && r.Source.Kind != nil && *layer.Source.Kind != *r.Source.Kind {
			// A kind change starts a fresh source so stale git/image fields
			// cannot leak into the new declaration.
			r.Source = &Source{}
		}
		mergeSource(r.Source, layer.Source)
	}
	if layer.Runtime != nil {
		if r.Runtime == nil {
			r.Runtime = &Runtime{}
		}
		mergeRuntime(r.Runtime, layer.Runtime)
	}
	if layer.Processes != nil {
		processes, err := convertProcesses(*layer.Processes)
		if err != nil {
			return err
		}
		r.Processes, r.ProcessesSet = processes, true
	}
	if layer.Config != nil {
		if r.Config == nil {
			r.Config = map[string]string{}
		}
		r.ConfigSet = true
		if len(*layer.Config) == 0 {
			r.Config = map[string]string{}
		} else {
			for key, value := range *layer.Config {
				r.Config[key] = value
			}
		}
	}
	if layer.SecretNames != nil {
		names, err := normalizeNames(*layer.SecretNames)
		if err != nil {
			return err
		}
		r.SecretNames, r.SecretNamesSet = names, true
	}
	if layer.Domains != nil {
		domains, err := convertDomains(*layer.Domains)
		if err != nil {
			return err
		}
		r.Domains, r.DomainsSet = domains, true
	}
	if layer.Resources != nil {
		resources, err := convertResources(*layer.Resources)
		if err != nil {
			return err
		}
		r.Resources, r.ResourcesSet = resources, true
	}
	if r.Source != nil {
		if err := validateSourceShape(r.Source); err != nil {
			return err
		}
	}
	if r.Runtime != nil {
		if err := validateRuntimeShape(r.Runtime); err != nil {
			return err
		}
	}
	return nil
}

func mergeSource(dst, src *Source) {
	if src.Kind != nil {
		dst.Kind = src.Kind
	}
	if src.URL != nil {
		dst.URL = src.URL
	}
	if src.Ref != nil {
		dst.Ref = src.Ref
	}
	if src.CredentialSecret != nil {
		dst.CredentialSecret = src.CredentialSecret
	}
	if src.ContextPath != nil {
		dst.ContextPath = src.ContextPath
	}
	if src.Builder != nil {
		dst.Builder = src.Builder
	}
	if src.Image != nil {
		dst.Image = src.Image
	}
	if src.BuildEnv != nil {
		if dst.BuildEnv == nil {
			dst.BuildEnv = &map[string]string{}
		}
		if len(*src.BuildEnv) == 0 {
			*dst.BuildEnv = map[string]string{}
		} else {
			for key, value := range *src.BuildEnv {
				(*dst.BuildEnv)[key] = value
			}
		}
	}
	if src.GoTargets != nil {
		targets := append([]buildpacks.GoTarget{}, (*src.GoTargets)...)
		dst.GoTargets = &targets
	}
	if src.Buildpacks != nil {
		mergeBuildpacks(dst, src.Buildpacks)
	}
}

func mergeBuildpacks(dst *Source, src *BuildpackSettings) {
	dst.buildpacksSpecified = true
	if src.Runtime == nil && src.Frontend == nil && src.Advanced == nil {
		dst.Buildpacks = nil
		return
	}
	if dst.Buildpacks == nil {
		dst.Buildpacks = &BuildpackSettings{}
	}
	out := dst.Buildpacks
	if src.Runtime != nil {
		out.Runtime = ptrValue(*src.Runtime)
	}
	if src.Frontend != nil {
		f := src.Frontend
		if f.Runtime == nil && f.Directory == nil && f.Scripts == nil && f.KeepNodeRuntime == nil {
			out.Frontend = nil
		} else {
			if out.Frontend == nil {
				out.Frontend = &BuildpackFrontend{}
			}
			if f.Runtime != nil {
				out.Frontend.Runtime = ptrValue(*f.Runtime)
			}
			if f.Directory != nil {
				out.Frontend.Directory = ptrValue(*f.Directory)
			}
			if f.Scripts != nil {
				values := append([]string{}, (*f.Scripts)...)
				out.Frontend.Scripts = &values
			}
			if f.KeepNodeRuntime != nil {
				out.Frontend.KeepNodeRuntime = ptrValue(*f.KeepNodeRuntime)
			}
		}
	}
	if src.Advanced != nil {
		a := src.Advanced
		if a.GoVersion == nil && a.GoBuildFlags == nil && a.NodeVersion == nil {
			out.Advanced = nil
		} else {
			if out.Advanced == nil {
				out.Advanced = &BuildpackAdvanced{}
			}
			if a.GoVersion != nil {
				out.Advanced.GoVersion = ptrValue(*a.GoVersion)
			}
			if a.GoBuildFlags != nil {
				out.Advanced.GoBuildFlags = ptrValue(*a.GoBuildFlags)
			}
			if a.NodeVersion != nil {
				out.Advanced.NodeVersion = ptrValue(*a.NodeVersion)
			}
		}
	}
}

func mergeRuntime(dst, src *Runtime) {
	if src.HealthType != nil {
		dst.HealthType = src.HealthType
	}
	if src.HealthPath != nil {
		dst.HealthPath = src.HealthPath
	}
	if src.HealthTimeoutSeconds != nil {
		dst.HealthTimeoutSeconds = src.HealthTimeoutSeconds
	}
	if src.ReleaseCommand != nil {
		command := append([]string(nil), (*src.ReleaseCommand)...)
		dst.ReleaseCommand = &command
	}
}

func validateSourceShape(source *Source) error {
	if source.Kind == nil {
		if source.URL != nil && strings.TrimSpace(*source.URL) != "" &&
			source.Image != nil && strings.TrimSpace(*source.Image) != "" {
			return errors.New("source: set only url or image, with a matching kind")
		}
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(*source.Kind)) {
	case "git":
		if source.URL == nil || strings.TrimSpace(*source.URL) == "" {
			return errors.New("source.url is required for a git source")
		}
		if source.Image != nil && strings.TrimSpace(*source.Image) != "" {
			return errors.New("source.image cannot be set for a git source")
		}
	case "image":
		if source.Image == nil || strings.TrimSpace(*source.Image) == "" {
			return errors.New("source.image is required for an image source")
		}
		if source.URL != nil && strings.TrimSpace(*source.URL) != "" {
			return errors.New("source.url cannot be set for an image source")
		}
		if source.Ref != nil && strings.TrimSpace(*source.Ref) != "" {
			return errors.New("source.ref cannot be set for an image source")
		}
		if source.CredentialSecret != nil && strings.TrimSpace(*source.CredentialSecret) != "" {
			return errors.New("source.credential_secret cannot be set for an image source")
		}
		if source.ContextPath != nil && strings.TrimSpace(*source.ContextPath) != "" {
			return errors.New("source.context_path cannot be set for an image source")
		}
		if source.Builder != nil && strings.TrimSpace(*source.Builder) != "" {
			return errors.New("source.builder cannot be set for an image source")
		}
		if source.BuildEnv != nil && len(*source.BuildEnv) > 0 {
			return errors.New("source.build_env cannot be set for an image source")
		}
		if source.GoTargets != nil || source.Buildpacks != nil || source.buildpacksSpecified {
			return errors.New("source.go_targets and source.buildpacks cannot be set for an image source")
		}
	default:
		return errors.New("source.kind: use git or image")
	}
	if source.Kind == nil || strings.EqualFold(strings.TrimSpace(*source.Kind), "git") {
		binding := sourceBinding(source)
		if _, err := buildpacks.NormalizeTargets(binding.GoTargets); err != nil {
			return fmt.Errorf("source.go_targets: %w", err)
		}
		if _, err := buildpacks.NormalizeSettings(binding.Buildpacks); err != nil {
			return fmt.Errorf("source.buildpacks: %w", err)
		}
	}
	return nil
}

func validateRuntimeShape(runtime *Runtime) error {
	if runtime.HealthTimeoutSeconds != nil && (*runtime.HealthTimeoutSeconds < 1 || *runtime.HealthTimeoutSeconds > 300) {
		return errors.New("runtime.health_timeout_seconds: use 1–300 seconds")
	}
	if runtime.ReleaseCommand != nil && len(*runtime.ReleaseCommand) > 128 {
		return errors.New("runtime.release_command: use at most 128 arguments")
	}
	return nil
}

func convertProcesses(values []Process) ([]platform.ProcessSpec, error) {
	if len(values) > 32 {
		return nil, errors.New("processes: use at most 32 processes")
	}
	processes := make([]platform.ProcessSpec, 0, len(values))
	for index, value := range values {
		processes = append(processes, platform.ProcessSpec{
			Name: value.Name, Kind: value.Kind, Command: value.Command,
			Args: append([]string(nil), value.Args...), Replicas: value.Replicas,
			ContainerPort: value.ContainerPort,
		})
		if value.Name == "" {
			return nil, fmt.Errorf("processes[%d].name is required", index)
		}
	}
	return processes, nil
}

func convertDomains(values []Domain) ([]Domain, error) {
	seen := map[string]bool{}
	primary := 0
	domains := make([]Domain, 0, len(values))
	for index, value := range values {
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value.Name), "."))
		if name == "" {
			return nil, fmt.Errorf("domains[%d].name is required", index)
		}
		if err := platform.ValidateDomain(name); err != nil {
			return nil, fmt.Errorf("domains[%d].name: %w", index, err)
		}
		if seen[name] {
			return nil, fmt.Errorf("domains[%d].name: duplicate domain %q", index, name)
		}
		if value.CheckPath != "" {
			if !validCheckPath(value.CheckPath) {
				return nil, fmt.Errorf("domains[%d].check_path: use an absolute path without whitespace, controls, query, or fragment", index)
			}
		}
		if value.Primary {
			primary++
		}
		seen[name] = true
		domains = append(domains, Domain{Name: name, Primary: value.Primary, CheckPath: value.CheckPath})
	}
	if len(domains) > 0 && primary != 1 {
		return nil, errors.New("domains: exactly one domain must be primary")
	}
	return domains, nil
}

// ResourceApplySpecs returns the canonical resource apply inputs for the
// declarations in this resolved definition. Attach is resolved through
// ResourceAttachments; an absent attach defaults to true.
func (r *Resolved) ResourceApplySpecs() ([]platform.ResourceApplyInput, error) {
	if !r.ResourcesSet {
		return nil, nil
	}
	if r.App == "" || r.Environment == "" {
		return nil, errors.New("app and environment are required before resource declarations can be applied")
	}
	out := make([]platform.ResourceApplyInput, 0, len(r.Resources))
	for _, declaration := range r.Resources {
		spec := platform.ResourceSpec{
			Image: declaration.Image, Port: declaration.Port,
			Env: cloneStringMap(declaration.Env), SecretEnv: cloneStringMap(declaration.SecretEnv),
			Command: append([]string(nil), declaration.Command...),
			Health: platform.ResourceHealthSpec{
				Type: declaration.Health.Type, Command: append([]string(nil), declaration.Health.Command...),
				TimeoutSeconds: declaration.Health.TimeoutSeconds, IntervalMS: declaration.Health.IntervalMS,
			},
		}
		for _, mount := range declaration.Mounts {
			spec.Mounts = append(spec.Mounts, platform.ResourceMount{Name: mount.Name, Destination: mount.Destination, ReadOnly: mount.ReadOnly})
		}
		normalized, err := platform.NormalizeResourceSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("resources.%s: %w", declaration.Name, err)
		}
		out = append(out, platform.ResourceApplyInput{
			App: r.App, Environment: r.Environment, Name: declaration.Name, ResourceSpec: normalized,
		})
	}
	return out, nil
}

// ResourceAttachments returns the resource names the selected revision must
// depend on. A declaration with attach = false is provisioned but not attached.
func (r *Resolved) ResourceAttachments() ([]string, bool) {
	if !r.ResourcesSet {
		return nil, false
	}
	names := make([]string, 0, len(r.Resources))
	for _, declaration := range r.Resources {
		if declaration.Attach != nil && !*declaration.Attach {
			continue
		}
		names = append(names, declaration.Name)
	}
	sort.Strings(names)
	return names, true
}

func convertResources(values []Resource) ([]Resource, error) {
	if len(values) > 32 {
		return nil, errors.New("resources: use at most 32 declarations")
	}
	seen := map[string]bool{}
	out := make([]Resource, 0, len(values))
	for index, value := range values {
		if err := platform.ValidateResourceName(value.Name); err != nil {
			return nil, fmt.Errorf("resources[%d].name: %w", index, err)
		}
		if seen[value.Name] {
			return nil, fmt.Errorf("resources[%d].name: duplicate resource %q", index, value.Name)
		}
		seen[value.Name] = true
		if len(value.Health.Command) == 0 {
			return nil, fmt.Errorf("resources[%d].health.command: provide an exec probe argv", index)
		}
		for key, name := range value.SecretEnv {
			if _, plain := value.Env[key]; plain {
				return nil, fmt.Errorf("resources[%d].secret_env.%s: a key cannot be both plain configuration and a secret reference", index, key)
			}
			if _, err := platform.NormalizeResourceSpec(platform.ResourceSpec{
				Image: nonEmpty(value.Image), SecretEnv: map[string]string{key: name},
				Health: platform.ResourceHealthSpec{Command: value.Health.Command},
			}); err != nil {
				return nil, fmt.Errorf("resources[%d].secret_env.%s: %w", index, key, err)
			}
		}
		if _, err := platform.NormalizeResourceSpec(platform.ResourceSpec{
			Image: value.Image, Port: value.Port, Env: value.Env, SecretEnv: value.SecretEnv,
			Mounts: appdefResourceMounts(value.Mounts), Command: value.Command,
			Health: platform.ResourceHealthSpec{
				Type: value.Health.Type, Command: value.Health.Command,
				TimeoutSeconds: value.Health.TimeoutSeconds, IntervalMS: value.Health.IntervalMS,
			},
		}); err != nil {
			return nil, fmt.Errorf("resources[%d]: %w", index, err)
		}
		out = append(out, value)
	}
	return out, nil
}

func nonEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "scratch"
	}
	return value
}

func appdefResourceMounts(values []ResourceMount) []platform.ResourceMount {
	out := make([]platform.ResourceMount, 0, len(values))
	for _, value := range values {
		out = append(out, platform.ResourceMount{Name: value.Name, Destination: value.Destination, ReadOnly: value.ReadOnly})
	}
	return out
}

func validCheckPath(path string) bool {
	if path == "" || !strings.HasPrefix(path, "/") || len(path) > 256 {
		return false
	}
	for _, r := range path {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\x00' || r == '?' || r == '#' {
			return false
		}
	}
	return true
}

func normalizeNames(values []string) ([]string, error) {
	if len(values) > 128 {
		return nil, errors.New("secret_names: use at most 128 names")
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(values))
	for index, value := range values {
		name := strings.TrimSpace(value)
		if name == "" {
			return nil, fmt.Errorf("secret_names[%d]: name is empty", index)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// CreateSpec maps a resolved definition to the existing first-environment
// action. It rejects states that the first revision cannot represent.
func (r *Resolved) CreateSpec(domainAddresses []string) (platform.EnvironmentSpec, error) {
	app := r.App
	if app == "" {
		return platform.EnvironmentSpec{}, errors.New("app is required")
	}
	if r.Environment == "" {
		return platform.EnvironmentSpec{}, errors.New("environment is required")
	}
	if err := platform.ValidateName(app); err != nil {
		return platform.EnvironmentSpec{}, fmt.Errorf("app: %w", err)
	}
	if err := platform.ValidateName(r.Environment); err != nil {
		return platform.EnvironmentSpec{}, fmt.Errorf("environment: %w", err)
	}
	node := r.Node
	if node == "" {
		node = "main"
	}
	if r.Source == nil {
		return platform.EnvironmentSpec{}, errors.New("source is required; add [source] with kind = \"git\" or kind = \"image\"")
	}
	source := sourceBinding(r.Source)
	runtime := platform.RuntimeSpec{
		Node: node, HealthType: "http", HealthPath: platform.DefaultHealthPath,
		HealthTimeoutSeconds: platform.DefaultHealthTimeout,
	}
	if r.Runtime != nil {
		if r.Runtime.HealthType != nil {
			runtime.HealthType = *r.Runtime.HealthType
		}
		if r.Runtime.HealthPath != nil {
			runtime.HealthPath = *r.Runtime.HealthPath
		}
		if r.Runtime.HealthTimeoutSeconds != nil {
			runtime.HealthTimeoutSeconds = *r.Runtime.HealthTimeoutSeconds
		}
		if r.Runtime.ReleaseCommand != nil {
			if len(*r.Runtime.ReleaseCommand) > 0 {
				runtime.ReleaseCommand = &platform.CommandSpec{Argv: append([]string(nil), (*r.Runtime.ReleaseCommand)...)}
			}
		}
	}
	processes := append([]platform.ProcessSpec(nil), r.Processes...)
	if !r.ProcessesSet {
		processes = []platform.ProcessSpec{{Name: "web", Kind: "web", Args: []string{}, Replicas: 1, ContainerPort: platform.DefaultContainerPort}}
	} else if len(processes) == 0 {
		return platform.EnvironmentSpec{}, errors.New("processes: an explicit empty formation cannot be used to create an environment; omit processes for the default web process")
	}
	containerPort := platform.DefaultContainerPort
	for _, process := range processes {
		if process.Kind == "web" && process.ContainerPort > 0 {
			containerPort = process.ContainerPort
			break
		}
	}
	if r.DomainsSet && len(r.Domains) > 0 {
		primary := 0
		for _, domain := range r.Domains {
			if domain.Primary {
				primary++
			}
		}
		if primary != 1 {
			return platform.EnvironmentSpec{}, errors.New("domains: mark exactly one domain as primary")
		}
	}
	config := map[string]string{}
	if r.ConfigSet {
		for key, value := range r.Config {
			config[key] = value
		}
	}
	secretNames := append([]string(nil), r.SecretNames...)
	if r.Source.CredentialSecret != nil && *r.Source.CredentialSecret != "" {
		for _, name := range secretNames {
			if name == *r.Source.CredentialSecret {
				return platform.EnvironmentSpec{}, errors.New("secret_names: the git read credential is not injected into the application")
			}
		}
	}
	domains := make([]platform.DomainDeclaration, 0, len(r.Domains))
	for _, domain := range r.Domains {
		domains = append(domains, platform.DomainDeclaration{
			Domain: domain.Name, Primary: domain.Primary, CheckPath: domain.CheckPath,
		})
	}
	resourceNames, _ := r.ResourceAttachments()
	spec := platform.EnvironmentSpec{
		Name: r.Environment, Node: node, BuildNode: r.BuildNode,
		ContainerPort: containerPort, Bind: platform.DefaultBind,
		Health: platform.HealthInput{
			Type: runtime.HealthType, Path: runtime.HealthPath, TimeoutSeconds: runtime.HealthTimeoutSeconds,
		},
		Source: source, Runtime: runtime, Processes: processes, Config: config,
		SecretNames: secretNames, Resources: resourceNames, Domains: domains,
	}
	if len(domains) > 0 {
		spec.DomainAddresses = append([]string(nil), domainAddresses...)
	}
	state := platform.RevisionState{
		Source: spec.Source, Runtime: spec.Runtime, Processes: spec.Processes,
		Config: spec.Config, SecretNames: spec.SecretNames,
	}
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return platform.EnvironmentSpec{}, err
	}
	spec.Source, spec.Runtime, spec.Processes = state.Source, state.Runtime, state.Processes
	spec.Config, spec.SecretNames = state.Config, state.SecretNames
	if err := validateDomainBinding(spec.Domain, spec.DomainAddresses); err != nil {
		return platform.EnvironmentSpec{}, err
	}
	return spec, nil
}

func validateDomainBinding(name string, addresses []string) error {
	if name != "" {
		if err := platform.ValidateDomain(name); err != nil {
			return fmt.Errorf("domains[0].name: %w", err)
		}
	}
	for index, value := range addresses {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("domains[0]: address %d is empty", index)
		}
	}
	return nil
}

func sourceBinding(source *Source) platform.SourceBinding {
	binding := platform.SourceBinding{}
	if source.Kind != nil {
		binding.Kind = strings.ToLower(strings.TrimSpace(*source.Kind))
	}
	if source.URL != nil {
		binding.URL = *source.URL
	}
	if source.Ref != nil {
		binding.Ref = *source.Ref
	}
	if source.CredentialSecret != nil {
		binding.CredentialSecretName = *source.CredentialSecret
	}
	if source.ContextPath != nil {
		binding.ContextPath = *source.ContextPath
	}
	if source.Builder != nil {
		binding.Builder = *source.Builder
	}
	if source.Image != nil {
		binding.Image = *source.Image
	}
	if source.BuildEnv != nil {
		binding.BuildEnv = make(map[string]string, len(*source.BuildEnv))
		for key, value := range *source.BuildEnv {
			binding.BuildEnv[key] = value
		}
	}
	if source.GoTargets != nil {
		binding.GoTargets = append([]buildpacks.GoTarget{}, (*source.GoTargets)...)
	}
	if source.Buildpacks != nil {
		s := source.Buildpacks
		settings := &buildpacks.Settings{}
		if s.Runtime != nil {
			settings.Runtime = *s.Runtime
		}
		if s.Frontend != nil {
			f := s.Frontend
			settings.Frontend = &buildpacks.Frontend{}
			if f.Runtime != nil {
				settings.Frontend.Runtime = *f.Runtime
			}
			if f.Directory != nil {
				settings.Frontend.Directory = *f.Directory
			}
			if f.Scripts != nil {
				settings.Frontend.Scripts = append([]string{}, (*f.Scripts)...)
			}
			if f.KeepNodeRuntime != nil {
				settings.Frontend.KeepNodeRuntime = *f.KeepNodeRuntime
			}
		}
		if s.Advanced != nil {
			a := s.Advanced
			settings.Advanced = &buildpacks.Advanced{}
			if a.GoVersion != nil {
				settings.Advanced.GoVersion = *a.GoVersion
			}
			if a.GoBuildFlags != nil {
				settings.Advanced.GoBuildFlags = *a.GoBuildFlags
			}
			if a.NodeVersion != nil {
				settings.Advanced.NodeVersion = *a.NodeVersion
			}
		}
		binding.Buildpacks = settings
	}
	return binding
}

func decodeError(err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		fields := make([]string, 0, len(strict.Errors))
		for index := range strict.Errors {
			item := &strict.Errors[index]
			key := strings.Join(item.Key(), ".")
			if key == "" {
				key = "(root)"
			}
			row, column := item.Position()
			fields = append(fields, fmt.Sprintf("%s at line %d, column %d", key, row, column))
		}
		sort.Strings(fields)
		return fmt.Errorf("unknown or unsupported field(s): %s", strings.Join(fields, "; "))
	}
	var decode *toml.DecodeError
	if errors.As(err, &decode) {
		row, column := decode.Position()
		return fmt.Errorf("invalid TOML at line %d, column %d: %s", row, column, decode.Error())
	}
	return fmt.Errorf("invalid TOML: %s", err)
}
