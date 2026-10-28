package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func hostResult(data any) result {
	raw, _ := json.MarshalIndent(data, "", "  ")
	return result{data: data, plain: string(raw)}
}

// contextClient resolves the active saved context into the node RPC backend.
// There is no direct HTTP or token path: every operator command reaches the
// node through SSH inside WireGuard.
func (a *app) contextClient(cmd *cobra.Command) (Backend, error) {
	return a.nodeClient(cmd)
}

func (a *app) nodeClient(cmd *cobra.Command) (*nodeClient, error) {
	return a.nodeClientFor(cmd, "")
}

func (a *app) nodeClientFor(cmd *cobra.Command, name string) (*nodeClient, error) {
	current, err := contextByName(cmd, name)
	if err != nil {
		return nil, err
	}
	// One command reuses one client: the version check runs once and every
	// call shares one SSH connection over one WireGuard tunnel.
	if client, ok := a.nodeClients[current.Name]; ok {
		return client, nil
	}
	if current.SSH == nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "context "+current.Name+" has no SSH route", "Import a server init handoff with eregion login, or re-run server init.")
	}
	remote, err := routeRemote(current.SSH, false)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the context's SSH route is not usable", err.Error())
	}
	autoUpgrade := current.AutoUpgradeNodes
	if cmd != nil {
		autoUpgrade = autoUpgrade || boolFlag(cmd, "auto-upgrade-nodes")
	}
	remote.KeepAlive()
	client := &nodeClient{
		remote: remote, route: current.SSH, contextName: current.Name,
		operator: current.SSH.User, autoUpgrade: autoUpgrade, dryRun: boolFlag(cmd, "dry-run"), stderr: cmd.ErrOrStderr(),
		wait: humanPlainAt(cmd),
	}
	if a.nodeClients == nil {
		a.nodeClients = map[string]*nodeClient{}
	}
	a.nodeClients[current.Name] = client
	return client, nil
}

// contextFlag returns the root --context value for a command. A leaf sees it
// as an inherited persistent flag; tests may pass the root itself.
func contextFlag(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	for _, flags := range []*pflag.FlagSet{cmd.Flags(), cmd.InheritedFlags(), cmd.PersistentFlags()} {
		if flags.Lookup("context") == nil {
			continue
		}
		value, _ := flags.GetString("context")
		return strings.TrimSpace(value)
	}
	return ""
}

// resolveContextName is the context a node command addresses: --context, then
// EREGION_CONTEXT, then the store's current context (an empty name).
func resolveContextName(cmd *cobra.Command) string {
	if name := contextFlag(cmd); name != "" {
		return name
	}
	return strings.TrimSpace(os.Getenv("EREGION_CONTEXT"))
}

// selectedContext loads the context a command addresses: --context, then
// EREGION_CONTEXT, then the store's current context. Every command-path read
// of context state goes through contextByName, so local identity/route reads
// and the node client can never disagree about which server they address.
func selectedContext(cmd *cobra.Command) (host.Context, error) {
	return contextByName(cmd, "")
}

// contextByName loads name, or the selected context when name is empty. A
// positional node name (eregion ssh <node>) is the only non-empty caller.
func contextByName(cmd *cobra.Command, name string) (host.Context, error) {
	store, err := host.LoadContextStore("")
	if err != nil {
		return host.Context{}, err
	}
	if name == "" {
		name = resolveContextName(cmd)
	}
	current, err := store.Get(name)
	if err != nil {
		return host.Context{}, contextLookupError(name, err)
	}
	return current, nil
}

// requireAgreedContext refuses --context when the command creates its own
// context: login names it from the bundle, server init from its answers.
func requireAgreedContext(cmd *cobra.Command, name string) error {
	explicit := contextFlag(cmd)
	if explicit == "" || explicit == name {
		return nil
	}
	return contract.NewFault(contract.Usage, "--context "+explicit+" does not match the context this command creates ("+name+")", "Omit --context, or use the name this command creates.")
}

// contextLookupError names an unknown explicit context. The store's own error
// only knows the current context, so a flag or variable needs its own message.
func contextLookupError(name string, err error) error {
	if name == "" {
		return err
	}
	var fault *contract.Fault
	if errors.As(err, &fault) && fault.Code == contract.NotFound {
		named := contract.NewFault(contract.NotFound, "context "+name+" was not found", "List contexts with eregion context list.")
		named.Details = map[string]any{"context": name}
		return named
	}
	return err
}

