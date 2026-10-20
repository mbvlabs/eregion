package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type result struct {
	command string
	data    any
	plain   string
	next    []string
}

type spec struct {
	use      string
	meta     CommandMeta
	args     cobra.PositionalArgs
	flags    func(*cobra.Command)
	run      func(*cobra.Command, []string) (result, error)
	children []spec
}

type app struct {
	root    *cobra.Command
	meta    registry
	result  result
	started bool
	secrets *secrets.Registry
}

func readMeta(summary string, when, examples []string, next ...string) CommandMeta {
	return CommandMeta{
		Summary: summary, WhenToUse: when, Examples: examples, Next: next,
		Audience: []string{"agent", "human"}, Platforms: []string{"linux", "darwin", "windows"},
		NeedsRestart: "never", Output: "result",
		ErrorCodes: []contract.Code{contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.Internal, contract.Cancelled, contract.Unavailable},
	}
}

func newApp() *app {
	a := &app{meta: make(registry), secrets: &secrets.Registry{}}
	rootSpec := spec{
		use: "eregion",
		meta: readMeta("Agent-first application platform",
			[]string{"Discover the implemented CLI surface before choosing an operation."},
			[]string{"eregion commands --json", "eregion --help"}, "eregion commands --json"),
		run: func(cmd *cobra.Command, _ []string) (result, error) {
			return result{
				data:  map[string]any{"version": buildinfo.Current().Version, "stage": "M0.2 — durable diagnostic operations", "commands": a.meta.catalog(a.root)},
				plain: "eregion — agent-first application platform\n\nM0.2: PostgreSQL/River diagnostic operations and automation API.\nNo host bootstrap or application deployment commands yet.\n\nNext: eregion commands --json",
			}, nil
		},
	}
	a.root = a.command(rootSpec)
	a.root.SilenceErrors, a.root.SilenceUsage = true, true
	a.root.CompletionOptions.DisableDefaultCmd = true
	a.root.PersistentFlags().String("output", "plain", "Output mode: plain or json")
	a.root.PersistentFlags().Bool("json", false, "Shorthand for --output json")
	a.root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		var required *pflag.ValueRequiredError
		if errors.As(err, &required) {
			name := "--" + required.GetFlag().Name
			return &contract.Fault{Code: contract.InputRequired, Message: name + " requires a value", Hint: "Supply a value for " + name + ".", Details: map[string]any{"flag": name}}
		}
		return contract.NewFault(contract.Usage, err.Error(), "Inspect flags with eregion help --json.")
	})
	for _, s := range append(a.specs(), a.runtimeSpecs()...) {
		a.attach(a.root, s)
	}
	a.root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		a.started = true
		a.result = a.help(cmd)
		a.result.command = cmd.CommandPath()
	})
	for _, cmd := range walk(a.root) {
		cmd.InitDefaultHelpFlag()
	}
	return a
}

func (a *app) command(s spec) *cobra.Command {
	cmd := &cobra.Command{Use: s.use, Short: s.meta.Summary, Long: s.meta.Summary, Example: strings.Join(s.meta.Examples, "\n")}
	args := s.args
	if args == nil {
		args = cobra.NoArgs
	}
	cmd.Args = func(cmd *cobra.Command, values []string) error {
		if err := args(cmd, values); err != nil {
			var fault *contract.Fault
			if errors.As(err, &fault) {
				return err
			}
			return contract.NewFault(contract.Usage, err.Error(), "Inspect the command with --help --json.")
		}
		return nil
	}
	if s.run != nil {
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			a.started = true
			res, err := s.run(cmd, args)
			res.command = cmd.CommandPath()
			res.next = s.meta.Next
			a.result = res
			return err
		}
	}
	if s.flags != nil {
		s.flags(cmd)
	}
	a.meta[cmd] = s.meta
	return cmd
}

func (a *app) attach(parent *cobra.Command, s spec) {
	cmd := a.command(s)
	parent.AddCommand(cmd)
	if parent == a.root && cmd.Name() == "help" {
		a.root.SetHelpCommand(cmd)
	}
	for _, child := range s.children {
		a.attach(cmd, child)
	}
}

