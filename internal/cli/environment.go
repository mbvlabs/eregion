package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

func (a *app) envSpecs() []spec {
	parent := operationMeta("Create and inspect application environments", "eregion env list --app api --json")
	parent.WhenToUse = []string{"Manage the unit of deployment: its source binding, runtime, processes and domains."}

	create := operationMeta("Create an environment with its first desired-state revision", "eregion env create --app api --name production --node main --git-url https://github.com/acme/api --json")
	create.WhenToUse = []string{"Run it bare in a terminal for the guided setup. JSON and no-TTY take every value from flags and never prompt."}
	create.Mutating, create.DryRun, create.Prompts = true, true, true
	create.InputFlags = []string{
		"app", "name", "node", "build-node", "git-url", "git-ref", "git-credential-file",
		"git-credential-name", "image", "builder", "context-path", "build-env", "container-port",
		"health-path", "process", "config", "secret", "domain",
	}
	create.Examples = []string{
		"eregion env create",
		"eregion env create --app api --name staging --node main --git-url https://github.com/acme/api --git-ref next --domain staging.example.com --json",
	}
	create.Next = []string{"eregion env status <app> <environment> --json", "eregion domain add <app> <environment> <hostname>"}

	update := operationMeta("Change an environment as a new desired-state revision", "eregion env update api production --git-ref release-2026-10 --json")
	update.WhenToUse = []string{"Change source, placement, runtime or the process formation as a new immutable revision. Plain config uses config set; secret names use secret set. No deploy is queued."}
	update.Mutating, update.DryRun = true, true
	update.Examples = []string{
		"eregion env update api production --health-path /readyz --json",
		"eregion env update api production --process 'web=web:8080:2:' --json",
	}
	update.Next = []string{"eregion env status <app> <environment> --json"}

	list := operationMeta("List environments and their desired state", "eregion env list --app api --json")
	list.Examples = []string{"eregion env list --app api --json"}

	status := operationMeta("Show one environment's revision, processes and deployability", "eregion env status api production --json")
	status.Examples = []string{"eregion env status api production --json"}
	status.Next = []string{"eregion process list <app> <environment> --json", "eregion config list <app> <environment> --json"}

	return []spec{{
		use:  "env",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "create",
				meta: create,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("app", "", "Existing application name")
					cmd.Flags().String("name", "", "Environment name")
					addSetupFlags(cmd, true)
				},
				run: a.envCreate,
			},
			{
				use:   "update <app> <environment>",
				meta:  update,
				args:  platformTwoNames("app", "environment"),
				flags: addEnvironmentUpdateFlags,
				run:   a.envUpdate,
			},
			{
				use:  "list",
				meta: list,
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addInventoryFlags(cmd)
				},
				run: a.envList,
			},
			{
				use:  "status <app> <environment>",
				meta: status,
				args: platformTwoNames("app", "environment"),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.envStatus(cmd, args[0], args[1])
				},
			},
			a.envDeleteSpec(),
		},
	}}
}

func addEnvironmentUpdateFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("node", "", "Application node; empty keeps the current node")
	f.String("build-node", "", "Builder node; empty builds on the application node")
	f.String("git-url", "", "HTTPS git repository URL without credentials")
	f.String("git-ref", "", "Branch or tag; empty clears to the repository default")
	f.String("git-credential-file", "", "Protected 0600 file containing a new repository read token")
	f.String("git-credential-name", "", "Vault secret name for the read token; empty clears the name")
	f.String("image", "", "Existing OCI image reference; replaces a git source")
	f.String("builder", "", "Buildpacks builder image for a git source")
	f.String("context-path", "", "Build context path inside the repository")
	f.StringArray("build-env", nil, "Buildpacks environment KEY=VALUE (repeatable; replaces the map)")
	f.String("health-type", "", "Health check type: http or tcp")
	f.String("health-path", "", "Loopback health path")
	f.Int("health-timeout", 0, "Health timeout in seconds (1–300)")
	f.String("release-command", "", "One-off release command line; empty clears it")
	f.StringArray("process", nil, "Complete process formation entry NAME=KIND:PORT:REPLICAS:COMMAND ARGS (repeatable; replaces all processes)")
	f.String("idempotency-key", "", "Stable submission key; one is generated when empty")
	f.Bool("dry-run", false, "Validate and show the new revision without accepting work or writing secrets")
}

