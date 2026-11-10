package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// CommandMeta is the agent-facing safety/discovery contract. Values describe
// implemented behavior, not promises about commands planned for later milestones.
type CommandMeta struct {
	Summary           string          `json:"summary"`
	WhenToUse         []string        `json:"when_to_use"`
	Examples          []string        `json:"examples"`
	Next              []string        `json:"next,omitempty"`
	Audience          []string        `json:"audience"`
	TerminalOnly      bool            `json:"terminal_only,omitempty"`
	HumanFactor       bool            `json:"human_factor,omitempty"`
	Platforms         []string        `json:"platforms"`
	Mutating          bool            `json:"mutating"`
	MutatingFlags     []string        `json:"mutating_flags,omitempty"`
	Destructive       bool            `json:"destructive"`
	DestructiveFlags  []string        `json:"destructive_flags,omitempty"`
	DryRun            bool            `json:"supports_dry_run"`
	DryRunReason      string          `json:"dry_run_unavailable_reason,omitempty"`
	Confirmation      string          `json:"confirmation,omitempty"`
	Prompts           bool            `json:"prompts"`
	InputFlags        []string        `json:"input_flags,omitempty"`
	Remote            bool            `json:"remote"`
	Environment       []string        `json:"environment,omitempty"`
	NeedsRestart      string          `json:"needs_restart"`
	Output            string          `json:"output"`
	OutputModes       []string        `json:"output_modes"`
	DurableSubmission bool            `json:"durable_submission,omitempty"`
	ErrorCodes        []contract.Code `json:"error_codes"`
}

// terminalOnlyMeta marks a command that is executed by a person at an
// interactive terminal. Audience describes who can use it, while OutputModes
// describes what output the implementation can actually produce; a terminal
// command has no machine mode even though every command inherits --json.
func terminalOnlyMeta(meta CommandMeta) CommandMeta {
	meta.Audience = []string{"human"}
	meta.TerminalOnly = true
	meta.OutputModes = []string{"plain"}
	return meta
}

type Flag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Usage     string `json:"usage"`
	Required  bool   `json:"required"`
}

type Command struct {
	Path string `json:"path"`
	Use  string `json:"use"`
	CommandMeta
	Flags []Flag `json:"flags"`
}

type registry map[*cobra.Command]CommandMeta

func walk(root *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{root}
	for _, child := range root.Commands() {
		out = append(out, walk(child)...)
	}
	return out
}