func (a *app) valueFlags() []string {
	var names []string
	for _, cmd := range walk(a.root) {
		for _, flags := range []*pflag.FlagSet{cmd.LocalFlags(), cmd.PersistentFlags()} {
			flags.VisitAll(func(flag *pflag.Flag) {
				if flag.NoOptDefVal == "" && flag.Name != "output" {
					names = append(names, flag.Name)
				}
			})
		}
	}
	return names
}

func (a *app) specs() []spec {
	return []spec{
		{
			use:  "version",
			meta: readMeta("Print binary and build information", []string{"Identify the binary before reporting issues or checking compatibility."}, []string{"eregion version", "eregion version --json"}, "eregion commands --json"),
			run: func(_ *cobra.Command, _ []string) (result, error) {
				info := buildinfo.Current()
				return result{data: info, plain: fmt.Sprintf("eregion %s\ncommit: %s\nplatform: %s/%s\ngo: %s\ndirty: %t", info.Version, info.Commit, info.OS, info.Arch, info.Go, info.Dirty)}, nil
			},
		},
		{
			use:  "commands",
			meta: readMeta("List implemented commands, flags, and safety metadata", []string{"Discover available commands and their machine-readable contracts. The entire catalog is validated before execution."}, []string{"eregion commands --json", "eregion commands --path version --json"}, "eregion help --json"),
			flags: func(cmd *cobra.Command) {
				cmd.Flags().String("path", "", "Restrict discovery to an exact command path, such as version")
			},
			run: a.commands,
		},
		{
			use:  "help [command...]",
			meta: readMeta("Describe a command without executing it", []string{"Inspect usage, flags, and examples in plain text or JSON."}, []string{"eregion help", "eregion help commands --json", "eregion version --help --json"}),
			args: cobra.ArbitraryArgs,
			run: func(_ *cobra.Command, args []string) (result, error) {
				cmd, err := a.resolve(args)
				if err != nil {
					return result{}, err
				}
				return a.help(cmd), nil
			},
		},
	}
}

func (a *app) commands(cmd *cobra.Command, _ []string) (result, error) {
	commands := a.meta.catalog(a.root)
	path, _ := cmd.Flags().GetString("path")
	if cmd.Flags().Changed("path") {
		if strings.TrimSpace(path) == "" {
			return result{}, &contract.Fault{Code: contract.InputRequired, Message: "--path requires a command path", Hint: "Use --path version, or omit --path to list all commands.", Details: map[string]any{"flag": "--path"}}
		}
		selected, err := a.resolve(strings.Fields(path))
		if err != nil {
			return result{}, err
		}
		commands = []Command{a.meta.describe(selected)}
	}
	var plain strings.Builder
	plain.WriteString("Implemented commands (metadata validated)\n\n")
	for _, command := range commands {
		fmt.Fprintf(&plain, "  %-20s %s\n", command.Path, command.Summary)
	}
	return result{data: map[string]any{"commands": commands}, plain: plain.String()}, nil
}

func (a *app) resolve(args []string) (*cobra.Command, error) {
	if len(args) > 0 && args[0] == a.root.Name() {
		args = args[1:]
	}
	cmd, remaining, err := a.root.Find(args)
	if err != nil || len(remaining) != 0 {
		return nil, contract.NewFault(contract.Usage, "unknown command path", "Discover exact paths with eregion commands --json.")
	}
	return cmd, nil
}

func (a *app) help(cmd *cobra.Command) result {
	command := a.meta.describe(cmd)
	text := plainHelp(command)
	if cmd.HasSubCommands() {
		text += "\nCommands:\n"
		for _, child := range cmd.Commands() {
			text += fmt.Sprintf("  %-12s %s\n", child.Name(), child.Short)
		}
	}
	return result{command: cmd.CommandPath(), data: command, plain: text, next: a.meta[cmd].Next}
}
