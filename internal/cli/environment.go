package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/protect"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

func (a *app) envSpecs() []spec {
	parent := operationMeta("Create and inspect application environments", "eregion env list --app api --json")
	parent.WhenToUse = []string{"Manage the unit of deployment: its source binding, runtime, processes and domains."}

	create := followingMeta(operationMeta("Create an environment with its first desired-state revision", "eregion env create --app api --name production --node main --git-url https://github.com/acme/api --json"))
	create.WhenToUse = []string{"Run it bare in a terminal for the guided setup. The first deploy is queued from the new revision unless --no-deploy. JSON and no-TTY take every value from flags and never prompt."}
	create.Mutating, create.DryRun, create.Prompts = true, true, true
	create.InputFlags = []string{
		"app", "name", "node", "build-node", "git-url", "git-ref", "git-credential-file",
		"git-credential-name", "image", "builder", "context-path", "build-env", "container-port",
		"health-path", "process", "config", "secret", "domain", "domain-check-path", "from", "no-deploy", "detach",
		"protected", "unprotected",
	}
	create.Examples = []string{
		"eregion env create",
		"eregion env create --app api --name staging --node main --git-url https://github.com/acme/api --git-ref next --domain staging.example.com --json",
		"eregion env create --from ./eregion.toml --name pr-42 --no-deploy --json",
	}
	create.Next = []string{"eregion env status <app> <environment> --json", "eregion domain add <app> <environment> <hostname>"}

	update := operationMeta("Change an environment as a new desired-state revision", "eregion env update api production --git-ref release-2026-10 --json")
	update.WhenToUse = []string{"Change source, placement, runtime or the process formation as a new immutable revision. In a terminal with no change flags it walks the current revision prefilled, shows the diff and confirms; with flags or JSON/no TTY it is flag-only. Plain config uses config set; secret names use secret set. No deploy is queued. With an authenticated gh, --github-webhook creates or updates the repository webhook for --auto-deploy or --rotate-webhook-secret; manual paste remains the fallback."}
	update.Mutating, update.DryRun, update.Prompts = true, true, true
	update.InputFlags = []string{
		"node", "build-node", "git-url", "git-ref", "git-credential-file", "git-credential-name",
		"image", "builder", "context-path", "health-type", "health-path", "health-timeout",
		"release-command", "process", "from", "github-webhook",
	}
	update.Examples = []string{
		"eregion env update api production",
		"eregion env update api production --health-path /readyz --json",
		"eregion env update api production --process 'web=web:8080:2:' --json",
		"eregion env update api production --auto-deploy",
		"eregion env update api production --auto-deploy --github-webhook",
		"eregion env update api production --rotate-webhook-secret",
		"eregion env update api production --from ./eregion.toml --dry-run --json",
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
					cmd.Flags().Bool("no-deploy", false, "Do not queue the first deploy")
					cmd.Flags().Bool("detach", false, "Submit and return without following the create and first deploy")
					addProtectionFlags(cmd)
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
			a.envProtectionSpec(true),
			a.envProtectionSpec(false),
			a.envDeleteSpec(),
		},
	}}
}

// envProtectionSpec builds env protect (protected=true) and env unprotect
// (protected=false). Lowering protection is the only direction that needs the
// human factor; the node/control plane verifies it against locked state.
func (a *app) envProtectionSpec(protected bool) spec {
	use := "protect <app> <environment>"
	summary := "Protect an environment so a real deletion needs the human factor"
	example := "eregion env protect api production --json"
	when := []string{
		"Protection is durable environment state, not a desired-state revision.",
		"Protecting is agent-safe and never asks for a code. Unprotecting asks for a TOTP code at a human terminal and refuses JSON/no-TTY without one.",
	}
	if !protected {
		use = "unprotect <app> <environment>"
		summary = "Lower an environment's deletion protection; requires the human factor"
		example = "eregion env unprotect api pr-42"
		when = []string{
			"Turning protection off on a protected environment requires the D7 TOTP human factor at a human terminal.",
			"An already-unprotected environment is unchanged without a code. An unprotected, non-last environment can then be deleted by an agent with exact target confirmation.",
		}
	}
	meta := followingMeta(operationMeta(summary, example))
	meta.Mutating, meta.DryRun = true, true
	meta.Prompts = !protected
	meta.InputFlags = []string{"detach"}
	meta.WhenToUse = when
	meta.Next = []string{"eregion env status <app> <environment> --json", "eregion env delete <app> <environment> --confirm <app>/<environment>"}
	return spec{
		use:  use,
		meta: meta,
		args: platformTwoNames("app", "environment"),
		flags: func(cmd *cobra.Command) {
			cmd.Flags().Bool("dry-run", false, "Validate and show the protection change without accepting work")
			cmd.Flags().Bool("detach", false, "Return immediately after acceptance instead of following in a terminal")
			cmd.Flags().String("idempotency-key", "", "Stable submission key; one is generated when empty")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			return a.runEnvProtection(cmd, args[0], args[1], protected)
		},
	}
}

