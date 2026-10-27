package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// setupMode selects the identity fields of the shared application setup. App
// creation names a new app and its first environment; environment creation
// names an existing app and a new environment.
type setupMode int

const (
	setupApp setupMode = iota
	setupEnvironment
)

const defaultNodeName = "main"

// assignmentKeyPattern is the plain-configuration and Buildpacks key shape; it
// matches the platform's own environment-key rule.
var assignmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// setupProcessNamePattern matches the platform's process-name rule.
var setupProcessNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// setupAnswers is one complete first-environment declaration. SecretValues
// holds plaintext only until the vault write; the revision receives names.
type setupAnswers struct {
	App         string
	Environment string
	Node        string
	BuildNode   string

	SourceKind      string
	GitURL          string
	GitRef          string
	CredentialName  string
	CredentialValue []byte
	Image           string
	Builder         string
	ContextPath     string
	BuildEnv        map[string]string

	ContainerPort int
	HealthType    string
	HealthPath    string
	HealthTimeout int
	// ReleaseCommand is the one-off command each deploy runs before the
	// switch, such as a migration; empty runs none.
	ReleaseCommand string

	Processes    []platform.ProcessSpec
	Config       map[string]string
	SecretValues map[string][]byte
	Domain       string
	Deploy       bool
}

// humanInteractive reports whether this invocation may ask questions. JSON
// output, a non-terminal stdin or a dumb terminal always takes the flag path.
func humanInteractive(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	return !jsonMode && outputMode != "json" &&
		term.IsTerminal(int(os.Stdin.Fd())) && ui.Enabled(os.Stderr) //nolint:gosec // G115: file descriptors fit in int.
}

// addSetupFlags adds the source, runtime, process, configuration, secret and
// domain inputs shared by app create and env create.
func addSetupFlags(cmd *cobra.Command, withDomain bool) {
	f := cmd.Flags()
	f.String("node", defaultNodeName, "Application node with the application role and docker capability")
	f.String("build-node", "", "Optional builder node (builder + buildpacks); defaults to --node")
	f.String("git-url", "", "HTTPS git repository URL without credentials")
	f.String("git-ref", "", "Branch or tag to build; empty uses the repository default")
	f.String("git-credential-file", "", "Protected 0600 file containing the repository read token")
	f.String("git-credential-name", "", "Vault secret name for the read token (default GIT_READ_TOKEN)")
	f.String("image", "", "Existing OCI image reference; alternative to --git-url")
	f.String("builder", platform.DefaultBuilder, "Buildpacks builder image for a git source")
	f.String("context-path", "", "Build context path inside the repository")
	f.StringArray("build-env", nil, "Buildpacks environment KEY=VALUE (repeatable)")
	f.Int("container-port", platform.DefaultContainerPort, "Web process container port")
	f.String("health-type", "http", "Health check type: http or tcp")
	f.String("health-path", platform.DefaultHealthPath, "Loopback health path")
	f.Int("health-timeout", platform.DefaultHealthTimeout, "Health timeout in seconds (1–300)")
	f.String("release-command", "", "Command line each deploy runs once before the switch, such as a migration")
	f.StringArray("process", nil, "Complete process entry NAME=KIND:PORT:REPLICAS:COMMAND ARGS (repeatable; replaces the default web process). A worker has no port: queue=worker::1:/app/bin/queue")
	f.StringArray("config", nil, "Plain configuration KEY=VALUE (repeatable)")
	f.StringArray("secret", nil, "Vault secret NAME=PATH with a protected 0600 value file (repeatable)")
	f.String("idempotency-key", "", "Stable submission key; one is generated when empty")
	f.Bool("dry-run", false, "Validate and show the plan without accepting work or writing secrets")
	if withDomain {
		f.String("domain", "", "Public hostname for the environment; empty adds no binding")
	}
}

func (a *app) registerSecretValue(value []byte) {
	if a.secrets == nil {
		a.secrets = &secrets.Registry{}
	}
	a.secrets.Add(string(value))
}

// setupPromptError turns a Ctrl+C/Esc in a guided setup into an explicit
// cancellation instead of an internal error. Setup writes nothing before its
// confirmation, so a cancelled prompt leaves no operation behind.
func setupPromptError(command string, err error) error {
	if errors.Is(err, ui.ErrAborted) {
		return contract.NewFault(contract.Cancelled, command+" cancelled", "No operation was submitted and no secret value was written.")
	}
	return err
}

func flagInt(cmd *cobra.Command, name string) int {
	if cmd == nil || cmd.Flags().Lookup(name) == nil {
		return 0
	}
	value, _ := cmd.Flags().GetInt(name)
	return value
}

func flagArray(cmd *cobra.Command, name string) []string {
	if cmd == nil || cmd.Flags().Lookup(name) == nil {
		return nil
	}
	value, _ := cmd.Flags().GetStringArray(name)
	return value
}

func flagChanged(cmd *cobra.Command, name string) bool {
	return cmd != nil && cmd.Flags().Lookup(name) != nil && cmd.Flags().Changed(name)
}

