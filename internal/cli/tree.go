package cli

import (
	"errors"
	"fmt"
	"sort"
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
	// streamed means the command wrote its own terminal records (NDJSON,
	// append-only plain, or the interactive view) and the runner must not
	// append a second envelope.
	streamed bool
	// streamFault is a failure the runner still has to show after a streamed
	// command wrote its own output. ssh --command uses it for the sudo refusal,
	// where the remote stream alone would hide the fault code and its fix.
	streamFault error
}

type spec struct {
	use      string
	hidden   bool
	meta     CommandMeta
	args     cobra.PositionalArgs
	flags    func(*cobra.Command)
	run      func(*cobra.Command, []string) (result, error)
	children []spec
}

type app struct {
	root        *cobra.Command
	meta        registry
	result      result
	started     bool
	helpErr     error
	helpCommand *cobra.Command
	secrets     *secrets.Registry
	// nodeClients holds one client per resolved workspace access for the command's life.
	nodeClients map[string]*nodeClient
	// backend overrides the node transport in tests; nil uses the saved workspace access.
	backend Backend
	// loginSeams replaces the probe and readiness check in tests.
	loginSeams loginSeams
}

func readMeta(summary string, when, examples []string, next ...string) CommandMeta {
	return CommandMeta{
		Summary: summary, WhenToUse: when, Examples: examples, Next: next,
		Audience: []string{"agent", "human"}, Platforms: []string{"linux", "darwin"},
		NeedsRestart: "never", Output: "result", OutputModes: []string{"plain", "json"},
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
			commands := a.meta.catalog(a.root)
			return result{
				data:  map[string]any{"version": buildinfo.Current().Version, "commands": commands},
				plain: a.overview(),
			}, nil
		},
	}
	a.root = a.command(rootSpec)
	a.root.SilenceErrors, a.root.SilenceUsage = true, true
	a.root.CompletionOptions.DisableDefaultCmd = true
	a.root.PersistentFlags().String("output", "plain", "Output mode: plain or json; commands that follow an operation also accept ndjson")
	a.root.PersistentFlags().Bool("json", false, "Shorthand for --output json")
	a.root.PersistentFlags().Bool("auto-upgrade-nodes", false, "Install this CLI's build on a node whose build differs before the command runs")
	a.root.PersistentFlags().String("workspace", "", "Verified workspace name; overrides EREGION_WORKSPACE and the current workspace")
	a.root.PersistentFlags().String("workspace-file", "", "Private workspace store (defaults to EREGION_WORKSPACE_FILE or XDG config)")
	a.root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		var required *pflag.ValueRequiredError
		if errors.As(err, &required) {
			name := "--" + required.GetFlag().Name
			return &contract.Fault{Code: contract.InputRequired, Message: name + " requires a value", Hint: "Supply a value for " + name + ".", Details: map[string]any{"flag": name}}
		}
		return contract.NewFault(contract.Usage, err.Error(), "Inspect flags with eregion help --json.")
	})
	for _, specs := range [][]spec{a.specs(), a.runtimeSpecs(), a.nodeSpecs(), a.tunnelSpecs(), a.serverSpecs(), a.hostSpecs(), a.loginSpecs(), a.domainSpecs(), a.dnsProviderSpecs(), a.workspaceCommandSpecs(), a.appSpecs(), a.envSpecs(), a.processSpecs(), a.configSpecs(), a.deliverySpecs(), a.imageSpecs(), a.retentionSpecs(), a.resourceSpecs(), a.secretSpecs(), a.secretsSpecs(), a.protectSpecs(), a.tokenSpecs(), a.upgradeSpecs(), a.guideSpecs(), a.uiSpecs(), a.logsSpecs(), a.metricsSpecs(), a.diagnoseSpecs()} {
		for _, s := range specs {
			a.attach(a.root, s)
		}
	}
	a.attachServerExtras()
	a.root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		a.started = true
		args := cmd.Flags().Args()
		// Cobra handles --help before validating positional arguments. A
		// group with no positional usage must not turn an unknown child into
		// successful parent help. Commands that declare positional inputs keep
		// their normal help behavior, including leaf arguments.
		if cmd.HasSubCommands() && len(strings.Fields(cmd.Use)) == 1 && len(args) > 0 {
			a.helpErr = unknownCommandFault(fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath()), suggestCommandPaths(cmd, args[0]))
			return
		}
		if cmd == a.helpCommand && len(args) > 0 {
			if _, err := a.resolve(args); err != nil {
				a.helpErr = err
				return
			}
		}
		a.result = a.help(cmd)
		a.result.command = cmd.CommandPath()
	})
	for _, cmd := range walk(a.root) {
		cmd.InitDefaultHelpFlag()
	}
	return a
}

