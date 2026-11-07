package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

func (a *app) appSpecs() []spec {
	parent := operationMeta("Create and inspect applications", "eregion app list --json")
	parent.WhenToUse = []string{"Discover the application model before creating, inspecting or deploying an app."}

	create := followingMeta(operationMeta("Create an application with production and staging", "eregion app create --name api --node main --git-url https://github.com/acme/api --json"))
	create.WhenToUse = []string{"Run it bare in a terminal for the guided setup: app, shared source, processes, config, secrets and production's domain. The same recipe creates production and staging atomically. Agents pass every flag; JSON and no-TTY never prompt. Both start protected; creation never asks for a TOTP code."}
	create.Mutating, create.DryRun, create.Prompts = true, true, true
	create.InputFlags = []string{
		"name", "node", "build-node", "git-url", "git-ref", "git-credential-file",
		"git-credential-name", "image", "builder", "context-path", "build-env", "container-port",
		"health-path", "process", "config", "secret", "domain", "domain-check-path", "from", "no-deploy", "detach",
		"protected", "unprotected",
	}
	create.Examples = []string{
		"eregion app create",
		"eregion app create --name api --node main --git-url https://github.com/acme/api --git-ref main --container-port 8080 --health-path /healthz --domain api.example.com --json",
		"eregion app create --name api --node main --image ghcr.io/acme/api:2026-10-20 --no-deploy --json",
		"eregion app create --from ./eregion.toml --no-deploy --json",
	}
	create.Next = []string{"eregion env status <app> <environment> --json", "eregion app status <app> --json"}

	list := operationMeta("List applications", "eregion app list --json")
	list.Examples = []string{"eregion app list --json"}

	status := operationMeta("Show one application and its environments", "eregion app status api --json")
	status.Examples = []string{"eregion app status api --json"}
	status.Next = []string{"eregion env status <app> <environment> --json"}

	export := operationMeta("Export one environment's desired state as an eregion.toml", "eregion app export api production")
	export.WhenToUse = []string{"Generate a deterministic app definition from the current revision, process formation, plain config, secret names, domain bindings and managed resource declarations. Secret values, releases, health and TLS observations are never exported."}
	export.InputFlags = []string{"out"}
	export.Examples = []string{
		"eregion app export api production",
		"eregion app export api production --out ./eregion.toml --json",
	}
	export.Next = []string{"eregion env update api production --from ./eregion.toml --dry-run --json"}

	return []spec{{
		use:  "app",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "create",
				meta: create,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("name", "", "Application name")
					cmd.Flags().Bool("no-deploy", false, "Do not request the first deploy")
					cmd.Flags().Bool("detach", false, "Submit and return without following the create and first deploy")
					addProtectionFlags(cmd)
					addSetupFlags(cmd, true)
				},
				run: a.appCreate,
			},
			{
				use:  "list",
				meta: list,
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("app", "", "Filter by app name")
					cmd.Flags().Int("limit", 50, "Maximum applications (1–200)")
				},
				run: a.appList,
			},
			{
				use:  "status <app>",
				meta: status,
				args: platformOneName("app"),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.appStatus(cmd, args[0])
				},
			},
			{
				use:  "export <app> <environment>",
				meta: export,
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("out", "", "Write the definition to this new file instead of stdout")
				},
				run: a.appExport,
			},
			a.appDeleteSpec(),
		},
	}}
}

