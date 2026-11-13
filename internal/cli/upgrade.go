package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/release"
	"github.com/mbvlabs/eregion/internal/upgrade"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const nodesUpgradeCommand = "eregion apply --auto-upgrade-nodes"

var upgradeEnvNames = []string{
	"XDG_CACHE_HOME",
}

type upgradeResult struct {
	From         upgrade.Version   `json:"from"`
	To           upgrade.Version   `json:"to"`
	Path         string            `json:"path"`
	PreviousPath string            `json:"previous_path,omitempty"`
	Changed      bool              `json:"changed"`
	DryRun       bool              `json:"dry_run"`
	Warnings     []upgrade.Warning `json:"warnings,omitempty"`
	Nodes        string            `json:"nodes"`
	NodesCommand string            `json:"nodes_command"`
}

// upgradeSpecs is attached by the root orchestrator. It advertises only the
// local CLI upgrade; node upgrades belong to `apply --auto-upgrade-nodes`.
func (a *app) upgradeSpecs() []spec {
	meta := readMeta("Install a signed eregion release for this CLI only",
		[]string{"Move this operator CLI to a newer signed release on its release channel. Nodes are never upgraded by this command."},
		[]string{"eregion upgrade --dry-run --json", "eregion upgrade", "eregion upgrade --channel edge"}, "eregion version")
	meta.Audience = []string{"agent", "human"}
	meta.Platforms = []string{"linux", "darwin"}
	meta.Mutating, meta.DryRun, meta.Prompts, meta.Remote = true, true, true, true
	meta.InputFlags = []string{"version", "channel", "yes"}
	meta.NeedsRestart = "never"
	meta.Environment = upgradeEnvNames
	meta.ErrorCodes = []contract.Code{
		contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.NotFound, contract.PreconditionFailed, contract.Conflict, contract.Unavailable,
		contract.Internal, contract.Cancelled,
	}

	return []spec{{
		use:  "upgrade",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("version", "", "Release version to install; defaults to the channel's current manifest")
			cmd.Flags().String("channel", "", "Release channel: edge, rc or stable")
			cmd.Flags().Bool("yes", false, "Replace the binary without an interactive confirmation")
			cmd.Flags().Bool("dry-run", false, "Resolve and print the from/to plan without downloading or replacing")
		},
		run: a.upgradeRun,
	}}
}

