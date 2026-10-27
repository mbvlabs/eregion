package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/viewer"
	"github.com/spf13/cobra"
)

func addDeleteFlags(cmd *cobra.Command) {
	cmd.Flags().String("confirm", "", "Target-naming confirmation token: <app> or <app>/<environment>")
	cmd.Flags().Bool("dry-run", false, "Show the deletion plan without accepting work")
	cmd.Flags().String("idempotency-key", "", "Stable submission key; one is generated when empty")
	cmd.Flags().Bool("detach", false, "Return immediately after acceptance instead of following in a terminal")
}

func (a *app) appDeleteSpec() spec {
	meta := platformDestructiveMeta("Delete an application and every environment it owns",
		"eregion app delete api --confirm api --json")
	meta.DryRun = true
	meta.Confirmation = "target"
	meta.WhenToUse = []string{
		"Remove an app after workload cleanup. The confirmation token must name the current app; --yes is never accepted.",
		"Operation and event history is kept. Other apps keep serving and their Caddy routes are not rewritten away.",
	}
	meta.Examples = []string{
		"eregion app delete api --dry-run --json",
		"eregion app delete api --confirm api --json",
		"eregion app delete api --confirm api --detach",
	}
	meta.Next = []string{"eregion operations list --app <app> --json", "eregion app list --json"}
	return spec{
		use:   "delete <app>",
		meta:  meta,
		args:  platformOneName("app"),
		flags: addDeleteFlags,
		run: func(cmd *cobra.Command, args []string) (result, error) {
			target, err := platform.NewAppTarget(args[0])
			if err != nil {
				return result{}, err
			}
			confirm, _ := cmd.Flags().GetString("confirm")
			return a.runDelete(cmd, platform.KindAppDelete, target, args[0], "", confirm)
		},
	}
}

func (a *app) envDeleteSpec() spec {
	meta := platformDestructiveMeta("Delete one environment after workload cleanup",
		"eregion env delete api production --confirm api/production --json")
	meta.DryRun = true
	meta.Confirmation = "target"
	meta.WhenToUse = []string{
		"Remove one environment after its containers, routes and secrets are cleaned up. Deleting the last environment also deletes the app.",
		"The confirmation token must name the current environment as app/environment; --yes is never accepted.",
	}
	meta.Examples = []string{
		"eregion env delete api production --dry-run --json",
		"eregion env delete api production --confirm api/production --json",
	}
	meta.Next = []string{"eregion env list --app <app> --json", "eregion operations list --app <app> --environment <environment> --json"}
	return spec{
		use:   "delete <app> <environment>",
		meta:  meta,
		args:  platformTwoNames("app", "environment"),
		flags: addDeleteFlags,
		run: func(cmd *cobra.Command, args []string) (result, error) {
			target, err := platform.NewEnvironmentTarget(args[0], args[1])
			if err != nil {
				return result{}, err
			}
			confirm, _ := cmd.Flags().GetString("confirm")
			return a.runDelete(cmd, platform.KindEnvironmentDelete, target, args[0], args[1], confirm)
		},
	}
}

type deleteResult struct {
	App               string              `json:"app"`
	Environment       string              `json:"environment,omitempty"`
	ConfirmationToken string              `json:"confirmation_token,omitempty"`
	Operation         *contract.Operation `json:"operation,omitempty"`
	DryRun            bool                `json:"dry_run,omitempty"`
	Preview           *platform.Preview   `json:"preview,omitempty"`
}

func (a *app) runDelete(cmd *cobra.Command, kind string, target platform.Target, app, environment, confirm string) (result, error) {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	detach, _ := cmd.Flags().GetBool("detach")
	token, err := platform.ConfirmationToken(target)
	if err != nil {
		return result{}, err
	}
	var input any
	if environment == "" {
		input = platform.AppDeleteInput{App: app, Confirm: confirm}
	} else {
		input = platform.EnvironmentDeleteInput{App: app, Environment: environment, Confirm: confirm}
	}
	if dryRun {
		preview, err := a.previewAction(cmd, kind, target.String(), input)
		if err != nil {
			return result{}, err
		}
		data := deleteResult{App: app, Environment: environment, ConfirmationToken: token, DryRun: true, Preview: &preview}
		var plain strings.Builder
		fmt.Fprintf(&plain, "Dry run: delete %s\n", target.String())
		for _, step := range preview.Steps {
			fmt.Fprintf(&plain, "  step: %s\n", step)
		}
		for _, warning := range preview.Warnings {
			fmt.Fprintf(&plain, "  warning: %s\n", warning)
		}
		fmt.Fprintf(&plain, "confirmation_token: %s\n", token)
		return result{data: data, plain: plain.String()}, nil
	}
	if err := platform.ValidateTargetConfirmation(target, confirm); err != nil {
		return result{}, err
	}
	op, err := a.submitAccepted(cmd, kind, target.String(), input, confirm)
	if err != nil {
		return result{}, err
	}
	data := deleteResult{App: app, Environment: environment, Operation: &op}
	if humanInteractive(cmd) && !detach {
		client, err := a.client(cmd)
		if err != nil {
			return result{}, err
		}
		return a.followAccepted(cmd, client, op)
	}
	plain := fmt.Sprintf("Deletion accepted for %s\noperation: %s\nstate: %s\nnext: eregion operations follow %s --json\n",
		target.String(), op.ID, op.State, op.ID)
	return result{data: data, plain: plain}, nil
}

// followAccepted attaches the existing plain operation presenter after a
// mutation in a terminal. Ctrl-C detaches; it never cancels the operation. A
// caller may append command-specific outcome lines after the finish line.
func (a *app) followAccepted(cmd *cobra.Command, client Backend, op contract.Operation, outcome ...func(context.Context, Backend, contract.Operation) []string) (result, error) {
	config := followConfig(cmd)
	if config == nil {
		return result{}, nil
	}
	clean := a.followCleaner()
	presenter := viewer.NewPlain(config.out, config.err, clean)
	summary, err := viewer.Follow(withoutWait(cmd.Context()), client, op.ID, viewer.FollowOptions{
		PollInterval: viewer.DefaultPollInterval, Clean: clean,
	}, presenter.Observe)
	err = followError(summary, err)
	if finishErr := presenter.Finish(summary, err, resumeCommand+" "+string(op.ID), time.Now()); finishErr != nil && err == nil {
		err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	if len(outcome) > 0 && outcome[0] != nil && !summary.Detached && summary.State.Terminal() {
		for _, line := range outcome[0](context.WithoutCancel(cmd.Context()), client, op) {
			if _, writeErr := fmt.Fprintln(config.out, clean(line)); writeErr != nil && err == nil {
				err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
			}
		}
	}
	return result{data: newFollowResult(summary), streamed: true}, err
}