func (a *app) envCreate(cmd *cobra.Command, _ []string) (res result, err error) {
	interactive := humanInteractive(cmd)
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ui.Header("eregion env create", 80))
	}
	ans, err := a.readSetup(cmd, setupEnvironment)
	if err != nil {
		return result{}, setupPromptError("env create", err)
	}
	defer ans.clearSecrets()
	spec, err := ans.environmentSpec()
	if err != nil {
		return result{}, err
	}
	exists, err := a.appExists(cmd, ans.App)
	if err != nil {
		return result{}, err
	}
	if !exists {
		return result{}, contract.NewFault(contract.NotFound, "app "+ans.App+" was not found", "Create it with eregion app create, or choose an existing app.")
	}
	exists, err = a.environmentExists(cmd, ans.App, ans.Environment)
	if err != nil {
		return result{}, err
	}
	if exists {
		return result{}, contract.NewFault(contract.Conflict, "environment "+ans.App+"/"+ans.Environment+" already exists", "Change it with eregion env update, or choose another name.")
	}
	input := platform.EnvironmentCreateInput{App: ans.App, EnvironmentSpec: spec}
	target, err := platform.NewAppTarget(ans.App)
	if err != nil {
		return result{}, err
	}
	preview, err := a.previewAction(cmd, platform.KindEnvironmentCreate, target.String(), input)
	if err != nil {
		return result{}, err
	}
	plan := setupPlan(ans, preview)
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ui.PanelFit("Plan for environment "+ans.App+"/"+ans.Environment, plan, 80))
	}
	dry := dryRun(cmd)
	if interactive && !dry {
		ok, err := ui.YesNoDefault("Create environment "+ans.App+"/"+ans.Environment+" now?", "The environment and its first revision are accepted in one submission.", true)
		if err != nil {
			return result{}, setupPromptError("env create", err)
		}
		if !ok {
			return result{}, contract.NewFault(contract.Cancelled, "environment creation was not confirmed", "Nothing was written; the plan above is unchanged.")
		}
	}
	summary := setupResult{
		App: ans.App, Environment: ans.Environment, Node: ans.Node,
		Source: spec.Source, Processes: spec.Processes, ConfigKeys: sortedKeys(spec.Config),
		CredentialSecretName: ans.CredentialName, Domain: ans.Domain,
		Warnings: preview.Warnings, created: "environment",
	}
	if record, ok := domainDNSRecord(ans.Domain); ok {
		summary.DNSRecord = &record
	}
	if dry {
		summary.DryRun, summary.Preview, summary.plan = true, &preview, plan
		if interactive {
			return result{streamed: true}, nil
		}
		return result{data: summary, plain: summary.plainText()}, nil
	}

	secretNames, err := a.writeSetupSecrets(cmd, ans)
	if err != nil {
		return result{}, err
	}
	summary.SecretNames = secretNames
	op, err := a.submitAccepted(cmd, platform.KindEnvironmentCreate, target.String(), input)
	if err != nil {
		return result{}, err
	}
	summary.OperationID = op.ID
	if ans.Domain != "" {
		envTarget, err := platform.NewEnvironmentTarget(ans.App, ans.Environment)
		if err != nil {
			return result{}, err
		}
		domainOp, err := a.submitAccepted(cmd, platform.KindDomainAdd, envTarget.String(), platform.DomainInput{
			App: ans.App, Environment: ans.Environment, Domain: ans.Domain,
		})
		if err != nil {
			fault := contract.Classify(err)
			return result{}, contract.NewFault(fault.Code,
				"environment "+ans.App+"/"+ans.Environment+" was created, but adding domain "+ans.Domain+" failed: "+fault.Message,
				"Add it with eregion domain add "+ans.App+" "+ans.Environment+" "+ans.Domain+"; environment operation "+string(op.ID)+".")
		}
		summary.DomainOperationID = domainOp.ID
	}
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		rows := [][2]string{
			{"node", ans.Node},
			{"source", describeSource(spec.Source)},
			{"operation", string(op.ID)},
		}
		if ans.Domain != "" {
			rows = append(rows, [2]string{"domain", ans.Domain})
			dns := "point " + ans.Domain + " at the main node's public IPv4 address"
			if summary.DNSRecord != nil {
				dns = summary.DNSRecord.Type + " " + summary.DNSRecord.Name + " " + summary.DNSRecord.Value
			}
			rows = append(rows, [2]string{"DNS record", dns})
		}
		fmt.Fprintln(os.Stderr, ui.Card("Environment "+ans.App+"/"+ans.Environment+" created", rows, []string{
			ui.Dim("the revision is desired state, not a running deployment"),
			ui.Dim("next:"),
			"  " + ui.Code("eregion env status "+ans.App+" "+ans.Environment+" --json"),
		}))
		return result{streamed: true}, nil
	}
	return result{data: summary, plain: summary.plainText()}, nil
}

