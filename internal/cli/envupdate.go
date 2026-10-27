package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// guidedEnvironmentUpdate is the result of the interactive env update
// interview: the patch, an optional new read credential, and the human-readable
// change list shown before the confirmation.
type guidedEnvironmentUpdate struct {
	Patch          platform.EnvironmentUpdateInput
	Credential     []byte
	CredentialName string
	Changed        []string
	Diff           []string
}

// guidedUpdateAnswers is the collected interview result before the patch and
// diff are built. Keeping this separate makes the patch/diff mapping testable.
type guidedUpdateAnswers struct {
	Node           string
	BuildNode      string
	Source         platform.SourceBinding
	Runtime        platform.RuntimeSpec
	Processes      []platform.ProcessSpec
	Credential     []byte
	CredentialName string
}

// envUpdateGuided asks for the current revision's editable fields, prefilled.
// It never submits or writes a vault value; the caller previews the patch and
// confirms before doing either.
func (a *app) envUpdateGuided(cmd *cobra.Command, current platform.Environment) (*guidedEnvironmentUpdate, error) {
	base := revisionStateOf(current)
	var credential []byte
	keep := false
	defer func() {
		if !keep {
			clearBytes(credential)
		}
	}()

	node, err := a.askNode(cmd, "Application node", "Runs the containers", current.Node, platform.RoleApplication, platform.CapabilityDocker, "", "")
	if err != nil {
		return nil, err
	}
	buildNode, err := a.askNode(cmd, "Builder node", "Runs the Buildpacks builds", current.BuildNode, platform.RoleBuilder, platform.CapabilityBuildpacks, "The application node", node)
	if err != nil {
		return nil, err
	}

	source, credentialValue, credentialName, err := a.guidedSource(base.Source)
	if err != nil {
		return nil, err
	}
	credential = credentialValue

	runtime, err := guidedRuntime(base.Runtime, node)
	if err != nil {
		return nil, err
	}
	processes, err := guidedProcesses(base.Processes)
	if err != nil {
		return nil, err
	}

	edit := buildGuidedUpdate(current, guidedUpdateAnswers{
		Node: node, BuildNode: buildNode, Source: source, Runtime: runtime,
		Processes: processes, Credential: credential, CredentialName: credentialName,
	})
	keep = true
	return edit, nil
}

// buildGuidedUpdate maps prefilled answers to a partial environment patch and
// the human-readable diff shown before confirmation.
func buildGuidedUpdate(current platform.Environment, answers guidedUpdateAnswers) *guidedEnvironmentUpdate {
	base := revisionStateOf(current)
	edit := &guidedEnvironmentUpdate{Credential: answers.Credential, CredentialName: answers.CredentialName}
	patch := platform.EnvironmentUpdateInput{App: current.App, Environment: current.Name}
	changed := make([]string, 0, 5)
	diff := make([]string, 0, 8)
	if answers.Node != current.Node {
		node := answers.Node
		patch.Node = &node
		changed = append(changed, "node")
		diff = append(diff, "node: "+displayValue(current.Node)+" → "+displayValue(answers.Node))
	}
	if answers.BuildNode != current.BuildNode {
		buildNode := answers.BuildNode
		patch.BuildNode = &buildNode
		changed = append(changed, "build_node")
		diff = append(diff, "build node: "+displayValue(current.BuildNode)+" → "+displayValue(answers.BuildNode))
	}
	if !sourceEqual(base.Source, answers.Source) {
		source := answers.Source
		patch.Source = &source
		changed = append(changed, "source")
		diff = append(diff, sourceDiff(base.Source, answers.Source)...)
	}
	if len(answers.Credential) > 0 {
		if !slices.Contains(changed, "source") {
			source := answers.Source
			patch.Source = &source
			changed = append(changed, "source")
		}
		diff = append(diff, "source.read_credential: replaced")
	}
	currentRuntime := base.Runtime
	currentRuntime.Node = answers.Node
	if !runtimeEqual(currentRuntime, answers.Runtime) {
		runtime := answers.Runtime
		patch.Runtime = &runtime
		changed = append(changed, "runtime")
		diff = append(diff, runtimeDiff(base.Runtime, answers.Runtime)...)
	}
	if !processSpecsEqual(base.Processes, answers.Processes) {
		processes := answers.Processes
		patch.Processes = &processes
		changed = append(changed, "processes")
		diff = append(diff, processSpecsDiff(base.Processes, answers.Processes)...)
	}
	edit.Patch, edit.Changed, edit.Diff = patch, changed, diff
	return edit
}