// validateIdempotencyFlag rejects a malformed key before any vault write, so a
// bad key cannot leave stored values behind.
func validateIdempotencyFlag(cmd *cobra.Command) error {
	if cmd == nil || cmd.Flags().Lookup("idempotency-key") == nil {
		return nil
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if key == "" {
		return nil
	}
	return contract.ValidateIdempotencyKey(key)
}

func askValidated(title, description, def string, validate func(string) error) (string, error) {
	for {
		value, err := ui.Input(title, description, def)
		if err != nil {
			return "", err
		}
		if validate == nil {
			return value, nil
		}
		if err := validate(value); err != nil {
			fmt.Fprintln(os.Stderr, ui.Warn(err.Error()))
			def = value
			continue
		}
		return value, nil
	}
}

func askPort(title, description, def string) (int, error) {
	for {
		value, err := ui.Input(title, description, def)
		if err != nil {
			return 0, err
		}
		if port, convErr := strconv.Atoi(value); convErr == nil && port >= 1 && port <= 65535 {
			return port, nil
		}
		fmt.Fprintln(os.Stderr, ui.Warn("Use a port from 1 to 65535."))
		def = value
	}
}

func askReplicas(title, def string) (int, error) {
	for {
		value, err := ui.Input(title, "1–32 replicas", def)
		if err != nil {
			return 0, err
		}
		if replicas, convErr := strconv.Atoi(value); convErr == nil && replicas >= 1 && replicas <= 32 {
			return replicas, nil
		}
		fmt.Fprintln(os.Stderr, ui.Warn("Use between 1 and 32 replicas."))
		def = value
	}
}

// askIntRange asks for one integer within an inclusive range, prefilled with def.
func askIntRange(title, description, def string, min, max int) (int, error) {
	for {
		value, err := ui.Input(title, description, def)
		if err != nil {
			return 0, err
		}
		if number, convErr := strconv.Atoi(value); convErr == nil && number >= min && number <= max {
			return number, nil
		}
		fmt.Fprintln(os.Stderr, ui.Warn(fmt.Sprintf("Use a number from %d to %d.", min, max)))
		def = value
	}
}

// readSetup reads the flags, asks only for what is still missing in a terminal
// and returns one validated declaration. Every question has a flag equivalent.
func (a *app) readSetup(cmd *cobra.Command, mode setupMode) (*setupAnswers, error) {
	interactive := humanInteractive(cmd)
	ans := &setupAnswers{
		Node:           flagString(cmd, "node"),
		BuildNode:      flagString(cmd, "build-node"),
		GitURL:         flagString(cmd, "git-url"),
		GitRef:         flagString(cmd, "git-ref"),
		Image:          flagString(cmd, "image"),
		Builder:        flagString(cmd, "builder"),
		ContextPath:    flagString(cmd, "context-path"),
		ContainerPort:  flagInt(cmd, "container-port"),
		HealthType:     strings.ToLower(flagString(cmd, "health-type")),
		HealthPath:     flagString(cmd, "health-path"),
		HealthTimeout:  flagInt(cmd, "health-timeout"),
		ReleaseCommand: flagString(cmd, "release-command"),
		Domain:         flagString(cmd, "domain"),
		Config:         map[string]string{},
		SecretValues:   map[string][]byte{},
	}
	if ans.Node == "" {
		ans.Node = defaultNodeName
	}
	if ans.HealthType == "" {
		ans.HealthType = "http"
	}
	if ans.HealthPath == "" {
		ans.HealthPath = platform.DefaultHealthPath
	}
	if ans.HealthTimeout == 0 {
		ans.HealthTimeout = platform.DefaultHealthTimeout
	}
	if err := validateIdempotencyFlag(cmd); err != nil {
		return nil, err
	}
	keepSecrets := false
	defer func() {
		if !keepSecrets {
			ans.clearSecrets()
		}
	}()

	switch mode {
	case setupApp:
		ans.App = flagString(cmd, "name")
		ans.Environment = flagString(cmd, "environment")
		if ans.Environment == "" {
			ans.Environment = "production"
		}
	case setupEnvironment:
		ans.App = flagString(cmd, "app")
		ans.Environment = flagString(cmd, "name")
	}

	if ans.App == "" {
		if !interactive {
			flag := "--name"
			if mode == setupEnvironment {
				flag = "--app"
			}
			return nil, contract.NewFault(contract.InputRequired, flag+" is required without a terminal", "Pass "+flag+" with the app name.")
		}
		value, err := askValidated("App name", "Lowercase letters, digits and inner hyphens", "", platform.ValidateName)
		if err != nil {
			return nil, err
		}
		ans.App = value
	}
	if ans.Environment == "" {
		if !interactive {
			return nil, contract.NewFault(contract.InputRequired, "--name is required without a terminal", "Pass --name with the environment name.")
		}
		value, err := askValidated("Environment name", "For example production or staging", "", platform.ValidateName)
		if err != nil {
			return nil, err
		}
		ans.Environment = value
	}
	if interactive && !flagChanged(cmd, "environment") && mode == setupApp {
		value, err := askValidated("First environment", "The unit of deployment", ans.Environment, platform.ValidateName)
		if err != nil {
			return nil, err
		}
		ans.Environment = value
	}
	if interactive && !flagChanged(cmd, "node") {
		value, err := a.askNode(cmd, "Application node", "Runs the containers", ans.Node, platform.RoleApplication, platform.CapabilityDocker, "", "")
		if err != nil {
			return nil, err
		}
		ans.Node = value
	}
	if interactive && !flagChanged(cmd, "build-node") {
		value, err := a.askNode(cmd, "Builder node", "Runs the Buildpacks builds", ans.BuildNode, platform.RoleBuilder, platform.CapabilityBuildpacks, "The application node", ans.Node)
		if err != nil {
			return nil, err
		}
		ans.BuildNode = value
	}

	switch {
	case ans.GitURL != "" && ans.Image != "":
		return nil, contract.NewFault(contract.ValidationFailed, "choose one source", "Use --git-url or --image, not both.")
	case ans.GitURL != "":
		ans.SourceKind = "git"
	case ans.Image != "":
		ans.SourceKind = "image"
	case interactive:
		kind, err := ui.Choose("Source", "How this environment gets its application image", [][2]string{
			{"HTTPS git repository", "git"},
			{"Existing image", "image"},
		})
		if err != nil {
			return nil, err
		}
		ans.SourceKind = kind
	default:
		return nil, contract.NewFault(contract.InputRequired, "a source is required without a terminal", "Pass --git-url or --image.")
	}

	if ans.SourceKind == "git" {
		if ans.GitURL == "" {
			value, err := askValidated("HTTPS git repository", "No credentials in the URL; the node checks its allowed hosts", "", validateGitURLShape)
			if err != nil {
				return nil, err
			}
			ans.GitURL = value
		}
		if err := validateGitURLShape(ans.GitURL); err != nil {
			return nil, err
		}
		if interactive && !flagChanged(cmd, "git-ref") {
			value, err := ui.Input("Branch or tag", "Empty uses the repository's default branch", ans.GitRef)
			if err != nil {
				return nil, err
			}
			ans.GitRef = value
		}
		if err := a.readSetupCredential(cmd, ans, interactive); err != nil {
			return nil, err
		}
		if interactive && !flagChanged(cmd, "builder") {
			value, err := askValidated("Buildpacks builder", "A pinned builder image on the node; empty uses the platform default", ans.Builder, func(value string) error {
				if value == "" {
					return nil
				}
				return platform.ValidateImage(value)
			})
			if err != nil {
				return nil, err
			}
			ans.Builder = value
		}
		if interactive && !flagChanged(cmd, "context-path") {
			value, err := ui.Input("Build context path", "Empty builds the repository root", ans.ContextPath)
			if err != nil {
				return nil, err
			}
			ans.ContextPath = value
		}
		values, err := parseAssignments(flagArray(cmd, "build-env"), "--build-env")
		if err != nil {
			return nil, err
		}
		ans.BuildEnv = values
	} else {
		if ans.Image == "" {
			value, err := askValidated("Container image", "registry/repository:tag or digest", "", platform.ValidateImage)
			if err != nil {
				return nil, err
			}
			ans.Image = value
		}
		if flagChanged(cmd, "builder") || flagChanged(cmd, "context-path") || len(flagArray(cmd, "build-env")) > 0 {
			return nil, contract.NewFault(contract.ValidationFailed, "build settings need a git source", "An image source does not run Buildpacks; omit --builder, --context-path and --build-env.")
		}
	}

	if interactive && !flagChanged(cmd, "container-port") && len(flagArray(cmd, "process")) == 0 {
		port, err := askPort("Web container port", "The port the web process listens on inside the container", fmt.Sprint(ans.ContainerPort))
		if err != nil {
			return nil, err
		}
		ans.ContainerPort = port
	}
	if interactive && !flagChanged(cmd, "health-path") {
		value, err := ui.Input("Health path", "Loopback HTTP path used by the deploy health check", ans.HealthPath)
		if err != nil {
			return nil, err
		}
		ans.HealthPath = value
	}

	if values := flagArray(cmd, "process"); len(values) > 0 {
		processes, err := parseProcessSpecs(values)
		if err != nil {
			return nil, err
		}
		ans.Processes = processes
	} else if interactive {
		ans.Processes = []platform.ProcessSpec{defaultSetupWebProcess(ans.ContainerPort)}
		if err := readExtraProcesses(ans); err != nil {
			return nil, err
		}
	}

	if values := flagArray(cmd, "config"); len(values) > 0 {
		config, err := parseAssignments(values, "--config")
		if err != nil {
			return nil, err
		}
		ans.Config = config
	} else if interactive {
		if err := readConfigPairs(ans); err != nil {
			return nil, err
		}
	}

	if values := flagArray(cmd, "secret"); len(values) > 0 {
		valuesByPath, err := parseSecretFlags(values)
		if err != nil {
			return nil, err
		}
		for name, path := range valuesByPath {
			if ans.CredentialName != "" && name == ans.CredentialName {
				return nil, contract.NewFault(contract.ValidationFailed, "secret name conflicts with the git read credential", "The read credential is not injected into the app; choose another secret name.")
			}
			value, err := readProtectedSecretFile(path)
			if err != nil {
				return nil, err
			}
			a.registerSecretValue(value)
			ans.SecretValues[name] = value
		}
	} else if interactive {
		if err := a.readSetupSecrets(ans); err != nil {
			return nil, err
		}
	}

	if interactive && !flagChanged(cmd, "domain") {
		value, err := ui.Input("Public domain", "For example api.example.com; empty adds it later", ans.Domain)
		if err != nil {
			return nil, err
		}
		ans.Domain = value
	}
	if ans.Domain != "" {
		ans.Domain = strings.ToLower(strings.TrimSuffix(ans.Domain, "."))
		if err := platform.ValidateDomain(ans.Domain); err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid domain", err.Error())
		}
	}

	switch {
	case boolFlag(cmd, "no-deploy"):
		ans.Deploy = false
	case interactive && !flagChanged(cmd, "no-deploy"):
		deploy, err := ui.YesNoDefault("Request the first deploy?", "The submission queues it from the new revision; --no-deploy skips it. A first deploy needs a domain.", ans.Domain != "")
		if err != nil {
			return nil, err
		}
		ans.Deploy = deploy
	default:
		ans.Deploy = true
	}

	if _, err := ans.environmentSpec(); err != nil {
		return nil, err
	}
	keepSecrets = true
	return ans, nil
}

