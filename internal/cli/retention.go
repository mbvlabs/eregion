package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/viewer"
	"github.com/spf13/cobra"
)

// retentionSpecs is the D30 operator surface: one dry-run/confirmation-gated
// prune and the policy commands that describe or change desired state.
func (a *app) retentionSpecs() []spec {
	return []spec{
		a.pruneSpec(),
		{
			use:  "retention",
			meta: deliveryReadMeta("Inspect or change the disk-retention policy", "eregion retention show --json"),
			run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
			children: []spec{
				a.retentionShowSpec(),
				a.retentionSetSpec(),
			},
		},
	}
}

func (a *app) pruneSpec() spec {
	meta := followingMeta(platformDestructiveMeta(
		"Prune disk artifacts on the connected node under the effective policy",
		"eregion prune --dry-run --json"))
	meta.Confirmation = "target"
	meta.InputFlags = []string{"confirm"}
	meta.Examples = []string{
		"eregion prune --dry-run --json",
		"eregion prune --dry-run",
		"eregion prune --confirm main",
		"eregion prune --confirm main --detach --json",
	}
	meta.WhenToUse = []string{
		"Preview or run the same planner the daily maintenance job uses. A real prune requires --confirm naming the connected server; --yes alone never suffices.",
		"A dry run lists every planned item and its bytes, changes nothing, and needs no confirmation.",
		"Release rows are never deleted: a release outside the policy records pruned_at, and rollback to it refuses clearly.",
	}
	meta.Next = []string{"eregion operations list --json", "eregion retention show --json"}
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ConfirmationRequired)
	return spec{
		use:  "prune",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().Bool("dry-run", false, "List planned items without accepting work")
			cmd.Flags().String("confirm", "", "Target-naming confirmation: the connected server name")
			cmd.Flags().Bool("detach", false, "Accept and return without following the operation")
			cmd.Flags().String("idempotency-key", "", "Stable submission key; generated when omitted")
			cmd.Flags().Duration("poll-interval", viewer.DefaultPollInterval, "Follow poll interval")
		},
		run: a.prune,
	}
}

func (a *app) prune(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	info, err := client.NodeInfo(cmd.Context())
	if err != nil {
		return result{}, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		plan, err := client.RetentionPlan(cmd.Context(), info.Node, "", "")
		if err != nil {
			return result{}, err
		}
		preview := prunePreviewResult{
			Node: info.Node, DryRun: true, Items: plan.Items,
			PlannedBytes: plan.PlannedBytes, Warnings: plan.Warnings,
		}
		var plain strings.Builder
		fmt.Fprintf(&plain, "Dry run: disk retention on %s\n", info.Node)
		writeRetentionPlanLines(&plain, plan.Items)
		fmt.Fprintf(&plain, "planned_bytes: %d\n", plan.PlannedBytes)
		for _, warning := range plan.Warnings {
			fmt.Fprintf(&plain, "warning: %s\n", warning)
		}
		return result{data: preview, plain: plain.String()}, nil
	}

	confirm, _ := cmd.Flags().GetString("confirm")
	if err := platform.ValidateTargetConfirmation(platform.Target{Kind: platform.TargetNode, Node: info.Node}, confirm); err != nil {
		return result{}, err
	}
	follow := !boolFlag(cmd, "detach") && (followsByDefault(cmd) || streamsNDJSON(cmd))
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	input := platform.RetentionPruneInput{Node: info.Node}
	target := platform.Target{Kind: platform.TargetNode, Node: info.Node}
	operation, err := a.submitAccepted(cmd, platform.KindRetentionPrune, target.String(), input, confirm)
	if err != nil {
		return result{}, err
	}
	if !follow {
		return platformOperationResult(mustJSON(operation), false), nil
	}
	return a.followAccepted(cmd, client, operation)
}

type prunePreviewResult struct {
	Node         string           `json:"node"`
	DryRun       bool             `json:"dry_run"`
	Items        []retention.Item `json:"items"`
	PlannedBytes int64            `json:"planned_bytes"`
	Warnings     []string         `json:"warnings,omitempty"`
}

