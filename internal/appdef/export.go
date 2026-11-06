package appdef

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/platform"
)

// Export builds one deterministic, self-contained definition for the selected
// environment. It includes supported desired state only: revision source,
// runtime, process formation, plain config, secret NAMES, domain bindings and
// resource declarations. Observed health, releases, credentials and TLS state
// are never exported. A resource that the revision depends on must still have
// an inventory definition; it is refused rather than silently dropped.
func Export(app, environment, buildNode string, revision *platform.EnvironmentRevision, domains []platform.DomainBinding, resources []platform.Resource) (string, error) {
	if err := platform.ValidateName(app); err != nil {
		return "", fmt.Errorf("app: %w", err)
	}
	if err := platform.ValidateName(environment); err != nil {
		return "", fmt.Errorf("environment: %w", err)
	}
	if revision == nil {
		return "", errors.New("the environment has no current revision; nothing is exportable")
	}
	if revision.AutoDeploy {
		return "", errors.New("auto_deploy is enabled and is not representable in this definition version")
	}
	if revision.WebhookSecretName != "" {
		return "", errors.New("webhook_secret_name is set and is not representable in this definition version")
	}
	if !revision.Retention.IsZero() {
		return "", errors.New("the environment has a retention override and is not representable in this definition version")
	}
	// Resource declarations are exported from the explicit inventory, never
	// inferred from a revision name alone. A resource that no longer exists is
	// an error: silently dropping a dependency would change the environment.
	byName := make(map[string]platform.Resource, len(resources))
	for _, resource := range resources {
		if resource.App != "" && resource.App != app {
			return "", fmt.Errorf("resource %q belongs to app %q, not %q", resource.Name, resource.App, app)
		}
		if resource.Environment != "" && resource.Environment != environment {
			return "", fmt.Errorf("resource %q belongs to environment %q, not %q", resource.Name, resource.Environment, environment)
		}
		byName[resource.Name] = resource
	}
	for _, name := range revision.Resources {
		if _, ok := byName[name]; !ok {
			return "", fmt.Errorf("the revision depends on resource %q but its definition is unavailable", name)
		}
	}
	state := platform.CloneRevisionState(revision.RevisionState)
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return "", err
	}
	if state.Source.Kind == "" {
		return "", errors.New("the environment has no source binding; add one before exporting")
	}

	sortedDomains := append([]platform.DomainBinding(nil), domains...)
	sort.Slice(sortedDomains, func(i, j int) bool { return sortedDomains[i].Domain < sortedDomains[j].Domain })
	primary := 0
	seen := map[string]bool{}
	for index := range sortedDomains {
		binding := &sortedDomains[index]
		if binding.App != "" && binding.App != app {
			return "", fmt.Errorf("domain %q belongs to app %q, not %q", binding.Domain, binding.App, app)
		}
		if binding.Environment != "" && binding.Environment != environment {
			return "", fmt.Errorf("domain %q belongs to environment %q, not %q", binding.Domain, binding.Environment, environment)
		}
		if err := platform.ValidateDomain(binding.Domain); err != nil {
			return "", fmt.Errorf("domain %q: %w", binding.Domain, err)
		}
		if seen[binding.Domain] {
			return "", fmt.Errorf("duplicate domain %q", binding.Domain)
		}
		seen[binding.Domain] = true
		if binding.Primary {
			primary++
		}
	}
	if primary > 1 {
		return "", errors.New("more than one domain is marked primary")
	}
	if len(sortedDomains) > 0 && primary == 0 {
		return "", errors.New("the environment has domain bindings but no primary domain")
	}

	processes := append([]platform.ProcessSpec(nil), state.Processes...)
	sort.Slice(processes, func(i, j int) bool { return processes[i].Name < processes[j].Name })

	var out strings.Builder
	fmt.Fprintf(&out, "version = %d\n", Version)
	fmt.Fprintf(&out, "app = %s\n", quote(app))
	fmt.Fprintf(&out, "environment = %s\n", quote(environment))
	fmt.Fprintf(&out, "node = %s\n", quote(state.Runtime.Node))
	fmt.Fprintf(&out, "build_node = %s\n", quote(buildNode))
	fmt.Fprintf(&out, "secret_names = %s\n", stringArray(state.SecretNames))
	if len(processes) == 0 {
		out.WriteString("processes = []\n")
	}
	if len(sortedDomains) == 0 {
		out.WriteString("domains = []\n")
	}

	out.WriteString("\n[source]\n")
	out.WriteString("kind = " + quote(state.Source.Kind) + "\n")
	switch state.Source.Kind {
	case "git":
		fmt.Fprintf(&out, "url = %s\n", quote(state.Source.URL))
		fmt.Fprintf(&out, "ref = %s\n", quote(state.Source.Ref))
		fmt.Fprintf(&out, "credential_secret = %s\n", quote(state.Source.CredentialSecretName))
		fmt.Fprintf(&out, "context_path = %s\n", quote(state.Source.ContextPath))
		fmt.Fprintf(&out, "builder = %s\n", quote(state.Source.Builder))
	case "image":
		fmt.Fprintf(&out, "image = %s\n", quote(state.Source.Image))
	}
	if state.Source.Kind == "git" && len(state.Source.BuildEnv) == 0 {
		out.WriteString("\n[source.build_env]\n")
	} else if len(state.Source.BuildEnv) > 0 {
		out.WriteString("\n[source.build_env]\n")
		for _, key := range sortedKeys(state.Source.BuildEnv) {
			fmt.Fprintf(&out, "%s = %s\n", tomlKey(key), quote(state.Source.BuildEnv[key]))
		}
	}

	out.WriteString("\n[runtime]\n")
	fmt.Fprintf(&out, "health_type = %s\n", quote(state.Runtime.HealthType))
	fmt.Fprintf(&out, "health_path = %s\n", quote(state.Runtime.HealthPath))
	fmt.Fprintf(&out, "health_timeout_seconds = %d\n", state.Runtime.HealthTimeoutSeconds)
	if state.Runtime.ReleaseCommand != nil {
		fmt.Fprintf(&out, "release_command = %s\n", stringArray(state.Runtime.ReleaseCommand.Argv))
	} else {
		out.WriteString("release_command = []\n")
	}

	out.WriteString("\n[config]\n")
	for _, key := range sortedKeys(state.Config) {
		fmt.Fprintf(&out, "%s = %s\n", tomlKey(key), quote(state.Config[key]))
	}

	if len(processes) > 0 {
		out.WriteString("\n")
		for _, process := range processes {
			out.WriteString("[[processes]]\n")
			fmt.Fprintf(&out, "name = %s\n", quote(process.Name))
			fmt.Fprintf(&out, "kind = %s\n", quote(process.Kind))
			if process.Command != "" {
				fmt.Fprintf(&out, "command = %s\n", quote(process.Command))
			}
			if len(process.Args) > 0 {
				fmt.Fprintf(&out, "args = %s\n", stringArray(process.Args))
			}
			fmt.Fprintf(&out, "replicas = %d\n", process.Replicas)
			if process.ContainerPort > 0 {
				fmt.Fprintf(&out, "container_port = %d\n", process.ContainerPort)
			}
			out.WriteString("\n")
		}
	}

	attached := make(map[string]bool, len(revision.Resources))
	for _, name := range revision.Resources {
		attached[name] = true
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		resource := byName[name]
		out.WriteString("\n[[resources]]\n")
		fmt.Fprintf(&out, "name = %s\n", quote(resource.Name))
		fmt.Fprintf(&out, "image = %s\n", quote(resource.Spec.Image))
		if resource.Spec.Port > 0 {
			fmt.Fprintf(&out, "port = %d\n", resource.Spec.Port)
		}
		if !attached[resource.Name] {
			out.WriteString("attach = false\n")
		}
		if len(resource.Spec.Command) > 0 {
			fmt.Fprintf(&out, "command = %s\n", stringArray(resource.Spec.Command))
		}
		if len(resource.Spec.Env) > 0 {
			out.WriteString("\n[resources.env]\n")
			for _, key := range sortedKeys(resource.Spec.Env) {
				fmt.Fprintf(&out, "%s = %s\n", tomlKey(key), quote(resource.Spec.Env[key]))
			}
		}
		if len(resource.Spec.SecretEnv) > 0 {
			out.WriteString("\n[resources.secret_env]\n")
			for _, key := range sortedKeys(resource.Spec.SecretEnv) {
				fmt.Fprintf(&out, "%s = %s\n", tomlKey(key), quote(resource.Spec.SecretEnv[key]))
			}
		}
		out.WriteString("\n[resources.health]\n")
		healthType := resource.Spec.Health.Type
		if healthType == "" {
			healthType = "exec"
		}
		fmt.Fprintf(&out, "type = %s\n", quote(healthType))
		fmt.Fprintf(&out, "command = %s\n", stringArray(resource.Spec.Health.Command))
		if resource.Spec.Health.TimeoutSeconds > 0 {
			fmt.Fprintf(&out, "timeout_seconds = %d\n", resource.Spec.Health.TimeoutSeconds)
		}
		if resource.Spec.Health.IntervalMS > 0 {
			fmt.Fprintf(&out, "interval_ms = %d\n", resource.Spec.Health.IntervalMS)
		}
		for _, mount := range resource.Spec.Mounts {
			out.WriteString("\n[[resources.mounts]]\n")
			fmt.Fprintf(&out, "name = %s\n", quote(mount.Name))
			fmt.Fprintf(&out, "destination = %s\n", quote(mount.Destination))
			if mount.ReadOnly {
				out.WriteString("read_only = true\n")
			}
		}
		out.WriteString("\n")
	}

	for index, binding := range sortedDomains {
		if index == 0 {
			out.WriteString("\n")
		}
		out.WriteString("[[domains]]\n")
		fmt.Fprintf(&out, "name = %s\n", quote(binding.Domain))
		fmt.Fprintf(&out, "primary = %t\n", binding.Primary)
		if binding.CheckPath != "" {
			fmt.Fprintf(&out, "check_path = %s\n", quote(binding.CheckPath))
		}
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n") + "\n", nil
}