func (a *app) readSetupCredential(cmd *cobra.Command, ans *setupAnswers, interactive bool) error {
	path := flagString(cmd, "git-credential-file")
	name := flagString(cmd, "git-credential-name")
	if path != "" {
		if name == "" {
			name = "GIT_READ_TOKEN"
		}
		if err := vault.ValidateName(name); err != nil {
			return err
		}
		value, err := readProtectedSecretFile(path)
		if err != nil {
			return err
		}
		a.registerSecretValue(value)
		ans.CredentialName, ans.CredentialValue = name, value
		return nil
	}
	if name != "" {
		return contract.NewFault(contract.InputRequired, "--git-credential-name needs a value", "Pass --git-credential-file with a protected 0600 file, or omit both flags.")
	}
	if !interactive {
		return nil
	}
	private, err := ui.YesNoDefault("Private repository?", "Store a read credential in this environment's vault; only its name enters the revision", false)
	if err != nil {
		return err
	}
	if !private {
		return nil
	}
	name, err = askValidated("Credential secret name", "The vault name for the repository read token", "GIT_READ_TOKEN", vault.ValidateName)
	if err != nil {
		return err
	}
	token, err := ui.Secret("Repository read token")
	if err != nil {
		return err
	}
	if token == "" {
		return contract.NewFault(contract.InputRequired, "the read token is empty", "Enter a token, or answer No to the private-repository question.")
	}
	value := []byte(token)
	a.registerSecretValue(value)
	ans.CredentialName, ans.CredentialValue = name, value
	return nil
}

func readExtraProcesses(ans *setupAnswers) error {
	extras, err := readAdditionalProcesses(ans.Processes)
	if err != nil {
		return err
	}
	ans.Processes = append(ans.Processes, extras...)
	return nil
}

