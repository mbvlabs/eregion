package upgrade

import (
	"fmt"
	"sort"
	"strings"
)

// GuideSchema and GuideVersion version the generated agent guide. Additive
// fields are allowed within a guide version; removals or meaning changes need a
// new version.
const (
	GuideSchema  = "eregion/guide/v1"
	GuideVersion = 1
)

// GuideCommand is one implemented command descriptor derived from the live
// command registry. Planned commands never appear.
type GuideCommand struct {
	Path        string   `json:"path"`
	Summary     string   `json:"summary"`
	Mutating    bool     `json:"mutating"`
	Destructive bool     `json:"destructive"`
	DryRun      bool     `json:"supports_dry_run"`
	Prompts     bool     `json:"prompts"`
	Remote      bool     `json:"remote"`
	InputFlags  []string `json:"input_flags,omitempty"`
	Examples    []string `json:"examples"`
	Next        []string `json:"next,omitempty"`
	ErrorCodes  []string `json:"error_codes"`
}

// UpgradeGuide is the upgrade-specific part of the versioned guide.
type UpgradeGuide struct {
	ManifestSchema string   `json:"manifest_schema"`
	TrustAnchor    []string `json:"trust_anchor"`
	Workflow       []string `json:"workflow"`
	Recovery       []string `json:"recovery"`
	SchemaPolicy   string   `json:"schema_policy"`
	Compatibility  string   `json:"compatibility"`
	Limitations    []string `json:"limitations"`
}

// Guide is the generated, versioned agent guide.
type Guide struct {
	Schema        string         `json:"schema"`
	GuideVersion  int            `json:"guide_version"`
	BinaryVersion string         `json:"binary_version"`
	Contract      string         `json:"contract"`
	NodeProtocol  int            `json:"node_protocol"`
	Commands      []GuideCommand `json:"commands"`
	Upgrade       UpgradeGuide   `json:"upgrade"`
}

// NewGuide builds a guide from the implemented command catalog.
func NewGuide(binaryVersion, contract string, nodeProtocol int, commands []GuideCommand) Guide {
	sorted := append([]GuideCommand(nil), commands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return Guide{
		Schema:        GuideSchema,
		GuideVersion:  GuideVersion,
		BinaryVersion: binaryVersion,
		Contract:      contract,
		NodeProtocol:  nodeProtocol,
		Commands:      sorted,
		Upgrade: UpgradeGuide{
			ManifestSchema: ManifestSchema,
			TrustAnchor: []string{
				"EREGION_UPGRADE_PUBLIC_KEY_FILE points at the published Ed25519 public key (preferred; regular non-symlink file, not group/world writable).",
				"EREGION_UPGRADE_PUBLIC_KEY may carry the same public key inline. Public keys are not secrets; private keys are never accepted by the CLI.",
			},
			Workflow: []string{
				"upgrade check --manifest MANIFEST --target operator --schema-version N",
				"upgrade apply --manifest MANIFEST --target operator --schema-version N --dry-run",
				"review changes and rerun with --confirm TOKEN",
				"upgrade status --target operator",
			},
			Recovery: []string{
				"An interrupted apply or rollback leaves a journal entry; the next apply or rollback reconciles it under the upgrade lock.",
				"upgrade rollback --target operator --dry-run prints a restore plan and token.",
				"upgrade rollback --target operator --confirm TOKEN restores one verified recorded backup; it never restores an arbitrary path.",
			},
			SchemaPolicy:  "Database migrations are forward-only. A release may require a newer schema; a newer schema with an older release is incompatible and is never downgraded in place.",
			Compatibility: "OS, architecture, contract version, node protocol, upgrade_from, and the forward-only schema direction must all pass before an artifact is downloaded.",
			Limitations: []string{
				"Applying an operator binary does not restart services or prove a coordinated control-plane rollout.",
				"CheckControlPlaneCompatibility validates an operator-supplied inventory; it does not observe live processes or prove HA.",
				"Downloads refuse redirects, so GitHub release-asset redirect URLs are not followed; publish direct HTTPS object URLs or use --artifact-dir.",
				"The node target is Linux-only and must match the control plane's node protocol.",
			},
		},
	}
}

// RenderMarkdown renders the same guide for humans and agents that read text.
// It contains only implemented command metadata.
func RenderMarkdown(guide Guide) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# eregion agent guide\n\n")
	fmt.Fprintf(&out, "- Schema: `%s`\n- Guide version: %d\n- Binary version: `%s`\n- Result contract: `%s`\n- Node protocol: %d\n\n", guide.Schema, guide.GuideVersion, guide.BinaryVersion, guide.Contract, guide.NodeProtocol)
	out.WriteString("## Commands\n\n")
	for _, command := range guide.Commands {
		fmt.Fprintf(&out, "### `%s`\n\n%s\n\n", command.Path, command.Summary)
		fmt.Fprintf(&out, "- Mutating: %t\n- Destructive: %t\n- Supports dry-run: %t\n- Prompts: %t\n- Remote: %t\n", command.Mutating, command.Destructive, command.DryRun, command.Prompts, command.Remote)
		if len(command.InputFlags) > 0 {
			fmt.Fprintf(&out, "- Noninteractive inputs: `--%s`\n", strings.Join(command.InputFlags, "`, `--"))
		}
		if len(command.Examples) > 0 {
			out.WriteString("- Examples:\n")
			for _, example := range command.Examples {
				fmt.Fprintf(&out, "  - `%s`\n", example)
			}
		}
		if len(command.Next) > 0 {
			out.WriteString("- Next:\n")
			for _, next := range command.Next {
				fmt.Fprintf(&out, "  - `%s`\n", next)
			}
		}
		out.WriteString("\n")
	}
	out.WriteString("## Upgrade workflow\n\n")
	for _, step := range guide.Upgrade.Workflow {
		fmt.Fprintf(&out, "- `%s`\n", step)
	}
	out.WriteString("\n### Trust anchor\n\n")
	for _, item := range guide.Upgrade.TrustAnchor {
		fmt.Fprintf(&out, "- %s\n", item)
	}
	out.WriteString("\n### Compatibility\n\n")
	fmt.Fprintf(&out, "%s\n\n", guide.Upgrade.Compatibility)
	out.WriteString("### Schema policy\n\n")
	fmt.Fprintf(&out, "%s\n\n", guide.Upgrade.SchemaPolicy)
	out.WriteString("### Recovery\n\n")
	for _, item := range guide.Upgrade.Recovery {
		fmt.Fprintf(&out, "- %s\n", item)
	}
	out.WriteString("\n### Limitations\n\n")
	for _, item := range guide.Upgrade.Limitations {
		fmt.Fprintf(&out, "- %s\n", item)
	}
	return out.String()
}