type envProtectionResult struct {
	App         string              `json:"app"`
	Environment string              `json:"environment"`
	Protected   bool                `json:"protected"`
	Dropped     bool                `json:"dropped,omitempty"`
	Operation   *contract.Operation `json:"operation,omitempty"`
	DryRun      bool                `json:"dry_run,omitempty"`
	Preview     *platform.Preview   `json:"preview,omitempty"`
}

func (a *app) runEnvProtection(cmd *cobra.Command, app, environment string, protected bool) (result, error) {
	env, _, err := a.readEnvironment(cmd, app, environment)
	if err != nil {
		return result{}, err
	}
	target, err := platform.NewEnvironmentTarget(app, environment)
	if err != nil {
		return result{}, err
	}
	input := platform.EnvironmentProtectInput{App: app, Environment: environment, Protected: protected}
	if dryRun(cmd) {
		preview, err := a.previewAction(cmd, platform.KindEnvironmentProtect, target.String(), input)
		if err != nil {
			return result{}, err
		}
		state := "protected"
		if !protected {
			state = "unprotected"
		}
		plain := fmt.Sprintf("Dry run: set %s %s\n%s\n", target.String(), state, preview.Summary)
		return result{data: envProtectionResult{App: app, Environment: environment, Protected: protected, DryRun: true, Preview: &preview}, plain: plain}, nil
	}
	var code string
	if !protected && env.Protected {
		if err := requireHuman(cmd, "eregion env unprotect"); err != nil {
			return result{}, err
		}
		code, err = readProtectCode(cmd, "Code from the authenticator app")
		if err != nil {
			return result{}, err
		}
	}
	op, err := a.submitAcceptedWithSecrets(cmd, platform.KindEnvironmentProtect, target.String(), input, "", map[string]string{protect.CodeSecret: code})
	if err != nil {
		return result{}, err
	}
	data := envProtectionResult{App: app, Environment: environment, Protected: protected, Operation: &op}
	if !boolFlag(cmd, "detach") && (humanInteractive(cmd) || streamsNDJSON(cmd)) {
		client, err := a.client(cmd)
		if err != nil {
			return result{}, err
		}
		return a.followAccepted(cmd, client, op)
	}
	state := "protected"
	if !protected {
		state = "unprotected"
	}
	plain := fmt.Sprintf("Environment %s/%s is now %s\noperation: %s\nstate: %s\n", app, environment, state, op.ID, op.State)
	return result{data: data, plain: plain}, nil
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
	f.String("from", "", "Read an eregion.toml definition as the complete supported desired state; config is plain, secrets are names only; revision and webhook flags cannot be combined")
	f.Bool("auto-deploy", false, "Enable push to deploy for a GitHub source; --auto-deploy=false disables it")
	f.Bool("rotate-webhook-secret", false, "Replace the push-to-deploy secret and show the new one once")
	f.Bool("github-webhook", false, "Create or update the repository webhook with an authenticated gh; needs --auto-deploy or --rotate-webhook-secret")
	f.String("confirm", "", "Target-naming confirmation for destructive definition removals: app/environment")
	f.StringArray("confirm-domain", nil, "Target-naming confirmation for each domain removed by a definition (repeatable)")
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
	spec, err := ans.environmentSpec(cmd)
	if err != nil {
		return result{}, err
	}
	follow := !boolFlag(cmd, "detach") && (streamsNDJSON(cmd) || (interactive && ans.Deploy && followsByDefault(cmd)))
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	if err := validateIdempotencyFlag(cmd); err != nil {
		return result{}, err
	}
	if key, _ := cmd.Flags().GetString("idempotency-key"); key == "" {
		// A named key reaches preview first: preview knows whether it replays
		// an accepted submission and must skip the same conflict checks.
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
	}
	input := platform.EnvironmentCreateInput{App: ans.App, EnvironmentSpec: spec, Deploy: ans.Deploy}
	target, err := platform.NewAppTarget(ans.App)
	if err != nil {
		return result{}, err
	}
	preview, err := a.previewAction(cmd, platform.KindEnvironmentCreate, target.String(), input)
	if err != nil {
		return result{}, err
	}
	plan := setupPlan(ans, preview, cmd)
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ui.PanelFit("Plan for environment "+ans.App+"/"+ans.Environment, plan, 80))
	}
	dry := dryRun(cmd)
	if interactive && !dry {
		ok, err := ui.YesNoDefault("Create environment "+ans.App+"/"+ans.Environment+" now?", "The environment and its first revision are accepted in one submission; a requested first deploy is queued from it.", true)
		if err != nil {
			return result{}, setupPromptError("env create", err)
		}
		if !ok {
			return result{}, contract.NewFault(contract.Cancelled, "environment creation was not confirmed", "Nothing was written; the plan above is unchanged.")
		}
	}
	summary := setupResult{
		App: ans.App, Environment: ans.Environment, Protected: protectedSpec(spec), Node: ans.Node,
		Source: spec.Source, Processes: spec.Processes, ConfigKeys: sortedKeys(spec.Config),
		CredentialSecretName: ans.CredentialName, Domain: ans.Domain, DomainCheckPath: ans.DomainCheckPath, Domains: ans.Domains, Deploy: ans.Deploy,
		Warnings: preview.Warnings, created: "environment",
	}
	if record, ok := domainDNSRecord(ans.Domain, cmd); ok {
		summary.DNSRecord = &record
	}
	if ans.Deploy && ans.Domain == "" {
		summary.Warnings = append(summary.Warnings, "The first deploy has no domain; the deployability gate refuses it until a primary domain is bound.")
	}
	if len(ans.ResourceSpecs) > 0 {
		summary.Warnings = append(summary.Warnings, "A first deploy was not queued because the definition declares managed resources; wait for resource health, then run `eregion deploy "+ans.App+" "+ans.Environment+"`.")
	}
	summary.SecretNames = setupSecretNames(ans)
	if dry {
		summary.DryRun, summary.Preview, summary.plan = true, &preview, plan
		if interactive {
			return result{streamed: true}, nil
		}
		return result{data: summary, plain: summary.plainText()}, nil
	}

	if preview.Replay {
		// The original submission already wrote the values; a replay only
		// needs the same accepted operation.
		summary.SecretNames = setupSecretNames(ans)
	} else {
		if _, err := a.writeSetupSecrets(cmd, ans, spec); err != nil {
			return result{}, err
		}
		summary.SecretNames = setupSecretNames(ans)
	}
	op, err := a.submitAccepted(cmd, platform.KindEnvironmentCreate, target.String(), input, "")
	if err != nil {
		return result{}, err
	}
	summary.OperationID = op.ID
	summary.FirstDeployOperationID = op.FirstDeployOperationID
	if len(ans.ResourceSpecs) > 0 {
		resourceIDs, err := a.provisionDefinitionResources(cmd, ans.ResourceSpecs)
		if err != nil {
			summary.Warnings = append(summary.Warnings, "The environment was created, but resource provisioning was not accepted: "+err.Error()+". Reapply with `eregion resource apply`.")
		}
		summary.ResolvedResourceIDs = resourceIDs
		if len(resourceIDs) > 0 {
			summary.Warnings = append(summary.Warnings, "Managed resources were accepted for provisioning; they are not ready until their health checks pass, and the first deploy waits for attached resources.")
		}
	}
	if interactive {
		if ans.Deploy && followsByDefault(cmd) {
			client, err := a.client(cmd)
			if err != nil {
				return result{}, err
			}
			return a.followGuidedCreate(cmd, client, op, target.String(), a.deployOutcome(ans.App, ans.Environment))
		}
		fmt.Fprintln(os.Stderr, "")
		rows := [][2]string{
			{"node", ans.Node},
			{"source", describeSource(spec.Source)},
			{"operation", string(op.ID)},
		}
		if ans.Deploy {
			rows = append(rows, [2]string{"first deploy", "queued from the revision"})
			if summary.FirstDeployOperationID != "" {
				rows = append(rows, [2]string{"first deploy operation", string(summary.FirstDeployOperationID)})
			}
		}
		if ans.Domain != "" {
			rows = append(rows, [2]string{"domain", ans.Domain})
			dns := "point " + ans.Domain + " at the main node's public IPv4 address"
			if summary.DNSRecord != nil {
				dns = summary.DNSRecord.Type + " " + summary.DNSRecord.Name + " " + summary.DNSRecord.Value
			}
			rows = append(rows, [2]string{"DNS record", dns})
		}
		for _, id := range summary.ResolvedResourceIDs {
			rows = append(rows, [2]string{"resource operation", string(id)})
		}
		footer := []string{
			ui.Dim("the revision is desired state, not a running deployment"),
			ui.Dim("next:"),
			"  " + ui.Code("eregion env status "+ans.App+" "+ans.Environment+" --json"),
		}
		if summary.FirstDeployOperationID != "" {
			footer = append(footer, "  "+ui.Code("eregion operations follow "+string(summary.FirstDeployOperationID)))
		}
		fmt.Fprintln(os.Stderr, ui.Card("Environment "+ans.App+"/"+ans.Environment+" created", rows, footer))
		return result{streamed: true}, nil
	}
	if streamsNDJSON(cmd) && !boolFlag(cmd, "detach") {
		client, err := a.client(cmd)
		if err != nil {
			return result{}, err
		}
		if ans.Deploy && op.FirstDeployOperationID != "" {
			return a.followGuidedCreate(cmd, client, op, target.String(), a.deployOutcome(ans.App, ans.Environment))
		}
		return a.followAccepted(cmd, client, op)
	}
	return result{data: summary, plain: summary.plainText()}, nil
}

