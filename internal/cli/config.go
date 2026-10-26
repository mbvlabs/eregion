package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

func (a *app) configSpecs() []spec {
	parent := operationMeta("Inspect and change plain environment configuration", "eregion config list api production --json")

	set := operationMeta("Set plain configuration in a new environment revision", "eregion config set api production APP_ENV=production --json")
	set.WhenToUse = []string{"Set non-secret environment variables. Values are plain in the operation and revision; use secret set for credentials. No deploy is queued."}
	set.Mutating, set.DryRun = true, true
	set.Examples = []string{
		"eregion config set api production APP_ENV=production LOG_LEVEL=info --json",
	}
	set.Next = []string{"eregion config list <app> <environment> --json"}

	unset := operationMeta("Unset plain configuration in a new environment revision", "eregion config unset api production LOG_LEVEL --json")
	unset.Mutating, unset.DryRun = true, true
	unset.Examples = []string{"eregion config unset api production LOG_LEVEL --json"}
	unset.Next = []string{"eregion config list <app> <environment> --json"}

	list := operationMeta("List plain configuration from the current revision", "eregion config list api production --json")
	list.Examples = []string{"eregion config list api production --json"}

	return []spec{{
		use:  "config",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "set <app> <environment> KEY=VALUE...",
				meta: set,
				args: configSetArgs,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("idempotency-key", "", "Stable submission key; one is generated when empty")
					cmd.Flags().Bool("dry-run", false, "Validate and show the new revision without accepting work")
				},
				run: a.configSet,
			},
			{
				use:  "unset <app> <environment> KEY...",
				meta: unset,
				args: configUnsetArgs,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("idempotency-key", "", "Stable submission key; one is generated when empty")
					cmd.Flags().Bool("dry-run", false, "Validate and show the new revision without accepting work")
				},
				run: a.configUnset,
			},
			{
				use:  "list <app> <environment>",
				meta: list,
				args: platformTwoNames("app", "environment"),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.configList(cmd, args[0], args[1])
				},
			},
		},
	}}
}

func configSetArgs(_ *cobra.Command, args []string) error {
	if len(args) < 3 {
		return contract.NewFault(contract.InputRequired, "at least one KEY=VALUE is required", "Use eregion config set <app> <environment> KEY=VALUE.")
	}
	if err := platform.ValidateName(args[0]); err != nil {
		return err
	}
	if err := platform.ValidateName(args[1]); err != nil {
		return err
	}
	_, err := parseAssignments(args[2:], "config")
	return err
}

func configUnsetArgs(_ *cobra.Command, args []string) error {
	if len(args) < 3 {
		return contract.NewFault(contract.InputRequired, "at least one configuration key is required", "Use eregion config unset <app> <environment> KEY.")
	}
	if err := platform.ValidateName(args[0]); err != nil {
		return err
	}
	if err := platform.ValidateName(args[1]); err != nil {
		return err
	}
	_, err := parseConfigKeys(args[2:])
	return err
}

func (a *app) configSet(cmd *cobra.Command, args []string) (result, error) {
	config, err := parseAssignments(args[2:], "config")
	if err != nil {
		return result{}, err
	}
	input := platform.ConfigSetInput{App: args[0], Environment: args[1], Config: config}
	target := "env/" + args[0] + "/" + args[1]
	preview, err := a.previewAction(cmd, platform.KindConfigSet, target, input)
	if err != nil {
		return result{}, err
	}
	keys := sortedKeys(config)
	data := map[string]any{"app": args[0], "environment": args[1], "keys": keys}
	if dryRun(cmd) {
		data["dry_run"] = true
		data["preview"] = preview
		plain := fmt.Sprintf("Dry run: set %s in %s/%s\n", strings.Join(keys, ", "), args[0], args[1])
		plain += updatePreviewText(preview)
		return result{data: data, plain: plain}, nil
	}
	op, err := a.submitAccepted(cmd, platform.KindConfigSet, target, input)
	if err != nil {
		return result{}, err
	}
	data["operation_id"] = op.ID
	plain := fmt.Sprintf("Set %s in a new revision of %s/%s.\noperation: %s\nnext: eregion config list %s %s --json\n",
		strings.Join(keys, ", "), args[0], args[1], op.ID, args[0], args[1])
	return result{data: data, plain: plain}, nil
}

func (a *app) configUnset(cmd *cobra.Command, args []string) (result, error) {
	keys, err := parseConfigKeys(args[2:])
	if err != nil {
		return result{}, err
	}
	input := platform.ConfigUnsetInput{App: args[0], Environment: args[1], Keys: keys}
	target := "env/" + args[0] + "/" + args[1]
	preview, err := a.previewAction(cmd, platform.KindConfigUnset, target, input)
	if err != nil {
		return result{}, err
	}
	data := map[string]any{"app": args[0], "environment": args[1], "keys": keys}
	if dryRun(cmd) {
		data["dry_run"] = true
		data["preview"] = preview
		plain := fmt.Sprintf("Dry run: unset %s in %s/%s\n", strings.Join(keys, ", "), args[0], args[1])
		plain += updatePreviewText(preview)
		return result{data: data, plain: plain}, nil
	}
	op, err := a.submitAccepted(cmd, platform.KindConfigUnset, target, input)
	if err != nil {
		return result{}, err
	}
	data["operation_id"] = op.ID
	plain := fmt.Sprintf("Unset %s in a new revision of %s/%s.\noperation: %s\nnext: eregion config list %s %s --json\n",
		strings.Join(keys, ", "), args[0], args[1], op.ID, args[0], args[1])
	return result{data: data, plain: plain}, nil
}

func (a *app) configList(cmd *cobra.Command, app, environment string) (result, error) {
	env, _, err := a.readEnvironment(cmd, app, environment)
	if err != nil {
		return result{}, err
	}
	config := map[string]string{}
	if env.Revision != nil {
		config = env.Revision.Config
	} else if env.Env != nil {
		config = env.Env
	}
	keys := sortedKeys(config)
	plain := fmt.Sprintf("No plain configuration for %s/%s.\n", app, environment)
	if len(keys) > 0 {
		var out strings.Builder
		fmt.Fprintf(&out, "Config for %s/%s\n", app, environment)
		for _, key := range keys {
			fmt.Fprintf(&out, "  %s=%s\n", key, config[key])
		}
		plain = out.String()
	}
	return result{data: map[string]any{
		"app": app, "environment": environment, "config": config, "keys": keys,
	}, plain: plain}, nil
}

func parseConfigKeys(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 64 {
		return nil, contract.NewFault(contract.ValidationFailed, "use 1–64 configuration keys", "List each key once.")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, key := range values {
		if !assignmentKeyPattern.MatchString(key) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid configuration key", "Use 1–128 ASCII letters, digits, and underscores; do not start with a digit.")
		}
		if seen[key] {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate configuration key", "List each key once.")
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}
