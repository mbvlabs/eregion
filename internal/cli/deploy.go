package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/viewer"
	"github.com/spf13/cobra"
)

// deliveryMeta is the common metadata for the release pipeline commands. They
// are node-mode operations: the CLI never opens the database or enqueues work.
func deliveryMeta(summary, example string) CommandMeta {
	return platformMutationMeta(summary, example)
}

func deliveryReadMeta(summary, example string) CommandMeta {
	meta := platformMeta(summary, example)
	meta.ErrorCodes = append(meta.ErrorCodes, contract.NotFound)
	return meta
}

func (a *app) deliverySpecs() []spec {
	return []spec{
		{
			use:  "deploy <app> <environment>",
			meta: deliveryMeta("Build the environment's source at a ref and deploy it with a blue/green switch", "eregion deploy api production --json"),
			args: platformTwoNames("app", "environment"),
			flags: func(cmd *cobra.Command) {
				addDeliveryFlags(cmd)
				cmd.Flags().String("ref", "", "Git branch, tag, or commit to build; defaults to the current revision's ref")
				cmd.Flags().String("release", "", "Redeploy an existing release ID instead of building from source")
			},
			run: a.deployCommand,
		},
		{
			use:  "rollback <app> <environment>",
			meta: deliveryRollbackMeta(),
			args: platformTwoNames("app", "environment"),
			flags: func(cmd *cobra.Command) {
				addDeliveryFlags(cmd)
				cmd.Flags().String("to", "", "Release to restore; defaults to the previous successful release")
				cmd.Flags().String("confirm", "", "Target confirmation: <app>/<environment>")
			},
			run: a.rollbackCommand,
		},
		{
			use:  "restart <app> <environment>",
			meta: deliveryMeta("Reapply the serving release with the current revision through the blue/green path", "eregion restart api production --json"),
			args: platformTwoNames("app", "environment"),
			flags: func(cmd *cobra.Command) {
				addDeliveryFlags(cmd)
			},
			run: a.restartCommand,
		},
		a.buildSpec(),
		a.releaseSpec(),
	}
}

func deliveryRollbackMeta() CommandMeta {
	meta := deliveryMeta("Restore an earlier release with its recorded revision and secret revisions", "eregion rollback api production --to rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --confirm api/production --json")
	meta.Destructive = true
	meta.Confirmation = "target"
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ConfirmationRequired)
	return meta
}

func addDeliveryFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("dry-run", false, "Validate the action and print its plan without submitting")
	cmd.Flags().Bool("detach", false, "Submit and return without following the operation")
	cmd.Flags().String("idempotency-key", "", "Stable submission key; generated when omitted")
	cmd.Flags().Duration("poll-interval", viewer.DefaultPollInterval, "Follow poll interval")
}

func (a *app) buildSpec() spec {
	return spec{
		use:  "build",
		meta: deliveryReadMeta("Inspect, follow, stop, or retry Buildpacks builds", "eregion build list api production --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "list <app> <environment>",
				meta: deliveryReadMeta("List builds for one environment", "eregion build list api production --json"),
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					cmd.Flags().Int("limit", 50, "Maximum builds (1–200)")
					cmd.Flags().String("state", "", "Filter by pending, running, succeeded, failed, or cancelled")
				},
				run: a.buildList,
			},
			{
				use:  "logs <build-id>",
				meta: deliveryReadMeta("Read the build's operation events, optionally following new lines", "eregion build logs bld_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --follow --json"),
				args: buildIDArg,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().Bool("follow", false, "Follow the build operation until it finishes or the viewer is detached")
					cmd.Flags().Duration("poll-interval", viewer.DefaultPollInterval, "Follow poll interval")
				},
				run: a.buildLogs,
			},
			{
				use:  "stop <build-id>",
				meta: deliveryMeta("Cancel one build and signal its running pack process", "eregion build stop bld_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json"),
				args: buildIDArg,
				flags: func(cmd *cobra.Command) {
					addDeliveryFlags(cmd)
				},
				run: a.buildStop,
			},
			{
				use:  "retry <build-id>",
				meta: deliveryMeta("Start a new build and release of the same exact commit", "eregion build retry bld_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json"),
				args: buildIDArg,
				flags: func(cmd *cobra.Command) {
					addDeliveryFlags(cmd)
				},
				run: a.buildRetry,
			},
		},
	}
}

