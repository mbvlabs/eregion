package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
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
	// One command reuses one client: the version check runs once and every
	// call shares one SSH connection over one WireGuard tunnel.
	if client, ok := a.nodeClients[name]; ok {
		return client, nil
	}
	store, err := host.LoadContextStore("")
	if err != nil {
		return nil, err
	}
	current, err := store.Get(name)
	if err != nil {
		return nil, err
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
		remote: remote, route: current.SSH,
		operator: current.SSH.User, autoUpgrade: autoUpgrade, stderr: cmd.ErrOrStderr(),
		wait: humanPlainAt(cmd),
	}
	if a.nodeClients == nil {
		a.nodeClients = map[string]*nodeClient{}
	}
	a.nodeClients[name] = client
	return client, nil
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