func (r registry) validate(root *cobra.Command) error {
	var failures []string
	for _, cmd := range walk(root) {
		meta, ok := r[cmd]
		add := func(message string) { failures = append(failures, cmd.CommandPath()+": "+message) }
		if !ok {
			add("missing metadata")
			continue
		}
		if cmd.RunE == nil {
			add("missing implemented handler")
		}
		if strings.TrimSpace(meta.Summary) == "" || meta.Summary != cmd.Short {
			add("summary must match command help")
		}
		if !nonempty(meta.WhenToUse) || !nonempty(meta.Examples) {
			add("when_to_use and examples are required")
		}
		if !nonempty(meta.Audience) || !allIn(meta.Audience, []string{"agent", "human", "node"}) {
			add("audience must contain agent, human, or node")
		}
		if !nonempty(meta.Platforms) || !allIn(meta.Platforms, []string{"linux", "darwin"}) {
			add("platforms must list linux or darwin")
		}
		if slices.Contains(meta.Audience, "node") && (len(meta.Platforms) != 1 || meta.Platforms[0] != "linux") {
			add("node commands require Linux-only platforms")
		}
		nodePath := root.Name() + " node"
		if slices.Contains(meta.Audience, "node") && cmd.CommandPath() != nodePath && !strings.HasPrefix(cmd.CommandPath(), nodePath+" ") {
			add("node audience is only valid under " + nodePath)
		}
		if (cmd.CommandPath() == nodePath || strings.HasPrefix(cmd.CommandPath(), nodePath+" ")) && !slices.Contains(meta.Audience, "node") {
			add("commands under " + nodePath + " require node audience")
		}
		if !slices.Contains([]string{"never", "possible", "required"}, meta.NeedsRestart) {
			add("needs_restart must be never, possible, or required")
		}
		if !slices.Contains([]string{"result", "events"}, meta.Output) {
			add("output must be result or events")
		}
		if !nonempty(meta.OutputModes) || !allIn(meta.OutputModes, []string{"plain", "json", "ndjson"}) || !slices.Contains(meta.OutputModes, "plain") {
			add("output_modes must list plain, and only additionally json or ndjson")
		}
		if meta.TerminalOnly {
			if slices.Contains(meta.OutputModes, "json") || slices.Contains(meta.OutputModes, "ndjson") {
				add("terminal-only commands must not advertise machine output")
			}
			if len(meta.Audience) != 1 || meta.Audience[0] != "human" {
				add("terminal-only commands require human-only audience")
			}
		} else if !slices.Contains(meta.OutputModes, "json") {
			add("non-terminal commands must list plain and json output")
		}
		if meta.DurableSubmission && !meta.Mutating {
			add("durable submissions must be mutating")
		}
		if meta.HumanFactor && !meta.Mutating {
			add("human-factor commands must be mutating")
		}
		if len(meta.ErrorCodes) == 0 {
			add("error_codes are required")
		}
		for _, code := range meta.ErrorCodes {
			if !contract.KnownCode(code) {
				add("unknown error code " + string(code))
			}
		}
		mutating := meta.Mutating || len(meta.MutatingFlags) > 0
		if mutating && !meta.DryRun && strings.TrimSpace(meta.DryRunReason) == "" {
			add("mutations require dry-run or an explicit reason")
		}
		if meta.DryRun && (meta.DryRunReason != "" || flagFor(cmd, "dry-run") == nil) {
			add("dry-run requires a flag and no exemption")
		}
		if flag := flagFor(cmd, "dry-run"); meta.DryRun && flag != nil && flag.Value.Type() != "bool" {
			add("--dry-run must be boolean")
		}
		if meta.Destructive && !meta.Mutating {
			add("destructive commands must be mutating")
		}
		if meta.Destructive && len(meta.DestructiveFlags) > 0 {
			add("use destructive or destructive_flags, not both")
		}
		if (meta.Destructive || len(meta.DestructiveFlags) > 0) && meta.Confirmation == "" {
			add("destructive actions require confirmation")
		}
		if meta.Confirmation != "" && (meta.Confirmation != "plan-bound" && meta.Confirmation != "target" || flagFor(cmd, "confirm") == nil) {
			add("confirmation must be plan-bound or target and expose --confirm")
		}
		if flag := flagFor(cmd, "confirm"); meta.Confirmation != "" && flag != nil && flag.Value.Type() != "string" {
			add("--confirm must accept a token string")
		}
		if meta.Prompts && len(meta.InputFlags) == 0 {
			add("prompts require noninteractive input flags")
		}
		for _, name := range append(append(slices.Clone(meta.MutatingFlags), meta.DestructiveFlags...), meta.InputFlags...) {
			if flagFor(cmd, name) == nil {
				add("undefined metadata flag --" + name)
			}
		}
		for _, name := range meta.DestructiveFlags {
			if !meta.Mutating && !slices.Contains(meta.MutatingFlags, name) {
				add("destructive flag must also be mutating: --" + name)
			}
		}
		for _, next := range meta.Next {
			parts := strings.Fields(next)
			if len(parts) < 2 || parts[0] != root.Name() {
				add("next must reference an implemented command")
				continue
			}
			target, remaining, err := root.Find(parts[1:])
			if err != nil || (target == root && len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-")) {
				add("next must reference an implemented command: " + next)
			}
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return &contract.Fault{Code: contract.Internal, Message: "command metadata validation failed", Hint: "Fix the command registry before publishing this binary.", Details: map[string]any{"violations": failures}}
	}
	return nil
}

func (r registry) catalog(root *cobra.Command) []Command {
	commands := make([]Command, 0, len(r))
	for _, cmd := range walk(root) {
		if commandHidden(cmd) {
			continue
		}
		commands = append(commands, r.describe(cmd))
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Path < commands[j].Path })
	return commands
}

// commandHidden reports whether a command or one of its parents is hidden.
// Node-mode commands are attached to a hidden `node` parent so discovery lists
// only the operator surface while the node paths stay executable.
func commandHidden(cmd *cobra.Command) bool {
	for current := cmd; current != nil && current.Parent() != nil; current = current.Parent() {
		if current.Hidden {
			return true
		}
	}
	return false
}