func (a *app) releaseSpec() spec {
	return spec{
		use:  "release",
		meta: deliveryReadMeta("Inspect immutable releases", "eregion release list api production --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "list <app> <environment>",
				meta: deliveryReadMeta("List releases for one environment with their commit and image identity", "eregion release list api production --json"),
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					cmd.Flags().Int("limit", 50, "Maximum releases (1–200)")
					cmd.Flags().String("state", "", "Filter by pending, ready, or failed")
				},
				run: a.releaseList,
			},
		},
	}
}

func buildIDArg(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return contract.NewFault(contract.InputRequired, "build ID is required", "Use the build ID returned by build list or a deploy operation.")
	}
	return platform.BuildID(args[0]).Validate()
}

// deployInput maps the command's flags to the shared action input and rejects
// ambiguous combinations before any node call.
func deployInput(app, environment, ref, release string) (platform.DeployInput, error) {
	ref = strings.TrimSpace(ref)
	release = strings.TrimSpace(release)
	if ref != "" && release != "" {
		return platform.DeployInput{}, contract.NewFault(contract.ValidationFailed, "--ref and --release cannot be combined", "Deploy source at a ref, or redeploy an existing release.")
	}
	input := platform.DeployInput{App: app, Environment: environment, Ref: ref}
	if release != "" {
		input.ReleaseID = platform.ReleaseID(release)
		if err := input.ReleaseID.Validate(); err != nil {
			return platform.DeployInput{}, err
		}
	}
	return input, nil
}

func rollbackInput(app, environment, to string) (platform.RollbackInput, error) {
	input := platform.RollbackInput{App: app, Environment: environment}
	to = strings.TrimSpace(to)
	if to != "" {
		input.ToReleaseID = platform.ReleaseID(to)
		if err := input.ToReleaseID.Validate(); err != nil {
			return platform.RollbackInput{}, err
		}
	}
	return input, nil
}

// plainOutput reports whether this invocation renders human text rather than a
// machine envelope.
func plainOutput(cmd *cobra.Command) bool {
	config := followConfig(cmd)
	return config == nil || config.mode == string(output.Plain)
}

// followsByDefault is the terminal rule for mutating delivery commands: follow
// the accepted operation unless --detach was given or output is not an attached
// plain terminal.
func followsByDefault(cmd *cobra.Command) bool {
	if boolFlag(cmd, "detach") {
		return false
	}
	config := followConfig(cmd)
	if config == nil || config.mode != string(output.Plain) {
		return false
	}
	return isTerminal(config.in) && isTerminal(config.out)
}

func deliveryPollInterval(cmd *cobra.Command) time.Duration {
	if flag := cmd.Flags().Lookup("poll-interval"); flag != nil {
		value, _ := cmd.Flags().GetDuration("poll-interval")
		if value > 0 {
			return value
		}
	}
	return viewer.DefaultPollInterval
}

func previewText(preview platform.Preview) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Plan: %s\n", preview.Summary)
	for _, step := range preview.Steps {
		fmt.Fprintf(&out, "  - %s\n", step)
	}
	for _, warning := range preview.Warnings {
		fmt.Fprintf(&out, "  warning: %s\n", warning)
	}
	return out.String()
}

func (a *app) previewDelivery(cmd *cobra.Command, client Backend, kind, target string, input any) (platform.Preview, json.RawMessage, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return platform.Preview{}, nil, contract.NewFault(contract.Internal, "could not encode the action input", "Report the command and binary version.")
	}
	out, err := client.SubmitAction(cmd.Context(), kind, target, raw, "", "", true)
	if err != nil {
		return platform.Preview{}, nil, err
	}
	var preview platform.Preview
	if json.Unmarshal(out, &preview) != nil || !preview.DryRun {
		return platform.Preview{}, nil, invalidNodeData("action preview")
	}
	return preview, out, nil
}

// deliveryAction is one mutating action run through the shared preview,
// submission and follow path. Load returns the human plan and the raw preview;
// it is only called for a dry run or a terminal follow.
type deliveryAction struct {
	Kind    string
	Target  string
	Input   any
	Confirm string
	Load    func() (string, json.RawMessage, error)
	Outcome func(context.Context, Backend, contract.Operation) []string
}

func (a *app) previewLoader(cmd *cobra.Command, client Backend, kind, target string, input any) func() (string, json.RawMessage, error) {
	return func() (string, json.RawMessage, error) {
		preview, raw, err := a.previewDelivery(cmd, client, kind, target, input)
		if err != nil {
			return "", nil, err
		}
		return previewText(preview), raw, nil
	}
}

