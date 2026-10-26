package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/guide"
	"github.com/spf13/cobra"
)

type skillHarness struct {
	name      string
	label     string
	directory string
}

type skillInstallation struct {
	Harness string `json:"harness"`
	Path    string `json:"path"`
}

var skillHarnesses = []skillHarness{
	{name: "codex", label: "Codex", directory: ".codex/skills/eregion"},
	{name: "claude", label: "Claude", directory: ".claude/skills/eregion"},
	{name: "pi", label: "Pi", directory: ".pi/skills/eregion"},
	{name: "opencode", label: "OpenCode", directory: ".opencode/skills/eregion"},
	{name: "crush", label: "Crush", directory: ".crush/skills/eregion"},
}

// guideSpecs is the agent surface: the guide itself and the installer that
// writes the same text into a harness's SKILL.md.
func (a *app) guideSpecs() []spec {
	guideParent := readMeta("Print operator and agent guides",
		[]string{"Read how to operate eregion before running mutating commands."},
		[]string{"eregion guide agents"}, "eregion guide agents")
	guideParent.Platforms = []string{"linux", "darwin"}

	agentsMeta := readMeta("Print the agent operating guide",
		[]string{"Load the rules for JSON output, discovery, confirm tokens, idempotency and exit codes."},
		[]string{"eregion guide agents", "eregion guide agents --json"},
		"eregion skill install", "eregion commands --json")
	agentsMeta.Platforms = []string{"linux", "darwin"}

	skillParent := readMeta("Install the eregion agent skill",
		[]string{"Copy the agent operating guide into a coding-agent harness."},
		[]string{"eregion skill install", "eregion skill install --dry-run"})
	skillParent.Platforms = []string{"linux", "darwin"}
	skillParent.Next = []string{"eregion guide agents"}

	installMeta := readMeta("Write the eregion skill into agent harness directories",
		[]string{"Install the agent operating guide as SKILL.md so a coding-agent harness can find it."},
		[]string{"eregion skill install --harness claude", "eregion skill install --harness claude,opencode --dir /srv/app --dry-run"})
	installMeta.Platforms = []string{"linux", "darwin"}
	installMeta.Mutating, installMeta.DryRun, installMeta.Prompts = true, true, true
	installMeta.InputFlags = []string{"harness"}
	installMeta.Next = []string{"eregion guide agents"}

	return []spec{
		{
			use:  "guide",
			meta: guideParent,
			run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
			children: []spec{{
				use:  "agents",
				meta: agentsMeta,
				run:  a.guideAgents,
			}},
		},
		{
			use:  "skill",
			meta: skillParent,
			run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
			children: []spec{{
				use:  "install",
				meta: installMeta,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().StringSlice("harness", nil, "Harnesses to install: codex, claude, pi, opencode, crush")
					cmd.Flags().String("dir", "", "Directory to install into (default: the current directory)")
					cmd.Flags().Bool("dry-run", false, "Print the target paths without writing")
				},
				run: a.skillInstall,
			}},
		},
	}
}

// guideAgents prints exactly the document that skill install writes. The body
// is the same in plain and JSON output; JSON carries it in data.markdown.
func (a *app) guideAgents(_ *cobra.Command, _ []string) (result, error) {
	body := guide.Agents
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return result{data: map[string]string{"markdown": body}, plain: body}, nil
}

func skillDocument() string {
	body := strings.TrimSpace(guide.Agents)
	return "---\nname: eregion\ndescription: Operate eregion application servers with the eregion CLI. Use --json, read error.code, keep idempotency keys, and ask a human before passing destructive confirm tokens.\n---\n\n" + body + "\n"
}