// currentBuildNode is not part of EnvironmentRevision; it lives on the
// environment row, so the caller supplies it in the build_node field.

func quote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&out, `\u%04X`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

func stringArray(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, quote(value))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func tomlKey(key string) string {
	for _, r := range key {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return quote(key)
		}
	}
	if key == "" {
		return quote(key)
	}
	return key
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// UpdatePatch maps the resolved definition to the existing partial update
// action. A definition is the complete supported desired state: absent
// collections and maps are empty, so a removed process, config key, secret name
// or build environment is a detected change rather than silently preserved
// server state. Server-managed state that this schema does not own (auto-deploy,
// webhook routing and retention) stays untouched.
func (r *Resolved) UpdatePatch(current platform.Environment, currentDomains []platform.DomainBinding) (platform.EnvironmentUpdateInput, []string, []string, error) {
	patch := platform.EnvironmentUpdateInput{App: current.App, Environment: current.Name, ExpectedRevisionID: current.RevisionID}
	base := currentRevisionState(current)
	if err := platform.NormalizeRevisionState(&base); err != nil {
		return patch, nil, nil, err
	}
	if !r.NodeSet {
		return patch, nil, nil, errors.New("node is required for an update definition; export an existing environment to get a complete file")
	}
	if r.Source == nil {
		return patch, nil, nil, errors.New("source is required for an update definition; export an existing environment to get a complete file")
	}
	if err := currentDomainsMatch(current, currentDomains); err != nil {
		return patch, nil, nil, err
	}

	desired := platform.RevisionState{
		Source:            sourceBinding(r.Source),
		Runtime:           runtimeFromDefinition(r.Runtime, r.Node),
		Processes:         append([]platform.ProcessSpec(nil), r.Processes...),
		Config:            cloneStringMap(r.Config),
		SecretNames:       append([]string(nil), r.SecretNames...),
		Resources:         resourceAttachments(r),
		AutoDeploy:        base.AutoDeploy,
		WebhookSecretName: base.WebhookSecretName,
		Retention:         base.Retention.Clone(),
	}
	if desired.Source.Kind == "" {
		return patch, nil, nil, errors.New("source.kind is required for an update definition")
	}
	if err := platform.NormalizeRevisionState(&desired); err != nil {
		return patch, nil, nil, err
	}

	changed := make([]string, 0, 8)
	diff := make([]string, 0, 16)
	if desired.Runtime.Node != current.Node {
		node := r.Node
		patch.Node = &node
		changed = append(changed, "node")
		diff = append(diff, "node: "+display(current.Node)+" -> "+display(node))
	}
	if r.BuildNode != current.BuildNode {
		buildNode := r.BuildNode
		patch.BuildNode = &buildNode
		changed = append(changed, "build_node")
		diff = append(diff, "build_node: "+display(current.BuildNode)+" -> "+display(buildNode))
	}
	if !sourceEqual(base.Source, desired.Source) {
		source := desired.Source
		patch.Source = &source
		changed = append(changed, "source")
		diff = append(diff, sourceDiff(base.Source, desired.Source)...)
	}
	if !runtimeEqual(base.Runtime, desired.Runtime) {
		runtime := desired.Runtime
		patch.Runtime = &runtime
		changed = append(changed, "runtime")
		diff = append(diff, runtimeDiff(base.Runtime, desired.Runtime)...)
	}
	if !processesEqual(base.Processes, desired.Processes) {
		processes := append([]platform.ProcessSpec(nil), desired.Processes...)
		patch.Processes = &processes
		changed = append(changed, "processes")
		diff = append(diff, processDiff(base.Processes, desired.Processes)...)
	}
	if !stringMapEqual(base.Config, desired.Config) {
		config := cloneStringMap(desired.Config)
		patch.Config = &config
		changed = append(changed, "config")
		diff = append(diff, stringMapDiff("config", base.Config, desired.Config)...)
	}
	if !stringSliceEqual(base.SecretNames, desired.SecretNames) {
		names := append([]string(nil), desired.SecretNames...)
		patch.SecretNames = &names
		changed = append(changed, "secret_names")
		diff = append(diff, stringSliceDiff("secret_names", base.SecretNames, desired.SecretNames)...)
	}
	if !stringSliceEqual(base.Resources, desired.Resources) {
		names := append([]string(nil), desired.Resources...)
		patch.Resources = &names
		changed = append(changed, "resources")
		diff = append(diff, stringSliceDiff("resources", base.Resources, desired.Resources)...)
	}
	return patch, changed, diff, nil
}