func (a *app) runDelivery(cmd *cobra.Command, client Backend, action deliveryAction) (result, error) {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	follow := followsByDefault(cmd)
	var plain string
	var raw json.RawMessage
	if dryRun || follow {
		var err error
		plain, raw, err = action.Load()
		if err != nil {
			return result{}, err
		}
	}
	if dryRun {
		if !plainOutput(cmd) {
			plain = ""
		}
		return result{data: raw, plain: plain}, nil
	}
	if follow {
		if _, err := fmt.Fprint(followConfig(cmd).out, a.followCleaner()(plain+"\n")); err != nil {
			return result{}, contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
		}
	}
	operation, err := a.submitAccepted(cmd, action.Kind, action.Target, action.Input, action.Confirm)
	if err != nil {
		return result{}, err
	}
	if !follow {
		return platformOperationResult(mustJSON(operation), false), nil
	}
	return a.followAccepted(cmd, client, operation, action.Outcome)
}

func (a *app) deployCommand(cmd *cobra.Command, args []string) (result, error) {
	input, err := deployInput(args[0], args[1], stringFlag(cmd, "ref"), stringFlag(cmd, "release"))
	if err != nil {
		return result{}, err
	}
	target, err := platform.NewEnvironmentTarget(args[0], args[1])
	if err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.runDelivery(cmd, client, deliveryAction{
		Kind: platform.KindDeploy, Target: target.String(), Input: input,
		Load: func() (string, json.RawMessage, error) {
			plan, err := a.loadDeployPlan(cmd, client, args[0], args[1], input)
			if err != nil {
				return "", nil, err
			}
			return a.renderDeployPlan(plan) + "\n" + previewText(plan.Preview), plan.Raw, nil
		},
		Outcome: a.deployOutcome(args[0], args[1]),
	})
}