func writeRetentionPlanLines(out *strings.Builder, items []retention.Item) {
	for _, item := range items {
		fmt.Fprintf(out, "%-18s %s", item.Kind, item.Reference)
		if item.Bytes > 0 {
			fmt.Fprintf(out, " bytes=%d", item.Bytes)
		}
		if item.App != "" || item.Environment != "" {
			fmt.Fprintf(out, " %s/%s", item.App, item.Environment)
		}
		if len(item.Tags) > 0 {
			fmt.Fprintf(out, " tags=%s", strings.Join(item.Tags, ","))
		}
		if item.Reason != "" {
			fmt.Fprintf(out, " reason=%s", item.Reason)
		}
		out.WriteString("\n")
	}
}

func (a *app) retentionShowSpec() spec {
	meta := deliveryReadMeta("Show the server retention default or one environment's effective policy",
		"eregion retention show --app api --env production --json")
	meta.Next = []string{"eregion retention set --help --json", "eregion prune --dry-run --json"}
	return spec{
		use:  "show",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("app", "", "Application name; with --env selects the environment policy")
			cmd.Flags().String("env", "", "Environment name")
		},
		run: a.retentionShow,
	}
}

func (a *app) retentionShow(cmd *cobra.Command, _ []string) (result, error) {
	appName, _ := cmd.Flags().GetString("app")
	environment, _ := cmd.Flags().GetString("env")
	if (appName == "") != (environment == "") {
		return result{}, contract.NewFault(contract.InputRequired, "app and env are required together", "Pass --app <app> --env <environment>, or neither for the server policy.")
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	view, err := client.RetentionShow(cmd.Context(), appName, environment)
	if err != nil {
		return result{}, err
	}
	return result{data: view, plain: retentionPolicyPlain(view)}, nil
}

func retentionPolicyPlain(view retention.PolicyView) string {
	var out strings.Builder
	fmt.Fprintf(&out, "scope: %s\n", view.Scope)
	if view.App != "" {
		fmt.Fprintf(&out, "app: %s\nenvironment: %s\n", view.App, view.Environment)
	}
	fields := []struct {
		name  string
		value any
	}{
		{"keep_releases", view.Effective.KeepReleases},
		{"keep_newer_than_days", view.Effective.KeepNewerThanDays},
		{"artifact_grace_days", view.Effective.ArtifactGraceDays},
		{"buildpack_cache_max_bytes", view.Effective.BuildpackCacheMaxBytes},
		{"buildpack_cache_max_percent", view.Effective.BuildpackCacheMaxPercent},
	}
	for _, field := range fields {
		fmt.Fprintf(&out, "%s: %v (%s)\n", field.name, field.value, view.Sources[field.name])
	}
	return out.String()
}

func (a *app) retentionSetSpec() spec {
	meta := platformMeta("Change the server retention default or an environment's override",
		"eregion retention set --app api --env production --keep-releases 5 --keep-days 10 --artifact-grace-days 14 --json")
	meta.Mutating = true
	meta.Prompts = true
	meta.InputFlags = []string{"keep-releases", "keep-days", "artifact-grace-days", "cache-cap-bytes", "cache-cap-percent", "clear-override"}
	meta.DryRunReason = "Retention set writes desired state; inspect retention show first. Environment changes commit a new revision."
	meta.Next = []string{"eregion retention show --app <app> --env <environment> --json", "eregion env status <app> <environment> --json"}
	meta.Examples = []string{
		"eregion retention set --keep-releases 10 --keep-days 14 --artifact-grace-days 14 --cache-cap-bytes 10737418240 --cache-cap-percent 80 --json",
		"eregion retention set --app api --env production --keep-releases 5 --keep-days 10 --artifact-grace-days 14 --json",
		"eregion retention set --app api --env production --clear-override --json",
	}
	meta.WhenToUse = []string{
		"Server scope changes the node-wide default and the Buildpacks cache caps. Environment scope commits a new immutable revision; unspecified fields inherit the server value.",
		"Without change flags a terminal walks the release-scoped values. --json or no TTY never prompts and refuses when no value is given.",
	}
	return spec{
		use:  "set",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("app", "", "Application name; with --env selects an environment override")
			cmd.Flags().String("env", "", "Environment name")
			cmd.Flags().Int("keep-releases", 0, "Newest releases kept per environment")
			cmd.Flags().Int("keep-days", 0, "Days a release is kept regardless of count")
			cmd.Flags().Int("artifact-grace-days", 0, "Days an unreferenced registry artifact is kept")
			cmd.Flags().Int64("cache-cap-bytes", 0, "Buildpacks cache absolute cap in bytes (server only)")
			cmd.Flags().Int("cache-cap-percent", 0, "Filesystem used-percentage cap for the cache (server only)")
			cmd.Flags().Bool("clear-override", false, "Remove the environment override; environment scope only")
		},
		run: a.retentionSet,
	}
}