// cloneEnvironmentSpec copies one first-environment declaration deeply enough
// that staging can be derived from the shared flag recipe without aliasing the
// production maps or slices.
func cloneEnvironmentSpec(spec platform.EnvironmentSpec) platform.EnvironmentSpec {
	out := spec
	out.Env = cloneStringMap(spec.Env)
	out.SecretRefs = cloneStringMap(spec.SecretRefs)
	out.Config = cloneStringMap(spec.Config)
	out.SecretNames = append([]string(nil), spec.SecretNames...)
	out.Resources = append([]string(nil), spec.Resources...)
	out.Domains = append([]platform.DomainDeclaration(nil), spec.Domains...)
	out.DomainAddresses = append([]string(nil), spec.DomainAddresses...)
	out.Processes = make([]platform.ProcessSpec, len(spec.Processes))
	for i, process := range spec.Processes {
		process.Args = append([]string(nil), process.Args...)
		out.Processes[i] = process
	}
	out.Source.BuildEnv = cloneStringMap(spec.Source.BuildEnv)
	if spec.Runtime.ReleaseCommand != nil {
		command := *spec.Runtime.ReleaseCommand
		command.Argv = append([]string(nil), command.Argv...)
		out.Runtime.ReleaseCommand = &command
	}
	if spec.Protected != nil {
		value := *spec.Protected
		out.Protected = &value
	}
	return out
}

func protectionLabel(protection *bool, name string) string {
	protected := platform.DefaultEnvironmentProtection(name)
	if protection != nil {
		protected = *protection
	}
	if protected {
		return "protected"
	}
	return "unprotected"
}

func protectedSpec(spec platform.EnvironmentSpec) bool {
	if spec.Protected != nil {
		return *spec.Protected
	}
	return platform.DefaultEnvironmentProtection(spec.Name)
}

// appResourceSpecs returns one declaration per environment in recipe order.
// The flag path has production-only declarations in ResourceSpecs; the file
// path stores both under AdditionalResourceSpecs.
func appResourceSpecs(ans *setupAnswers, specs []platform.EnvironmentSpec) []platform.ResourceApplyInput {
	if len(ans.AdditionalResourceSpecs) == 0 {
		return append([]platform.ResourceApplyInput(nil), ans.ResourceSpecs...)
	}
	out := make([]platform.ResourceApplyInput, 0, len(ans.ResourceSpecs))
	for _, spec := range specs {
		out = append(out, ans.AdditionalResourceSpecs[spec.Name]...)
	}
	return out
}

func summarizeEnvironmentSpecs(specs []platform.EnvironmentSpec) []setupEnvironmentResult {
	out := make([]setupEnvironmentResult, 0, len(specs))
	for _, spec := range specs {
		domain := spec.Domain
		if domain == "" && len(spec.Domains) > 0 {
			for _, declaration := range spec.Domains {
				if declaration.Primary {
					domain = declaration.Domain
					break
				}
			}
		}
		out = append(out, setupEnvironmentResult{Name: spec.Name, Node: spec.Node, Protected: protectedSpec(spec), Domain: domain})
	}
	return out
}

