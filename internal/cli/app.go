package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

func (a *app) appSpecs() []spec {
	parent := operationMeta("Create and inspect applications", "eregion app list --json")
	parent.WhenToUse = []string{"Discover the application model before creating, inspecting or deploying an app."}

	create := operationMeta("Create an application with its first environment", "eregion app create --name api --environment production --node main --git-url https://github.com/acme/api --json")
	create.WhenToUse = []string{"Run it bare in a terminal for the guided setup: app, environment, source, processes, config, secrets and a domain. Agents pass every flag; JSON and no-TTY never prompt."}
	create.Mutating, create.DryRun, create.Prompts = true, true, true
	create.InputFlags = []string{
		"name", "environment", "node", "build-node", "git-url", "git-ref", "git-credential-file",
		"git-credential-name", "image", "builder", "context-path", "build-env", "container-port",
		"health-path", "process", "config", "secret", "domain", "no-deploy",
	}
	create.Examples = []string{
		"eregion app create",
		"eregion app create --name api --environment production --node main --git-url https://github.com/acme/api --git-ref main --container-port 8080 --health-path /healthz --domain api.example.com --json",
		"eregion app create --name api --environment production --node main --image ghcr.io/acme/api:2026-10-20 --no-deploy --json",
	}
	create.Next = []string{"eregion env status <app> <environment> --json", "eregion app status <app> --json"}

	list := operationMeta("List applications", "eregion app list --json")
	list.Examples = []string{"eregion app list --json"}

	status := operationMeta("Show one application and its environments", "eregion app status api --json")
	status.Examples = []string{"eregion app status api --json"}
	status.Next = []string{"eregion env status <app> <environment> --json"}

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
					cmd.Flags().String("environment", "production", "Name of the first environment")
					cmd.Flags().Bool("no-deploy", false, "Do not request the first deploy")
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
			a.appDeleteSpec(),
		},
	}}
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
	spec, err := ans.environmentSpec()
	if err != nil {
		return result{}, err
	}
	input := platform.AppCreateInput{Name: ans.App, Environment: &spec, Deploy: ans.Deploy}

	exists, err := a.appExists(cmd, ans.App)
	if err != nil {
		return result{}, err
	}
	if exists {
		return result{}, contract.NewFault(contract.Conflict, "app "+ans.App+" already exists", "Choose another name, or inspect it with eregion app status "+ans.App+".")
	}
	preview, err := a.previewAction(cmd, platform.KindAppCreate, platform.PlatformTarget().String(), input)
	if err != nil {
		return result{}, err
	}
	plan := setupPlan(ans, preview)
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
		App: ans.App, Environment: ans.Environment, Node: ans.Node,
		Source: spec.Source, Processes: spec.Processes, ConfigKeys: sortedKeys(spec.Config),
		CredentialSecretName: ans.CredentialName, Domain: ans.Domain, Deploy: ans.Deploy,
		Warnings: preview.Warnings, created: "app",
	}
	if record, ok := domainDNSRecord(ans.Domain); ok {
		summary.DNSRecord = &record
	}
	if ans.Deploy && ans.Domain == "" {
		summary.Warnings = append(summary.Warnings, "The first deploy has no domain; public verification will fail until a domain is added.")
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
	op, err := a.submitAccepted(cmd, platform.KindAppCreate, platform.PlatformTarget().String(), input, "")
	if err != nil {
		return result{}, err
	}
	summary.OperationID = op.ID
	if ans.Domain != "" {
		target, err := platform.NewEnvironmentTarget(ans.App, ans.Environment)
		if err != nil {
			return result{}, err
		}
		domainOp, err := a.submitAccepted(cmd, platform.KindDomainAdd, target.String(), platform.DomainInput{
			App: ans.App, Environment: ans.Environment, Domain: ans.Domain,
		}, "")
		if err != nil {
			fault := contract.Classify(err)
			return result{}, contract.NewFault(fault.Code,
				"app "+ans.App+" was created, but adding domain "+ans.Domain+" failed: "+fault.Message,
				"Add it with eregion domain add "+ans.App+" "+ans.Environment+" "+ans.Domain+"; app operation "+string(op.ID)+".")
		}
		summary.DomainOperationID = domainOp.ID
	}
	if interactive {
		fmt.Fprintln(os.Stderr, "")
		rows := [][2]string{
			{"environment", ans.Environment},
			{"node", ans.Node},
			{"source", describeSource(spec.Source)},
			{"operation", string(op.ID)},
		}
		if ans.Deploy {
			rows = append(rows, [2]string{"first deploy", "queued from the revision"})
		}
		if ans.Domain != "" {
			rows = append(rows, [2]string{"domain", ans.Domain})
			dns := "point " + ans.Domain + " at the main node's public IPv4 address"
			if summary.DNSRecord != nil {
				dns = summary.DNSRecord.Type + " " + summary.DNSRecord.Name + " " + summary.DNSRecord.Value
			}
			rows = append(rows, [2]string{"DNS record", dns})
		}
		fmt.Fprintln(os.Stderr, ui.Card("App "+ans.App+" created", rows, []string{
			ui.Dim("the revision is desired state, not a running deployment"),
			ui.Dim("next:"),
			"  " + ui.Code("eregion env status "+ans.App+" "+ans.Environment+" --json"),
		}))
		return result{streamed: true}, nil
	}
	return result{data: summary, plain: summary.plainText()}, nil
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
	plain := "No applications. Create one with eregion app create.\n"
	if len(rows) > 0 {
		plain = writeTable([]string{"NAME", "ID", "UPDATED"}, rows)
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
