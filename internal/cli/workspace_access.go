package cli

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type workspaceSnapshotKey struct{}

// workspaceSnapshot is one resolved record per command invocation. JSON, UI,
// RPC, export and telemetry all read this cached access record rather than
// resolving independently after a store change.
type workspaceSnapshot struct {
	mu       sync.Mutex
	set      bool
	selector string
	path     string
	access   host.WorkspaceAccess
	group    host.WorkspaceGroup
}

func workspaceSnapshotOf(cmd *cobra.Command) *workspaceSnapshot {
	if cmd == nil || cmd.Context() == nil {
		return nil
	}
	snapshot, _ := cmd.Context().Value(workspaceSnapshotKey{}).(*workspaceSnapshot)
	return snapshot
}

func hostResult(data any) result {
	raw, _ := json.MarshalIndent(data, "", "  ")
	return result{data: data, plain: string(raw)}
}

// nodeClient resolves the selected verified workspace access into the node RPC
// backend. There is no direct HTTP or token path.
func (a *app) nodeClient(cmd *cobra.Command) (*nodeClient, error) {
	access, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		return nil, err
	}
	return a.nodeClientForAccess(cmd, access)
}

func (a *app) nodeClientForAccess(cmd *cobra.Command, current host.WorkspaceAccess) (*nodeClient, error) {
	if client, ok := a.nodeClients[current.Name]; ok {
		return client, nil
	}
	if current.SSH == nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "workspace "+current.Name+" has no SSH route", "Log in with a current protected export, or re-run server init.")
	}
	remote, err := routeRemote(current.SSH, false)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the workspace SSH route is not usable", err.Error())
	}
	autoUpgrade := current.AutoUpgradeNodes
	if cmd != nil {
		autoUpgrade = autoUpgrade || boolFlag(cmd, "auto-upgrade-nodes")
	}
	remote.KeepAlive()
	client := &nodeClient{
		remote: remote, route: current.SSH, accessName: current.Name, workspaceID: workspaceIDOf(current), workspaceName: verifiedWorkspaceName(current),
		operator: current.SSH.User, autoUpgrade: autoUpgrade, dryRun: boolFlag(cmd, "dry-run"), stderr: cmd.ErrOrStderr(),
		wait: humanPlainAt(cmd),
	}
	if a.nodeClients == nil {
		a.nodeClients = map[string]*nodeClient{}
	}
	a.nodeClients[current.Name] = client
	return client, nil
}

func workspaceIDOf(access host.WorkspaceAccess) string {
	if access.Workspace == nil {
		return ""
	}
	return access.Workspace.ID
}

func workspaceDisplayName(access host.WorkspaceAccess) string {
	if access.Workspace != nil && access.Workspace.Name != "" {
		return access.Workspace.Name
	}
	return "unconfigured"
}

func verifiedWorkspaceName(access host.WorkspaceAccess) string {
	if access.Workspace == nil {
		return ""
	}
	return access.Workspace.Name
}

// workspaceFlag returns the root --workspace value. A leaf sees it as an
// inherited persistent flag; tests may pass the root itself.
func workspaceFlag(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	for _, flags := range []*pflag.FlagSet{cmd.Flags(), cmd.InheritedFlags(), cmd.PersistentFlags()} {
		if flags.Lookup("workspace") == nil {
			continue
		}
		value, _ := flags.GetString("workspace")
		return strings.TrimSpace(value)
	}
	return ""
}

// resolveWorkspaceSelector is the explicit NAME target: --workspace, then
// EREGION_WORKSPACE, then the saved current workspace (empty selector).
func resolveWorkspaceSelector(cmd *cobra.Command) string {
	if name := workspaceFlag(cmd); name != "" {
		return name
	}
	return strings.TrimSpace(os.Getenv("EREGION_WORKSPACE"))
}

func loadWorkspaceStore(cmd *cobra.Command) (*host.WorkspaceStore, error) {
	path := ""
	if cmd != nil {
		for _, flags := range []*pflag.FlagSet{cmd.Flags(), cmd.InheritedFlags(), cmd.PersistentFlags()} {
			if flags.Lookup("workspace-file") == nil {
				continue
			}
			path, _ = flags.GetString("workspace-file")
			break
		}
	}
	return host.LoadWorkspaceStore(strings.TrimSpace(path))
}