func (a *app) retentionSet(cmd *cobra.Command, _ []string) (result, error) {
	appName, _ := cmd.Flags().GetString("app")
	environment, _ := cmd.Flags().GetString("env")
	if appName == "" && environment != "" {
		return result{}, contract.NewFault(contract.InputRequired, "--app is required with --env", "Pass both names, or neither to change the server default.")
	}
	if appName != "" && environment == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--env is required with --app", "Pass both names, or neither to change the server default.")
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	changedKeepReleases := cmd.Flags().Changed("keep-releases")
	changedKeepDays := cmd.Flags().Changed("keep-days")
	changedArtifactGrace := cmd.Flags().Changed("artifact-grace-days")
	changedCacheBytes := cmd.Flags().Changed("cache-cap-bytes")
	changedCachePercent := cmd.Flags().Changed("cache-cap-percent")
	clearOverride, _ := cmd.Flags().GetBool("clear-override")

	guided := !changedKeepReleases && !changedKeepDays && !changedArtifactGrace && !changedCacheBytes && !changedCachePercent && !clearOverride
	if guided {
		config := followConfig(cmd)
		mode, stdinTTY, stdoutTTY := "", false, false
		if config != nil {
			mode, stdinTTY, stdoutTTY = config.mode, isTerminal(config.in), isTerminal(config.out)
		}
		if !retentionSetGuided(mode, boolFlag(cmd, "json"), stdinTTY, stdoutTTY) {
			return result{}, contract.NewFault(contract.InputRequired, "retention values are required without a terminal", "Pass --keep-releases, --keep-days and --artifact-grace-days; --cache-cap-bytes/--cache-cap-percent are server-only.")
		}
	}

	if environment == "" {
		return a.retentionSetServer(cmd, client, guided, changedKeepReleases, changedKeepDays, changedArtifactGrace, changedCacheBytes, changedCachePercent, clearOverride)
	}
	return a.retentionSetEnvironment(cmd, client, appName, environment, guided, changedKeepReleases, changedKeepDays, changedArtifactGrace, changedCacheBytes, changedCachePercent, clearOverride)
}

func (a *app) retentionSetServer(cmd *cobra.Command, client Backend, guided bool,
	changedKeepReleases, changedKeepDays, changedArtifactGrace, changedCacheBytes, changedCachePercent bool, clearOverride bool) (result, error) {
	if clearOverride {
		return result{}, contract.NewFault(contract.ValidationFailed, "--clear-override applies to an environment", "Name --app and --env, or use explicit server values.")
	}
	view, err := client.RetentionShow(cmd.Context(), "", "")
	if err != nil {
		return result{}, err
	}
	policy := view.Server
	if guided {
		values, err := guidedRetentionValues(policy)
		if err != nil {
			return result{}, err
		}
		policy.KeepReleases, policy.KeepNewerThanDays, policy.ArtifactGraceDays = values[0], values[1], values[2]
	}
	if changedKeepReleases {
		policy.KeepReleases, _ = cmd.Flags().GetInt("keep-releases")
	}
	if changedKeepDays {
		policy.KeepNewerThanDays, _ = cmd.Flags().GetInt("keep-days")
	}
	if changedArtifactGrace {
		policy.ArtifactGraceDays, _ = cmd.Flags().GetInt("artifact-grace-days")
	}
	if changedCacheBytes {
		policy.BuildpackCacheMaxBytes, _ = cmd.Flags().GetInt64("cache-cap-bytes")
	}
	if changedCachePercent {
		policy.BuildpackCacheMaxPercent, _ = cmd.Flags().GetInt("cache-cap-percent")
	}
	if err := policy.Validate(); err != nil {
		return result{}, err
	}
	updated, err := client.RetentionSetServer(cmd.Context(), policy)
	if err != nil {
		return result{}, err
	}
	return result{data: updated, plain: retentionPolicyPlain(updated)}, nil
}