func (a *app) envUpdate(cmd *cobra.Command, args []string) (result, error) {
	if from := flagString(cmd, "from"); from != "" {
		return a.envUpdateFromFile(cmd, args[0], args[1], from)
	}
	if err := validateIdempotencyFlag(cmd); err != nil {
		return result{}, err
	}
	if err := validateGitHubWebhookFlags(cmd); err != nil {
		return result{}, err
	}
	appName, environment := args[0], args[1]
	current, _, err := a.readEnvironment(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	patch, credential, credentialName, changed, err := a.buildEnvironmentUpdate(cmd, current)
	if err != nil {
		clearBytes(credential)
		return result{}, err
	}
	webhookRequested := flagChanged(cmd, "auto-deploy")
	if !webhookRequested {
		webhookRequested, _ = cmd.Flags().GetBool("rotate-webhook-secret")
	}
	guided := false
	var diff []string
	if len(changed) == 0 && !webhookRequested && humanInteractive(cmd) {
		edit, err := a.envUpdateGuided(cmd, current)
		if err != nil {
			clearBytes(credential)
			return result{}, setupPromptError("env update", err)
		}
		patch, credential, credentialName = edit.Patch, edit.Credential, edit.CredentialName
		changed, diff = edit.Changed, edit.Diff
		guided = true
	}
	var webhook webhookUpdate
	if !guided {
		webhook, err = a.readWebhookUpdate(cmd, appName, environment, current, patch)
		if err != nil {
			clearBytes(credential)
			return result{}, err
		}
		if err := a.offerGitHubWebhook(cmd, &webhook); err != nil {
			clearBytes(credential)
			return result{}, setupPromptError("env update", err)
		}
		applyWebhookPatch(&patch, webhook)
		if webhook.Enabled != nil {
			changed = append(changed, "auto_deploy")
		}
	}
	defer clearBytes(credential)
	rotateOnly := webhook.Rotated && len(changed) == 0
	if len(changed) == 0 {
		if guided {
			fmt.Fprintln(os.Stderr, ui.Note("No changes; nothing was submitted."))
			return result{streamed: true}, nil
		}
		if !rotateOnly && !webhook.AlreadyEnabled {
			return result{}, contract.NewFault(contract.InputRequired, "nothing to update", "Pass at least one of --node, --build-node, a source flag, --health-type, --health-path, --health-timeout, --release-command, --process, --auto-deploy or --rotate-webhook-secret.")
		}
	}
	var preview platform.Preview
	if len(changed) > 0 {
		preview, err = a.previewAction(cmd, platform.KindEnvironmentUpdate, "env/"+appName+"/"+environment, patch)
		if err != nil {
			return result{}, err
		}
	}
	resultData := struct {
		App                  string                          `json:"app"`
		Environment          string                          `json:"environment"`
		OperationID          contract.OperationID            `json:"operation_id,omitempty"`
		Changed              []string                        `json:"changed"`
		CredentialSecretName string                          `json:"credential_secret_name,omitempty"`
		WebhookSecretName    string                          `json:"webhook_secret_name,omitempty"`
		WebhookURL           string                          `json:"webhook_url,omitempty"`
		WebhookBranch        string                          `json:"webhook_branch,omitempty"`
		WebhookRotated       bool                            `json:"webhook_rotated,omitempty"`
		GitHubWebhook        *githubWebhookReport            `json:"github_webhook,omitempty"`
		Patch                platform.EnvironmentUpdateInput `json:"patch"`
		DryRun               bool                            `json:"dry_run,omitempty"`
	}{App: appName, Environment: environment, Changed: changed, CredentialSecretName: credentialName,
		WebhookSecretName: webhook.SecretName, WebhookURL: webhook.URL, WebhookBranch: webhook.Branch, WebhookRotated: webhook.Rotated, Patch: patch}
	if webhook.AlreadyEnabled && len(changed) == 0 {
		report := a.setupWebhookWithGitHub(cmd, webhook)
		resultData.GitHubWebhook = report
		plain := "Push to deploy is already enabled for " + appName + "/" + environment + " at " + webhook.URL + ".\nUse --rotate-webhook-secret to replace the secret.\n"
		if report != nil {
			plain += report.summary() + "\n"
		}
		return result{data: resultData, plain: plain}, nil
	}
	if guided {
		body := strings.Join(diff, "\n")
		if body == "" {
			body = "changed: " + strings.Join(changed, ", ")
		}
		for _, warning := range preview.Warnings {
			body += "\nwarning: " + warning
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ui.PanelFit("Changes to "+appName+"/"+environment, output.SafeText(body), 80))
		if dryRun(cmd) {
			return result{streamed: true}, nil
		}
		ok, err := ui.YesNoDefault("Update "+appName+"/"+environment+" now?", "The change is a new immutable revision; no deploy is queued.", true)
		if err != nil {
			return result{}, setupPromptError("env update", err)
		}
		if !ok {
			return result{}, contract.NewFault(contract.Cancelled, "environment update was not confirmed", "Nothing was submitted and no secret value was written.")
		}
	} else if dryRun(cmd) {
		resultData.DryRun = true
		var plain strings.Builder
		fmt.Fprintf(&plain, "Dry run: update %s/%s\n", appName, environment)
		if len(changed) > 0 {
			plain.WriteString(updatePreviewText(preview))
		}
		if webhook.URL != "" {
			if webhook.Rotated {
				fmt.Fprintf(&plain, "Would rotate the push-to-deploy secret for %s (branch %s).\n", webhook.URL, webhook.Branch)
			} else {
				fmt.Fprintf(&plain, "Push to deploy: %s (branch %s)\n", webhook.URL, webhook.Branch)
			}
		}
		if webhook.GitHub {
			plain.WriteString("Would use gh to create or update the repository webhook after the environment change.\n")
		}
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
	if err := a.writeWebhookSecret(cmd, appName, environment, webhook); err != nil {
		return result{}, err
	}
	if rotateOnly {
		report := a.setupWebhookWithGitHub(cmd, webhook)
		resultData.GitHubWebhook = report
		if report == nil || !report.configured() {
			printWebhookOnce(cmd, webhook)
		}
		plain := "Rotated the push-to-deploy secret for " + appName + "/" + environment + "; the new value is active immediately.\n" + webhook.URL + "\n"
		if report != nil {
			plain += report.summary() + "\n"
		}
		return result{data: resultData, plain: plain}, nil
	}
	op, err := a.submitAccepted(cmd, platform.KindEnvironmentUpdate, "env/"+appName+"/"+environment, patch, "")
	if err != nil {
		return result{}, err
	}
	resultData.OperationID = op.ID
	report := a.setupWebhookWithGitHub(cmd, webhook)
	resultData.GitHubWebhook = report
	if report == nil || !report.configured() {
		printWebhookOnce(cmd, webhook)
	}
	if guided {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ui.Card("Environment "+appName+"/"+environment+" updated", [][2]string{
			{"changed", strings.Join(changed, ", ")},
			{"operation", string(op.ID)},
		}, []string{
			ui.Dim("no deploy was queued; the next deploy applies the revision"),
			ui.Dim("next:"),
			"  " + ui.Code("eregion env status "+appName+" "+environment+" --json"),
		}))
		return result{streamed: true}, nil
	}
	plain := fmt.Sprintf("Updated %s/%s desired state.\noperation: %s\nchanged: %s\nnext: eregion env status %s %s --json\n",
		appName, environment, op.ID, strings.Join(changed, ", "), appName, environment)
	if webhook.URL != "" {
		if report != nil && report.configured() {
			plain += "push to deploy: " + webhook.URL + "\n" + report.summary() + "\n"
		} else {
			plain += "push to deploy: " + webhook.URL + " (secret shown once on stderr)\n"
		}
	}
	if report != nil && !report.configured() {
		plain += "warning: " + report.summary() + "\n"
	}
	return result{data: resultData, plain: plain}, nil
}

// domainPlan is the file's explicit domain change set. Primary changes and
// check-path edits are refused because the existing binding commands cannot
// update them in place safely; additions and non-primary removals are applied.
type domainPlan struct {
	Adds    []appdef.Domain
	Updates []appdef.Domain
	Removes []platform.DomainBinding
}

func planDomainChanges(want []appdef.Domain, current []platform.DomainBinding) (domainPlan, error) {
	plan := domainPlan{}
	if len(want) == 0 && len(current) > 0 {
		return plan, errors.New("domains: a definition cannot remove every binding; keep one primary domain")
	}
	currentByName := make(map[string]platform.DomainBinding, len(current))
	for _, binding := range current {
		currentByName[binding.Domain] = binding
	}
	wantByName := make(map[string]appdef.Domain, len(want))
	for _, domain := range want {
		wantByName[domain.Name] = domain
		binding, exists := currentByName[domain.Name]
		if exists {
			if binding.Primary != domain.Primary || binding.CheckPath != domain.CheckPath {
				plan.Updates = append(plan.Updates, domain)
			}
			continue
		}
		plan.Adds = append(plan.Adds, domain)
	}
	for _, binding := range current {
		if _, keep := wantByName[binding.Domain]; keep {
			continue
		}
		plan.Removes = append(plan.Removes, binding)
	}
	return plan, nil
}

func (p domainPlan) changes() int { return len(p.Adds) + len(p.Updates) + len(p.Removes) }

// envUpdateFromFile applies one resolved definition to existing desired state.
// It writes no secret value. Domain additions, promotions and non-final
// removals use the same explicit domain actions; a definition cannot remove
// every binding.
func domainChangeDiff(plan domainPlan) []string {
	out := make([]string, 0, plan.changes())
	for _, domain := range plan.Adds {
		out = append(out, "domain "+domain.Name+": added")
	}
	for _, domain := range plan.Updates {
		mode := "updated"
		if domain.Primary {
			mode = "promoted primary"
		}
		out = append(out, "domain "+domain.Name+": "+mode)
	}
	for _, binding := range plan.Removes {
		out = append(out, "domain "+binding.Domain+": removed")
	}
	return out
}

func validateDomainRemovalConfirmation(removes []platform.DomainBinding, confirmed []string) error {
	want := make(map[string]bool, len(removes))
	for _, binding := range removes {
		want[binding.Domain] = true
	}
	provided := make(map[string]bool, len(confirmed))
	for _, domain := range confirmed {
		domain = strings.TrimSpace(domain)
		if !want[domain] {
			return contract.NewFault(contract.ValidationFailed, "--confirm-domain names "+domain+", which is not being removed", "Pass --confirm-domain with each hostname shown as removed in the preview.")
		}
		provided[domain] = true
	}
	for domain := range want {
		if !provided[domain] {
			return contract.NewFault(contract.ConfirmationRequired, "removing a domain needs confirmation", "Pass --confirm-domain "+domain+" for each domain the definition removes.")
		}
	}
	return nil
}

func (a *app) applyDefinitionDomains(cmd *cobra.Command, appName, environment string, plan domainPlan) ([]contract.OperationID, error) {
	ids := make([]contract.OperationID, 0, plan.changes())
	for _, domain := range plan.Adds {
		input := platform.DomainInput{
			App: appName, Environment: environment, Domain: domain.Name,
			Primary: domain.Primary, CheckPath: domain.CheckPath, Addresses: mainNodeAddresses(cmd),
		}
		op, err := a.submitAccepted(cmd, platform.KindDomainAdd, "env/"+appName+"/"+environment, input, "")
		if err != nil {
			return ids, err
		}
		ids = append(ids, op.ID)
	}
	for _, domain := range plan.Updates {
		input := platform.DomainInput{
			App: appName, Environment: environment, Domain: domain.Name,
			Primary: domain.Primary, CheckPath: domain.CheckPath, Addresses: mainNodeAddresses(cmd),
		}
		op, err := a.submitAccepted(cmd, platform.KindDomainAdd, "env/"+appName+"/"+environment, input, "")
		if err != nil {
			return ids, err
		}
		ids = append(ids, op.ID)
	}
	for _, binding := range plan.Removes {
		input := platform.DomainInput{App: appName, Environment: environment, Domain: binding.Domain, Confirm: binding.Domain}
		op, err := a.submitAccepted(cmd, platform.KindDomainRemove, "env/"+appName+"/"+environment, input, binding.Domain)
		if err != nil {
			return ids, err
		}
		ids = append(ids, op.ID)
	}
	return ids, nil
}

func (a *app) envUpdateFromFile(cmd *cobra.Command, appName, environment, path string) (result, error) {
	if err := validateIdempotencyFlag(cmd); err != nil {
		return result{}, err
	}
	for _, name := range definitionRevisionFlags {
		if flagChanged(cmd, name) {
			return result{}, contract.NewFault(contract.ValidationFailed, "definition file and revision flags conflict", "Use --from alone for revision fields; keep only identity, output, dry-run and idempotency flags.")
		}
	}
	for _, name := range []string{"auto-deploy", "rotate-webhook-secret", "github-webhook"} {
		if flagChanged(cmd, name) {
			return result{}, contract.NewFault(contract.ValidationFailed, "definition file and webhook flags conflict", "Configure push to deploy with env update --auto-deploy without --from.")
		}
	}
	definition, err := readDefinitionFile(path)
	if err != nil {
		return result{}, err
	}
	if definition.App != "" && definition.App != appName {
		return result{}, contract.NewFault(contract.ValidationFailed, "definition app disagrees with the command target", "Use the file's app, or edit the definition.")
	}
	resolved, err := definition.Resolve(environment)
	if err != nil {
		return result{}, definitionFault(err)
	}
	resolved.App = appName
	current, _, err := a.readEnvironment(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	resourceSpecs, err := resolved.ResourceApplySpecs()
	if err != nil {
		return result{}, definitionFault(err)
	}
	domains, err := a.readDomainBindings(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	if err := definitionDomainState(current, domains); err != nil {
		return result{}, err
	}
	domainChanges, err := planDomainChanges(resolved.Domains, domains)
	if err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, "definition cannot be applied", err.Error())
	}
	patch, changed, diff, err := resolved.UpdatePatch(current, domains)
	if err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, "definition cannot be applied", err.Error())
	}
	domainDiff := domainChangeDiff(domainChanges)
	revisionChanged := len(changed) > 0
	if domainChanges.changes() > 0 {
		changed = append(changed, "domains")
		diff = append(diff, domainDiff...)
	}
	data := definitionUpdateResult{
		App: appName, Environment: environment, From: path, Changed: changed, Diff: diff, Patch: patch,
	}
	if !revisionChanged && domainChanges.changes() == 0 && len(resourceSpecs) == 0 {
		data.Plain = "No changes; nothing was submitted."
		return result{data: data, plain: data.Plain}, nil
	}
	var preview platform.Preview
	if revisionChanged {
		preview, err = a.previewAction(cmd, platform.KindEnvironmentUpdate, "env/"+appName+"/"+environment, patch)
		if err != nil {
			return result{}, err
		}
		data.Warnings = preview.Warnings
	}
	if dryRun(cmd) {
		data.DryRun = true
		if revisionChanged {
			data.Preview = &preview
		}
		data.Plain = definitionUpdateText(data, true)
		if humanInteractive(cmd) {
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, ui.PanelFit("Dry run: changes to "+appName+"/"+environment, output.SafeText(data.Plain), 80))
			return result{streamed: true}, nil
		}
		return result{data: data, plain: data.Plain}, nil
	}
	var operationID contract.OperationID
	if revisionChanged || domainChanges.changes() > 0 {
		if revisionChanged && definitionRemoval(changed, diff) {
			// Destruction is never implied by a file edit. The node validates the
			// same target-naming token against current state, so a stale CLI cannot
			// authorize an unseen removal.
			if err := platform.ValidateTargetConfirmation(patch.Target(), flagString(cmd, "confirm")); err != nil {
				return result{}, err
			}
			patch.Confirm = strings.TrimSpace(flagString(cmd, "confirm"))
			data.Patch = patch
		}
		if len(domainChanges.Removes) > 0 {
			confirmed, _ := cmd.Flags().GetStringArray("confirm-domain")
			if err := validateDomainRemovalConfirmation(domainChanges.Removes, confirmed); err != nil {
				return result{}, err
			}
		}
		if humanInteractive(cmd) {
			body := definitionUpdateText(data, true)
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, ui.PanelFit("Changes to "+appName+"/"+environment, output.SafeText(body), 80))
			ok, err := ui.YesNoDefault("Update "+appName+"/"+environment+" now?", "The change is a new immutable revision; no deploy is queued and no secret value is written.", true)
			if err != nil {
				return result{}, setupPromptError("env update", err)
			}
			if !ok {
				return result{}, contract.NewFault(contract.Cancelled, "environment update was not confirmed", "Nothing was submitted.")
			}
		}
		if revisionChanged {
			op, err := a.submitAccepted(cmd, platform.KindEnvironmentUpdate, "env/"+appName+"/"+environment, patch, flagString(cmd, "confirm"))
			if err != nil {
				return result{}, err
			}
			operationID = op.ID
		}
	}
	data.OperationID = operationID
	domainOperations, err := a.applyDefinitionDomains(cmd, appName, environment, domainChanges)
	if err != nil {
		data.Warnings = append(data.Warnings, "The revision was accepted, but domain changes were not fully accepted: "+err.Error()+". Use eregion domain add/remove to retry.")
	}
	data.DomainOperations = domainOperations
	if len(resourceSpecs) > 0 {
		// A resource declaration is explicit even when the revision itself does
		// not change, such as an attach = false declaration used only to
		// provision the container. The applies stay separate accepted actions.
		operations, err := a.provisionDefinitionResources(cmd, resourceSpecs)
		if err != nil {
			data.Warnings = append(data.Warnings, "The revision was accepted, but resource provisioning was not: "+err.Error()+". Use `eregion resource apply` to retry.")
		}
		data.ResourceOperations = operations
	}
	data.Plain = definitionUpdateText(data, false)
	if !revisionChanged {
		data.Plain = "No revision changes.\n"
		for _, id := range data.DomainOperations {
			data.Plain += "domain operation: " + string(id) + "\n"
		}
		for _, id := range data.ResourceOperations {
			data.Plain += "resource operation: " + string(id) + "\n"
		}
		for _, warning := range data.Warnings {
			data.Plain += "warning: " + warning + "\n"
		}
	}
	if humanInteractive(cmd) {
		fmt.Fprintln(os.Stderr, "")
		if len(changed) > 0 {
			fmt.Fprintln(os.Stderr, ui.Card("Environment "+appName+"/"+environment+" updated", [][2]string{
				{"changed", strings.Join(changed, ", ")},
				{"operation", string(operationID)},
			}, []string{
				ui.Dim("no deploy was queued; the next deploy applies the revision"),
				ui.Dim("next:"),
				"  " + ui.Code("eregion env status "+appName+" "+environment+" --json"),
			}))
		} else {
			fmt.Fprintln(os.Stderr, output.SafeText(data.Plain))
		}
		return result{streamed: true}, nil
	}
	return result{data: data, plain: data.Plain}, nil
}