func selectedWorkspace(cmd *cobra.Command) (host.WorkspaceAccess, host.WorkspaceGroup, error) {
	store, err := loadWorkspaceStore(cmd)
	if err != nil {
		return host.WorkspaceAccess{}, host.WorkspaceGroup{}, err
	}
	selector := resolveWorkspaceSelector(cmd)
	snapshot := workspaceSnapshotOf(cmd)
	if snapshot != nil {
		snapshot.mu.Lock()
		if snapshot.set && snapshot.selector == selector && snapshot.path == store.Path {
			access, group := snapshot.access, snapshot.group
			snapshot.mu.Unlock()
			return access, group, nil
		}
		snapshot.mu.Unlock()
	}
	access, group, err := store.ResolveWorkspace(selector)
	if err != nil {
		return host.WorkspaceAccess{}, host.WorkspaceGroup{}, err
	}
	if snapshot != nil {
		snapshot.mu.Lock()
		snapshot.set, snapshot.selector, snapshot.path = true, selector, store.Path
		snapshot.access, snapshot.group = access, group
		snapshot.mu.Unlock()
	}
	return access, group, nil
}

func selectedWorkspaceAccess(cmd *cobra.Command) (host.WorkspaceAccess, error) {
	access, _, err := selectedWorkspace(cmd)
	return access, err
}

// rejectWorkspaceSelection keeps import/create identity independent of ambient
// workspace selection.
func rejectWorkspaceSelection(cmd *cobra.Command, action string) error {
	if workspaceFlag(cmd) != "" {
		return contract.NewFault(contract.Usage,
			"--workspace selects an existing workspace; "+action+" creates or replaces identity",
			"Omit --workspace. Use the command's explicit creation/assignment flag or a current protected export.")
	}
	if strings.TrimSpace(os.Getenv("EREGION_WORKSPACE")) != "" {
		return contract.NewFault(contract.Usage,
			"EREGION_WORKSPACE selects an existing workspace; "+action+" creates or replaces identity",
			"Unset EREGION_WORKSPACE for this command.")
	}
	return nil
}

func workspaceLookupError(selector string, err error) error {
	if selector == "" {
		return err
	}
	var fault *contract.Fault
	if errors.As(err, &fault) && fault.Code == contract.NotFound {
		named := contract.NewFault(contract.NotFound, "workspace "+selector+" was not found", "List saved workspaces with eregion workspace list.")
		named.Details = map[string]any{"workspace": selector}
		return named
	}
	return err
}

func (a *app) workspaceCommandSpecs() []spec {
	meta := func(summary, example string, mutating bool) CommandMeta {
		m := readMeta(summary, []string{summary + ". Never prints credentials; no interactive prompts."}, []string{example})
		m.Mutating = mutating
		if mutating {
			m.DryRunReason = "Local workspace access edits have no remote effect; inspect workspace show/list before changing them."
		}
		m.Environment = []string{"EREGION_WORKSPACE_FILE"}
		m.ErrorCodes = append(m.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound, contract.Conflict)
		return m
	}
	return []spec{{use: "workspace", meta: meta("Manage workspaces known to this machine", "eregion workspace list --json", false),
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "list", meta: meta("List saved workspaces, one entry per authoritative ID", "eregion workspace list --json", false), run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := loadWorkspaceStore(cmd)
				if err != nil {
					return result{}, err
				}
				return hostResult(store.ListWorkspaces()), nil
			}},
			{use: "show [NAME_OR_ID]", meta: meta("Show one saved workspace by name or ID", "eregion workspace show thelab --json", false), args: cobra.MaximumNArgs(1), run: func(cmd *cobra.Command, args []string) (result, error) {
				store, err := loadWorkspaceStore(cmd)
				if err != nil {
					return result{}, err
				}
				selector := ""
				if len(args) == 1 {
					selector = args[0]
				}
				view, err := store.ShowWorkspace(selector)
				return hostResult(view), err
			}},
			{use: "use NAME_OR_ID", meta: meta("Select the current workspace", "eregion workspace use thelab", true), args: exactOneArg("workspace name or ID", "eregion workspace use NAME_OR_ID"), run: func(cmd *cobra.Command, args []string) (result, error) {
				store, err := loadWorkspaceStore(cmd)
				if err != nil {
					return result{}, err
				}
				view, err := store.UseWorkspace(args[0])
				return hostResult(view), err
			}},
			a.workspaceRemoveSpec(meta),
		}}}
}