func resourceAttachments(r *Resolved) []string {
	names, _ := r.ResourceAttachments()
	return names
}

func runtimeFromDefinition(runtime *Runtime, node string) platform.RuntimeSpec {
	out := platform.RuntimeSpec{
		Node: node, HealthType: "http", HealthPath: platform.DefaultHealthPath,
		HealthTimeoutSeconds: platform.DefaultHealthTimeout,
	}
	if runtime == nil {
		return out
	}
	if runtime.HealthType != nil {
		out.HealthType = *runtime.HealthType
	}
	if runtime.HealthPath != nil {
		out.HealthPath = *runtime.HealthPath
	}
	if runtime.HealthTimeoutSeconds != nil {
		out.HealthTimeoutSeconds = *runtime.HealthTimeoutSeconds
	}
	if runtime.ReleaseCommand != nil && len(*runtime.ReleaseCommand) > 0 {
		out.ReleaseCommand = &platform.CommandSpec{Argv: append([]string(nil), (*runtime.ReleaseCommand)...)}
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

func currentRevisionState(current platform.Environment) platform.RevisionState {
	if current.Revision != nil {
		return platform.CloneRevisionState(current.Revision.RevisionState)
	}
	return platform.RevisionState{
		Runtime: platform.RuntimeSpec{
			Node: current.Node, HealthType: current.Health.Type, HealthPath: current.Health.Path,
			HealthTimeoutSeconds: current.Health.TimeoutSeconds,
		},
		Processes: append([]platform.ProcessSpec(nil), current.Processes...),
		Config:    current.Env,
	}
}

// currentDomainsMatch refuses an ambiguous current state before the definition
// is compared with domain inventory. An omitted definition domain must never
// look like a match just because the caller supplied no bindings; the
// environment row's own bindings are part of the comparison.
func currentDomainsMatch(current platform.Environment, bindings []platform.DomainBinding) error {
	currentByName := make(map[string]platform.DomainBinding, len(bindings))
	for _, binding := range bindings {
		currentByName[binding.Domain] = binding
	}
	rowByName := make(map[string]bool, len(current.Domains))
	for _, name := range current.Domains {
		rowByName[name] = true
		if _, ok := currentByName[name]; !ok {
			return fmt.Errorf("domains: current binding for %q is unavailable; refresh the environment and retry", name)
		}
	}
	if len(current.Domains) > 0 {
		for _, binding := range bindings {
			if !rowByName[binding.Domain] {
				return fmt.Errorf("domains: binding %q is not in the environment's current domain list; refresh the environment and retry", binding.Domain)
			}
		}
	}
	if current.PrimaryDomain != "" {
		binding, ok := currentByName[current.PrimaryDomain]
		if !ok || !binding.Primary {
			return errors.New("domains: the current primary domain binding is unavailable; refresh the environment and retry")
		}
	}
	return nil
}

func sourceEqual(a, b platform.SourceBinding) bool {
	if a.Kind != b.Kind || a.URL != b.URL || a.Ref != b.Ref ||
		a.CredentialSecretName != b.CredentialSecretName || a.ContextPath != b.ContextPath ||
		a.Builder != b.Builder || a.Image != b.Image || len(a.BuildEnv) != len(b.BuildEnv) {
		return false
	}
	for key, value := range a.BuildEnv {
		if b.BuildEnv[key] != value {
			return false
		}
	}
	return true
}

func runtimeEqual(a, b platform.RuntimeSpec) bool {
	if a.Node != b.Node || a.HealthType != b.HealthType || a.HealthPath != b.HealthPath ||
		a.HealthTimeoutSeconds != b.HealthTimeoutSeconds {
		return false
	}
	if a.ReleaseCommand == nil || b.ReleaseCommand == nil {
		return a.ReleaseCommand == b.ReleaseCommand
	}
	return stringSliceEqual(a.ReleaseCommand.Argv, b.ReleaseCommand.Argv)
}

func processesEqual(a, b []platform.ProcessSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index].Name != b[index].Name || a[index].Kind != b[index].Kind ||
			a[index].Command != b[index].Command || a[index].Replicas != b[index].Replicas ||
			a[index].ContainerPort != b[index].ContainerPort ||
			!stringSliceEqual(a[index].Args, b[index].Args) {
			return false
		}
	}
	return true
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func sourceDiff(old, new platform.SourceBinding) []string {
	out := []string{}
	add := func(name, before, after string) {
		if before != after {
			out = append(out, "source."+name+": "+display(before)+" -> "+display(after))
		}
	}
	add("kind", old.Kind, new.Kind)
	add("url", old.URL, new.URL)
	add("ref", old.Ref, new.Ref)
	add("credential_secret", old.CredentialSecretName, new.CredentialSecretName)
	add("context_path", old.ContextPath, new.ContextPath)
	add("builder", old.Builder, new.Builder)
	add("image", old.Image, new.Image)
	if !stringMapEqual(old.BuildEnv, new.BuildEnv) {
		out = append(out, "source.build_env changed")
	}
	return out
}

func runtimeDiff(old, new platform.RuntimeSpec) []string {
	out := []string{}
	add := func(name, before, after string) {
		if before != after {
			out = append(out, "runtime."+name+": "+display(before)+" -> "+display(after))
		}
	}
	add("health_type", old.HealthType, new.HealthType)
	add("health_path", old.HealthPath, new.HealthPath)
	add("health_timeout_seconds", strconv.Itoa(old.HealthTimeoutSeconds), strconv.Itoa(new.HealthTimeoutSeconds))
	oldCommand, newCommand := "", ""
	if old.ReleaseCommand != nil {
		oldCommand = strings.Join(old.ReleaseCommand.Argv, " ")
	}
	if new.ReleaseCommand != nil {
		newCommand = strings.Join(new.ReleaseCommand.Argv, " ")
	}
	add("release_command", oldCommand, newCommand)
	return out
}

func processDiff(old, new []platform.ProcessSpec) []string {
	oldByName := map[string]platform.ProcessSpec{}
	for _, process := range old {
		oldByName[process.Name] = process
	}
	newByName := map[string]platform.ProcessSpec{}
	for _, process := range new {
		newByName[process.Name] = process
	}
	out := []string{}
	for _, process := range old {
		if _, ok := newByName[process.Name]; !ok {
			out = append(out, "process "+process.Name+": removed")
		}
	}
	for _, process := range new {
		previous, ok := oldByName[process.Name]
		if !ok {
			out = append(out, "process "+process.Name+": added")
			continue
		}
		if !processesEqual([]platform.ProcessSpec{previous}, []platform.ProcessSpec{process}) {
			out = append(out, "process "+process.Name+": changed")
		}
	}
	return out
}

func stringMapDiff(label string, old, new map[string]string) []string {
	out := []string{}
	for _, key := range sortedKeys(old) {
		if value, ok := new[key]; !ok {
			out = append(out, label+"."+key+": unset")
		} else if value != old[key] {
			out = append(out, label+"."+key+": "+old[key]+" -> "+value)
		}
	}
	for _, key := range sortedKeys(new) {
		if _, ok := old[key]; !ok {
			out = append(out, label+"."+key+": set")
		}
	}
	return out
}

func stringSliceDiff(label string, old, new []string) []string {
	out := []string{}
	for _, value := range old {
		if !contains(new, value) {
			out = append(out, label+"."+value+": removed")
		}
	}
	for _, value := range new {
		if !contains(old, value) {
			out = append(out, label+"."+value+": added")
		}
	}
	return out
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func display(value string) string {
	if value == "" {
		return "(empty)"
	}
	return value
}