func (a *app) retentionSetEnvironment(cmd *cobra.Command, client Backend, appName, environment string, guided bool,
	changedKeepReleases, changedKeepDays, changedArtifactGrace, changedCacheBytes, changedCachePercent bool, clearOverride bool) (result, error) {
	if changedCacheBytes || changedCachePercent {
		return result{}, contract.NewFault(contract.ValidationFailed, "the Buildpacks cache cap is server-wide", "Set --cache-cap-bytes and --cache-cap-percent without --app/--env.")
	}
	if err := platform.ValidateName(appName); err != nil {
		return result{}, err
	}
	if err := platform.ValidateName(environment); err != nil {
		return result{}, err
	}
	view, err := client.RetentionShow(cmd.Context(), appName, environment)
	if err != nil {
		return result{}, err
	}
	override := view.Override.Clone()
	if clearOverride {
		override = retention.Override{}
	}
	if guided {
		values, err := guidedRetentionValues(view.Effective)
		if err != nil {
			return result{}, err
		}
		override.KeepReleases, override.KeepNewerThanDays, override.ArtifactGraceDays = &values[0], &values[1], &values[2]
	}
	if changedKeepReleases {
		value, _ := cmd.Flags().GetInt("keep-releases")
		override.KeepReleases = &value
	}
	if changedKeepDays {
		value, _ := cmd.Flags().GetInt("keep-days")
		override.KeepNewerThanDays = &value
	}
	if changedArtifactGrace {
		value, _ := cmd.Flags().GetInt("artifact-grace-days")
		override.ArtifactGraceDays = &value
	}
	if err := override.Validate(); err != nil {
		return result{}, err
	}
	target := platform.Target{Kind: platform.TargetEnvironment, App: appName, Environment: environment}
	input := platform.EnvironmentUpdateInput{App: appName, Environment: environment, Retention: &override}
	operation, err := a.submitAccepted(cmd, platform.KindEnvironmentUpdate, target.String(), input, "")
	if err != nil {
		return result{}, err
	}
	data := map[string]any{
		"app": appName, "environment": environment,
		"operation_id": operation.ID, "retention": override,
	}
	plain := fmt.Sprintf("Retention override for %s/%s committed as a new environment revision.\noperation: %s\nnext: eregion retention show --app %s --env %s --json\n",
		appName, environment, operation.ID, appName, environment)
	return result{data: data, plain: plain}, nil
}

// retentionSetGuided permits the interactive value interview only on an
// attached plain terminal. Machine modes never prompt even when stdin/stdout
// happen to be terminals.
func retentionSetGuided(mode string, jsonFlag, stdinTTY, stdoutTTY bool) bool {
	if jsonFlag || mode != "plain" {
		return false
	}
	return stdinTTY && stdoutTTY
}

func guidedRetentionValues(policy retention.Policy) ([3]int, error) {
	var values [3]int
	questions := []struct {
		title string
		value int
	}{
		{"Releases kept per environment", policy.KeepReleases},
		{"Days a release is kept regardless of count", policy.KeepNewerThanDays},
		{"Days an unreferenced registry artifact is kept", policy.ArtifactGraceDays},
	}
	for index, question := range questions {
		answer, err := ui.Input(question.title, "Every value also has a flag for JSON and no-TTY use.", fmt.Sprint(question.value))
		if err != nil {
			return values, contract.NewFault(contract.Cancelled, "retention values were not confirmed", "Nothing was written.")
		}
		value, err := parsePositiveInt(answer)
		if err != nil {
			return values, err
		}
		values[index] = value
	}
	return values, nil
}

func parsePositiveInt(value string) (int, error) {
	value = strings.TrimSpace(value)
	parsed := 0
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, contract.NewFault(contract.ValidationFailed, "retention values must be positive integers", "Use a whole number within the documented range.")
		}
		parsed = parsed*10 + int(char-'0')
	}
	if parsed <= 0 {
		return 0, contract.NewFault(contract.ValidationFailed, "retention values must be positive integers", "Use a whole number within the documented range.")
	}
	return parsed, nil
}