func (a *app) contextSpecs() []spec {
	meta := func(summary, example string, mutating bool) CommandMeta {
		m := readMeta(summary, []string{summary + ". Never prints credentials; no interactive prompts."}, []string{example})
		m.Mutating = mutating
		if mutating {
			m.DryRunReason = "Local credential-store edits have no remote effect; inspect context show/list before changing them."
		}
		m.Environment = []string{"EREGION_CONTEXT_FILE"}
		m.ErrorCodes = append(m.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound)
		return m
	}
	load := func(cmd *cobra.Command) (*host.ContextStore, error) {
		path, _ := cmd.Flags().GetString("context-file")
		return host.LoadContextStore(path)
	}
	return []spec{{use: "context", meta: meta("Manage private operator contexts", "eregion context list --json", false),
		flags: func(cmd *cobra.Command) {
			cmd.PersistentFlags().String("context-file", "", "Private context store (defaults to EREGION_CONTEXT_FILE or XDG config)")
		},
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "list", meta: meta("List saved contexts", "eregion context list --json", false), run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := load(cmd)
				if err != nil {
					return result{}, err
				}
				return hostResult(store.List()), nil
			}},
			{use: "show", meta: meta("Show one saved context", "eregion context show --name production --json", false), flags: contextNameFlag, run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := load(cmd)
				if err != nil {
					return result{}, err
				}
				name, _ := cmd.Flags().GetString("name")
				view, err := store.Show(name)
				return hostResult(view), err
			}},
			{use: "use", meta: meta("Select the active context", "eregion context use --name production", true), flags: contextNameFlag, run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := load(cmd)
				if err != nil {
					return result{}, err
				}
				name, _ := cmd.Flags().GetString("name")
				view, err := store.Use(name)
				return hostResult(view), err
			}},
			a.contextRemoveSpec(meta),
		}}}
}

// contextRemoveSpec deletes a context and its key files. It is destructive and
// takes the context name as the confirmation token (MASTER D12).
func (a *app) contextRemoveSpec(meta func(string, string, bool) CommandMeta) spec {
	removeMeta := meta("Remove a saved context and its local key files", "eregion context remove production --confirm production --json", true)
	removeMeta.Mutating, removeMeta.Destructive, removeMeta.Confirmation = true, true, "target"
	removeMeta.DryRunReason = "Removal deletes local files; inspect the context with context show first and name it in --confirm."
	return spec{
		use:  "remove <name>",
		meta: removeMeta,
		args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return contract.NewFault(contract.InputRequired, "the context name is required", "Use eregion context remove <name> --confirm <name>.")
			}
			return host.ValidateContextName(args[0])
		},
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("confirm", "", "The context name; must match exactly")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			name := args[0]
			path, _ := cmd.Flags().GetString("context-file")
			store, err := host.LoadContextStore(path)
			if err != nil {
				return result{}, err
			}
			saved, err := store.Get(name)
			if err != nil {
				return result{}, err
			}
			confirm, _ := cmd.Flags().GetString("confirm")
			if confirm == "" || confirm != name {
				fault := contract.NewFault(contract.ConfirmationRequired, "--confirm must name the context", "Pass --confirm "+name+".")
				fault.Details = map[string]any{"confirm_token": name}
				return result{}, fault
			}
			// A live tunnel for this context would hold the node's peer.
			closeWireGuardRoute(saved.SSH)
			removed, err := store.Remove(name)
			if err != nil {
				return result{}, err
			}
			dir := ""
			if store.Path != "" {
				dir = filepath.Join(filepath.Dir(store.Path), "servers", name)
			} else if resolved, err := identityDir(name); err == nil {
				dir = resolved
			}
			if dir != "" {
				if err := os.RemoveAll(dir); err != nil {
					return result{}, err
				}
			}
			return result{data: map[string]any{"removed": name, "changed": removed}, plain: "removed context " + name}, nil
		},
	}
}

func contextNameFlag(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "Context name; defaults to the active context")
}
