package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/protect"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

func (a *app) envSpecs() []spec {
	parent := operationMeta(
		"Create and inspect application environments",
		"eregion env list --app api --json",
	)
	parent.WhenToUse = []string{
		"Manage the unit of deployment: its optional Git build recipe, runtime, processes and domains.",
	}

	create := followingMeta(
		operationMeta(
			"Create an environment with its first desired-state revision",
			"eregion env create --app api --name production --node main --git-url https://github.com/acme/api --json",
		),
	)
	create.WhenToUse = []string{
		"Run it bare in a terminal for the guided setup. The first deploy is queued from the new revision unless --no-deploy. JSON and no-TTY take every value from flags and never prompt.",
	}
	create.Mutating, create.DryRun, create.Prompts = true, true, true
	create.InputFlags = []string{
		"app",
		"name",
		"node",
		"build-node",
		"git-url",
		"git-ref",
		"git-credential-file",
		"git-credential-name",
		"github-auth",
		"builder",
		"context-path",
		"build-env",
		"container-port",
		"health-path",
		"process",
		"config",
		"secret",
		"domain",
		"domain-check-path",
		"from",
		"no-deploy",
		"detach",
		"protected",
		"unprotected",
		"buildpack",
		"keep-file",
		"frontend-directory",
		"frontend-script",
		"frontend-start-script",
		"target",
		"go-version",
		"go-build-flags",
		"node-version",
	}
	create.Examples = []string{
		"eregion env create",
		"eregion env create --app api --name staging --node main --git-url https://github.com/acme/api --git-ref next --domain staging.example.com --json",
		"eregion env create --app api --name production --node main --git-url https://github.com/acme/api --github-auth --no-deploy --json",
		"eregion env create --from ./eregion.toml --name pr-42 --no-deploy --json",
	}
	create.Next = []string{
		"eregion env status <app> <environment> --json",
		"eregion domain add <app> <environment> <hostname>",
	}

	update := followingMeta(
		operationMeta(
			"Change an environment as a new desired-state revision",
			"eregion env update api production --git-ref release-2026-10 --json",
		),
	)
	update.WhenToUse = []string{
		"Change source, placement, runtime or the process formation as a new immutable revision. In a terminal with no change flags it walks the current revision prefilled, shows the diff and confirms; with flags or JSON/no TTY it is flag-only. Plain config uses config set; secret names use secret set. No deploy is queued. --github-auth reads the local authenticated gh token for a github.com source as the new read credential; it is opt-in because that token may carry broader scopes. With an authenticated gh, --github-webhook creates or updates the repository webhook for --auto-deploy or --rotate-webhook-secret; manual paste remains the fallback.",
	}
	update.WhenToUse = append(
		update.WhenToUse,
		"Git Buildpacks settings include ordered --buildpack references and optional Go and Node inputs; image artifacts are selected at deploy time, not stored in the revision. --build-env '', --target '' and --buildpack '' clear their respective lists. App export --inspect is not applicable.",
		"With --from, use --dry-run to plan, then submit the exact plan nonce and digest with a stable idempotency key. Removal requires --confirm app/environment. No deploy is queued; --confirm-domain is not supported.",
	)
	update.Mutating, update.DryRun, update.Prompts = true, true, true
	update.InputFlags = []string{
		"node",
		"build-node",
		"git-url",
		"git-ref",
		"git-credential-file",
		"git-credential-name",
		"github-auth",
		"builder",
		"context-path",
		"build-env",
		"health-type",
		"health-path",
		"health-timeout",
		"release-command",
		"process",
		"from",
		"github-webhook",
		"plan-nonce",
		"confirm-plan",
		"idempotency-key",
		"confirm",
		"detach",
		"buildpack",
		"keep-file",
		"frontend-directory",
		"frontend-script",
		"frontend-start-script",
		"target",
		"go-version",
		"go-build-flags",
		"node-version",
	}
	update.Examples = []string{
		"eregion env update api production",
		"eregion env update api production --health-path /readyz --json",
		"eregion env update api production --process 'web=web:8080:2:' --json",
		"eregion env update api production --auto-deploy",
		"eregion env update api production --auto-deploy --github-webhook",
		"eregion env update api production --github-auth --json",
		"eregion env update api production --rotate-webhook-secret",
		"eregion env update api production --from ./eregion.toml --dry-run --json",
		"eregion env update api production --from ./eregion.toml --plan-nonce NONCE --confirm-plan DIGEST --idempotency-key KEY --json",
	}
	update.Next = []string{"eregion env status <app> <environment> --json"}

	list := operationMeta(
		"List environments and their desired state",
		"eregion env list --app api --json",
	)
	list.Examples = []string{"eregion env list --app api --json"}

	status := operationMeta(
		"Show one environment's revision, processes and deployability",
		"eregion env status api production --json",
	)
	status.Examples = []string{"eregion env status api production --json"}
	status.Next = []string{
		"eregion process list <app> <environment> --json",
		"eregion config list <app> <environment> --json",
	}

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
					cmd.Flags().
						Bool("detach", false, "Submit and return without following the create and first deploy")
					addProtectionFlags(cmd)
					addSetupFlags(cmd, true)
				},
				run: a.envCreate,
			},
			{
				use:   "update <app> <environment>",
				meta:  update,
				args:  platformTwoNames(),
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
				args: platformTwoNames(),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.envStatus(cmd, args[0], args[1])
				},
			},
			a.envProtectionSpec(true),
			a.envProtectionSpec(false),
			a.envPromoteSpec(),
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
	meta.HumanFactor = !protected
	meta.InputFlags = []string{"detach"}
	meta.WhenToUse = when
	meta.Next = []string{
		"eregion env status <app> <environment> --json",
		"eregion env delete <app> <environment> --confirm <app>/<environment>",
	}
	return spec{
		use:  use,
		meta: meta,
		args: platformTwoNames(),
		flags: func(cmd *cobra.Command) {
			cmd.Flags().
				Bool("dry-run", false, "Validate and show the protection change without accepting work")
			cmd.Flags().
				Bool("detach", false, "Return immediately after acceptance instead of following in a terminal")
			cmd.Flags().
				String("idempotency-key", "", "Stable submission key; one is generated when empty")
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

func (a *app) runEnvProtection(
	cmd *cobra.Command,
	app, environment string,
	protected bool,
) (result, error) {
	env, err := a.readEnvironment(cmd, app, environment)
	if err != nil {
		return result{}, err
	}
	target, err := platform.NewEnvironmentTarget(app, environment)
	if err != nil {
		return result{}, err
	}
	input := platform.EnvironmentProtectInput{
		App:         app,
		Environment: environment,
		Protected:   protected,
	}
	if dryRun(cmd) {
		preview, err := a.previewAction(
			cmd,
			platform.KindEnvironmentProtect,
			target.String(),
			input,
		)
		if err != nil {
			return result{}, err
		}
		state := "protected"
		if !protected {
			state = "unprotected"
		}
		plain := fmt.Sprintf("Dry run: set %s %s\n%s\n", target.String(), state, preview.Summary)
		return result{
			data: envProtectionResult{
				App:         app,
				Environment: environment,
				Protected:   protected,
				DryRun:      true,
				Preview:     &preview,
			},
			plain: plain,
		}, nil
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
	op, err := a.submitAcceptedWithSecrets(
		cmd,
		platform.KindEnvironmentProtect,
		target.String(),
		input,
		"",
		map[string]string{protect.CodeSecret: code},
	)
	if err != nil {
		return result{}, err
	}
	data := envProtectionResult{
		App:         app,
		Environment: environment,
		Protected:   protected,
		Operation:   &op,
	}
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
	plain := fmt.Sprintf(
		"Environment %s/%s is now %s\noperation: %s\nstate: %s\n",
		app,
		environment,
		state,
		op.ID,
		op.State,
	)
	return result{data: data, plain: plain}, nil
}

func addEnvironmentUpdateFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("node", "", "Application node; empty keeps the current node")
	f.String("build-node", "", "Builder node; empty builds on the application node")
	f.String("git-url", "", "HTTPS git repository URL without credentials")
	f.String("git-ref", "", "Branch or tag; empty clears to the repository default")
	f.String(
		"git-credential-file",
		"",
		"Protected 0600 file containing a new repository read token",
	)
	f.String(
		"git-credential-name",
		"",
		"Vault secret name for the read token; empty clears the name",
	)
	f.Bool(
		"github-auth",
		false,
		"Use the local authenticated gh token as the github.com read credential (opt-in; broader scopes may apply)",
	)
	f.String(
		"builder",
		"",
		"Buildpacks builder image; required for Git builds; empty clears it (no built-in default)",
	)
	f.String("context-path", "", "Build context path inside the repository")
	f.StringArray(
		"build-env",
		nil,
		"Buildpacks environment KEY=VALUE (repeatable; replaces the map)",
	)
	addBuildpackFlags(cmd)
	f.String("health-type", "", "Health check type: http or tcp")
	f.String("health-path", "", "Loopback health path")
	f.Int("health-timeout", 0, "Health timeout in seconds (1–300)")
	f.String(
		"release-command",
		"",
		"Release command run once per release (quoted argv, no shell); empty clears it",
	)
	f.StringArray(
		"process",
		nil,
		"Complete process formation entry NAME=KIND:PORT:REPLICAS:COMMAND ARGS (repeatable; replaces all processes)",
	)
	f.String(
		"from",
		"",
		"Read an eregion.toml definition as the complete supported desired state; config is plain, secrets are names only; revision and webhook flags cannot be combined",
	)
	f.Bool(
		"auto-deploy",
		false,
		"Enable push to deploy for a GitHub source; --auto-deploy=false disables it",
	)
	f.Bool(
		"rotate-webhook-secret",
		false,
		"Replace the push-to-deploy secret and show the new one once",
	)
	f.Bool(
		"github-webhook",
		false,
		"Create or update the repository webhook with an authenticated gh; needs --auto-deploy or --rotate-webhook-secret",
	)
	f.String(
		"confirm",
		"",
		"Target-naming confirmation for destructive definition removals: app/environment",
	)
	f.StringArray("confirm-domain", nil, "Not supported with --from; use --confirm app/environment")
	f.String("plan-nonce", "", "Nonce from the approved definition plan")
	f.String("confirm-plan", "", "Exact digest from the approved definition plan")
	f.String("idempotency-key", "", "Stable submission key; one is generated when empty")
	f.Bool(
		"dry-run",
		false,
		"Validate and show the new revision without accepting work or writing secrets",
	)
	f.Bool("detach", false, "Return after accepting a definition operation")
	followFlags(cmd)
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
	spec, err := a.envCreateSpec(cmd, ans)
	if err != nil {
		return result{}, err
	}
	follow := !boolFlag(cmd, "detach") &&
		(streamsNDJSON(cmd) || (interactive && ans.Deploy && followsByDefault(cmd)))
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	if err := a.checkEnvCreateTarget(cmd, ans); err != nil {
		return result{}, err
	}
	input := platform.EnvironmentCreateInput{
		App:             ans.App,
		EnvironmentSpec: spec,
		Deploy:          ans.Deploy,
	}
	target, err := platform.NewAppTarget(ans.App)
	if err != nil {
		return result{}, err
	}
	preview, deferred, plan, err := a.envCreatePlan(cmd, ans, input, target, interactive)
	if err != nil {
		return result{}, err
	}
	if err := confirmEnvCreate(ans, interactive, dryRun(cmd)); err != nil {
		return result{}, err
	}
	summary := newEnvCreateSummary(ans, spec, preview, deferred, cmd)
	if dryRun(cmd) {
		summary.DryRun, summary.Preview, summary.plan = true, &preview, plan
		if interactive {
			return result{streamed: true}, nil
		}
		return result{data: summary, plain: summary.plainText()}, nil
	}
	if err := a.prepareEnvCreateSecrets(cmd, ans, spec, preview, &summary); err != nil {
		return result{}, err
	}
	op, err := a.submitAccepted(cmd, platform.KindEnvironmentCreate, target.String(), input, "")
	if err != nil {
		return result{}, err
	}
	summary.OperationID = op.ID
	summary.FirstDeployOperationID = op.FirstDeployOperationID
	if err := a.submitEnvCreateResources(cmd, ans, op.ID, &summary); err != nil {
		return result{}, err
	}
	if interactive {
		return a.envCreateInteractiveResult(cmd, ans, spec, summary, op, target)
	}
	if streamsNDJSON(cmd) && !boolFlag(cmd, "detach") {
		return a.envCreateNDJSONResult(cmd, ans, op, target)
	}
	return result{data: summary, plain: summary.plainText()}, nil
}

func (a *app) envCreateSpec(cmd *cobra.Command, ans *setupAnswers) (platform.EnvironmentSpec, error) {
	if ans.ImageRepository != "" {
		appRecord, err := a.findApp(cmd, ans.App)
		if err != nil {
			return platform.EnvironmentSpec{}, err
		}
		if appRecord.ImageRepository != ans.ImageRepository {
			return platform.EnvironmentSpec{}, contract.NewFault(
				contract.ValidationFailed,
				"environment definition cannot change the app image repository",
				"The image repository is app-level immutable metadata; remove image_repository from the environment's definition.",
			)
		}
	}
	spec, err := ans.environmentSpec(cmd)
	if err != nil {
		return platform.EnvironmentSpec{}, err
	}
	specs, err := applyGitHubAuthToSpecs(cmd, ans, []platform.EnvironmentSpec{spec})
	if err != nil {
		return platform.EnvironmentSpec{}, err
	}
	if err := validateEnvironmentGitCredentialRoles(specs); err != nil {
		return platform.EnvironmentSpec{}, err
	}
	return specs[0], nil
}

func (a *app) checkEnvCreateTarget(cmd *cobra.Command, ans *setupAnswers) error {
	if err := validateIdempotencyFlag(cmd); err != nil {
		return err
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if key != "" {
		return nil
	}
	// A named key reaches preview first: preview knows whether it replays
	// an accepted submission and must skip the same conflict checks.
	exists, err := a.appExists(cmd, ans.App)
	if err != nil {
		return err
	}
	if !exists {
		return contract.NewFault(
			contract.NotFound,
			"app "+ans.App+" was not found",
			"Create it with eregion app create, or choose an existing app.",
		)
	}
	exists, err = a.environmentExists(cmd, ans.App, ans.Environment)
	if err != nil {
		return err
	}
	if exists {
		return contract.NewFault(
			contract.Conflict,
			"environment "+ans.App+"/"+ans.Environment+" already exists",
			"Change it with eregion env update, or choose another name.",
		)
	}
	return nil
}

func (a *app) envCreatePlan(
	cmd *cobra.Command,
	ans *setupAnswers,
	input platform.EnvironmentCreateInput,
	target platform.Target,
	interactive bool,
) (platform.Preview, []deferredDefinitionResource, string, error) {
	preview, err := a.previewAction(cmd, platform.KindEnvironmentCreate, target.String(), input)
	if err != nil {
		return platform.Preview{}, nil, "", err
	}
	var deferred []deferredDefinitionResource
	if dryRun(cmd) && !preview.Replay {
		_, deferred, err = a.bootstrapDefinitionResources(cmd, ans.ResourceSpecs, true)
		if err != nil {
			return platform.Preview{}, nil, "", err
		}
		for _, item := range deferred {
			preview.Warnings = append(preview.Warnings, item.warning())
		}
	}
	plan := setupPlan(ans, preview, cmd)
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(
			os.Stderr,
			ui.PanelFit("Plan for environment "+ans.App+"/"+ans.Environment, plan, 80),
		)
	}
	return preview, deferred, plan, nil
}

func confirmEnvCreate(ans *setupAnswers, interactive, dry bool) error {
	if !interactive || dry {
		return nil
	}
	ok, err := ui.YesNoDefault(
		"Create environment "+ans.App+"/"+ans.Environment+" now?",
		"The environment and its first revision are accepted in one submission; a requested first deploy is queued from it.",
		true,
	)
	if err != nil {
		return setupPromptError("env create", err)
	}
	if !ok {
		return contract.NewFault(
			contract.Cancelled,
			"environment creation was not confirmed",
			"Nothing was written; the plan above is unchanged.",
		)
	}
	return nil
}

func newEnvCreateSummary(
	ans *setupAnswers,
	spec platform.EnvironmentSpec,
	preview platform.Preview,
	deferred []deferredDefinitionResource,
	cmd *cobra.Command,
) setupResult {
	summary := setupResult{
		App:                  ans.App,
		Environment:          ans.Environment,
		Protected:            protectedSpec(spec),
		Node:                 ans.Node,
		Source:               spec.Source,
		Processes:            spec.Processes,
		ConfigKeys:           sortedKeys(spec.Config),
		CredentialSecretName: ans.CredentialName,
		Domain:               ans.Domain,
		DomainCheckPath:      ans.DomainCheckPath,
		Domains:              ans.Domains,
		Deploy:               ans.Deploy,
		Warnings:             preview.Warnings,
		DeferredResources:    deferred,
		created:              "environment",
	}
	if record, ok := domainDNSRecord(ans.Domain, cmd); ok {
		summary.DNSRecord = &record
	}
	if ans.Deploy && ans.Domain == "" {
		summary.Warnings = append(
			summary.Warnings,
			"The first deploy has no domain; the deployability gate refuses it until a primary domain is bound.",
		)
	}
	if len(ans.ResourceSpecs) > 0 {
		summary.Warnings = append(
			summary.Warnings,
			"A first deploy was not queued because the definition declares managed resources; wait for resource health, then run `eregion deploy "+ans.App+" "+ans.Environment+"`.",
		)
	}
	summary.SecretNames = setupSecretNames(ans)
	return summary
}

func (a *app) prepareEnvCreateSecrets(
	cmd *cobra.Command,
	ans *setupAnswers,
	spec platform.EnvironmentSpec,
	preview platform.Preview,
	summary *setupResult,
) error {
	if preview.Replay {
		// The original submission already wrote the values; a replay only
		// needs the same accepted operation.
		summary.SecretNames = setupSecretNames(ans)
		return nil
	}
	if err := a.fillGitHubAuthCredential(cmd, ans); err != nil {
		return err
	}
	if _, err := a.writeSetupSecrets(cmd, ans, spec); err != nil {
		return err
	}
	summary.SecretNames = setupSecretNames(ans)
	return nil
}

func (a *app) submitEnvCreateResources(
	cmd *cobra.Command,
	ans *setupAnswers,
	operationID contract.OperationID,
	summary *setupResult,
) error {
	if len(ans.ResourceSpecs) == 0 {
		return nil
	}
	resourceIDs, deferred, err := a.bootstrapDefinitionResources(cmd, ans.ResourceSpecs, false)
	if err != nil {
		return resourceAfterAcceptedFault(err, operationID)
	}
	summary.ResolvedResourceIDs = resourceIDs
	summary.DeferredResources = deferred
	for _, item := range deferred {
		summary.Warnings = append(summary.Warnings, item.warning())
	}
	if len(resourceIDs) > 0 {
		summary.Warnings = append(
			summary.Warnings,
			"Managed resources were accepted for provisioning; they are not ready until their health checks pass, and the first deploy waits for attached resources.",
		)
	}
	return nil
}

func (a *app) envCreateInteractiveResult(
	cmd *cobra.Command,
	ans *setupAnswers,
	spec platform.EnvironmentSpec,
	summary setupResult,
	op contract.Operation,
	target platform.Target,
) (result, error) {
	if ans.Deploy && followsByDefault(cmd) {
		client, err := a.client(cmd)
		if err != nil {
			return result{}, err
		}
		return a.followGuidedCreate(
			cmd,
			client,
			op,
			target.String(),
			a.deployOutcome(ans.App, ans.Environment),
		)
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
			rows = append(
				rows,
				[2]string{"first deploy operation", string(summary.FirstDeployOperationID)},
			)
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
	for _, item := range summary.DeferredResources {
		footer = append(footer, item.warning())
	}
	if summary.FirstDeployOperationID != "" {
		footer = append(
			footer,
			"  "+ui.Code("eregion operations follow "+string(summary.FirstDeployOperationID)),
		)
	}
	fmt.Fprintln(
		os.Stderr,
		ui.Card("Environment "+ans.App+"/"+ans.Environment+" created", rows, footer),
	)
	return result{streamed: true}, nil
}

func (a *app) envCreateNDJSONResult(
	cmd *cobra.Command,
	ans *setupAnswers,
	op contract.Operation,
	target platform.Target,
) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	if ans.Deploy && op.FirstDeployOperationID != "" {
		return a.followGuidedCreate(
			cmd,
			client,
			op,
			target.String(),
			a.deployOutcome(ans.App, ans.Environment),
		)
	}
	return a.followAccepted(cmd, client, op)
}

type envUpdateState struct {
	appName          string
	environment      string
	current          platform.Environment
	patch            platform.EnvironmentUpdateInput
	credential       []byte
	credentialName   string
	changed          []string
	diff             []string
	webhookRequested bool
	guided           bool
	webhook          webhookUpdate
	preview          platform.Preview
	report           *githubWebhookReport
}

type envUpdateResult struct {
	App                  string                          `json:"app"`
	Environment          string                          `json:"environment"`
	OperationID          contract.OperationID            `json:"operation_id,omitempty"`
	Changed              []string                        `json:"changed"`
	CredentialSecretName string                          `json:"credential_secret_name,omitempty"`
	WebhookSecretName    string                          `json:"webhook_secret_name,omitempty"`
	WebhookURL           string                          `json:"webhook_url,omitempty"`
	WebhookBranch        string                          `json:"webhook_branch,omitempty"`
	WebhookRotated       bool                            `json:"webhook_rotated,omitempty"`
	WebhookReplay        bool                            `json:"webhook_replay,omitempty"`
	WebhookActivated     bool                            `json:"webhook_secret_activated,omitempty"`
	WebhookMessage       string                          `json:"webhook_message,omitempty"`
	GitHubWebhook        *githubWebhookReport            `json:"github_webhook,omitempty"`
	Patch                platform.EnvironmentUpdateInput `json:"patch"`
	DryRun               bool                            `json:"dry_run,omitempty"`
}

func (a *app) readEnvUpdateState(cmd *cobra.Command, args []string) (*envUpdateState, error) {
	if err := validateFlagBasedEnvUpdate(cmd); err != nil {
		return nil, err
	}
	appName, environment := args[0], args[1]
	current, err := a.readEnvironment(cmd, appName, environment)
	if err != nil {
		return nil, err
	}
	patch, credentialName, changed, err := a.buildEnvironmentUpdate(cmd, current)
	if err != nil {
		return nil, err
	}
	webhookRequested := flagChanged(cmd, "auto-deploy")
	if !webhookRequested {
		webhookRequested, _ = cmd.Flags().GetBool("rotate-webhook-secret")
	}
	state := &envUpdateState{
		appName: appName, environment: environment, current: current,
		patch: patch, credentialName: credentialName, changed: changed,
		webhookRequested: webhookRequested,
	}
	if len(changed) == 0 && !webhookRequested && humanInteractive(cmd) {
		if err := a.applyGuidedEnvUpdate(cmd, state); err != nil {
			return nil, err
		}
	}
	if !state.guided {
		if err := a.applyEnvUpdateWebhook(cmd, state); err != nil {
			clearBytes(state.credential)
			return nil, err
		}
	}
	if err := validateEnvUpdateCredentialSelection(state); err != nil {
		clearBytes(state.credential)
		return nil, err
	}
	return state, nil
}

func validateFlagBasedEnvUpdate(cmd *cobra.Command) error {
	for _, name := range []string{"from", "plan-nonce", "confirm-plan", "confirm-domain", "detach", "interactive", "ascii", "reduced-motion", "poll-interval"} {
		if flagChanged(cmd, name) {
			return contract.NewFault(
				contract.Usage,
				"--"+name+" requires a definition with --from",
				"Use definition flags with --from, or omit them for flag-based updates.",
			)
		}
	}
	if streamsNDJSON(cmd) {
		return contract.NewFault(
			contract.InvalidOutputMode,
			"flag-based environment updates do not stream NDJSON",
			"Use plain or JSON output, or use --from to follow a definition operation.",
		)
	}
	if err := validateIdempotencyFlag(cmd); err != nil {
		return err
	}
	if err := validateGitHubAuthFlags(cmd); err != nil {
		return err
	}
	return validateGitHubWebhookFlags(cmd)
}

func (a *app) applyGuidedEnvUpdate(cmd *cobra.Command, state *envUpdateState) error {
	edit, err := a.envUpdateGuided(cmd, state.current)
	if err != nil {
		return setupPromptError("env update", err)
	}
	state.patch, state.credential, state.credentialName = edit.Patch, edit.Credential, edit.CredentialName
	state.changed, state.diff = edit.Changed, edit.Diff
	state.guided = true
	return nil
}

func (a *app) applyEnvUpdateWebhook(cmd *cobra.Command, state *envUpdateState) error {
	webhook, err := a.readWebhookUpdate(cmd, state.appName, state.environment, state.current, state.patch)
	if err != nil {
		return err
	}
	state.webhook = webhook
	applyWebhookPatch(&state.patch, state.webhook)
	if webhook.Enabled != nil && envUpdateAutoDeployChanged(state.current, webhook) {
		state.changed = append(state.changed, "auto_deploy")
	}
	if webhook.Rotated {
		state.changed = append(state.changed, "webhook_secret_rotation")
	}
	return nil
}

func envUpdateAutoDeployChanged(current platform.Environment, webhook webhookUpdate) bool {
	enabled := *webhook.Enabled
	currentAutoDeploy := current.Revision != nil && current.Revision.AutoDeploy
	currentName := ""
	if current.Revision != nil {
		currentName = current.Revision.WebhookSecretName
	}
	return current.Revision == nil || currentAutoDeploy != enabled ||
		(enabled && currentName != webhook.SecretName)
}

func validateEnvUpdateCredentialSelection(state *envUpdateState) error {
	runtimeSecrets, webhookSecret := revisionCredentialRoles(state.current)
	if state.patch.WebhookSecretName != nil {
		webhookSecret = *state.patch.WebhookSecretName
	}
	validateSelected := state.credentialName != ""
	if state.guided && len(state.credential) == 0 {
		existing := ""
		if state.current.Revision != nil {
			existing = state.current.Revision.Source.CredentialSecretName
		}
		validateSelected = state.credentialName != existing
	}
	if !validateSelected {
		return nil
	}
	return validateGitCredentialRole(state.credentialName, runtimeSecrets, webhookSecret)
}

func (a *app) envUpdateNoChanges(state *envUpdateState) (result, bool, error) {
	if len(state.changed) > 0 {
		return result{}, false, nil
	}
	if state.guided {
		fmt.Fprintln(os.Stderr, ui.Note("No changes; nothing was submitted."))
		return result{streamed: true}, true, nil
	}
	if state.webhookRequested {
		return result{}, false, nil
	}
	return result{}, true, contract.NewFault(
		contract.InputRequired,
		"nothing to update",
		"Pass at least one of --node, --build-node, a source flag, --health-type, --health-path, --health-timeout, --release-command, --process, --auto-deploy or --rotate-webhook-secret.",
	)
}

func (a *app) envUpdatePreview(cmd *cobra.Command, state *envUpdateState) (platform.Preview, error) {
	if len(state.changed) == 0 && !state.webhookRequested {
		return platform.Preview{}, nil
	}
	return a.previewAction(
		cmd,
		platform.KindEnvironmentUpdate,
		"env/"+state.appName+"/"+state.environment,
		state.patch,
	)
}

func (a *app) envUpdate(cmd *cobra.Command, args []string) (result, error) {
	if from := flagString(cmd, "from"); from != "" {
		return a.envUpdateFromFile(cmd, args[0], args[1], from)
	}
	state, err := a.readEnvUpdateState(cmd, args)
	if err != nil {
		return result{}, err
	}
	defer func() { clearBytes(state.credential) }()
	if res, done, err := a.envUpdateNoChanges(state); done {
		return res, err
	}
	preview, err := a.envUpdatePreview(cmd, state)
	if err != nil {
		return result{}, err
	}
	state.preview = preview
	resultData := newEnvUpdateResult(state)
	if res, done, err := presentEnvUpdateGuided(cmd, state); done {
		return res, err
	}
	if dryRun(cmd) {
		return envUpdateDryRunResult(state, resultData), nil
	}
	if err := a.applyEnvUpdateCredentials(cmd, state); err != nil {
		return result{}, err
	}
	op, receipt, err := a.submitEnvUpdate(cmd, state)
	if err != nil {
		return result{}, err
	}
	resultData.OperationID = op.ID
	if res, done, err := a.finishEnvUpdateWebhook(cmd, state, receipt, op, &resultData); done {
		return res, err
	}
	if state.guided {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(
			os.Stderr,
			ui.Card("Environment "+state.appName+"/"+state.environment+" updated", [][2]string{
				{"changed", strings.Join(state.changed, ", ")},
				{"operation", string(op.ID)},
			}, []string{
				ui.Dim("no deploy was queued; the next deploy applies the revision"),
				ui.Dim("next:"),
				"  " + ui.Code("eregion env status "+state.appName+" "+state.environment+" --json"),
			}),
		)
		return result{streamed: true}, nil
	}
	return envUpdatePlainResult(state, op, resultData), nil
}

func newEnvUpdateResult(state *envUpdateState) envUpdateResult {
	return envUpdateResult{
		App:                  state.appName,
		Environment:          state.environment,
		Changed:              state.changed,
		CredentialSecretName: state.credentialName,
		WebhookSecretName:    state.webhook.SecretName,
		WebhookURL:           state.webhook.URL,
		WebhookBranch:        state.webhook.Branch,
		WebhookRotated:       state.webhook.Rotated,
		Patch:                state.patch,
	}
}

func presentEnvUpdateGuided(cmd *cobra.Command, state *envUpdateState) (result, bool, error) {
	if !state.guided {
		return result{}, false, nil
	}
	body := strings.Join(state.diff, "\n")
	if body == "" {
		body = "changed: " + strings.Join(state.changed, ", ")
	}
	for _, warning := range state.preview.Warnings {
		body += "\nwarning: " + warning
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(
		os.Stderr,
		ui.PanelFit("Changes to "+state.appName+"/"+state.environment, output.SafeText(body), 80),
	)
	if dryRun(cmd) {
		return result{streamed: true}, true, nil
	}
	ok, err := ui.YesNoDefault(
		"Update "+state.appName+"/"+state.environment+" now?",
		"The change is a new immutable revision; no deploy is queued.",
		true,
	)
	if err != nil {
		return result{}, true, setupPromptError("env update", err)
	}
	if !ok {
		return result{}, true, contract.NewFault(
			contract.Cancelled,
			"environment update was not confirmed",
			"Nothing was submitted and no secret value was written.",
		)
	}
	return result{}, false, nil
}

func envUpdateDryRunResult(state *envUpdateState, resultData envUpdateResult) result {
	resultData.DryRun = true
	var plain strings.Builder
	fmt.Fprintf(&plain, "Dry run: update %s/%s\n", state.appName, state.environment)
	if len(state.changed) > 0 {
		plain.WriteString(updatePreviewText(state.preview))
	}
	if state.webhook.URL != "" {
		if state.webhook.Rotated {
			fmt.Fprintf(
				&plain,
				"Would rotate the push-to-deploy secret for %s (branch %s).\n",
				state.webhook.URL,
				state.webhook.Branch,
			)
		} else {
			fmt.Fprintf(&plain, "Push to deploy: %s (branch %s)\n", state.webhook.URL, state.webhook.Branch)
		}
	}
	if state.webhook.GitHub {
		plain.WriteString(
			"Would use gh to create or update the repository webhook after the environment change.\n",
		)
	}
	return result{data: resultData, plain: plain.String()}
}

func (a *app) applyEnvUpdateCredentials(cmd *cobra.Command, state *envUpdateState) error {
	if err := a.prepareWebhookUpdate(cmd, &state.webhook, state.preview.Replay); err != nil {
		return err
	}
	if !state.preview.Replay && flagChanged(cmd, "git-credential-file") {
		value, err := readProtectedSecretFile(flagString(cmd, "git-credential-file"))
		if err != nil {
			return err
		}
		clearBytes(state.credential)
		state.credential = value
		a.registerSecretValue(state.credential)
	}
	if githubAuthRequested(cmd) && state.credentialName != "" && !state.preview.Replay {
		token, err := acquireGitHubAuthToken(cmd.Context())
		if err != nil {
			return err
		}
		a.registerSecretValue(token)
		state.credential = token
	}
	if state.preview.Replay || state.credentialName == "" || len(state.credential) == 0 {
		return nil
	}
	namespace, err := vault.NewNamespace(state.appName, state.environment, state.credentialName)
	if err != nil {
		return err
	}
	return a.putSecret(cmd, namespace, state.credential)
}

func (a *app) submitEnvUpdate(cmd *cobra.Command, state *envUpdateState) (contract.Operation, submissionReceipt, error) {
	if state.webhookRequested {
		return a.submitWebhookUpdate(cmd, state.appName, state.environment, state.patch, state.webhook)
	}
	op, err := a.submitAccepted(
		cmd,
		platform.KindEnvironmentUpdate,
		"env/"+state.appName+"/"+state.environment,
		state.patch,
		"",
	)
	return op, submissionReceipt{}, err
}

func (a *app) finishEnvUpdateWebhook(
	cmd *cobra.Command,
	state *envUpdateState,
	receipt submissionReceipt,
	op contract.Operation,
	resultData *envUpdateResult,
) (result, bool, error) {
	if !state.webhookRequested {
		return result{}, false, nil
	}
	resultData.WebhookReplay = receipt.Replay
	resultData.WebhookRotated = state.webhook.Rotated && !receipt.Replay
	resultData.WebhookActivated = receipt.WebhookSecretActivated
	if receipt.Replay {
		resultData.WebhookRotated = false
		if len(state.webhook.Secret) > 0 {
			clearBytes(state.webhook.Secret)
			state.webhook.Secret = nil
			resultData.WebhookMessage = "Concurrent admission replay: a local candidate secret was generated but was not activated, printed or installed; the winning submission owns the secret. The GitHub webhook was NOT retried or verified."
			plain := fmt.Sprintf(
				"Concurrent admission replay: returned the accepted operation %s. The local candidate secret was discarded and not printed; the winning submission owns the secret, and the GitHub webhook was NOT retried or verified.\nnext: eregion env status %s %s --json\n",
				op.ID,
				state.appName,
				state.environment,
			)
			return result{data: *resultData, plain: plain}, true, nil
		}
		resultData.WebhookMessage = "Idempotent replay: the original operation is returned; the GitHub webhook was NOT retried or verified."
		plain := fmt.Sprintf(
			"Idempotent replay: returned the original accepted operation %s; no secret was generated and the GitHub webhook was NOT retried or verified.\nnext: eregion env status %s %s --json\n",
			op.ID,
			state.appName,
			state.environment,
		)
		return result{data: *resultData, plain: plain}, true, nil
	}
	if len(state.webhook.Secret) > 0 && !receipt.WebhookSecretActivated {
		return result{}, true, contract.NewFault(
			contract.Internal,
			"the operation was admitted without activating the requested webhook secret",
			"Do not retry with the same key; inspect the operation and report the command and binary version.",
		)
	}
	if state.webhook.GitHub {
		report := a.setupWebhookWithGitHub(cmd, state.webhook)
		state.report = report
		resultData.GitHubWebhook = report
		if report != nil && !report.configured() {
			activation := "accepted"
			if len(state.changed) == 0 && !state.webhook.Rotated {
				activation = "already_enabled"
			}
			return result{}, true, webhookPartialFault(
				state.appName,
				state.environment,
				op.ID,
				activation,
				state.webhook.Rotated,
				*report,
			)
		}
	} else if len(state.webhook.Secret) > 0 {
		printWebhookOnce(cmd, state.webhook)
	}
	return result{}, false, nil
}

func envUpdatePlainResult(state *envUpdateState, op contract.Operation, resultData envUpdateResult) result {
	if envUpdateRotationOnly(state) {
		return envUpdateRotationResult(state, op, resultData)
	}
	if state.webhookRequested && len(state.changed) == 0 {
		return envUpdateNoStateChangeResult(state, op, resultData)
	}
	plain := fmt.Sprintf(
		"Updated %s/%s desired state.\noperation: %s\nchanged: %s\nnext: eregion env status %s %s --json\n",
		state.appName,
		state.environment,
		op.ID,
		strings.Join(state.changed, ", "),
		state.appName,
		state.environment,
	)
	if state.webhook.URL != "" {
		if state.report != nil && state.report.configured() {
			plain += "push to deploy: " + state.webhook.URL + "\n" + state.report.summary() + "\n"
		} else if len(state.webhook.Secret) > 0 {
			plain += "push to deploy: " + state.webhook.URL + " (secret shown once on stderr)\n"
		} else {
			plain += "push to deploy: " + state.webhook.URL + "\n"
		}
	}
	return result{data: resultData, plain: plain}
}

func envUpdateRotationOnly(state *envUpdateState) bool {
	if !state.webhook.Rotated {
		return false
	}
	for _, field := range state.changed {
		if field != "webhook_secret_rotation" {
			return false
		}
	}
	return true
}

func envUpdateRotationResult(state *envUpdateState, op contract.Operation, resultData envUpdateResult) result {
	plain := fmt.Sprintf(
		"Rotated the push-to-deploy webhook secret for %s/%s; desired state is unchanged.\noperation: %s\nnext: eregion env status %s %s --json\n",
		state.appName,
		state.environment,
		op.ID,
		state.appName,
		state.environment,
	)
	if state.report != nil && state.report.configured() {
		plain += "push to deploy: " + state.webhook.URL + "\n" + state.report.summary() + "\n"
	}
	return result{data: resultData, plain: plain}
}

func envUpdateNoStateChangeResult(state *envUpdateState, op contract.Operation, resultData envUpdateResult) result {
	status := "enabled"
	if state.webhook.Enabled != nil && !*state.webhook.Enabled {
		status = "disabled"
	}
	plain := fmt.Sprintf(
		"Push to deploy is already %s for %s/%s; no desired state changed.\noperation: %s\nnext: eregion env status %s %s --json\n",
		status,
		state.appName,
		state.environment,
		op.ID,
		state.appName,
		state.environment,
	)
	if state.report != nil && state.report.configured() {
		plain += "push to deploy: " + state.webhook.URL + "\n" + state.report.summary() + "\n"
	}
	return result{data: resultData, plain: plain}
}

func (a *app) envUpdateFromFile(
	cmd *cobra.Command,
	appName, environment, _ string,
) (result, error) {
	if githubAuthRequested(cmd) {
		return result{}, contract.NewFault(
			contract.Usage,
			"--github-auth cannot be combined with --from",
			"Set credentials separately without --from.",
		)
	}
	for _, name := range definitionRevisionFlags {
		if flagChanged(cmd, name) {
			return result{}, contract.NewFault(
				contract.ValidationFailed,
				"definition file and revision flags conflict",
				"Use --from alone for revision fields.",
			)
		}
	}
	for _, name := range []string{"auto-deploy", "rotate-webhook-secret", "github-webhook"} {
		if flagChanged(cmd, name) {
			return result{}, contract.NewFault(
				contract.ValidationFailed,
				"definition file and webhook flags conflict",
				"Configure push to deploy separately without --from.",
			)
		}
	}
	if flagChanged(cmd, "confirm-domain") {
		return result{}, contract.NewFault(
			contract.Usage,
			"--confirm-domain is not supported with --from",
			"Use --confirm "+appName+"/"+environment+" for destructive removals.",
		)
	}
	return a.definitionApplyFor(cmd, appName, environment)
}

// buildEnvironmentUpdate merges only the supplied flags over the current
// revision. Source and runtime are replaced wholesale by the action, so their
// unchanged parts are carried over here.
func (a *app) buildEnvironmentUpdate(
	cmd *cobra.Command,
	current platform.Environment,
) (platform.EnvironmentUpdateInput, string, []string, error) {
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
	if err := updateNodeFields(cmd, &patch, &changed); err != nil {
		return patch, "", nil, err
	}
	source, sourceChanged, credentialName, err := a.readUpdateSource(cmd, base.Source, current)
	if err != nil {
		return patch, "", nil, err
	}
	if sourceChanged {
		patch.Source = &source
		changed = append(changed, "source")
	}
	if err := updateRuntimeFields(cmd, current, base, &patch, &changed); err != nil {
		return patch, "", nil, err
	}
	if err := updateProcessFields(cmd, &patch, &changed); err != nil {
		return patch, "", nil, err
	}
	return patch, credentialName, changed, nil
}

func updateNodeFields(cmd *cobra.Command, patch *platform.EnvironmentUpdateInput, changed *[]string) error {
	if flagChanged(cmd, "node") {
		value := flagString(cmd, "node")
		if err := platform.ValidateName(value); err != nil {
			return err
		}
		patch.Node = &value
		*changed = append(*changed, "node")
	}
	if !flagChanged(cmd, "build-node") {
		return nil
	}
	value := flagString(cmd, "build-node")
	if value != "" {
		if err := platform.ValidateName(value); err != nil {
			return err
		}
	}
	patch.BuildNode = &value
	*changed = append(*changed, "build_node")
	return nil
}

func updateRuntimeFields(
	cmd *cobra.Command,
	current platform.Environment,
	base platform.RevisionState,
	patch *platform.EnvironmentUpdateInput,
	changed *[]string,
) error {
	runtimeChanged := flagChanged(cmd, "health-type") || flagChanged(cmd, "health-path") ||
		flagChanged(cmd, "health-timeout") || flagChanged(cmd, "release-command")
	if !runtimeChanged {
		return nil
	}
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
		command, err := commandSpecFromLine(flagString(cmd, "release-command"))
		if err != nil {
			return err
		}
		if command != nil && base.Runtime.ReleaseCommand != nil {
			command.TimeoutSeconds = base.Runtime.ReleaseCommand.TimeoutSeconds
		}
		runtime.ReleaseCommand = command
	}
	patch.Runtime = &runtime
	*changed = append(*changed, "runtime")
	return nil
}

func updateProcessFields(cmd *cobra.Command, patch *platform.EnvironmentUpdateInput, changed *[]string) error {
	if !flagChanged(cmd, "process") {
		return nil
	}
	processes, err := parseProcessSpecs(flagArray(cmd, "process"))
	if err != nil {
		return err
	}
	patch.Processes = &processes
	*changed = append(*changed, "processes")
	return nil
}

func (a *app) readUpdateSource(
	cmd *cobra.Command,
	base platform.SourceBinding,
	current platform.Environment,
) (platform.SourceBinding, bool, string, error) {
	githubAuth := githubAuthRequested(cmd)
	runtimeSecrets, webhookSecret := revisionCredentialRoles(current)
	if githubAuth && flagChanged(cmd, "git-credential-file") {
		return platform.SourceBinding{}, false, "", contract.NewFault(
			contract.Usage,
			"--github-auth and --git-credential-file choose different credentials",
			"Use --github-auth for the local gh token, or --git-credential-file for an explicit protected token file, not both.",
		)
	}
	if !updateSourceChanged(cmd, githubAuth) {
		return platform.SourceBinding{}, false, "", nil
	}
	source := platform.SourceBinding{}
	if base.URL != "" {
		source = platform.CloneRevisionState(platform.RevisionState{Source: base}).Source
	}
	if flagChanged(cmd, "git-url") {
		source.URL = flagString(cmd, "git-url")
	}
	if source.URL == "" {
		if canClearGitSource(cmd, githubAuth) {
			return platform.SourceBinding{}, true, "", nil
		}
		return platform.SourceBinding{}, false, "", contract.NewFault(
			contract.ValidationFailed,
			"a Git build recipe needs a repository URL",
			"Pass --git-url with the HTTPS repository URL, or clear it with --git-url '' to remove the Git source.",
		)
	}
	if err := validateGitURLShape(source.URL); err != nil {
		return platform.SourceBinding{}, false, "", err
	}
	if githubAuth {
		if _, ok := platform.GitHubRepository(source.URL); !ok {
			return platform.SourceBinding{}, false, "", contract.NewFault(
				contract.ValidationFailed,
				"--github-auth needs a GitHub HTTPS git source",
				"Use an https://github.com/owner/repository source; another forge needs --git-credential-file with a token for that host.",
			)
		}
	}
	if err := applyUpdateSourceFields(cmd, &source); err != nil {
		return platform.SourceBinding{}, false, "", err
	}
	return updateSourceCredential(cmd, githubAuth, runtimeSecrets, webhookSecret, source)
}

func updateSourceChanged(cmd *cobra.Command, githubAuth bool) bool {
	return githubAuth || flagChanged(cmd, "git-url") || flagChanged(cmd, "git-ref") ||
		flagChanged(cmd, "git-credential-file") || flagChanged(cmd, "git-credential-name") ||
		flagChanged(cmd, "builder") || flagChanged(cmd, "context-path") ||
		flagChanged(cmd, "build-env") || buildpackFlagsChanged(cmd)
}

func canClearGitSource(cmd *cobra.Command, githubAuth bool) bool {
	return flagChanged(cmd, "git-url") && !flagChanged(cmd, "git-ref") && !flagChanged(cmd, "builder") &&
		!flagChanged(cmd, "context-path") && !flagChanged(cmd, "build-env") &&
		!buildpackFlagsChanged(cmd) &&
		!flagChanged(cmd, "git-credential-name") &&
		!flagChanged(cmd, "git-credential-file") &&
		!githubAuth
}

func applyUpdateSourceFields(cmd *cobra.Command, source *platform.SourceBinding) error {
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
			return err
		}
		source.BuildEnv = values
	}
	return applyBuildpackFlags(cmd, source)
}