func (a *app) appCreate(cmd *cobra.Command, _ []string) (res result, err error) {
	interactive := humanInteractive(cmd)
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ui.Header("eregion app create", 80))
	}
	ans, err := a.readSetup(cmd, setupApp)
	if err != nil {
		return result{}, setupPromptError("app create", err)
	}
	defer ans.clearSecrets()
	specs := append([]platform.EnvironmentSpec(nil), ans.PrebuiltSpecs...)
	if len(specs) == 0 {
		production, err := ans.environmentSpec(cmd)
		if err != nil {
			return result{}, err
		}
		staging := cloneEnvironmentSpec(production)
		staging.Name = "staging"
		staging.Domain, staging.DomainPrimary, staging.DomainCheckPath = "", false, ""
		staging.Domains, staging.DomainAddresses = nil, nil
		specs = []platform.EnvironmentSpec{production, staging}
	}
	if len(specs) != 2 || specs[0].Name != "production" || specs[1].Name != "staging" {
		return result{}, contract.NewFault(contract.Internal, "app creation did not resolve production and staging", "Report the command and binary version.")
	}
	if err := validateUniqueEnvironmentDomains(specs); err != nil {
		return result{}, err
	}
	ans.PrebuiltSpecs = specs
	spec := specs[0]
	follow := !boolFlag(cmd, "detach") && (streamsNDJSON(cmd) || (interactive && ans.Deploy && followsByDefault(cmd)))
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	input := platform.AppCreateInput{Name: ans.App, Environments: specs, Deploy: ans.Deploy}

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
		if exists {
			return result{}, contract.NewFault(contract.Conflict, "app "+ans.App+" already exists", "Choose another name, or inspect it with eregion app status "+ans.App+".")
		}
	}
	preview, err := a.previewAction(cmd, platform.KindAppCreate, platform.PlatformTarget().String(), input)
	if err != nil {
		return result{}, err
	}
	plan := setupPlan(ans, preview, cmd)
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ui.PanelFit("Plan for app "+ans.App, plan, 80))
	}
	dry := dryRun(cmd)
	if interactive && !dry {
		ok, err := ui.YesNoDefault("Create app "+ans.App+" now?", "The app, its first environment and first revision are accepted in one submission; a requested first deploy is queued from it.", true)
		if err != nil {
			return result{}, setupPromptError("app create", err)
		}
		if !ok {
			return result{}, contract.NewFault(contract.Cancelled, "app creation was not confirmed", "Nothing was written; the plan above is unchanged.")
		}
	}
	summary := setupResult{
		App: ans.App, Environment: ans.Environment, Protected: protectedSpec(spec), Environments: summarizeEnvironmentSpecs(specs),
		Node: spec.Node, Source: spec.Source, Processes: spec.Processes, ConfigKeys: sortedKeys(spec.Config),
		CredentialSecretName: ans.CredentialName, Domain: ans.Domain, DomainCheckPath: ans.DomainCheckPath, Domains: ans.Domains, Deploy: ans.Deploy,
		Warnings: preview.Warnings, created: "app",
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
		if _, err := a.writeSetupSecretsForEnvironments(cmd, ans, specs); err != nil {
			return result{}, err
		}
		summary.SecretNames = setupSecretNames(ans)
	}
	op, err := a.submitAccepted(cmd, platform.KindAppCreate, platform.PlatformTarget().String(), input, "")
	if err != nil {
		return result{}, err
	}
	summary.OperationID = op.ID
	summary.FirstDeployOperationID = op.FirstDeployOperationID
	resourceSpecs := appResourceSpecs(ans, specs)
	if len(resourceSpecs) > 0 {
		resourceIDs, err := a.provisionDefinitionResources(cmd, resourceSpecs)
		if err != nil {
			summary.Warnings = append(summary.Warnings, "The environments were created, but resource provisioning was not accepted: "+err.Error()+". Reapply with `eregion resource apply`.")
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
			return a.followGuidedCreate(cmd, client, op, "env/"+ans.App+"/"+ans.Environment, a.deployOutcome(ans.App, ans.Environment))
		}
		fmt.Fprintln(os.Stderr, "")
		rows := [][2]string{
			{"environment", ans.Environment},
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
		fmt.Fprintln(os.Stderr, ui.Card("App "+ans.App+" created", rows, footer))
		return result{streamed: true}, nil
	}
	if streamsNDJSON(cmd) && !boolFlag(cmd, "detach") {
		client, err := a.client(cmd)
		if err != nil {
			return result{}, err
		}
		if ans.Deploy && op.FirstDeployOperationID != "" {
			return a.followGuidedCreate(cmd, client, op, "env/"+ans.App+"/"+ans.Environment, a.deployOutcome(ans.App, ans.Environment))
		}
		return a.followAccepted(cmd, client, op)
	}
	return result{data: summary, plain: summary.plainText()}, nil
}