func (a *app) upgradeRun(cmd *cobra.Command, _ []string) (result, error) {
	version := upgradeFlag(cmd, "version")
	channelFlag := upgradeFlag(cmd, "channel")
	yes, _ := cmd.Flags().GetBool("yes")
	dry := dryRun(cmd)

	target, err := upgrade.ResolveSelf()
	if err != nil {
		return result{}, err
	}
	channel, err := upgrade.SelectVersionChannel(channelFlag, version)
	if err != nil {
		return result{}, err
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	client := &release.Client{}
	manifest, err := resolveUpgradeManifest(ctx, client, channel, version, channelFlag == "", buildinfo.Version)
	if err != nil {
		return result{}, err
	}
	from := upgrade.Version{Version: buildinfo.Version, Channel: buildinfo.Channel}
	requested := strings.TrimSpace(version) != "" || strings.TrimSpace(channelFlag) != ""
	plan := upgrade.Decide(from, manifest, requested)
	plan.Target = target

	data := upgradeResult{
		From: plan.From, To: plan.To, Path: target, Changed: plan.Changed,
		DryRun: dry, Warnings: plan.Warnings, Nodes: "not upgraded", NodesCommand: nodesUpgradeCommand,
	}
	if !plan.Changed {
		return result{
			data:  data,
			plain: fmt.Sprintf("eregion is already on %s (%s).\nOnly the local CLI is managed here; nodes were not upgraded.\nNodes: %s\n", plan.From.Version, upgradeReleaseLabel(plan.From.Version), nodesUpgradeCommand),
		}, nil
	}
	if dry {
		plain := fmt.Sprintf("Would upgrade eregion %s → %s (%s)\nat %s\n\n", plan.From.Version, plan.To.Version, upgradeReleaseLabel(plan.To.Version), target)
		for _, warning := range plan.Warnings {
			plain += "warning: " + warning.Message + "\n"
		}
		plain += fmt.Sprintf("The previous binary is kept when the filesystem allows a hard link.\nOnly the local CLI would be upgraded; nodes were not upgraded.\nNodes: %s\n", nodesUpgradeCommand)
		return result{data: data, plain: plain}, nil
	}

	if !yes && upgradeCanPrompt(cmd) {
		ok, err := upgradePrompt(cmd, fmt.Sprintf("Upgrade eregion %s → %s (%s) at %s?", plan.From.Version, plan.To.Version, upgradeReleaseLabel(plan.To.Version), target))
		if err != nil {
			return result{}, err
		}
		if !ok {
			return result{}, contract.NewFault(
				contract.PreconditionFailed,
				"upgrade was not confirmed",
				"Re-run with --yes to skip the prompt; no binary was replaced.",
			)
		}
	}

	if !upgradeDirWritable(filepath.Dir(target)) {
		return result{}, contract.NewFault(
			contract.PreconditionFailed,
			"the directory holding this binary is not writable",
			fmt.Sprintf("Install with the channel's install.sh or run with permission to write %s; no binary was replaced.", filepath.Dir(target)),
		)
	}

	cached, err := client.Fetch(ctx, manifest, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return result{}, err
	}
	if err := upgrade.Preflight(ctx, cached, manifest); err != nil {
		return result{}, err
	}
	previous, warnings, err := upgrade.Swap(target, cached)
	if err != nil {
		return result{}, err
	}
	data.PreviousPath = previous
	data.Changed = true
	data.Warnings = append(data.Warnings, warnings...)

	lines := []string{
		fmt.Sprintf("eregion upgraded %s → %s (%s)", plan.From.Version, plan.To.Version, upgradeReleaseLabel(plan.To.Version)),
	}
	if previous != "" {
		lines = append(lines, "previous binary kept at "+previous)
	}
	for _, warning := range plan.Warnings {
		lines = append(lines, "warning: "+warning.Message)
	}
	for _, warning := range warnings {
		lines = append(lines, "warning: "+warning.Message)
	}
	lines = append(lines,
		"Only the local CLI was upgraded; nodes were not upgraded.",
		"Nodes: "+nodesUpgradeCommand,
	)
	return result{data: data, plain: strings.Join(lines, "\n") + "\n"}, nil
}

func upgradeReleaseLabel(version string) string {
	parsed, ok := release.ParseStable(strings.TrimPrefix(version, "v"))
	if ok && !parsed.Prerelease() {
		return "stable"
	}
	return "prerelease"
}

type upgradeManifestResolver interface {
	Resolve(context.Context, string, string) (release.Manifest, error)
}

func resolveUpgradeManifest(ctx context.Context, client upgradeManifestResolver, channel, version string, defaultChannel bool, currentVersion string) (release.Manifest, error) {
	manifest, err := client.Resolve(ctx, channel, version)
	followRC := channel == release.ChannelRC && defaultChannel && version == ""
	if err != nil && (!followRC || contract.Classify(err).Code != contract.NotFound) {
		return release.Manifest{}, err
	}
	if !followRC {
		return manifest, nil
	}
	// Prefer the candidate's major; if the pointer is absent, use the installed
	// RC's major. Only a genuinely absent pointer permits that fallback.
	lineVersion := currentVersion
	if err == nil {
		lineVersion = manifest.Version
	}
	line, ok := release.ParseStable(lineVersion)
	if !ok {
		return release.Manifest{}, contract.NewFault(contract.ValidationFailed, "invalid rc release version", "")
	}
	final, finalErr := client.Resolve(ctx, line.Channel(), "")
	if finalErr != nil {
		if err == nil && contract.Classify(finalErr).Code == contract.NotFound {
			return manifest, nil
		}
		return release.Manifest{}, finalErr
	}
	if err != nil {
		return final, nil
	}
	return upgrade.PreferFinal(manifest, final), nil
}

func upgradeCanPrompt(cmd *cobra.Command) bool {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	return !jsonMode && outputMode != "json" && term.IsTerminal(int(os.Stdin.Fd())) //nolint:gosec // G115: file descriptors fit in int.
}

func upgradePrompt(cmd *cobra.Command, question string) (bool, error) {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", question)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func upgradeDirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".eregion-writetest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func upgradeFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}