func (a *app) rollbackCommand(cmd *cobra.Command, args []string) (result, error) {
	input, err := rollbackInput(args[0], args[1], stringFlag(cmd, "to"))
	if err != nil {
		return result{}, err
	}
	target, err := platform.NewEnvironmentTarget(args[0], args[1])
	if err != nil {
		return result{}, err
	}
	confirm, _ := cmd.Flags().GetString("confirm")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if !dryRun {
		if err := platform.ValidateConfirmation(platform.KindRollback, target.String(), confirm); err != nil {
			return result{}, err
		}
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.runDelivery(cmd, client, deliveryAction{
		Kind: platform.KindRollback, Target: target.String(), Input: input, Confirm: confirm,
		Load:    a.previewLoader(cmd, client, platform.KindRollback, target.String(), input),
		Outcome: a.deployOutcome(args[0], args[1]),
	})
}

func (a *app) restartCommand(cmd *cobra.Command, args []string) (result, error) {
	target, err := platform.NewEnvironmentTarget(args[0], args[1])
	if err != nil {
		return result{}, err
	}
	input := platform.RestartInput{App: args[0], Environment: args[1]}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.runDelivery(cmd, client, deliveryAction{
		Kind: platform.KindRestart, Target: target.String(), Input: input,
		Load:    a.previewLoader(cmd, client, platform.KindRestart, target.String(), input),
		Outcome: a.deployOutcome(args[0], args[1]),
	})
}

func (a *app) buildStop(cmd *cobra.Command, args []string) (result, error) {
	buildID := platform.BuildID(args[0])
	target, err := platform.NewBuildTarget(buildID)
	if err != nil {
		return result{}, err
	}
	input := platform.BuildStopInput{BuildID: buildID}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.runDelivery(cmd, client, deliveryAction{
		Kind: platform.KindBuildStop, Target: target.String(), Input: input,
		Load: a.previewLoader(cmd, client, platform.KindBuildStop, target.String(), input),
	})
}

func (a *app) buildRetry(cmd *cobra.Command, args []string) (result, error) {
	buildID := platform.BuildID(args[0])
	target, err := platform.NewBuildTarget(buildID)
	if err != nil {
		return result{}, err
	}
	input := platform.BuildRetryInput{BuildID: buildID}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.runDelivery(cmd, client, deliveryAction{
		Kind: platform.KindBuildRetry, Target: target.String(), Input: input,
		Load:    a.previewLoader(cmd, client, platform.KindBuildRetry, target.String(), input),
		Outcome: a.buildOutcome,
	})
}

func (a *app) buildList(cmd *cobra.Command, args []string) (result, error) {
	raw, err := a.platformReadInventoryWith(cmd, "builds", deliveryFilters(cmd, args[0], args[1]))
	if err != nil {
		return result{}, err
	}
	var page struct {
		Items []platform.BuildRecord `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return result{}, invalidNodeData("build inventory")
	}
	return result{data: raw, plain: ui.Header("Builds for "+args[0]+"/"+args[1], 72) + "\n" + buildTable(page.Items)}, nil
}

func (a *app) releaseList(cmd *cobra.Command, args []string) (result, error) {
	raw, err := a.platformReadInventoryWith(cmd, "releases", deliveryFilters(cmd, args[0], args[1]))
	if err != nil {
		return result{}, err
	}
	var page struct {
		Items []platform.Release `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return result{}, invalidNodeData("release inventory")
	}
	return result{data: raw, plain: ui.Header("Releases for "+args[0]+"/"+args[1], 72) + "\n" + releaseTable(page.Items)}, nil
}

func deliveryFilters(cmd *cobra.Command, app, environment string) url.Values {
	filters := url.Values{"app": {app}, "environment": {environment}}
	if flag := cmd.Flags().Lookup("limit"); flag != nil {
		limit, _ := cmd.Flags().GetInt("limit")
		filters.Set("limit", fmt.Sprintf("%d", limit))
	}
	if flag := cmd.Flags().Lookup("state"); flag != nil {
		if state, _ := cmd.Flags().GetString("state"); state != "" {
			filters.Set("state", state)
		}
	}
	return filters
}

type buildLogsResult struct {
	Build  platform.BuildRecord `json:"build"`
	Events []contract.Event     `json:"events"`
	Cursor uint64               `json:"cursor,string"`
}

func (a *app) buildLogs(cmd *cobra.Command, args []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	build, err := a.findBuild(cmd, client, platform.BuildID(args[0]))
	if err != nil {
		return result{}, err
	}
	follow, _ := cmd.Flags().GetBool("follow")
	if follow {
		return a.followBuildLogs(cmd, client, build)
	}
	events, cursor, err := fetchOperationEvents(cmd.Context(), client, build.OperationID)
	if err != nil {
		return result{}, err
	}
	data := buildLogsResult{Build: build, Events: events, Cursor: cursor}
	if !plainOutput(cmd) {
		return result{data: data}, nil
	}
	config := followConfig(cmd)
	if config == nil {
		return result{}, contract.NewFault(contract.Internal, "viewer output is not configured", "Invoke the command through the eregion CLI entry point.")
	}
	var plain strings.Builder
	fmt.Fprintf(&plain, "build %s (%s)\n", build.ID, build.State)
	presenter := viewer.NewPlain(&plain, config.err, a.followCleaner())
	if len(events) > 0 {
		if err := presenter.Observe(viewer.Observation{Kind: viewer.ObservationEvents, Events: events}); err != nil {
			return result{}, contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
		}
	}
	if len(events) == 0 {
		plain.WriteString("no build log events yet\n")
	}
	return result{data: data, plain: plain.String()}, nil
}

func (a *app) followBuildLogs(cmd *cobra.Command, client Backend, build platform.BuildRecord) (result, error) {
	clean := a.followCleaner()
	opts := viewer.FollowOptions{PollInterval: deliveryPollInterval(cmd), Clean: clean}
	if !plainOutput(cmd) {
		summary, err := viewer.Follow(withoutWait(cmd.Context()), client, build.OperationID, opts, nil)
		err = followError(summary, err)
		return result{data: newFollowResult(summary)}, err
	}
	config := followConfig(cmd)
	if config == nil {
		return result{}, contract.NewFault(contract.Internal, "viewer output is not configured", "Invoke the command through the eregion CLI entry point.")
	}
	presenter := viewer.NewPlain(config.out, config.err, clean)
	summary, err := viewer.Follow(withoutWait(cmd.Context()), client, build.OperationID, opts, presenter.Observe)
	err = followError(summary, err)
	if finishErr := presenter.Finish(summary, err, "eregion operations follow "+string(build.OperationID), time.Now()); finishErr != nil && err == nil {
		err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	return result{data: newFollowResult(summary), streamed: true}, err
}

func (a *app) findBuild(cmd *cobra.Command, client Backend, id platform.BuildID) (platform.BuildRecord, error) {
	raw, err := client.PlatformInventory(cmd.Context(), "builds", url.Values{"build": {string(id)}, "limit": {"1"}})
	if err != nil {
		return platform.BuildRecord{}, err
	}
	var page struct {
		Items []platform.BuildRecord `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return platform.BuildRecord{}, invalidNodeData("build inventory")
	}
	for _, build := range page.Items {
		if build.ID == id {
			return build, nil
		}
	}
	return platform.BuildRecord{}, contract.NewFault(contract.NotFound, "build not found", "List builds for the environment with eregion build list.")
}

func fetchOperationEvents(ctx context.Context, client Backend, id contract.OperationID) ([]contract.Event, uint64, error) {
	var events []contract.Event
	cursor := uint64(0)
	for page := 0; page < 100; page++ {
		raw, err := client.Events(ctx, contract.Cursor{OperationID: id, After: cursor}, 200)
		if err != nil {
			return nil, cursor, err
		}
		var result operations.EventPage
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, cursor, invalidNodeData("build event page")
		}
		events = append(events, result.Events...)
		if result.Cursor.After <= cursor {
			break
		}
		cursor = result.Cursor.After
		if len(result.Events) < 200 {
			break
		}
	}
	return events, cursor, nil
}

type deployPlan struct {
	Environment platform.Environment
	Preview     platform.Preview
	Raw         json.RawMessage
	Ref         string
	Release     *platform.Release
}

func (a *app) loadDeployPlan(cmd *cobra.Command, client Backend, app, environment string, input platform.DeployInput) (deployPlan, error) {
	env, err := a.environmentItem(cmd, client, app, environment)
	if err != nil {
		return deployPlan{}, err
	}
	preview, raw, err := a.previewDelivery(cmd, client, platform.KindDeploy, "env/"+app+"/"+environment, input)
	if err != nil {
		return deployPlan{}, err
	}
	plan := deployPlan{Environment: env, Preview: preview, Raw: raw, Ref: input.Ref}
	if input.ReleaseID != "" {
		if release, ok := a.findRelease(cmd, client, app, environment, input.ReleaseID); ok {
			plan.Release = &release
		}
	}
	return plan, nil
}

func (a *app) environmentItem(cmd *cobra.Command, client Backend, app, environment string) (platform.Environment, error) {
	raw, err := client.PlatformInventory(cmd.Context(), "environments", url.Values{"app": {app}, "environment": {environment}, "limit": {"200"}})
	if err != nil {
		return platform.Environment{}, err
	}
	var page struct {
		Items []platform.Environment `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return platform.Environment{}, invalidNodeData("environment inventory")
	}
	for _, item := range page.Items {
		if item.App == app && item.Name == environment {
			return item, nil
		}
	}
	return platform.Environment{}, contract.NewFault(contract.NotFound, "environment not found", "Create the environment first, or check the app and environment names.")
}

func (a *app) findRelease(cmd *cobra.Command, client Backend, app, environment string, id platform.ReleaseID) (platform.Release, bool) {
	raw, err := client.PlatformInventory(cmd.Context(), "releases", url.Values{"app": {app}, "environment": {environment}, "limit": {"200"}})
	if err != nil {
		return platform.Release{}, false
	}
	var page struct {
		Items []platform.Release `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return platform.Release{}, false
	}
	for _, release := range page.Items {
		if release.ID == id {
			return release, true
		}
	}
	return platform.Release{}, false
}

func (a *app) renderDeployPlan(plan deployPlan) string {
	env := plan.Environment
	var out strings.Builder
	fmt.Fprintf(&out, "Deploy %s/%s\n", env.App, env.Name)
	if plan.Release != nil {
		fmt.Fprintf(&out, "release: %s %s\n", plan.Release.ID, plan.Release.Image)
		if plan.Release.CommitSHA != "" {
			fmt.Fprintf(&out, "commit: %s\n", shortCommit(plan.Release.CommitSHA))
		}
	} else if env.Revision != nil {
		source := env.Revision.Source
		switch source.Kind {
		case "git":
			fmt.Fprintf(&out, "source: git %s\n", source.URL)
			ref := plan.Ref
			if ref == "" {
				ref = source.Ref
			}
			if ref != "" {
				fmt.Fprintf(&out, "ref: %s\n", ref)
			}
		case "image":
			fmt.Fprintf(&out, "source: image %s\n", source.Image)
		default:
			fmt.Fprintln(&out, "source: (not bound)")
		}
	} else {
		fmt.Fprintln(&out, "source: (no current revision)")
	}
	if env.Revision != nil && env.Revision.ID != "" {
		fmt.Fprintf(&out, "revision: #%d %s\n", env.Revision.Number, env.Revision.ID)
	}
	processes := env.Processes
	if env.Revision != nil && len(env.Revision.Processes) > 0 {
		processes = env.Revision.Processes
	}
	if len(processes) > 0 {
		parts := make([]string, 0, len(processes))
		for _, process := range processes {
			replicas := process.Replicas
			if replicas == 0 {
				replicas = 1
			}
			part := fmt.Sprintf("%s x%d", process.Name, replicas)
			if process.ContainerPort > 0 {
				part += fmt.Sprintf(" port %d", process.ContainerPort)
			}
			parts = append(parts, part)
		}
		fmt.Fprintf(&out, "processes: %s\n", strings.Join(parts, ", "))
	}
	if env.ServingRelease != "" {
		fmt.Fprintf(&out, "serving: %s (stays until the switch passes)\n", env.ServingRelease)
	}
	if public := publicURL(env); public != "" {
		fmt.Fprintf(&out, "public url: %s\n", public)
	}
	return out.String()
}

func (a *app) deployOutcome(app, environment string) func(context.Context, Backend, contract.Operation) []string {
	return func(ctx context.Context, client Backend, operation contract.Operation) []string {
		raw, err := client.PlatformInventory(ctx, "environments", url.Values{"app": {app}, "environment": {environment}, "limit": {"200"}})
		if err != nil {
			return nil
		}
		var page struct {
			Items []platform.Environment `json:"items"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return nil
		}
		var env platform.Environment
		found := false
		for _, item := range page.Items {
			if item.App == app && item.Name == environment {
				env, found = item, true
				break
			}
		}
		if !found {
			return nil
		}
		lines := []string{}
		if build, ok := a.buildForOperation(ctx, client, operation.ID); ok && build.CommitSHA != "" {
			lines = append(lines, "commit: "+shortCommit(build.CommitSHA))
		}
		if env.ServingRelease == "" {
			lines = append(lines, "serving release: none")
		} else {
			lines = append(lines, "serving release: "+string(env.ServingRelease))
		}
		if public := publicURL(env); public != "" {
			lines = append(lines, "public url: "+public)
		} else {
			lines = append(lines, "public url: no domain binding")
		}
		return lines
	}
}

func (a *app) buildOutcome(ctx context.Context, client Backend, operation contract.Operation) []string {
	build, ok := a.buildForOperation(ctx, client, operation.ID)
	if !ok {
		return nil
	}
	lines := []string{"build: " + string(build.ID)}
	if build.ReleaseID != "" {
		lines = append(lines, "release: "+string(build.ReleaseID))
	}
	if build.CommitSHA != "" {
		lines = append(lines, "commit: "+shortCommit(build.CommitSHA))
	}
	return lines
}

func (a *app) buildForOperation(ctx context.Context, client Backend, id contract.OperationID) (platform.BuildRecord, bool) {
	raw, err := client.PlatformInventory(ctx, "builds", url.Values{"limit": {"200"}})
	if err != nil {
		return platform.BuildRecord{}, false
	}
	var page struct {
		Items []platform.BuildRecord `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return platform.BuildRecord{}, false
	}
	for _, build := range page.Items {
		if build.OperationID == id {
			return build, true
		}
	}
	return platform.BuildRecord{}, false
}

func publicURL(env platform.Environment) string {
	host := env.PrimaryDomain
	if host == "" && len(env.Domains) > 0 {
		host = env.Domains[0]
	}
	if host == "" {
		return ""
	}
	return "https://" + host
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func stringFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func buildTable(builds []platform.BuildRecord) string {
	if len(builds) == 0 {
		return "(no builds)\n"
	}
	rows := make([][]string, 0, len(builds))
	for _, build := range builds {
		commit := build.CommitSHA
		if len(commit) > 12 {
			commit = commit[:12]
		}
		created := build.CreatedAt.UTC().Format(time.RFC3339)
		rows = append(rows, []string{string(build.ID), string(build.State), commit, string(build.ReleaseID), created})
	}
	return writeTable([]string{"ID", "STATE", "COMMIT", "RELEASE", "CREATED"}, rows)
}

func releaseTable(releases []platform.Release) string {
	if len(releases) == 0 {
		return "(no releases)\n"
	}
	rows := make([][]string, 0, len(releases))
	for _, release := range releases {
		commit := release.CommitSHA
		if len(commit) > 12 {
			commit = commit[:12]
		}
		created := release.CreatedAt.UTC().Format(time.RFC3339)
		rows = append(rows, []string{string(release.ID), string(release.Status), commit, release.Image, created})
	}
	return writeTable([]string{"ID", "STATUS", "COMMIT", "IMAGE", "CREATED"}, rows)
}