// definitionDomainState refuses a file export/apply that would omit a domain
// the environment row says is bound. Domain inventory and the environment row
// must agree before a self-contained definition can be called faithful.
func definitionDomainState(current platform.Environment, bindings []platform.DomainBinding) error {
	seen := map[string]platform.DomainBinding{}
	for _, binding := range bindings {
		seen[binding.Domain] = binding
	}
	for _, domain := range current.Domains {
		if _, ok := seen[domain]; !ok {
			return contract.NewFault(contract.PreconditionFailed, "cannot represent this environment's domain state faithfully", "Domain "+domain+" is in the environment but not in the domain inventory; refresh the environment status and retry.")
		}
	}
	if current.PrimaryDomain != "" {
		binding, ok := seen[current.PrimaryDomain]
		if !ok || !binding.Primary {
			return contract.NewFault(contract.PreconditionFailed, "cannot represent this environment's domain state faithfully", "The primary domain binding is unavailable; refresh the environment status and retry.")
		}
	}
	return nil
}

func (a *app) appExport(cmd *cobra.Command, args []string) (result, error) {
	appName, environment := args[0], args[1]
	current, _, err := a.readEnvironment(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	if len(current.SecretRefs) > 0 && (current.Revision == nil || len(current.Revision.SecretNames) == 0) {
		return result{}, contract.NewFault(contract.PreconditionFailed, "this environment uses legacy secret_refs", "Move the environment to secret names with a new revision before exporting a definition.")
	}
	domains, err := a.readDomainBindings(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	if err := definitionDomainState(current, domains); err != nil {
		return result{}, err
	}
	_, resources, _, err := a.resourceInventory(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	document, err := appdef.Export(appName, environment, current.BuildNode, current.Revision, domains, resources)
	if err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "cannot export this environment faithfully", err.Error())
	}
	outPath := flagString(cmd, "out")
	if outPath != "" {
		file, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G304: operator-selected export path; O_EXCL avoids clobbering.
		if err != nil {
			return result{}, contract.NewFault(contract.PreconditionFailed, "cannot write the app definition", "Choose a new path; existing files are not overwritten.")
		}
		if _, err := file.WriteString(document); err != nil {
			_ = file.Close()
			_ = os.Remove(outPath)
			return result{}, contract.NewFault(contract.Unavailable, "cannot write the app definition", "Check the destination's free space and permissions.")
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(outPath)
			return result{}, contract.NewFault(contract.Unavailable, "cannot write the app definition", "Check the destination's free space and permissions.")
		}
	}
	data := struct {
		App         string `json:"app"`
		Environment string `json:"environment"`
		Format      string `json:"format"`
		Document    string `json:"document"`
		Out         string `json:"out,omitempty"`
	}{App: appName, Environment: environment, Format: "toml", Document: document, Out: outPath}
	plain := document
	if outPath != "" {
		plain = "Wrote " + outPath + "\n"
	}
	return result{data: data, plain: plain}, nil
}

func (a *app) appList(cmd *cobra.Command, _ []string) (result, error) {
	raw, err := a.platformReadInventory(cmd, "apps")
	if err != nil {
		return result{}, err
	}
	items, err := decodeInventoryItems[platform.App](raw)
	if err != nil {
		return result{}, err
	}
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{item.Name, string(item.ID), item.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")})
	}
	plain := ui.Header("Applications", 72) + "\nNo applications. Create one with eregion app create.\n"
	if len(rows) > 0 {
		plain = ui.Header("Applications", 72) + "\n" + writeTable([]string{"NAME", "ID", "UPDATED"}, rows)
	}
	return result{data: raw, plain: plain}, nil
}

func (a *app) appStatus(cmd *cobra.Command, app string) (result, error) {
	return a.appStatusView(cmd, app)
}

func revisionLabel(environment platform.Environment) string {
	if environment.Revision == nil {
		return "-"
	}
	return fmt.Sprintf("%d (%s)", environment.Revision.Number, environment.Revision.ID)
}

func processesLabel(environment platform.Environment) string {
	if len(environment.Processes) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(environment.Processes))
	for _, process := range environment.Processes {
		parts = append(parts, fmt.Sprintf("%s:%d", process.Name, process.Replicas))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func boolLabel(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