type definitionUpdateResult struct {
	App                string                          `json:"app"`
	Environment        string                          `json:"environment"`
	From               string                          `json:"from"`
	Changed            []string                        `json:"changed"`
	Diff               []string                        `json:"diff,omitempty"`
	Patch              platform.EnvironmentUpdateInput `json:"patch"`
	OperationID        contract.OperationID            `json:"operation_id,omitempty"`
	ResourceOperations []contract.OperationID          `json:"resource_operations,omitempty"`
	DomainOperations   []contract.OperationID          `json:"domain_operations,omitempty"`
	DryRun             bool                            `json:"dry_run,omitempty"`
	Preview            *platform.Preview               `json:"preview,omitempty"`
	Warnings           []string                        `json:"warnings,omitempty"`
	Plain              string                          `json:"-"`
}

func definitionRemoval(changed, diff []string) bool {
	for _, name := range changed {
		if name != "processes" && name != "config" && name != "secret_names" && name != "resources" {
			continue
		}
		for _, line := range diff {
			if (name == "processes" && strings.HasPrefix(line, "process ")) ||
				(strings.HasPrefix(line, name+".") && (strings.HasSuffix(line, ": removed") || strings.HasSuffix(line, ": unset"))) {
				return true
			}
		}
	}
	return false
}