func updateSourceCredential(
	cmd *cobra.Command,
	githubAuth bool,
	runtimeSecrets []string, webhookSecret string,
	source platform.SourceBinding,
) (platform.SourceBinding, bool, string, error) {
	if githubAuth {
		name := source.CredentialSecretName
		if flagChanged(cmd, "git-credential-name") {
			name = flagString(cmd, "git-credential-name")
		}
		if name == "" {
			name = defaultGitReadCredentialName
		}
		if err := validateUpdateCredentialName(name, runtimeSecrets, webhookSecret); err != nil {
			return platform.SourceBinding{}, false, "", err
		}
		source.CredentialSecretName = name
		return source, true, name, nil
	}
	if flagChanged(cmd, "git-credential-name") && !flagChanged(cmd, "git-credential-file") {
		name := flagString(cmd, "git-credential-name")
		if name != "" {
			if err := validateUpdateCredentialName(name, runtimeSecrets, webhookSecret); err != nil {
				return platform.SourceBinding{}, false, "", err
			}
		}
		source.CredentialSecretName = name
	}
	if !flagChanged(cmd, "git-credential-file") {
		return source, true, "", nil
	}
	name := flagString(cmd, "git-credential-name")
	if name == "" {
		name = source.CredentialSecretName
	}
	if name == "" {
		name = defaultGitReadCredentialName
	}
	if err := validateUpdateCredentialName(name, runtimeSecrets, webhookSecret); err != nil {
		return platform.SourceBinding{}, false, "", err
	}
	// The value is read only after an authoritative preview says this is a
	// fresh submission. A known replay never opens the secret file.
	source.CredentialSecretName = name
	return source, true, name, nil
}

func validateUpdateCredentialName(name string, runtimeSecrets []string, webhookSecret string) error {
	if err := vault.ValidateName(name); err != nil {
		return err
	}
	return validateGitCredentialRole(name, runtimeSecrets, webhookSecret)
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
				environment.App,
				environment.Name,
				environment.Node,
				revisionLabel(environment),
				protection,
				boolLabel(environment.Deployability.Deployable),
				strings.Join(environment.Deployability.Missing, ","),
				serving,
			})
		}
		plain = ui.Header(
			"Environments",
			72,
		) + "\n" + writeTable(
			[]string{
				"APP",
				"ENVIRONMENT",
				"NODE",
				"REVISION",
				"PROTECTION",
				"DEPLOYABLE",
				"MISSING",
				"SERVING RELEASE",
			},
			rows,
		)
	}
	return result{data: raw, plain: plain}, nil
}

func (a *app) envStatus(cmd *cobra.Command, app, environment string) (result, error) {
	return a.envStatusView(cmd, app, environment)
}