func (r registry) describe(cmd *cobra.Command) Command {
	flags := map[string]Flag{}
	collect := func(flag *pflag.Flag) {
		if !flag.Hidden {
			flags[flag.Name] = Flag{Name: flag.Name, Shorthand: flag.Shorthand, Type: flag.Value.Type(), Default: flag.DefValue, Usage: flag.Usage, Required: len(flag.Annotations[cobra.BashCompOneRequiredFlag]) > 0}
		}
	}
	cmd.InheritedFlags().VisitAll(collect)
	cmd.LocalFlags().VisitAll(collect)
	listed := make([]Flag, 0, len(flags))
	for _, flag := range flags {
		listed = append(listed, flag)
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].Name < listed[j].Name })
	return Command{Path: cmd.CommandPath(), Use: cmd.UseLine(), CommandMeta: r[cmd], Flags: listed}
}

func flagFor(cmd *cobra.Command, name string) *pflag.Flag {
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag
	}
	if flag := cmd.PersistentFlags().Lookup(name); flag != nil {
		return flag
	}
	return cmd.InheritedFlags().Lookup(name)
}

func nonempty(items []string) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			return false
		}
	}
	return true
}

func allIn(items, allowed []string) bool {
	for _, item := range items {
		if !slices.Contains(allowed, item) {
			return false
		}
	}
	return true
}

func plainHelp(command Command) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\n%s\n\n", command.Path, command.Summary)
	if len(command.WhenToUse) > 0 {
		out.WriteString("When to use:\n")
		for _, when := range command.WhenToUse {
			fmt.Fprintf(&out, "  %s\n", when)
		}
		out.WriteString("\n")
	}
	fmt.Fprintf(&out, "Usage: %s\n\n", command.Use)
	fmt.Fprintf(&out, "Audience: %s\n", strings.Join(command.Audience, ", "))
	if command.TerminalOnly {
		out.WriteString("Machine output: not supported (requires an interactive terminal)\n")
	} else {
		fmt.Fprintf(&out, "Machine output: %s\n", strings.Join(command.OutputModes, ", "))
	}
	if command.HumanFactor {
		out.WriteString("Human factor: effectful execution may require a code from the authenticator app at a terminal")
		if command.DryRun {
			out.WriteString("; --dry-run remains machine-safe")
		}
		out.WriteString(".\n")
	}
	var safety []string
	if command.Mutating {
		safety = append(safety, "mutating")
	}
	if command.Destructive {
		safety = append(safety, "destructive")
	}
	if command.DryRun {
		safety = append(safety, "supports --dry-run preview")
	}
	if command.Confirmation != "" {
		safety = append(safety, "confirmation "+command.Confirmation+" via --confirm")
	}
	if command.Prompts {
		safety = append(safety, "prompts at a terminal")
	}
	if len(command.InputFlags) > 0 {
		names := make([]string, 0, len(command.InputFlags))
		for _, name := range command.InputFlags {
			names = append(names, "--"+name)
		}
		safety = append(safety, "noninteractive inputs "+strings.Join(names, ", "))
	}
	if len(safety) > 0 {
		fmt.Fprintf(&out, "Safety: %s\n", strings.Join(safety, "; "))
	}
	if command.DurableSubmission {
		out.WriteString("Result: accepted work is a durable operation; follow it to observe completion.\n")
	}
	if command.Output == "events" {
		out.WriteString("Result: an event stream over durable operation state; this command does not submit work.\n")
	}
	out.WriteString("\nFlags:\n")
	for _, flag := range command.Flags {
		required := ""
		if flag.Required {
			required = " (required)"
		}
		fmt.Fprintf(&out, "  --%-12s %s%s\n", flag.Name, flag.Usage, required)
	}
	out.WriteString("\nExamples:\n")
	for _, example := range command.Examples {
		fmt.Fprintf(&out, "  %s\n", example)
	}
	if len(command.Next) > 0 {
		out.WriteString("\nNext:\n")
		for _, next := range command.Next {
			fmt.Fprintf(&out, "  %s\n", next)
		}
	}
	return out.String()
}