// readAdditionalProcesses asks for new worker/service processes and returns
// only the additions. The caller keeps the existing formation.
func readAdditionalProcesses(existing []platform.ProcessSpec) ([]platform.ProcessSpec, error) {
	seen := map[string]bool{}
	for _, process := range existing {
		seen[process.Name] = true
	}
	extras := []platform.ProcessSpec{}
	for {
		name, err := ui.Input("Add a process", "A worker or service name; empty to finish", "")
		if err != nil {
			return nil, err
		}
		if name == "" {
			return extras, nil
		}
		if !setupProcessNamePattern.MatchString(name) {
			fmt.Fprintln(os.Stderr, ui.Warn("Use a lowercase process name of 1–63 letters, digits, underscores, or hyphens."))
			continue
		}
		if seen[name] {
			fmt.Fprintln(os.Stderr, ui.Warn("Use each process name once."))
			continue
		}
		kind, err := ui.Choose("Process kind", "A worker has no port; a service has a private container port", [][2]string{
			{"Worker (no port)", "worker"},
			{"Service (private port only)", "service"},
		})
		if err != nil {
			return nil, err
		}
		line, err := ui.Input("Command", "Executable and arguments, split on spaces; no shell", "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			fmt.Fprintln(os.Stderr, ui.Warn("A worker or service process needs a command."))
			continue
		}
		port := 0
		if kind == "service" {
			port, err = askPort("Service container port", "Reachable privately on the node", "9000")
			if err != nil {
				return nil, err
			}
		}
		replicas, err := askReplicas("Replicas", "1")
		if err != nil {
			return nil, err
		}
		command, args := splitCommandLine(line)
		extras = append(extras, platform.ProcessSpec{
			Name: name, Kind: kind, Command: command, Args: args, Replicas: replicas, ContainerPort: port,
		})
		seen[name] = true
	}
}

func readConfigPairs(ans *setupAnswers) error {
	text := ""
	for {
		value, err := ui.Lines("Config", "One KEY=VALUE per line; paste a .env block. Blank lines and # comments are skipped. Ctrl+J for a new line, Enter to finish", text)
		if err != nil {
			return err
		}
		values, err := parseAssignments(envLines(value), "config")
		if err != nil {
			fmt.Fprintln(os.Stderr, ui.Warn(err.Error()))
			text = value
			continue
		}
		for key, item := range values {
			ans.Config[key] = item
		}
		return nil
	}
}

// envLines turns a pasted .env block into KEY=VALUE entries: blank lines and
// comments are dropped, an export prefix and matching quotes are removed.
func envLines(text string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		if key, value, ok := strings.Cut(line, "="); ok && len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				line = key + "=" + value[1:len(value)-1]
			}
		}
		out = append(out, line)
	}
	return out
}

func (a *app) readSetupSecrets(ans *setupAnswers) error {
	for {
		title := "Secret name"
		if n := len(ans.SecretValues); n > 0 {
			title = fmt.Sprintf("Another secret name (%d added)", n)
		}
		name, err := ui.Input(title, "Environment-variable form, e.g. DATABASE_URL; empty to finish", "")
		if err != nil {
			return err
		}
		if name == "" {
			return nil
		}
		if err := vault.ValidateName(name); err != nil {
			fmt.Fprintln(os.Stderr, ui.Warn(err.Error()))
			continue
		}
		if _, exists := ans.SecretValues[name]; exists {
			fmt.Fprintln(os.Stderr, ui.Warn("Set each secret name once."))
			continue
		}
		if ans.CredentialName != "" && name == ans.CredentialName {
			fmt.Fprintln(os.Stderr, ui.Warn("The git read credential is not injected; choose another secret name."))
			continue
		}
		value, err := ui.Secret("Secret value for " + name)
		if err != nil {
			return err
		}
		if value == "" {
			fmt.Fprintln(os.Stderr, ui.Warn("A secret value cannot be empty."))
			continue
		}
		decoded := []byte(value)
		a.registerSecretValue(decoded)
		ans.SecretValues[name] = decoded
	}
}

func (ans *setupAnswers) clearSecrets() {
	clearBytes(ans.CredentialValue)
	ans.CredentialValue = nil
	for name, value := range ans.SecretValues {
		clearBytes(value)
		delete(ans.SecretValues, name)
	}
}

