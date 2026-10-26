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

// UpgradeGuide is the release-specific part of the versioned guide.
type UpgradeGuide struct {
	Channels    []string `json:"channels"`
	TrustAnchor []string `json:"trust_anchor"`
	Workflow    []string `json:"workflow"`
	Nodes       []string `json:"nodes"`
	Limitations []string `json:"limitations"`
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
			Channels: []string{
				"edge follows every push to master and publishes edge-<12-hex>.",
				"stable-<major>.x follows one major line; a stable build without --channel follows its own line.",
				"stable follows the highest published stable line, including v1.0.0-alpha.N before the first final.",
				"install.sh per channel: / for stable, /1.x for the v1 line, /edge for edge.",
			},
			TrustAnchor: []string{
				"cosign keyless signatures from GitHub Actions OIDC; there is no release public key to configure.",
				"A manifest is signed by publish-stable.yml at refs/tags/v<version> or publish-edge.yml at refs/heads/master; the workflow identity is checked, not just the signature.",
				"Artifacts are verified by their signed SHA256 and again by the downloaded binary's reported version and channel.",
			},
			Workflow: []string{
				"upgrade --dry-run resolves the channel manifest and prints the from/to plan",
				"upgrade downloads and verifies the local CLI, checks the staged binary, then replaces it atomically",
				"upgrade keeps the previous binary at <path>.prev when the filesystem allows a hard link",
				"upgrade --channel edge|stable|stable-<major>.x selects a different channel; a dev build requires --channel",
			},
			Nodes: []string{
				"upgrade replaces only the operator CLI on this host.",
				"Nodes are not touched: run apply --auto-upgrade-nodes to move them to the CLI's build.",
				"An operator never installs a node binary from a different version than the CLI it runs.",
			},
			Limitations: []string{
				"A package-manager-managed binary is refused; upgrade with the package manager instead.",
				"Upgrading the CLI does not restart a service or prove that its node is compatible; the version lockstep check happens on the next operator command.",
				"Only linux and darwin amd64/arm64 builds are published.",
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
	out.WriteString("\n### Channels\n\n")
	for _, item := range guide.Upgrade.Channels {
		fmt.Fprintf(&out, "- %s\n", item)
	}
	out.WriteString("\n### Trust anchor\n\n")
	for _, item := range guide.Upgrade.TrustAnchor {
		fmt.Fprintf(&out, "- %s\n", item)
	}
	out.WriteString("\n### Nodes\n\n")
	for _, item := range guide.Upgrade.Nodes {
		fmt.Fprintf(&out, "- %s\n", item)
	}
	out.WriteString("\n### Limitations\n\n")
	for _, item := range guide.Upgrade.Limitations {
		fmt.Fprintf(&out, "- %s\n", item)
	}
	return out.String()
}