func (a *app) command(s spec) *cobra.Command {
	cmd := &cobra.Command{Use: s.use, Short: s.meta.Summary, Long: s.meta.Summary, Example: strings.Join(s.meta.Examples, "\n"), Hidden: s.hidden}
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
			return unknownCommandFault(err.Error(), suggestCommandPaths(cmd, unknownCommandToken(err)))
		}
		return nil
	}
	if s.run != nil {
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			a.started = true
			res, err := s.run(cmd, args)
			res.command = cmd.CommandPath()
			if res.next == nil {
				res.next = concreteNext(cmd, args, s.meta.Next)
			}
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
		a.helpCommand = cmd
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
				return result{data: info, plain: fmt.Sprintf("eregion %s\nchannel: %s\ncommit: %s\nplatform: %s/%s\ngo: %s\ndirty: %t", info.Version, info.Channel, info.Commit, info.OS, info.Arch, info.Go, info.Dirty)}, nil
			},
		},
		{
			use:  "commands",
			meta: readMeta("List implemented commands, flags, and safety metadata", []string{"Discover available commands and their machine-readable contracts. The entire catalog is validated before execution, and discovery never contacts a server or prompts."}, []string{"eregion commands --json", "eregion commands --search deploy", "eregion commands --search rollback --json", "eregion commands --check-meta", "eregion commands --path version --json"}, "eregion help --json"),
			flags: func(cmd *cobra.Command) {
				cmd.Flags().Bool("check-meta", false, "Validate every command's metadata and report the result")
				cmd.Flags().String("path", "", "Restrict discovery to an exact command path, such as version")
				cmd.Flags().String("search", "", "Search visible command paths, summaries, and examples")
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
	search, _ := cmd.Flags().GetString("search")
	check, _ := cmd.Flags().GetBool("check-meta")
	pathSet, searchSet := cmd.Flags().Changed("path"), cmd.Flags().Changed("search")
	active := 0
	for _, enabled := range []bool{pathSet, searchSet, check} {
		if enabled {
			active++
		}
	}
	if active > 1 {
		return result{}, &contract.Fault{
			Code:    contract.Usage,
			Message: "choose only one of --path, --search or --check-meta",
			Hint:    "Run one discovery mode at a time.",
			Details: map[string]any{"flags": []string{"--path", "--search", "--check-meta"}},
		}
	}
	if check {
		report := a.meta.check(a.root)
		if !report.OK {
			return result{}, metadataCheckFault(report)
		}
		return result{data: report, plain: fmt.Sprintf("Command metadata is complete (%d commands).", report.Checked)}, nil
	}
	if searchSet {
		if strings.TrimSpace(search) == "" {
			return result{}, &contract.Fault{Code: contract.InputRequired, Message: "--search requires search text", Hint: "Use --search deploy, or omit --search to list all commands.", Details: map[string]any{"flag": "--search"}}
		}
		matches := searchCatalog(commands, search)
		plain := ""
		if len(matches) > 0 {
			plain = fmt.Sprintf("Commands matching %q\n\n", strings.TrimSpace(search)) + plainCommandList(matches)
		}
		return result{data: map[string]any{"commands": matches}, plain: plain}, nil
	}
	if pathSet {
		if strings.TrimSpace(path) == "" {
			return result{}, &contract.Fault{Code: contract.InputRequired, Message: "--path requires a command path", Hint: "Use --path version, or omit --path to list all commands.", Details: map[string]any{"flag": "--path"}}
		}
		target, err := a.resolve(strings.Fields(path))
		if err != nil {
			return result{}, err
		}
		commands = []Command{a.meta.describe(target)}
	}
	return result{data: map[string]any{"commands": commands}, plain: "Implemented commands (metadata validated)\n\n" + plainCommandList(commands)}, nil
}

// overview is the no-argument operator map. It is generated from the same
// catalog as `commands --json`, so it never needs a workspace, server, or TUI.
func (a *app) overview() string {
	var plain strings.Builder
	fmt.Fprintf(&plain, "eregion — application platform\nversion: %s\n\n", buildinfo.Current().Version)
	plain.WriteString("Usage:\n  eregion [flags]\n  eregion [command]\n\nAvailable commands:\n")
	children := make([]*cobra.Command, 0, len(a.root.Commands()))
	for _, child := range a.root.Commands() {
		if !child.Hidden {
			children = append(children, child)
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].CommandPath() < children[j].CommandPath() })
	for _, child := range children {
		meta := a.meta[child]
		fmt.Fprintf(&plain, "  %-20s %s\n", child.Name(), meta.Summary)
	}
	plain.WriteString("\nFlags:\n")
	writePlainFlags(&plain, a.meta.describe(a.root).Flags)
	plain.WriteString("\nExamples:\n  eregion server --help\n  eregion app --help\n  eregion help env update\n")
	plain.WriteString("\nOffline discovery:\n")
	plain.WriteString("  eregion commands --json                 full catalog with flags and safety metadata\n")
	plain.WriteString("  eregion commands --search TEXT          search paths, summaries and examples\n")
	plain.WriteString("  eregion commands --check-meta           validate the catalog\n")
	plain.WriteString("  eregion help <command> --json           one command contract\n")
	plain.WriteString("\nAccepted work is not completed work: follow the returned operation before reporting success.\n")
	return plain.String()
}