// environmentSpec turns the answers into the validated first revision of a
// new environment.
func (ans *setupAnswers) environmentSpec() (platform.EnvironmentSpec, error) {
	if err := platform.ValidateName(ans.App); err != nil {
		return platform.EnvironmentSpec{}, err
	}
	if err := platform.ValidateName(ans.Environment); err != nil {
		return platform.EnvironmentSpec{}, err
	}
	if err := platform.ValidateName(ans.Node); err != nil {
		return platform.EnvironmentSpec{}, err
	}
	if ans.BuildNode != "" {
		if err := platform.ValidateName(ans.BuildNode); err != nil {
			return platform.EnvironmentSpec{}, err
		}
	}
	source := platform.SourceBinding{}
	switch ans.SourceKind {
	case "git":
		if err := validateGitURLShape(ans.GitURL); err != nil {
			return platform.EnvironmentSpec{}, err
		}
		if ans.Builder != "" {
			if err := platform.ValidateImage(ans.Builder); err != nil {
				return platform.EnvironmentSpec{}, err
			}
		}
		if ans.CredentialName != "" {
			if err := vault.ValidateName(ans.CredentialName); err != nil {
				return platform.EnvironmentSpec{}, err
			}
		}
		source = platform.SourceBinding{
			Kind: "git", URL: ans.GitURL, Ref: ans.GitRef,
			CredentialSecretName: ans.CredentialName, ContextPath: ans.ContextPath,
			Builder: ans.Builder, BuildEnv: ans.BuildEnv,
		}
	case "image":
		if err := platform.ValidateImage(ans.Image); err != nil {
			return platform.EnvironmentSpec{}, err
		}
		source = platform.SourceBinding{Kind: "image", Image: ans.Image}
	default:
		return platform.EnvironmentSpec{}, contract.NewFault(contract.ValidationFailed, "a source is required", "Use a git source or an image.")
	}

	if ans.HealthType != "http" && ans.HealthType != "tcp" {
		return platform.EnvironmentSpec{}, contract.NewFault(contract.ValidationFailed, "invalid health type", "Use http or tcp.")
	}
	if ans.HealthPath == "" {
		ans.HealthPath = platform.DefaultHealthPath
	}
	if ans.HealthTimeout == 0 {
		ans.HealthTimeout = platform.DefaultHealthTimeout
	}

	processes := ans.Processes
	containerPort := ans.ContainerPort
	if len(processes) == 0 {
		if containerPort == 0 {
			containerPort = platform.DefaultContainerPort
		}
		processes = []platform.ProcessSpec{defaultSetupWebProcess(containerPort)}
	}
	for _, process := range processes {
		if process.Kind == "web" && process.ContainerPort > 0 {
			containerPort = process.ContainerPort
			break
		}
	}
	if containerPort == 0 {
		containerPort = platform.DefaultContainerPort
	}

	secretNames := make([]string, 0, len(ans.SecretValues))
	for name := range ans.SecretValues {
		if err := vault.ValidateName(name); err != nil {
			return platform.EnvironmentSpec{}, err
		}
		secretNames = append(secretNames, name)
	}
	sort.Strings(secretNames)

	var domainAddresses []string
	if ans.Domain != "" {
		domainAddresses = mainNodeAddresses(context.Background())
	}
	return platform.EnvironmentSpec{
		Name: ans.Environment, Node: ans.Node, BuildNode: ans.BuildNode,
		ContainerPort: containerPort, Bind: platform.DefaultBind,
		Health: platform.HealthInput{Type: ans.HealthType, Path: ans.HealthPath, TimeoutSeconds: ans.HealthTimeout},
		Source: source,
		Runtime: platform.RuntimeSpec{
			Node: ans.Node, HealthType: ans.HealthType, HealthPath: ans.HealthPath,
			HealthTimeoutSeconds: ans.HealthTimeout,
			ReleaseCommand:       commandSpecFromLine(ans.ReleaseCommand),
		},
		Processes:       processes,
		Config:          ans.Config,
		SecretNames:     secretNames,
		Domain:          ans.Domain,
		DomainPrimary:   ans.Domain != "",
		DomainAddresses: domainAddresses,
	}, nil
}

func defaultSetupWebProcess(port int) platform.ProcessSpec {
	if port == 0 {
		port = platform.DefaultContainerPort
	}
	return platform.ProcessSpec{Name: "web", Kind: "web", Args: []string{}, Replicas: 1, ContainerPort: port}
}

// parseAssignments parses repeated KEY=VALUE inputs. Values are plain, not
// secrets, and cannot carry NUL or line breaks.
func parseAssignments(values []string, label string) (map[string]string, error) {
	if len(values) > 64 {
		return nil, contract.NewFault(contract.ValidationFailed, "too many "+label+" entries", "Use at most 64 entries.")
	}
	out := make(map[string]string, len(values))
	for _, value := range values {
		key, item, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.TrimSpace(key) != key || strings.ContainsAny(key, " \t\r\n") {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid "+label+" value", "Use KEY=VALUE with a nonempty key without whitespace.")
		}
		if !assignmentKeyPattern.MatchString(key) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid "+label+" key", "Use 1–128 ASCII letters, digits, and underscores; do not start with a digit.")
		}
		if _, exists := out[key]; exists {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate "+label+" key", "Set each key once.")
		}
		if strings.ContainsAny(item, "\x00\r\n") {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid "+label+" value", "Values cannot contain NUL or line breaks.")
		}
		out[key] = item
	}
	return out, nil
}

func parseSecretFlags(values []string) (map[string]string, error) {
	if len(values) > 64 {
		return nil, contract.NewFault(contract.ValidationFailed, "too many --secret entries", "Use at most 64 secret values.")
	}
	out := make(map[string]string, len(values))
	for _, value := range values {
		name, path, ok := strings.Cut(value, "=")
		if !ok || name == "" || path == "" {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid --secret value", "Use NAME=PATH with a vault secret name and a protected 0600 file.")
		}
		if err := vault.ValidateName(name); err != nil {
			return nil, err
		}
		if _, exists := out[name]; exists {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate --secret name", "Set each secret name once.")
		}
		out[name] = path
	}
	return out, nil
}

// parseProcessSpecs parses NAME=KIND:PORT:REPLICAS:COMMAND ARGS. Port,
// replicas and command are optional; the command line is split on spaces and
// never runs through a shell.
func parseProcessSpecs(values []string) ([]platform.ProcessSpec, error) {
	if len(values) > 32 {
		return nil, contract.NewFault(contract.ValidationFailed, "too many processes", "Use at most 32 processes.")
	}
	out := make([]platform.ProcessSpec, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		name, rest, ok := strings.Cut(value, "=")
		if !ok || name == "" || strings.TrimSpace(name) != name {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid --process value", "Use NAME=KIND:PORT:REPLICAS:COMMAND ARGS.")
		}
		if !setupProcessNamePattern.MatchString(name) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid process name", "Use a lowercase name of 1–63 letters, digits, underscores, or hyphens.")
		}
		if seen[name] {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate process name", "Use each process name once.")
		}
		seen[name] = true
		parts := strings.SplitN(rest, ":", 4)
		kind := strings.ToLower(strings.TrimSpace(parts[0]))
		if kind != "web" && kind != "worker" && kind != "service" {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid process kind", "Use web, worker, or service.")
		}
		port := 0
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			value, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil || value < 1 || value > 65535 {
				hint := "Use a container port from 1 to 65535."
				if kind == "worker" {
					hint = "A worker has no port; leave it empty: " + name + "=worker::" + strings.Join(parts[2:], ":")
				}
				return nil, contract.NewFault(contract.ValidationFailed, "invalid process port", hint)
			}
			port = value
		}
		replicas := 1
		if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
			value, err := strconv.Atoi(strings.TrimSpace(parts[2]))
			if err != nil || value < 1 || value > 32 {
				return nil, contract.NewFault(contract.ValidationFailed, "invalid process replicas", "Use between 1 and 32 replicas.")
			}
			replicas = value
		}
		command, args := "", []string{}
		if len(parts) > 3 {
			command, args = splitCommandLine(parts[3])
		}
		out = append(out, platform.ProcessSpec{Name: name, Kind: kind, Command: command, Args: args, Replicas: replicas, ContainerPort: port})
	}
	return out, nil
}