func (a *app) workspaceRemoveSpec(meta func(string, string, bool) CommandMeta) spec {
	removeMeta := meta("Remove a saved workspace and its owned local access files", "eregion workspace remove thelab --confirm thelab --json", true)
	removeMeta.Mutating, removeMeta.Destructive, removeMeta.Confirmation = true, true, "target"
	removeMeta.DryRunReason = "Removal deletes local access files; inspect the workspace with workspace show first and name it in --confirm."
	return spec{
		use:  "remove NAME_OR_ID",
		meta: removeMeta,
		args: exactOneArg("workspace name or ID", "eregion workspace remove NAME_OR_ID --confirm NAME_OR_ID"),
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("confirm", "", "The workspace name or ID; must match exactly")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			selector := args[0]
			store, err := loadWorkspaceStore(cmd)
			if err != nil {
				return result{}, err
			}
			confirm, _ := cmd.Flags().GetString("confirm")
			if confirm == "" || confirm != selector {
				fault := contract.NewFault(contract.ConfirmationRequired, "--confirm must name the workspace", "Pass --confirm "+selector+".")
				fault.Details = map[string]any{"confirm_token": selector}
				return result{}, fault
			}
			view, removed, retained, err := store.RemoveWorkspaceWithCleanup(selector, func(records []host.RemovedAccess) {
				for _, record := range records {
					closeWireGuardRoute(record.SSH)
				}
			})
			if err != nil {
				return result{}, err
			}
			data := map[string]any{"removed": view, "workspace_id": view.ID, "config_changed": true, "database_changed": false, "removed_access_records": len(removed)}
			plain := "Removed local workspace access " + view.ID + "; installation, database identity, applications and server are unchanged."
			if len(retained) > 0 {
				data["incomplete_cleanup"] = retained
				plain += "\nSome credential directories were retained because ownership or sharing could not be proven: " + strings.Join(retained, ", ")
			}
			return result{data: data, plain: plain + "\n"}, nil
		},
	}
}

func exactOneArg(label, example string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return contract.NewFault(contract.InputRequired, "the "+label+" is required", "Use "+example+".")
		}
		return nil
	}
}

func workspaceCredentialDir(cmd *cobra.Command, access host.WorkspaceAccess) (string, error) {
	if snapshot := workspaceSnapshotOf(cmd); snapshot != nil {
		snapshot.mu.Lock()
		if snapshot.set && snapshot.access.Name == access.Name {
			store := &host.WorkspaceStore{Path: snapshot.path}
			snapshot.mu.Unlock()
			return store.CredentialDirFor(access)
		}
		snapshot.mu.Unlock()
	}
	store, err := loadWorkspaceStore(cmd)
	if err != nil {
		return "", err
	}
	return store.CredentialDirFor(access)
}

func workspaceCredentialDirByName(cmd *cobra.Command, name string) (string, error) {
	if snapshot := workspaceSnapshotOf(cmd); snapshot != nil {
		snapshot.mu.Lock()
		if snapshot.set && snapshot.access.Name == name {
			access, path := snapshot.access, snapshot.path
			snapshot.mu.Unlock()
			store := &host.WorkspaceStore{Path: path}
			return store.CredentialDirFor(access)
		}
		snapshot.mu.Unlock()
	}
	store, err := loadWorkspaceStore(cmd)
	if err != nil {
		return "", err
	}
	access, ok := store.AccessByName(name)
	if !ok {
		return "", contract.NewFault(contract.NotFound, "workspace access "+name+" was not found", "Run eregion login or server init first.")
	}
	return store.CredentialDirFor(access)
}
