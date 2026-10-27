package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

func (a *app) processSpecs() []spec {
	parent := operationMeta("Inspect and scale environment processes", "eregion process list api production --json")

	list := operationMeta("List the environment's desired processes and replicas", "eregion process list api production --json")
	list.Examples = []string{"eregion process list api production --json"}

	scale := operationMeta("Change one process's replicas in a new revision", "eregion process scale api production worker 2 --json")
	scale.WhenToUse = []string{"Change replicas without touching source, configuration or secrets. A serving environment restarts its release through the blue/green path immediately; otherwise the next deploy applies the new revision."}
	scale.Mutating, scale.DryRun = true, true
	scale.Examples = []string{
		"eregion process scale api production worker 2 --json",
		"eregion process scale api production web 3 --dry-run --json",
	}
	scale.Next = []string{"eregion env status <app> <environment> --json"}

	return []spec{{
		use:  "process",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "list <app> <environment>",
				meta: list,
				args: platformTwoNames("app", "environment"),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					env, _, err := a.readEnvironment(cmd, args[0], args[1])
					if err != nil {
						return result{}, err
					}
					plain := ui.Header("Processes for "+args[0]+"/"+args[1], 72) + "\n"
					plain += writeProcessTable(env.Processes)
					return result{data: map[string]any{
						"app": args[0], "environment": args[1], "processes": env.Processes,
					}, plain: plain}, nil
				},
			},
			{
				use:  "scale <app> <environment> <process> <replicas>",
				meta: scale,
				args: processScaleArgs,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("idempotency-key", "", "Stable submission key; one is generated when empty")
					cmd.Flags().Bool("dry-run", false, "Validate and show the new revision without accepting work")
				},
				run: a.processScale,
			},
		},
	}}
}

func processScaleArgs(_ *cobra.Command, args []string) error {
	if len(args) != 4 {
		return contract.NewFault(contract.InputRequired, "app, environment, process and replicas are required", "Use eregion process scale <app> <environment> <process> <replicas>.")
	}
	if err := platform.ValidateName(args[0]); err != nil {
		return err
	}
	if err := platform.ValidateName(args[1]); err != nil {
		return err
	}
	if strings.TrimSpace(args[2]) == "" {
		return contract.NewFault(contract.InputRequired, "the process name is required", "List the formation with eregion process list.")
	}
	if !setupProcessNamePattern.MatchString(args[2]) {
		return contract.NewFault(contract.ValidationFailed, "invalid process name", "Use a lowercase name of 1–63 letters, digits, underscores, or hyphens.")
	}
	replicas, err := strconv.Atoi(args[3])
	if err != nil || replicas < 1 || replicas > 32 {
		return contract.NewFault(contract.ValidationFailed, "invalid replicas", "Use between 1 and 32 replicas.")
	}
	return nil
}

func (a *app) processScale(cmd *cobra.Command, args []string) (result, error) {
	replicas, _ := strconv.Atoi(args[3])
	input := platform.ProcessScaleInput{App: args[0], Environment: args[1], Process: args[2], Replicas: replicas}
	target := "env/" + args[0] + "/" + args[1]
	preview, err := a.previewAction(cmd, platform.KindProcessScale, target, input)
	if err != nil {
		return result{}, err
	}
	data := map[string]any{
		"app": args[0], "environment": args[1], "process": args[2], "replicas": replicas,
	}
	if dryRun(cmd) {
		data["dry_run"] = true
		data["preview"] = preview
		plain := fmt.Sprintf("Dry run: scale %s to %d replica(s)\n", args[2], replicas)
		plain += updatePreviewText(preview)
		return result{data: data, plain: plain}, nil
	}
	op, err := a.submitAccepted(cmd, platform.KindProcessScale, target, input, "")
	if err != nil {
		return result{}, err
	}
	data["operation_id"] = op.ID
	plain := fmt.Sprintf("Scaled process %s to %d replica(s) in a new revision.\noperation: %s\na serving environment restarts the release through the blue/green path; otherwise the next deploy applies it.\nnext: eregion env status %s %s --json\n",
		args[2], replicas, op.ID, args[0], args[1])
	return result{data: data, plain: plain}, nil
}