// splitCommandLine splits an executable and its arguments on spaces. It is not
// a shell: quoting, expansion and operators have no meaning.
func splitCommandLine(line string) (string, []string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", []string{}
	}
	return fields[0], fields[1:]
}

func commandSpecFromLine(line string) *platform.CommandSpec {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	return &platform.CommandSpec{Argv: fields}
}

// validateGitURLShape checks the parts every node can agree on without reading
// the node's EREGION_GIT_ALLOWED_HOSTS. The node still applies its allowlist.
func validateGitURLShape(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return contract.NewFault(contract.ValidationFailed, "invalid git source URL", "Use an https:// repository URL without credentials, query, or fragment.")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return contract.NewFault(contract.ValidationFailed, "invalid git source URL", "Git build sources must use the default HTTPS port.")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || net.ParseIP(host) != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid git source URL", "Use a repository hostname, not an IP address.")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return contract.NewFault(contract.ValidationFailed, "invalid git source URL", "Git build sources cannot target a loopback or internal name.")
	}
	return nil
}

// domainDNSRecord returns the A record the operator must create for the
// environment's public hostname, when the saved context exposes an IPv4.
func domainDNSRecord(domain string) (hostDNSRecord, bool) {
	if domain == "" {
		return hostDNSRecord{}, false
	}
	for _, value := range mainNodeAddresses(context.Background()) {
		if ip := net.ParseIP(value); ip != nil && ip.To4() != nil {
			return hostDNSRecord{Type: "A", Name: domain, Value: value}, true
		}
	}
	return hostDNSRecord{}, false
}

// setupPlan is the secret-free plan shown before a guided submit and by
// --dry-run in every mode.
func setupPlan(ans *setupAnswers, preview platform.Preview) string {
	var out strings.Builder
	fmt.Fprintf(&out, "app: %s\n", ans.App)
	fmt.Fprintf(&out, "environment: %s\n", ans.Environment)
	fmt.Fprintf(&out, "node: %s\n", ans.Node)
	if ans.BuildNode != "" {
		fmt.Fprintf(&out, "builder node: %s\n", ans.BuildNode)
	}
	if ans.SourceKind == "git" {
		ref := ans.GitRef
		if ref == "" {
			ref = "repository default"
		}
		fmt.Fprintf(&out, "source: git %s @ %s\n", ans.GitURL, ref)
		if ans.CredentialName != "" {
			fmt.Fprintf(&out, "read credential: %s\n", ans.CredentialName)
		}
		if ans.Builder != "" {
			fmt.Fprintf(&out, "builder: %s\n", ans.Builder)
		}
		if ans.ContextPath != "" {
			fmt.Fprintf(&out, "context path: %s\n", ans.ContextPath)
		}
		if len(ans.BuildEnv) > 0 {
			fmt.Fprintf(&out, "build environment: %s\n", strings.Join(sortedKeys(ans.BuildEnv), ", "))
		}
	} else {
		fmt.Fprintf(&out, "source: image %s\n", ans.Image)
	}
	for _, process := range planProcesses(ans) {
		fmt.Fprintf(&out, "process: %s\n", process)
	}
	fmt.Fprintf(&out, "health: %s %s (%ds timeout)\n", strings.ToUpper(ans.HealthType), ans.HealthPath, ans.HealthTimeout)
	if ans.ReleaseCommand != "" {
		fmt.Fprintf(&out, "release command: %s\n", ans.ReleaseCommand)
	}
	if len(ans.Config) > 0 {
		for _, key := range sortedKeys(ans.Config) {
			fmt.Fprintf(&out, "config: %s=%s\n", key, ans.Config[key])
		}
	}
	if len(ans.SecretValues) > 0 {
		fmt.Fprintf(&out, "secrets: %s\n", strings.Join(sortedKeysBytes(ans.SecretValues), ", "))
	}
	if ans.Domain != "" {
		fmt.Fprintf(&out, "domain: %s\n", ans.Domain)
		if record, ok := domainDNSRecord(ans.Domain); ok {
			fmt.Fprintf(&out, "DNS record: %s %s %s\n", record.Type, record.Name, record.Value)
		} else {
			fmt.Fprintf(&out, "DNS record: point %s at the main node's public IPv4 address\n", ans.Domain)
		}
		fmt.Fprintln(&out, "Create it at the DNS provider; a Cloudflare proxy in front is fine.")
	} else {
		fmt.Fprintln(&out, "domain: none; add one later with eregion domain add")
	}
	if ans.Deploy {
		fmt.Fprintln(&out, "first deploy: queued from the revision")
		if ans.Domain == "" {
			fmt.Fprintln(&out, "warning: the first deploy has no domain; the deployability gate refuses it until a primary domain is bound")
		}
	} else {
		fmt.Fprintln(&out, "first deploy: not requested")
	}
	for _, step := range preview.Steps {
		fmt.Fprintf(&out, "step: %s\n", step)
	}
	for _, warning := range preview.Warnings {
		fmt.Fprintf(&out, "warning: %s\n", warning)
	}
	return output.SafeText(strings.TrimRight(out.String(), "\n"))
}

func planProcesses(ans *setupAnswers) []string {
	processes := ans.Processes
	if len(processes) == 0 {
		return []string{fmt.Sprintf("web, 1 replica, port %d", ans.ContainerPort)}
	}
	out := make([]string, 0, len(processes))
	for _, process := range processes {
		line := process.Name
		if process.Kind != process.Name {
			line += " (" + process.Kind + ")"
		}
		line += fmt.Sprintf(", %d replica(s)", process.Replicas)
		if process.ContainerPort > 0 {
			line += fmt.Sprintf(", port %d", process.ContainerPort)
		}
		if process.Command != "" {
			line += ", command " + process.Command
			if len(process.Args) > 0 {
				line += " " + strings.Join(process.Args, " ")
			}
		}
		out = append(out, line)
	}
	return out
}