func plainCommandList(commands []Command) string {
	var plain strings.Builder
	for _, command := range commands {
		fmt.Fprintf(&plain, "  %-20s %s\n", command.Path, command.Summary)
	}
	return plain.String()
}

func (a *app) resolve(args []string) (*cobra.Command, error) {
	if len(args) > 0 && args[0] == a.root.Name() {
		args = args[1:]
	}
	cmd, remaining, err := a.root.Find(args)
	if err != nil || len(remaining) != 0 {
		parent := cmd
		if parent == nil {
			parent = a.root
		}
		token := ""
		for _, value := range remaining {
			if !strings.HasPrefix(value, "-") {
				token = value
				break
			}
		}
		return nil, unknownCommandFault("unknown command path", suggestCommandPaths(parent, token))
	}
	return cmd, nil
}

func (a *app) help(cmd *cobra.Command) result {
	command := a.meta.describe(cmd)
	if cmd == a.root {
		return result{command: cmd.CommandPath(), data: command, plain: a.overview(), next: a.meta[cmd].Next}
	}
	var children []Command
	for _, child := range cmd.Commands() {
		if !commandHidden(child) {
			children = append(children, a.meta.describe(child))
		}
	}
	text := plainHelp(command, children)
	return result{command: cmd.CommandPath(), data: command, plain: text, next: a.meta[cmd].Next}
}

// concreteNext fills <app>, <environment> and the command's other positional
// placeholders in the result's next commands from the arguments and flags it
// ran with, so an agent can run them as they are. The catalog keeps the
// templates.
func concreteNext(cmd *cobra.Command, args []string, next []string) []string {
	values := map[string]string{}
	for i, field := range strings.Fields(cmd.Use)[1:] {
		if i < len(args) && strings.HasPrefix(field, "<") && strings.HasSuffix(field, ">") {
			values[field] = args[i]
		}
	}
	for placeholder, flag := range map[string]string{"<app>": "app", "<environment>": "environment"} {
		if values[placeholder] == "" {
			values[placeholder] = flagString(cmd, flag)
		}
	}
	if values["<app>"] == "" && strings.HasPrefix(cmd.CommandPath(), "eregion app create") {
		values["<app>"] = flagString(cmd, "name")
	}
	out := make([]string, 0, len(next))
	for _, line := range next {
		for placeholder, value := range values {
			if value != "" {
				line = strings.ReplaceAll(line, placeholder, value)
			}
		}
		out = append(out, line)
	}
	return out
}