func (a *app) envUpdate(cmd *cobra.Command, args []string) (result, error) {
	if err := validateIdempotencyFlag(cmd); err != nil {
		return result{}, err
	}
	appName, environment := args[0], args[1]
	current, _, err := a.readEnvironment(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	patch, credential, credentialName, changed, err := a.buildEnvironmentUpdate(cmd, current)
	if err != nil {
		return result{}, err
	}
	defer clearBytes(credential)
	if len(changed) == 0 {
		return result{}, contract.NewFault(contract.InputRequired, "nothing to update", "Pass at least one of --node, --build-node, a source flag, --health-path, --release-command or --process.")
	}
	preview, err := a.previewAction(cmd, platform.KindEnvironmentUpdate, "env/"+appName+"/"+environment, patch)
	if err != nil {
		return result{}, err
	}
	resultData := struct {
		App                  string                          `json:"app"`
		Environment          string                          `json:"environment"`
		OperationID          contract.OperationID            `json:"operation_id,omitempty"`
		Changed              []string                        `json:"changed"`
		CredentialSecretName string                          `json:"credential_secret_name,omitempty"`
		Patch                platform.EnvironmentUpdateInput `json:"patch"`
		DryRun               bool                            `json:"dry_run,omitempty"`
	}{App: appName, Environment: environment, Changed: changed, CredentialSecretName: credentialName, Patch: patch}
	if dryRun(cmd) {
		resultData.DryRun = true
		var plain strings.Builder
		fmt.Fprintf(&plain, "Dry run: update %s/%s\n", appName, environment)
		plain.WriteString(updatePreviewText(preview))
		return result{data: resultData, plain: plain.String()}, nil
	}

	if credentialName != "" && len(credential) > 0 {
		namespace, err := vault.NewNamespace(appName, environment, credentialName)
		if err != nil {
			return result{}, err
		}
		if _, err := a.putSecret(cmd, namespace, credential); err != nil {
			return result{}, err
		}
	}
	op, err := a.submitAccepted(cmd, platform.KindEnvironmentUpdate, "env/"+appName+"/"+environment, patch)
	if err != nil {
		return result{}, err
	}
	resultData.OperationID = op.ID
	plain := fmt.Sprintf("Updated %s/%s desired state.\noperation: %s\nchanged: %s\nnext: eregion env status %s %s --json\n",
		appName, environment, op.ID, strings.Join(changed, ", "), appName, environment)
	return result{data: resultData, plain: plain}, nil
}

// buildEnvironmentUpdate merges only the supplied flags over the current
// revision. Source and runtime are replaced wholesale by the action, so their
// unchanged parts are carried over here.
func (a *app) buildEnvironmentUpdate(cmd *cobra.Command, current platform.Environment) (platform.EnvironmentUpdateInput, []byte, string, []string, error) {
	patch := platform.EnvironmentUpdateInput{App: current.App, Environment: current.Name}
	base := platform.RevisionState{}
	if current.Revision != nil {
		base = platform.CloneRevisionState(current.Revision.RevisionState)
	} else {
		base.Runtime = platform.RuntimeSpec{
			Node: current.Node, HealthType: current.Health.Type, HealthPath: current.Health.Path,
			HealthTimeoutSeconds: current.Health.TimeoutSeconds,
		}
		base.Processes = current.Processes
		base.Config = current.Env
	}
	changed := make([]string, 0, 4)

	if flagChanged(cmd, "node") {
		value := flagString(cmd, "node")
		if err := platform.ValidateName(value); err != nil {
			return patch, nil, "", nil, err
		}
		patch.Node = &value
		changed = append(changed, "node")
	}
	if flagChanged(cmd, "build-node") {
		value := flagString(cmd, "build-node")
		if value != "" {
			if err := platform.ValidateName(value); err != nil {
				return patch, nil, "", nil, err
			}
		}
		patch.BuildNode = &value
		changed = append(changed, "build_node")
	}

	source, sourceChanged, credential, credentialName, err := a.readUpdateSource(cmd, base.Source)
	if err != nil {
		return patch, nil, "", nil, err
	}
	if sourceChanged {
		patch.Source = &source
		changed = append(changed, "source")
	}

	runtimeChanged := flagChanged(cmd, "health-type") || flagChanged(cmd, "health-path") ||
		flagChanged(cmd, "health-timeout") || flagChanged(cmd, "release-command")
	if runtimeChanged {
		runtime := base.Runtime
		if runtime.Node == "" {
			runtime.Node = current.Node
		}
		if runtime.HealthType == "" {
			runtime.HealthType = "http"
		}
		if runtime.HealthPath == "" {
			runtime.HealthPath = platform.DefaultHealthPath
		}
		if runtime.HealthTimeoutSeconds == 0 {
			runtime.HealthTimeoutSeconds = platform.DefaultHealthTimeout
		}
		if patch.Node != nil {
			runtime.Node = *patch.Node
		}
		if flagChanged(cmd, "health-type") {
			runtime.HealthType = strings.ToLower(flagString(cmd, "health-type"))
		}
		if flagChanged(cmd, "health-path") {
			runtime.HealthPath = flagString(cmd, "health-path")
		}
		if flagChanged(cmd, "health-timeout") {
			runtime.HealthTimeoutSeconds = flagInt(cmd, "health-timeout")
		}
		if flagChanged(cmd, "release-command") {
			runtime.ReleaseCommand = commandSpecFromLine(flagString(cmd, "release-command"))
		}
		patch.Runtime = &runtime
		changed = append(changed, "runtime")
	}

	if flagChanged(cmd, "process") {
		processes, err := parseProcessSpecs(flagArray(cmd, "process"))
		if err != nil {
			return patch, nil, "", nil, err
		}
		patch.Processes = &processes
		changed = append(changed, "processes")
	}
	return patch, credential, credentialName, changed, nil
}

func (a *app) readUpdateSource(cmd *cobra.Command, base platform.SourceBinding) (platform.SourceBinding, bool, []byte, string, error) {
	imageChanged := flagChanged(cmd, "image")
	gitChanged := flagChanged(cmd, "git-url") || flagChanged(cmd, "git-ref") ||
		flagChanged(cmd, "git-credential-file") || flagChanged(cmd, "git-credential-name") ||
		flagChanged(cmd, "builder") || flagChanged(cmd, "context-path") || flagChanged(cmd, "build-env")
	if !imageChanged && !gitChanged {
		return platform.SourceBinding{}, false, nil, "", nil
	}
	if imageChanged && gitChanged {
		return platform.SourceBinding{}, false, nil, "", contract.NewFault(contract.ValidationFailed, "choose one source", "Use --image or a git flag, not both.")
	}
	if imageChanged {
		image := flagString(cmd, "image")
		if image == "" {
			return platform.SourceBinding{}, false, nil, "", contract.NewFault(contract.ValidationFailed, "an image source is empty", "Pass a registry/repository:tag or digest reference.")
		}
		source := platform.SourceBinding{Kind: "image", Image: image}
		return source, true, nil, "", nil
	}

	source := platform.SourceBinding{}
	if base.Kind == "git" {
		source = base
	}
	source.Kind = "git"
	if flagChanged(cmd, "git-url") {
		source.URL = flagString(cmd, "git-url")
	}
	if source.URL == "" {
		return platform.SourceBinding{}, false, nil, "", contract.NewFault(contract.ValidationFailed, "a git source needs a repository URL", "Pass --git-url with the HTTPS repository URL.")
	}
	if err := validateGitURLShape(source.URL); err != nil {
		return platform.SourceBinding{}, false, nil, "", err
	}
	if flagChanged(cmd, "git-ref") {
		source.Ref = flagString(cmd, "git-ref")
	}
	if flagChanged(cmd, "builder") {
		source.Builder = flagString(cmd, "builder")
	}
	if flagChanged(cmd, "context-path") {
		source.ContextPath = flagString(cmd, "context-path")
	}
	if flagChanged(cmd, "build-env") {
		values, err := parseAssignments(flagArray(cmd, "build-env"), "--build-env")
		if err != nil {
			return platform.SourceBinding{}, false, nil, "", err
		}
		source.BuildEnv = values
	}
	if flagChanged(cmd, "git-credential-name") && !flagChanged(cmd, "git-credential-file") {
		name := flagString(cmd, "git-credential-name")
		if name != "" {
			if err := vault.ValidateName(name); err != nil {
				return platform.SourceBinding{}, false, nil, "", err
			}
		}
		source.CredentialSecretName = name
	}
	if flagChanged(cmd, "git-credential-file") {
		name := flagString(cmd, "git-credential-name")
		if name == "" {
			name = source.CredentialSecretName
		}
		if name == "" {
			name = "GIT_READ_TOKEN"
		}
		if err := vault.ValidateName(name); err != nil {
			return platform.SourceBinding{}, false, nil, "", err
		}
		value, err := readProtectedSecretFile(flagString(cmd, "git-credential-file"))
		if err != nil {
			return platform.SourceBinding{}, false, nil, "", err
		}
		a.registerSecretValue(value)
		source.CredentialSecretName = name
		return source, true, value, name, nil
	}
	return source, true, nil, "", nil
}

func updatePreviewText(preview platform.Preview) string {
	var out strings.Builder
	if preview.Summary != "" {
		out.WriteString(preview.Summary + "\n")
	}
	for _, step := range preview.Steps {
		fmt.Fprintf(&out, "step: %s\n", step)
	}
	for _, warning := range preview.Warnings {
		fmt.Fprintf(&out, "warning: %s\n", warning)
	}
	return out.String()
}

func (a *app) envList(cmd *cobra.Command, _ []string) (result, error) {
	raw, err := a.platformReadInventory(cmd, "environments")
	if err != nil {
		return result{}, err
	}
	environments, err := decodeInventoryItems[platform.Environment](raw)
	if err != nil {
		return result{}, err
	}
	plain := "No environments.\n"
	if len(environments) > 0 {
		rows := make([][]string, 0, len(environments))
		for _, environment := range environments {
			serving := string(environment.ServingRelease)
			if serving == "" {
				serving = "-"
			}
			rows = append(rows, []string{
				environment.App, environment.Name, environment.Node, revisionLabel(environment),
				boolLabel(environment.Deployability.Deployable), strings.Join(environment.Deployability.Missing, ","), serving,
			})
		}
		plain = writeTable([]string{"APP", "ENVIRONMENT", "NODE", "REVISION", "DEPLOYABLE", "MISSING", "SERVING RELEASE"}, rows)
	}
	return result{data: raw, plain: plain}, nil
}

func (a *app) envStatus(cmd *cobra.Command, app, environment string) (result, error) {
	return a.envStatusView(cmd, app, environment)
}
