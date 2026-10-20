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
	Summary          string          `json:"summary"`
	WhenToUse        []string        `json:"when_to_use"`
	Examples         []string        `json:"examples"`
	Next             []string        `json:"next,omitempty"`
	Audience         []string        `json:"audience"`
	Platforms        []string        `json:"platforms"`
	Mutating         bool            `json:"mutating"`
	MutatingFlags    []string        `json:"mutating_flags,omitempty"`
	Destructive      bool            `json:"destructive"`
	DestructiveFlags []string        `json:"destructive_flags,omitempty"`
	DryRun           bool            `json:"supports_dry_run"`
	DryRunReason     string          `json:"dry_run_unavailable_reason,omitempty"`
	Confirmation     string          `json:"confirmation,omitempty"`
	Prompts          bool            `json:"prompts"`
	InputFlags       []string        `json:"input_flags,omitempty"`
	Remote           bool            `json:"remote"`
	Environment      []string        `json:"environment,omitempty"`
	NeedsRestart     string          `json:"needs_restart"`
	Output           string          `json:"output"`
	ErrorCodes       []contract.Code `json:"error_codes"`
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
		if !nonempty(meta.Platforms) || !allIn(meta.Platforms, []string{"linux", "darwin", "windows"}) {
			add("platforms must list supported operating systems")
		}
		if slices.Contains(meta.Audience, "node") && (len(meta.Platforms) != 1 || meta.Platforms[0] != "linux") {
			add("node commands require Linux-only platforms")
		}
		if !slices.Contains([]string{"never", "possible", "required"}, meta.NeedsRestart) {
			add("needs_restart must be never, possible, or required")
		}
		if !slices.Contains([]string{"result", "events"}, meta.Output) {
			add("output must be result or events")
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
		if meta.Confirmation != "" && (meta.Confirmation != "plan-bound" || flagFor(cmd, "confirm") == nil) {
			add("confirmation must be plan-bound and expose --confirm")
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
		commands = append(commands, r.describe(cmd))
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Path < commands[j].Path })
	return commands
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
	fmt.Fprintf(&out, "%s\n\n%s\n\nUsage: %s\n\nFlags:\n", command.Path, command.Summary, command.Use)
	for _, flag := range command.Flags {
		fmt.Fprintf(&out, "  --%-12s %s\n", flag.Name, flag.Usage)
	}
	out.WriteString("\nExamples:\n")
	for _, example := range command.Examples {
		fmt.Fprintf(&out, "  %s\n", example)
	}
	return out.String()
}