func sortedKeysBytes(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// previewAction validates one canonical action on the node without accepting
// work. It runs before any vault write so a rejected declaration cannot leave
// orphan secret values.
func (a *app) previewAction(cmd *cobra.Command, kind, target string, input any) (platform.Preview, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return platform.Preview{}, contract.NewFault(contract.Internal, "could not encode platform input", "Report the command and binary version.")
	}
	client, err := a.client(cmd)
	if err != nil {
		return platform.Preview{}, err
	}
	data, err := client.SubmitAction(cmd.Context(), kind, target, raw, "", "", true)
	if err != nil {
		return platform.Preview{}, err
	}
	var preview platform.Preview
	if json.Unmarshal(data, &preview) != nil || !preview.DryRun || preview.Kind != kind || preview.Target != target {
		return platform.Preview{}, invalidNodeData("action preview")
	}
	return preview, nil
}

// submitAccepted submits one action with a D12 target confirmation when the
// action requires one, returning the durable operation it was accepted as. The
// idempotency key is stable when the operator supplied one.
func (a *app) submitAccepted(cmd *cobra.Command, kind, target string, input any, confirm string) (contract.Operation, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return contract.Operation{}, contract.NewFault(contract.Internal, "could not encode platform input", "Report the command and binary version.")
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if key == "" {
		key = "cli:" + strings.TrimPrefix(kind, "platform.") + ":" + randomHex(8)
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return contract.Operation{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return contract.Operation{}, err
	}
	data, err := client.SubmitAction(cmd.Context(), kind, target, raw, key, confirm, false)
	if err != nil {
		return contract.Operation{}, err
	}
	var op contract.Operation
	if json.Unmarshal(data, &op) != nil || op.Validate() != nil || op.Kind != kind || op.Target != target {
		return contract.Operation{}, invalidNodeData("accepted operation")
	}
	return op, nil
}

// putSecret writes one secret value, replacing an existing value when a prior
// attempt already created the namespace. It is only for values the operator did
// not request with an explicit revision CAS guard.
func (a *app) putSecret(cmd *cobra.Command, namespace vault.Namespace, value []byte) (vault.Metadata, error) {
	client, err := a.client(cmd)
	if err != nil {
		return vault.Metadata{}, err
	}
	metadata, err := client.GetSecretMetadata(cmd.Context(), namespace)
	if err != nil {
		var fault *contract.Fault
		if errors.As(err, &fault) && fault.Code == contract.NotFound {
			return client.SetSecret(cmd.Context(), namespace, value, 0)
		}
		return vault.Metadata{}, err
	}
	return client.SetSecret(cmd.Context(), namespace, value, metadata.LatestRevision)
}

// writeSetupSecrets stores the read credential and the environment secrets
// before the app/environment submission. Only names enter the operation input.
func (a *app) writeSetupSecrets(cmd *cobra.Command, ans *setupAnswers) ([]string, error) {
	names := make([]string, 0, len(ans.SecretValues)+1)
	if ans.CredentialName != "" && len(ans.CredentialValue) > 0 {
		namespace, err := vault.NewNamespace(ans.App, ans.Environment, ans.CredentialName)
		if err != nil {
			return nil, err
		}
		if _, err := a.putSecret(cmd, namespace, ans.CredentialValue); err != nil {
			return nil, err
		}
	}
	for _, name := range sortedKeysBytes(ans.SecretValues) {
		namespace, err := vault.NewNamespace(ans.App, ans.Environment, name)
		if err != nil {
			return nil, err
		}
		if _, err := a.putSecret(cmd, namespace, ans.SecretValues[name]); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

// appExists and environmentExists let the create flows refuse an existing
// record before writing secret values.
func (a *app) appExists(cmd *cobra.Command, name string) (bool, error) {
	raw, err := a.platformReadInventoryWith(cmd, "apps", urlValues("app", name, "limit", "1"))
	if err != nil {
		return false, err
	}
	items, err := decodeInventoryItems[platform.App](raw)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (a *app) environmentExists(cmd *cobra.Command, app, environment string) (bool, error) {
	raw, err := a.platformReadInventoryWith(cmd, "environments", urlValues("app", app, "environment", environment, "limit", "1"))
	if err != nil {
		return false, err
	}
	items, err := decodeInventoryItems[platform.Environment](raw)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.App == app && item.Name == environment {
			return true, nil
		}
	}
	return false, nil
}

func decodeInventoryItems[T any](raw json.RawMessage) ([]T, error) {
	var page struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, invalidNodeData("inventory page")
	}
	return page.Items, nil
}

func urlValues(pairs ...string) map[string][]string {
	values := make(map[string][]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		values[pairs[i]] = []string{pairs[i+1]}
	}
	return values
}

// readEnvironment reads one environment's current revision and deployability.
func (a *app) readEnvironment(cmd *cobra.Command, app, environment string) (platform.Environment, json.RawMessage, error) {
	if err := platform.ValidateName(app); err != nil {
		return platform.Environment{}, nil, err
	}
	if err := platform.ValidateName(environment); err != nil {
		return platform.Environment{}, nil, err
	}
	raw, err := a.platformReadInventoryWith(cmd, "environments",
		urlValues("app", app, "environment", environment, "limit", "1"))
	if err != nil {
		return platform.Environment{}, nil, err
	}
	item, err := platformSelectItem(raw, environment)
	if err != nil {
		var fault *contract.Fault
		if errors.As(err, &fault) && fault.Code == contract.NotFound {
			return platform.Environment{}, nil, contract.NewFault(contract.NotFound,
				"environment "+app+"/"+environment+" was not found",
				"List environments with eregion env list --app "+app+".")
		}
		return platform.Environment{}, nil, err
	}
	var env platform.Environment
	if json.Unmarshal(item, &env) != nil || env.App != app || env.Name != environment {
		return platform.Environment{}, nil, invalidNodeData("environment")
	}
	return env, item, nil
}

func writeProcessTable(processes []platform.ProcessSpec) string {
	if len(processes) == 0 {
		return "(none)\n"
	}
	rows := make([][]string, 0, len(processes))
	for _, process := range processes {
		port := "-"
		if process.ContainerPort > 0 {
			port = fmt.Sprint(process.ContainerPort)
		}
		command := process.Command
		if len(process.Args) > 0 {
			command += " " + strings.Join(process.Args, " ")
		}
		if command == "" {
			command = "-"
		}
		rows = append(rows, []string{process.Name, process.Kind, fmt.Sprint(process.Replicas), port, command})
	}
	return writeTable([]string{"NAME", "KIND", "REPLICAS", "PORT", "COMMAND"}, rows)
}

func writeTable(headers []string, rows [][]string) string {
	return ui.Table(headers, rows)
}

func environmentDeployability(env platform.Environment) string {
	if env.Deployability.Deployable {
		return "yes"
	}
	missing := "unknown"
	if len(env.Deployability.Missing) > 0 {
		missing = strings.Join(env.Deployability.Missing, ",")
	}
	return "no (missing: " + missing + "; dns " + env.Deployability.DNS + ")"
}

func writeEnvironmentStatus(env platform.Environment) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Environment %s/%s\n", env.App, env.Name)
	fmt.Fprintf(&out, "  node: %s\n", env.Node)
	if env.Revision != nil {
		fmt.Fprintf(&out, "  revision: %d (%s)\n", env.Revision.Number, env.Revision.ID)
		fmt.Fprintf(&out, "  deployable: %s\n", environmentDeployability(env))
		fmt.Fprintf(&out, "  source: %s\n", describeSource(env.Revision.Source))
	} else {
		fmt.Fprintln(&out, "  revision: none")
	}
	if env.ServingRelease != "" {
		fmt.Fprintf(&out, "  serving release: %s\n", env.ServingRelease)
	} else {
		fmt.Fprintln(&out, "  serving release: none")
	}
	health := env.HealthState
	if health == "" {
		health = platform.HealthUnknown
	}
	fmt.Fprintf(&out, "  health: %s\n", health)
	out.WriteString("Processes\n")
	out.WriteString(writeProcessTable(env.Processes))
	if env.Revision != nil && len(env.Revision.Config) > 0 {
		out.WriteString("Config\n")
		for _, key := range sortedKeys(env.Revision.Config) {
			fmt.Fprintf(&out, "  %s=%s\n", key, env.Revision.Config[key])
		}
	}
	if env.Revision != nil && len(env.Revision.SecretNames) > 0 {
		fmt.Fprintf(&out, "Secrets\n  %s\n", strings.Join(env.Revision.SecretNames, ", "))
	}
	if len(env.Domains) > 0 {
		out.WriteString("Domains\n")
		for _, domain := range env.Domains {
			label := ""
			if domain == env.PrimaryDomain {
				label = " (primary)"
			}
			fmt.Fprintf(&out, "  %s%s\n", domain, label)
		}
	}
	return out.String()
}

// setupResult is the secret-free result of app create or env create.
type setupResult struct {
	App                  string                 `json:"app"`
	Environment          string                 `json:"environment"`
	Node                 string                 `json:"node"`
	Source               platform.SourceBinding `json:"source"`
	Processes            []platform.ProcessSpec `json:"processes"`
	ConfigKeys           []string               `json:"config_keys,omitempty"`
	SecretNames          []string               `json:"secret_names,omitempty"`
	CredentialSecretName string                 `json:"credential_secret_name,omitempty"`
	Domain               string                 `json:"domain,omitempty"`
	DNSRecord            *hostDNSRecord         `json:"dns_record,omitempty"`
	Deploy               bool                   `json:"deploy"`
	OperationID          contract.OperationID   `json:"operation_id,omitempty"`
	DryRun               bool                   `json:"dry_run,omitempty"`
	Preview              *platform.Preview      `json:"preview,omitempty"`
	Warnings             []string               `json:"warnings,omitempty"`

	created string
	plan    string
}

func (r setupResult) plainText() string {
	if r.DryRun {
		var out strings.Builder
		if r.created == "app" {
			fmt.Fprintf(&out, "Dry run: create app %s with environment %s\n", r.App, r.Environment)
		} else {
			fmt.Fprintf(&out, "Dry run: create environment %s/%s\n", r.App, r.Environment)
		}
		out.WriteString(r.plan)
		return strings.TrimRight(out.String(), "\n")
	}
	var out strings.Builder
	if r.created == "app" {
		fmt.Fprintf(&out, "Created app %s with environment %s on %s.\n", r.App, r.Environment, r.Node)
	} else {
		fmt.Fprintf(&out, "Created environment %s/%s on %s.\n", r.App, r.Environment, r.Node)
	}
	fmt.Fprintf(&out, "operation: %s\n", r.OperationID)
	if r.Deploy {
		out.WriteString("first deploy: queued from the revision\n")
	}
	if r.Domain != "" {
		fmt.Fprintf(&out, "domain: %s\n", r.Domain)
		if r.DNSRecord != nil {
			fmt.Fprintf(&out, "DNS record:\n  %s  %s  %s\n", r.DNSRecord.Type, r.DNSRecord.Name, r.DNSRecord.Value)
		} else {
			fmt.Fprintf(&out, "DNS record: point %s at the main node's public IPv4 address\n", r.Domain)
		}
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(&out, "warning: %s\n", warning)
	}
	fmt.Fprintf(&out, "next: eregion env status %s %s --json\n", r.App, r.Environment)
	return out.String()
}

func describeSource(source platform.SourceBinding) string {
	switch source.Kind {
	case "git":
		ref := source.Ref
		if ref == "" {
			ref = "default branch"
		}
		line := "git " + source.URL + " @ " + ref
		if source.CredentialSecretName != "" {
			line += " (credential " + source.CredentialSecretName + ")"
		}
		return line
	case "image":
		return "image " + source.Image
	default:
		return "none"
	}
}