func revisionStateOf(current platform.Environment) platform.RevisionState {
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

func (a *app) guidedSource(base platform.SourceBinding) (source platform.SourceBinding, credential []byte, credentialName string, err error) {
	keep := false
	defer func() {
		if !keep {
			clearBytes(credential)
			credential = nil
		}
	}()
	options := [][2]string{
		{"HTTPS git repository", "git"},
		{"Existing image", "image"},
	}
	if base.Kind == "" {
		options = append([][2]string{{"No source (deployability gap)", ""}}, options...)
	}
	kind, err := ui.ChooseDefault("Source", "How this environment gets its application image", options, base.Kind)
	if err != nil {
		return platform.SourceBinding{}, nil, "", err
	}
	switch kind {
	case "":
		keep = true
		return platform.SourceBinding{}, nil, "", nil
	case "image":
		image, err := askValidated("Container image", "registry/repository:tag or digest", base.Image, platform.ValidateImage)
		if err != nil {
			return platform.SourceBinding{}, nil, "", err
		}
		keep = true
		return platform.SourceBinding{Kind: "image", Image: image}, nil, "", nil
	case "git":
		url, err := askValidated("HTTPS git repository", "No credentials in the URL; the node checks its allowed hosts", base.URL, validateGitURLShape)
		if err != nil {
			return platform.SourceBinding{}, nil, "", err
		}
		ref, err := ui.Input("Branch or tag", "Empty uses the repository's default branch", base.Ref)
		if err != nil {
			return platform.SourceBinding{}, nil, "", err
		}
		name, err := askValidated("Credential secret name", "Empty for a public repository", base.CredentialSecretName, validateOptionalSecretName)
		if err != nil {
			return platform.SourceBinding{}, nil, "", err
		}
		if name != "" {
			replaceDefault := name != base.CredentialSecretName
			replace, err := ui.YesNoDefault("Replace the stored read token?", "The value is written only after the preview and confirmation; only the name enters the revision.", replaceDefault)
			if err != nil {
				return platform.SourceBinding{}, nil, "", err
			}
			if replace {
				token, err := ui.Secret("Repository read token")
				if err != nil {
					return platform.SourceBinding{}, nil, "", err
				}
				if token == "" {
					return platform.SourceBinding{}, nil, "", contract.NewFault(contract.InputRequired, "the repository read token is empty", "Enter a token, or answer No to replacing the stored read token.")
				}
				credential = []byte(token)
				a.registerSecretValue(credential)
			}
		}
		builderDefault := base.Builder
		if builderDefault == "" {
			builderDefault = platform.DefaultBuilder
		}
		builder, err := askValidated("Buildpacks builder", "A pinned builder image on the node; empty uses the platform default", builderDefault, validateOptionalImage)
		if err != nil {
			return platform.SourceBinding{}, nil, "", err
		}
		contextPath, err := ui.Input("Build context path", "Empty builds the repository root", base.ContextPath)
		if err != nil {
			return platform.SourceBinding{}, nil, "", err
		}
		keep = true
		return platform.SourceBinding{
			Kind: "git", URL: url, Ref: ref, CredentialSecretName: name,
			ContextPath: contextPath, Builder: builder, BuildEnv: base.BuildEnv,
		}, credential, name, nil
	default:
		return platform.SourceBinding{}, nil, "", contract.NewFault(contract.Internal, "unsupported guided source kind", "Report the command and binary version.")
	}
}

func guidedRuntime(base platform.RuntimeSpec, node string) (platform.RuntimeSpec, error) {
	healthType := base.HealthType
	if healthType == "" {
		healthType = "http"
	}
	healthType, err := ui.ChooseDefault("Health check", "Web health probe", [][2]string{
		{"HTTP", "http"},
		{"TCP", "tcp"},
	}, healthType)
	if err != nil {
		return platform.RuntimeSpec{}, err
	}
	healthPath := base.HealthPath
	if healthPath == "" {
		healthPath = platform.DefaultHealthPath
	}
	healthPath, err = ui.Input("Health path", "Loopback path used by the deploy health check", healthPath)
	if err != nil {
		return platform.RuntimeSpec{}, err
	}
	timeout := base.HealthTimeoutSeconds
	if timeout == 0 {
		timeout = platform.DefaultHealthTimeout
	}
	timeout, err = askIntRange("Health timeout", "1–300 seconds", strconv.Itoa(timeout), 1, 300)
	if err != nil {
		return platform.RuntimeSpec{}, err
	}
	releaseLine := ""
	if base.ReleaseCommand != nil {
		releaseLine = strings.Join(base.ReleaseCommand.Argv, " ")
	}
	releaseLine, err = ui.Input("Release command", "Executable and arguments, split on spaces; empty for none", releaseLine)
	if err != nil {
		return platform.RuntimeSpec{}, err
	}
	runtime := base
	runtime.Node = node
	runtime.HealthType = healthType
	runtime.HealthPath = healthPath
	runtime.HealthTimeoutSeconds = timeout
	runtime.ReleaseCommand = commandSpecFromLine(releaseLine)
	if runtime.ReleaseCommand != nil && base.ReleaseCommand != nil {
		runtime.ReleaseCommand.TimeoutSeconds = base.ReleaseCommand.TimeoutSeconds
	}
	return runtime, nil
}

func guidedProcesses(base []platform.ProcessSpec) ([]platform.ProcessSpec, error) {
	processes := make([]platform.ProcessSpec, len(base))
	for index, process := range base {
		process.Args = append([]string(nil), process.Args...)
		processes[index] = process
	}
	if len(processes) > 0 {
		fmt.Fprintln(os.Stderr, ui.Note("Current process formation; enter keeps a value."))
		fmt.Fprint(os.Stderr, writeProcessTable(processes))
	}
	for index := range processes {
		process := &processes[index]
		if process.Kind == "web" || process.Kind == "service" {
			port, err := askPort("Container port for "+process.Name, "The port inside the container", strconv.Itoa(process.ContainerPort))
			if err != nil {
				return nil, err
			}
			process.ContainerPort = port
		}
		line := process.Command
		if len(process.Args) > 0 {
			line += " " + strings.Join(process.Args, " ")
		}
		for {
			input, inputErr := ui.Input("Command for "+process.Name, "Executable and arguments, split on spaces; empty for a web process", line)
			if inputErr != nil {
				return nil, inputErr
			}
			line = input
			command, args := splitCommandLine(line)
			if process.Kind != "web" && command == "" {
				fmt.Fprintln(os.Stderr, ui.Warn("A worker or service process needs a command."))
				continue
			}
			process.Command, process.Args = command, args
			break
		}
		replicas, err := askReplicas("Replicas for "+processLabel(*process), strconv.Itoa(process.Replicas))
		if err != nil {
			return nil, err
		}
		process.Replicas = replicas
	}
	extras, err := readAdditionalProcesses(processes)
	if err != nil {
		return nil, err
	}
	processes = append(processes, extras...)
	return removeProcesses(processes)
}

func removeProcesses(processes []platform.ProcessSpec) ([]platform.ProcessSpec, error) {
	for {
		name, err := ui.Input("Remove a process", "Existing process name; empty keeps the formation", "")
		if err != nil {
			return nil, err
		}
		if name == "" {
			return processes, nil
		}
		index := slices.IndexFunc(processes, func(process platform.ProcessSpec) bool { return process.Name == name })
		if index < 0 {
			fmt.Fprintln(os.Stderr, ui.Warn("No process is named "+name+"."))
			continue
		}
		processes = append(processes[:index], processes[index+1:]...)
	}
}

func processLabel(process platform.ProcessSpec) string {
	if process.Kind == process.Name {
		return process.Name
	}
	return process.Name + " (" + process.Kind + ")"
}

func displayValue(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

func validateOptionalName(value string) error {
	if value == "" {
		return nil
	}
	return platform.ValidateName(value)
}

func validateOptionalImage(value string) error {
	if value == "" {
		return nil
	}
	return platform.ValidateImage(value)
}

func validateOptionalSecretName(value string) error {
	if value == "" {
		return nil
	}
	return vault.ValidateName(value)
}

func sourceEqual(a, b platform.SourceBinding) bool {
	return a.Kind == b.Kind && a.URL == b.URL && a.Ref == b.Ref &&
		a.CredentialSecretName == b.CredentialSecretName && a.ContextPath == b.ContextPath &&
		a.Builder == b.Builder && a.Image == b.Image && maps.Equal(a.BuildEnv, b.BuildEnv)
}

func runtimeEqual(a, b platform.RuntimeSpec) bool {
	return a.Node == b.Node && a.HealthType == b.HealthType && a.HealthPath == b.HealthPath &&
		a.HealthTimeoutSeconds == b.HealthTimeoutSeconds && commandSpecEqual(a.ReleaseCommand, b.ReleaseCommand)
}

func commandSpecEqual(a, b *platform.CommandSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.TimeoutSeconds == b.TimeoutSeconds && slices.Equal(a.Argv, b.Argv)
}

func processSpecsEqual(a, b []platform.ProcessSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if !processSpecEqual(a[index], b[index]) {
			return false
		}
	}
	return true
}

func processSpecEqual(a, b platform.ProcessSpec) bool {
	return a.Name == b.Name && a.Kind == b.Kind && a.Command == b.Command &&
		a.Replicas == b.Replicas && a.ContainerPort == b.ContainerPort && slices.Equal(a.Args, b.Args)
}

func sourceDiff(old, new platform.SourceBinding) []string {
	out := []string{}
	field := func(name, oldValue, newValue string) {
		if oldValue != newValue {
			out = append(out, "source."+name+": "+displayValue(oldValue)+" → "+displayValue(newValue))
		}
	}
	field("kind", old.Kind, new.Kind)
	field("url", old.URL, new.URL)
	field("ref", old.Ref, new.Ref)
	field("credential_secret_name", old.CredentialSecretName, new.CredentialSecretName)
	field("context_path", old.ContextPath, new.ContextPath)
	field("builder", old.Builder, new.Builder)
	field("image", old.Image, new.Image)
	if !maps.Equal(old.BuildEnv, new.BuildEnv) {
		out = append(out, "source.build_env changed")
	}
	return out
}

func runtimeDiff(old, new platform.RuntimeSpec) []string {
	out := []string{}
	field := func(name, oldValue, newValue string) {
		if oldValue != newValue {
			out = append(out, "runtime."+name+": "+displayValue(oldValue)+" → "+displayValue(newValue))
		}
	}
	field("health_type", old.HealthType, new.HealthType)
	field("health_path", old.HealthPath, new.HealthPath)
	field("health_timeout_seconds", strconv.Itoa(old.HealthTimeoutSeconds), strconv.Itoa(new.HealthTimeoutSeconds))
	oldRelease, newRelease := "", ""
	if old.ReleaseCommand != nil {
		oldRelease = strings.Join(old.ReleaseCommand.Argv, " ")
	}
	if new.ReleaseCommand != nil {
		newRelease = strings.Join(new.ReleaseCommand.Argv, " ")
	}
	field("release_command", oldRelease, newRelease)
	return out
}

func processSpecsDiff(old, new []platform.ProcessSpec) []string {
	out := []string{}
	oldByName := make(map[string]platform.ProcessSpec, len(old))
	for _, process := range old {
		oldByName[process.Name] = process
	}
	newByName := make(map[string]platform.ProcessSpec, len(new))
	for _, process := range new {
		newByName[process.Name] = process
	}
	for _, process := range old {
		if _, exists := newByName[process.Name]; !exists {
			out = append(out, "process "+process.Name+": removed ("+describeProcessSpec(process)+")")
		}
	}
	for _, process := range new {
		previous, exists := oldByName[process.Name]
		if !exists {
			out = append(out, "process "+process.Name+": added ("+describeProcessSpec(process)+")")
			continue
		}
		if previous.Replicas != process.Replicas {
			out = append(out, fmt.Sprintf("process %s.replicas: %d → %d", process.Name, previous.Replicas, process.Replicas))
		}
		if previous.Kind != process.Kind || previous.Command != process.Command ||
			!slices.Equal(previous.Args, process.Args) || previous.ContainerPort != process.ContainerPort {
			out = append(out, "process "+process.Name+": "+describeProcessSpec(previous)+" → "+describeProcessSpec(process))
		}
	}
	return out
}

func describeProcessSpec(process platform.ProcessSpec) string {
	parts := []string{process.Kind, fmt.Sprintf("%d replica(s)", process.Replicas)}
	if process.ContainerPort > 0 {
		parts = append(parts, fmt.Sprintf("port %d", process.ContainerPort))
	}
	if process.Command != "" {
		command := process.Command
		if len(process.Args) > 0 {
			command += " " + strings.Join(process.Args, " ")
		}
		parts = append(parts, "command "+command)
	}
	return strings.Join(parts, ", ")
}