func (a *app) skillInstall(cmd *cobra.Command, _ []string) (result, error) {
	requested, err := cmd.Flags().GetStringSlice("harness")
	if err != nil {
		return result{}, contract.NewFault(contract.Usage, err.Error(), "Use --harness with one or more of: "+validSkillHarnessNames()+".")
	}
	dir, err := cmd.Flags().GetString("dir")
	if err != nil {
		return result{}, contract.NewFault(contract.Usage, err.Error(), "Use --dir with a target directory.")
	}
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return result{}, contract.NewFault(contract.Internal, "could not resolve the skill directory", err.Error())
	}
	selected, err := selectSkillHarnesses(cmd, requested)
	if err != nil {
		return result{}, err
	}
	dry := dryRun(cmd)
	document := skillDocument()
	installations := make([]skillInstallation, 0, len(selected))
	labels := make([]string, 0, len(selected))
	for _, harness := range selected {
		target := filepath.Join(root, filepath.FromSlash(harness.directory), "SKILL.md")
		if !dry {
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return result{}, contract.NewFault(contract.Internal, "could not create the skill directory", err.Error())
			}
			if err := os.WriteFile(target, []byte(document), 0o644); err != nil { //nolint:gosec // G304: the operator-selected skill directory.
				return result{}, contract.NewFault(contract.Internal, "could not write the eregion skill", err.Error())
			}
		}
		installations = append(installations, skillInstallation{Harness: harness.name, Path: target})
		labels = append(labels, harness.label)
	}

	data := map[string]any{
		"name":          "eregion",
		"dry_run":       dry,
		"installations": installations,
	}
	if len(installations) == 1 {
		data["path"] = installations[0].Path
	}
	action := "Installed"
	if dry {
		action = "Would install"
	}
	lines := []string{fmt.Sprintf("%s the eregion skill for %s", action, strings.Join(labels, ", "))}
	for _, installation := range installations {
		lines = append(lines, "  "+installation.Path)
	}
	return result{data: data, plain: strings.Join(lines, "\n") + "\n"}, nil
}

func selectSkillHarnesses(cmd *cobra.Command, requested []string) ([]skillHarness, error) {
	if len(requested) > 0 {
		return parseSkillHarnessNames(requested)
	}
	if !skillCanPrompt(cmd) {
		return nil, contract.NewFault(
			contract.InputRequired,
			"skill install needs --harness",
			"Pass --harness with one or more of: "+validSkillHarnessNames()+".",
		)
	}
	return promptSkillHarnesses(cmd)
}

// skillCanPrompt matches the noninteractive decision of the rest of the CLI:
// structured output and anything that is not a terminal must receive flags.
func skillCanPrompt(cmd *cobra.Command) bool {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	return !jsonMode && outputMode != "json" && isTerminal(cmd.InOrStdin())
}

func promptSkillHarnesses(cmd *cobra.Command) ([]skillHarness, error) {
	writer := cmd.ErrOrStderr()
	if _, err := fmt.Fprintln(writer, "Select harnesses (comma-separated numbers):"); err != nil {
		return nil, err
	}
	for index, harness := range skillHarnesses {
		if _, err := fmt.Fprintf(writer, "  %d) %s\n", index+1, harness.label); err != nil {
			return nil, err
		}
	}
	if _, err := fmt.Fprint(writer, "Selection: "); err != nil {
		return nil, err
	}
	selection, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read harness selection: %w", err)
	}
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return nil, contract.NewFault(
			contract.InputRequired,
			"select at least one harness",
			"Pass --harness with one or more of: "+validSkillHarnessNames()+".",
		)
	}
	names := make([]string, 0, len(skillHarnesses))
	for _, value := range strings.Split(selection, ",") {
		number, convErr := strconv.Atoi(strings.TrimSpace(value))
		if convErr != nil || number < 1 || number > len(skillHarnesses) {
			return nil, contract.NewFault(
				contract.Usage,
				fmt.Sprintf("invalid harness selection %q", strings.TrimSpace(value)),
				fmt.Sprintf("Enter comma-separated numbers from 1 to %d.", len(skillHarnesses)),
			)
		}
		names = append(names, skillHarnesses[number-1].name)
	}
	return parseSkillHarnessNames(names)
}

func parseSkillHarnessNames(names []string) ([]skillHarness, error) {
	selected := make([]skillHarness, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, value := range names {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" {
			return nil, contract.NewFault(contract.Usage, "harness name cannot be empty", "Choose one or more of: "+validSkillHarnessNames()+".")
		}
		if seen[name] {
			continue
		}
		found := false
		for _, harness := range skillHarnesses {
			if harness.name == name {
				selected = append(selected, harness)
				seen[name] = true
				found = true
				break
			}
		}
		if !found {
			return nil, contract.NewFault(contract.Usage, fmt.Sprintf("unknown harness %q", value), "Choose one or more of: "+validSkillHarnessNames()+".")
		}
	}
	if len(selected) == 0 {
		return nil, contract.NewFault(contract.InputRequired, "select at least one harness", "Pass --harness with one or more of: "+validSkillHarnessNames()+".")
	}
	return selected, nil
}

func validSkillHarnessNames() string {
	names := make([]string, len(skillHarnesses))
	for i, harness := range skillHarnesses {
		names[i] = harness.name
	}
	return strings.Join(names, ", ")
}