func definitionUpdateText(data definitionUpdateResult, dry bool) string {
	var out strings.Builder
	if dry {
		fmt.Fprintf(&out, "Dry run: update %s/%s from %s\n", data.App, data.Environment, data.From)
	} else {
		fmt.Fprintf(&out, "Updated %s/%s from %s\noperation: %s\n", data.App, data.Environment, data.From, data.OperationID)
	}
	fmt.Fprintf(&out, "changed: %s\n", strings.Join(data.Changed, ", "))
	for _, line := range data.Diff {
		fmt.Fprintf(&out, "  %s\n", line)
	}
	for _, id := range data.DomainOperations {
		fmt.Fprintf(&out, "domain operation: %s\n", id)
	}
	for _, id := range data.ResourceOperations {
		fmt.Fprintf(&out, "resource operation: %s\n", id)
	}
	for _, step := range previewSteps(data.Preview) {
		fmt.Fprintf(&out, "  step: %s\n", step)
	}
	for _, warning := range data.Warnings {
		fmt.Fprintf(&out, "warning: %s\n", warning)
	}
	if dry {
		out.WriteString("No work was accepted.\n")
	} else {
		out.WriteString("No deploy was queued; the next deploy applies the revision.\n")
	}
	return out.String()
}

func previewSteps(preview *platform.Preview) []string {
	if preview == nil {
		return nil
	}
	return preview.Steps
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
			command := commandSpecFromLine(flagString(cmd, "release-command"))
			if command != nil && base.Runtime.ReleaseCommand != nil {
				command.TimeoutSeconds = base.Runtime.ReleaseCommand.TimeoutSeconds
			}
			runtime.ReleaseCommand = command
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
	plain := ui.Header("Environments", 72) + "\nNo environments.\n"
	if len(environments) > 0 {
		rows := make([][]string, 0, len(environments))
		for _, environment := range environments {
			serving := string(environment.ServingRelease)
			if serving == "" {
				serving = "-"
			}
			protection := "unprotected"
			if environment.Protected {
				protection = "protected"
			}
			rows = append(rows, []string{
				environment.App, environment.Name, environment.Node, revisionLabel(environment),
				protection, boolLabel(environment.Deployability.Deployable), strings.Join(environment.Deployability.Missing, ","), serving,
			})
		}
		plain = ui.Header("Environments", 72) + "\n" + writeTable([]string{"APP", "ENVIRONMENT", "NODE", "REVISION", "PROTECTION", "DEPLOYABLE", "MISSING", "SERVING RELEASE"}, rows)
	}
	return result{data: raw, plain: plain}, nil
}

func (a *app) envStatus(cmd *cobra.Command, app, environment string) (result, error) {
	return a.envStatusView(cmd, app, environment)
}
