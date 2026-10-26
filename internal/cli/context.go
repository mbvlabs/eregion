package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
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
	return &nodeClient{
		remote: remote, route: current.SSH,
		operator: current.SSH.User, autoUpgrade: autoUpgrade, stderr: cmd.ErrOrStderr(),
	}, nil
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
			a.contextCredentialSpec(meta),
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

// contextCredentialSpec adds a scoped automation actor to an existing server
// auth file. It is a plan-bound mutation: dry-run returns the revision and
// token, and apply requires that exact token. The credential is written once to
// an owner-only file; neither the plan nor the result prints it.
func (a *app) contextCredentialSpec(meta func(string, string, bool) CommandMeta) spec {
	credentialMeta := meta("Add a scoped automation credential to the server auth file", "eregion context credential add --auth-file /etc/eregion/auth.json --subject ci-deploy --scope operations:submit --action-grant platform.deploy:env/api/production --token-out /root/ci.token --dry-run --json", true)
	credentialMeta.Mutating, credentialMeta.DryRun, credentialMeta.Confirmation = true, true, "plan-bound"
	credentialMeta.DryRunReason = ""
	return spec{use: "credential", meta: meta("Manage scoped automation credentials", "eregion context credential add --dry-run --json", false),
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "add", meta: credentialMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("auth-file", "/etc/eregion/auth.json", "Server auth file containing token hashes")
				cmd.Flags().String("subject", "", "Stable CI actor subject")
				cmd.Flags().StringSlice("scope", nil, "Explicit scopes (operations:submit, operations:read, operations:cancel, system:read)")
				cmd.Flags().StringArray("action-grant", nil, "Repeatable kind:target grant, for example platform.deploy:env/api/production")
				cmd.Flags().String("token-out", "", "Absolute owner-only token output; never printed")
				cmd.Flags().Bool("dry-run", false, "Show the base revision and confirmation token without writing")
				cmd.Flags().String("confirm", "", "Exact plan-bound confirmation token")
			}, run: func(cmd *cobra.Command, _ []string) (result, error) {
				authFile, _ := cmd.Flags().GetString("auth-file")
				subject, _ := cmd.Flags().GetString("subject")
				scopes, _ := cmd.Flags().GetStringSlice("scope")
				grantFlags, _ := cmd.Flags().GetStringArray("action-grant")
				grants := make([]api.ActionGrant, 0, len(grantFlags))
				for _, raw := range grantFlags {
					grant, err := host.ParseActionGrant(raw)
					if err != nil {
						return result{}, err
					}
					grants = append(grants, grant)
				}
				tokenOut, _ := cmd.Flags().GetString("token-out")
				request := host.CredentialAddRequest{
					AuthFile: authFile, Actor: contract.Actor{Kind: "ci", Subject: subject},
					Scopes: scopes, Grants: grants, TokenOut: tokenOut,
				}
				if err := request.Validate(); err != nil {
					return result{}, err
				}
				revision, err := host.AuthRevision(authFile)
				if err != nil {
					return result{}, err
				}
				now := time.Now()
				plan, err := request.Confirmation(now)
				if err != nil {
					return result{}, err
				}
				dryRun, _ := cmd.Flags().GetBool("dry-run")
				if dryRun {
					return hostResult(map[string]any{
						"auth_file": authFile, "actor_kind": "ci", "actor_subject": subject,
						"scopes": scopes, "action_grants": grants, "base_revision": revision,
						"confirmation": plan, "dry_run": true,
					}), nil
				}
				confirmation, _ := cmd.Flags().GetString("confirm")
				if confirmation == "" {
					return result{}, contract.NewFault(contract.ConfirmationRequired, "--confirm is required to add a credential", "Run context credential add --dry-run and pass its confirmation token.")
				}
				report, err := request.Apply(confirmation, now)
				return hostResult(report), err
			}},
		}}
}

func contextNameFlag(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "Context name; defaults to the active context")
}
