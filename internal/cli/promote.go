package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// envPromoteSpec is the one dedicated staging-to-production artifact move.
// It has no target flag: production is fixed, so an operator or agent cannot
// accidentally promote into another environment. It is a deploy-family
// mutation (not destructive) and therefore has a dry run but no human factor.
func (a *app) envPromoteSpec() spec {
	meta := followingMeta(platformMutationMeta(
		"Promote an environment's serving release to production without rebuilding",
		"eregion env promote api staging --json"))
	meta.WhenToUse = []string{
		"Move the source environment's current serving release to the application's production environment as the exact same immutable image digest. No image is rebuilt and no source config, secret, domain or release command is copied.",
		"Production's current revision supplies the process formation, plain config, secret names/values, domain binding and release command; the ordinary blue/green health, Caddy switch, public verification and previous-container cleanup path runs unchanged. Production must already be deployable with a primary domain.",
		"The source must have a ready, unpruned serving release, and both environments must be on the same node. An artifact already serving in production, or an identical deployment already accepted, is refused rather than run twice. A real promotion is agent-safe; --json and no-TTY never prompt.",
	}
	meta.Examples = []string{
		"eregion env promote api staging --dry-run --json",
		"eregion env promote api staging --json",
		"eregion env promote api staging --detach --json",
	}
	meta.Next = []string{
		"eregion env status <app> production --json",
		"eregion release list <app> production --json",
		"eregion operations follow <id> --json",
	}
	return spec{
		use:   "promote <app> <environment>",
		meta:  meta,
		args:  platformTwoNames("app", "environment"),
		flags: addDeliveryFlags,
		run:   a.envPromoteCommand,
	}
}

func (a *app) envPromoteCommand(cmd *cobra.Command, args []string) (result, error) {
	appName, source := args[0], args[1]
	if source == platform.PromotionTargetEnvironment {
		return result{}, contract.NewFault(contract.ValidationFailed,
			"production cannot be promoted to itself",
			"Name the environment whose serving release should move to production, normally staging.")
	}
	target, err := platform.NewEnvironmentTarget(appName, platform.PromotionTargetEnvironment)
	if err != nil {
		return result{}, err
	}
	input := platform.PromoteInput{App: appName, SourceEnvironment: source}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.runDelivery(cmd, client, deliveryAction{
		Kind: platform.KindPromote, Target: target.String(), Input: input,
		Load: func() (string, json.RawMessage, error) {
			return a.loadPromotePlan(cmd, client, target.String(), input)
		},
		Outcome: a.promoteOutcome(appName, source),
	})
}

func (a *app) loadPromotePlan(cmd *cobra.Command, client Backend, target string, input platform.PromoteInput) (string, json.RawMessage, error) {
	preview, raw, err := a.previewDelivery(cmd, client, platform.KindPromote, target, input)
	if err != nil {
		return "", nil, err
	}
	return promotePreviewText(preview), raw, nil
}

func promotePreviewText(preview platform.Preview) string {
	value := func(key string) string {
		if raw, ok := preview.Data[key].(string); ok {
			return raw
		}
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Promote to %s\n", preview.Target)
	if source := value("source_environment"); source != "" {
		fmt.Fprintf(&out, "source environment: %s\n", source)
	}
	if id := value("source_release_id"); id != "" {
		fmt.Fprintf(&out, "source release: %s", id)
		if image := value("source_image"); image != "" {
			fmt.Fprintf(&out, " %s", image)
		}
		out.WriteString("\n")
	}
	if imageID := value("source_image_id"); imageID != "" {
		fmt.Fprintf(&out, "source image id: %s\n", imageID)
	}
	if commit := value("source_commit"); commit != "" {
		fmt.Fprintf(&out, "source commit: %s\n", commit)
	}
	if revision := value("target_revision_id"); revision != "" {
		fmt.Fprintf(&out, "production revision: %s\n", revision)
	}
	if serving := value("target_serving_release_id"); serving != "" {
		fmt.Fprintf(&out, "production serving: %s (stays until the switch passes)\n", serving)
	}
	out.WriteString(previewText(preview))
	return out.String()
}

func (a *app) promoteOutcome(app, source string) func(context.Context, Backend, contract.Operation) []string {
	outcome := a.deployOutcome(app, platform.PromotionTargetEnvironment)
	return func(ctx context.Context, client Backend, operation contract.Operation) []string {
		lines := outcome(ctx, client, operation)
		if len(lines) == 0 {
			return lines
		}
		return append([]string{"promoted from: " + app + "/" + source}, lines...)
	}
}
