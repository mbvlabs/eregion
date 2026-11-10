package host

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
)

const contextSchema = 2

var contextNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Context is one operator context. It holds the WireGuard identity and the SSH
// key that reach the node; there is no API token.
type Context struct {
	Name string `json:"name"`
	// SSH reaches the node through a pinned SSH tunnel inside the embedded
	// userspace WireGuard tunnel.
	SSH *ContextSSH `json:"ssh,omitempty"`
	// Workspace is the portable installation identity this local access
	// context reaches. Nil means explicitly unconfigured (an old bundle or an
	// alpha.6 installation before apply --workspace); it is never guessed from
	// the context or node name.
	Workspace *ContextWorkspace `json:"workspace,omitempty"`
	// AutoUpgradeNodes installs this CLI's build on a node whose build differs.
	AutoUpgradeNodes bool      `json:"auto_upgrade_nodes,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ContextWorkspace is the portable non-secret identity carried from the
// protected handoff after login verifies it against the node.
type ContextWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ContextSSH is the SSH route of a context. Identity is empty when ssh-agent
// or the default ~/.ssh keys authenticate.
type ContextSSH struct {
	Address    string `json:"address"`
	User       string `json:"user"`
	Identity   string `json:"identity,omitempty"`
	KnownHosts string `json:"known_hosts"`
	// WireGuard, when set, carries the SSH connection inside an embedded
	// userspace tunnel to the node (public SSH is closed).
	WireGuard *ContextWireGuard `json:"wireguard,omitempty"`
}

// ContextWireGuard is the operator peer of a node's wg0.
type ContextWireGuard struct {
	PrivateKeyFile string `json:"private_key_file"`
	Address        string `json:"address"`
	NodeAddress    string `json:"node_address"`
	NodePublicKey  string `json:"node_public_key"`
	Endpoint       string `json:"endpoint"`
}

// ContextView is safe to serialize.
type ContextView struct {
	Name             string            `json:"name"`
	Workspace        *ContextWorkspace `json:"workspace,omitempty"`
	AutoUpgradeNodes bool              `json:"auto_upgrade_nodes,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Current          bool              `json:"current"`
	// Route is how the CLI reaches the node, e.g. admin@10.78.0.1:22 over
	// WireGuard via 203.0.113.10:51820.
	Route string `json:"route,omitempty"`
}

// ContextStore is the on-disk context file.
type ContextStore struct {
	Schema   int              `json:"schema"`
	Current  string           `json:"current,omitempty"`
	Contexts []Context        `json:"contexts"`
	Path     string           `json:"-"`
	Now      func() time.Time `json:"-"`
}

// DefaultContextPath follows XDG and can be overridden explicitly for tests.
func DefaultContextPath() (string, error) {
	if path := os.Getenv("EREGION_CONTEXT_FILE"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "eregion", "contexts.json"), nil
}

// LoadContextStore reads path. A missing store is empty. An unsafe file mode is
// rejected rather than silently trusted.
func LoadContextStore(path string) (*ContextStore, error) {
	if path == "" {
		var err error
		path, err = DefaultContextPath()
		if err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "cannot locate the operator config directory", "Set EREGION_CONTEXT_FILE to an explicit private file.")
		}
	}
	store := &ContextStore{Schema: contextSchema, Path: path, Now: time.Now}
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return store, nil
	}
	if runtime.GOOS != "windows" && mode&0o077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "the context file is readable by group or others", "Run chmod 600 "+path+" before using it.")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(store); err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the context file is invalid", "Fix or move "+path+" aside; the file contains credentials and is never printed.")
	}
	if store.Schema != contextSchema {
		return nil, contract.NewFault(contract.PreconditionFailed, "the context file has an unsupported schema", "Use a compatible eregion binary or move the file aside.")
	}
	store.Path, store.Now = path, time.Now
	for _, context := range store.Contexts {
		if err := validateContext(context); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func validateContext(context Context) error {
	if !contextNamePattern.MatchString(context.Name) {
		return contract.NewFault(contract.ValidationFailed, "invalid context name", "Use lowercase letters, digits, hyphens, or underscores.")
	}
	return validateContextWorkspace(context.Workspace)
}

func validateContextWorkspace(workspace *ContextWorkspace) error {
	if workspace == nil {
		return nil
	}
	if err := controlplane.ValidateWorkspaceID(workspace.ID); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "the context has an invalid workspace ID", "Import a current protected export; do not edit the context file by hand.")
	}
	if err := controlplane.ValidateWorkspaceName(workspace.Name); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "the context has an invalid workspace name", "Import a current protected export; do not edit the context file by hand.")
	}
	return nil
}

// ValidateContextName checks a server/context name before any local state is
// written.
func ValidateContextName(name string) error {
	if !contextNamePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid server name "+name, "Use lowercase letters, digits, hyphens, or underscores, starting with a letter or digit.")
	}
	return nil
}

// Save writes the store atomically with mode 0600. Direct Save is a full
// replacement; individual mutations use withMutation so concurrent login,
// context-use, removal and workspace updates share one lock and reload the
// latest on-disk state before changing it.
func (s *ContextStore) Save() error {
	if s.Path == "" {
		return errors.New("context store has no path")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return engine.WithFileLock(s.Path+".lock", s.saveLocked)
}

func (s *ContextStore) saveLocked() error {
	if err := validateWorkspaceAliases(s.Contexts); err != nil {
		return err
	}
	s.Schema = contextSchema
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(s.Path, append(raw, '\n'), 0o600)
}

func validateWorkspaceAliases(contexts []Context) error {
	for index := range contexts {
		if err := workspaceAliasConflict(contexts[index+1:], contexts[index].Name, contexts[index].Workspace); err != nil {
			return err
		}
	}
	return nil
}

// withMutation serialises every context-store read-modify-write cycle under the
// same lock, then applies the mutation to the latest on-disk state. This keeps
// an unrelated concurrent `context use`, login replacement or another context
// import from being lost by a stale snapshot.
func (s *ContextStore) withMutation(change func(*ContextStore) error) error {
	if s.Path == "" {
		return errors.New("context store has no path")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return engine.WithFileLock(s.Path+".lock", func() error {
		fresh, err := LoadContextStore(s.Path)
		if err != nil {
			return err
		}
		if s.Now != nil {
			fresh.Now = s.Now
		}
		if err := change(fresh); err != nil {
			return err
		}
		*s = *fresh
		return nil
	})
}

// SaveContext adds or replaces a context and makes it current. Login imports
// one from the server init handoff.
func (s *ContextStore) SaveContext(context Context) (ContextView, error) {
	var saved Context
	err := s.withMutation(func(fresh *ContextStore) error {
		context.Name = strings.TrimSpace(context.Name)
		if !contextNamePattern.MatchString(context.Name) {
			return contract.NewFault(contract.ValidationFailed, "invalid context name", "Use lowercase letters, digits, hyphens, or underscores.")
		}
		if context.SSH == nil {
			return contract.NewFault(contract.ValidationFailed, "the context has no SSH route", "Import a server init handoff or re-run server init.")
		}
		if err := validateContextWorkspace(context.Workspace); err != nil {
			return err
		}
		if err := workspaceAliasConflict(fresh.Contexts, context.Name, context.Workspace); err != nil {
			return err
		}
		now := fresh.Now().UTC()
		if context.CreatedAt.IsZero() {
			context.CreatedAt = now
		}
		context.UpdatedAt = now
		found := false
		for index := range fresh.Contexts {
			if fresh.Contexts[index].Name != context.Name {
				continue
			}
			if fresh.Contexts[index].Workspace != nil && !sameWorkspaceMetadata(fresh.Contexts[index].Workspace, context.Workspace) {
				return workspaceMetadataConflict(context.Name, fresh.Contexts[index].Workspace, context.Workspace)
			}
			context.CreatedAt = fresh.Contexts[index].CreatedAt
			fresh.Contexts[index] = context
			found = true
			break
		}
		if !found {
			fresh.Contexts = append(fresh.Contexts, context)
		}
		sort.Slice(fresh.Contexts, func(i, j int) bool { return fresh.Contexts[i].Name < fresh.Contexts[j].Name })
		fresh.Current = context.Name
		if err := fresh.saveLocked(); err != nil {
			return err
		}
		saved = context
		return nil
	})
	if err != nil {
		return ContextView{}, err
	}
	return viewOf(saved, true), nil
}

// CheckWorkspaceAlias reports whether workspace would collide with another
// saved context. The same workspace ID under a different local context name is
// a valid alias; the same workspace name with another ID is not.
func (s *ContextStore) CheckWorkspaceAlias(name string, workspace *ContextWorkspace) error {
	return workspaceAliasConflict(s.Contexts, name, workspace)
}

// SetWorkspace updates one saved context's portable workspace metadata without
// changing which context is current. Apply uses SetWorkspaceIfRoute after a
// successful host convergence; setup uses this helper before it has a client
// route to compare.
func (s *ContextStore) SetWorkspace(name string, workspace *ContextWorkspace) (ContextView, error) {
	return s.setWorkspace(name, workspace, nil)
}

// SetWorkspaceIfRoute is the apply path: it only publishes workspace metadata
// when the saved route is still the exact route the node RPC used and a
// configured workspace ID/name is unchanged.
func (s *ContextStore) SetWorkspaceIfRoute(name string, workspace *ContextWorkspace, expected *ContextSSH) (ContextView, error) {
	return s.setWorkspace(name, workspace, expected)
}

func (s *ContextStore) setWorkspace(name string, workspace *ContextWorkspace, expected *ContextSSH) (ContextView, error) {
	var view ContextView
	err := s.withMutation(func(fresh *ContextStore) error {
		if err := validateContextWorkspace(workspace); err != nil {
			return err
		}
		for index := range fresh.Contexts {
			if fresh.Contexts[index].Name != name {
				continue
			}
			if expected != nil && !SameContextRoute(fresh.Contexts[index].SSH, expected) {
				fault := contract.NewFault(contract.Conflict,
					"context "+name+" changed while the apply was running",
					"Reload the context and retry; workspace metadata was not written over the replacement.")
				fault.Details = map[string]any{"context": name}
				return fault
			}
			if fresh.Contexts[index].Workspace != nil && !sameWorkspaceMetadata(fresh.Contexts[index].Workspace, workspace) {
				return workspaceMetadataConflict(name, fresh.Contexts[index].Workspace, workspace)
			}
			if err := workspaceAliasConflict(fresh.Contexts, name, workspace); err != nil {
				return err
			}
			var stored *ContextWorkspace
			if workspace != nil {
				copied := *workspace
				stored = &copied
			}
			fresh.Contexts[index].Workspace = stored
			fresh.Contexts[index].UpdatedAt = fresh.Now().UTC()
			if err := fresh.saveLocked(); err != nil {
				return err
			}
			view = viewOf(fresh.Contexts[index], fresh.Contexts[index].Name == fresh.Current)
			return nil
		}
		return contract.NewFault(contract.NotFound, "context not found", "List contexts with eregion context list.")
	})
	if err != nil {
		return ContextView{}, err
	}
	return view, nil
}

func sameWorkspaceMetadata(existing, incoming *ContextWorkspace) bool {
	if existing == nil || incoming == nil {
		return existing == nil && incoming == nil
	}
	return existing.ID == incoming.ID && existing.Name == incoming.Name
}

func workspaceMetadataConflict(name string, existing, incoming *ContextWorkspace) error {
	fault := contract.NewFault(contract.Conflict,
		"context "+name+" is already assigned to a different workspace",
		"The workspace ID and name are immutable locally. Refresh the export or remove the context explicitly; workspace metadata is never overwritten.")
	details := map[string]any{"context": name}
	if existing != nil {
		details["existing_workspace"] = existing.Name
		details["existing_workspace_id"] = existing.ID
	}
	if incoming != nil {
		details["incoming_workspace"] = incoming.Name
		details["incoming_workspace_id"] = incoming.ID
	}
	fault.Details = details
	return fault
}

func workspaceAliasConflict(contexts []Context, name string, workspace *ContextWorkspace) error {
	if workspace == nil {
		return nil
	}
	for _, saved := range contexts {
		if saved.Name == name || saved.Workspace == nil {
			continue
		}
		if saved.Workspace.Name == workspace.Name && saved.Workspace.ID != workspace.ID {
			fault := contract.NewFault(contract.Conflict,
				"workspace name "+workspace.Name+" is already used by context "+saved.Name+" with another ID",
				"A workspace name identifies one immutable workspace. Import the export for the existing workspace, or remove the conflicting context explicitly.")
			fault.Details = map[string]any{
				"workspace": workspace.Name,
				"context":   saved.Name, "workspace_id": saved.Workspace.ID,
				"incoming_context": name, "incoming_workspace_id": workspace.ID,
			}
			return fault
		}
	}
	return nil
}

// SameContextRoute reports exact route/identity equality without exposing key
// material. Apply compares this captured route with the latest saved context
// before publishing workspace metadata.
func SameContextRoute(left, right *ContextSSH) bool {
	return reflect.DeepEqual(left, right)
}

// Get returns a context by name, or the current one when name is empty.
func (s *ContextStore) Get(name string) (Context, error) {
	if name == "" {
		name = s.Current
	}
	if name == "" {
		return Context{}, contract.NewFault(contract.NotFound, "no current context is configured", "Run eregion login, or pass --name.")
	}
	for _, context := range s.Contexts {
		if context.Name == name {
			return context, nil
		}
	}
	return Context{}, contract.NewFault(contract.NotFound, "context not found", "List contexts with eregion context list.")
}

// Use selects the current context.
func (s *ContextStore) Use(name string) (ContextView, error) {
	var view ContextView
	err := s.withMutation(func(fresh *ContextStore) error {
		context, err := fresh.Get(name)
		if err != nil {
			return err
		}
		now := fresh.Now().UTC()
		for index := range fresh.Contexts {
			if fresh.Contexts[index].Name == context.Name {
				fresh.Contexts[index].UpdatedAt = now
				context = fresh.Contexts[index]
				break
			}
		}
		fresh.Current = context.Name
		if err := fresh.saveLocked(); err != nil {
			return err
		}
		view = viewOf(context, true)
		return nil
	})
	if err != nil {
		return ContextView{}, err
	}
	return view, nil
}

// Remove deletes a context and clears Current when needed.
func (s *ContextStore) Remove(name string) (bool, error) {
	err := s.withMutation(func(fresh *ContextStore) error {
		context, err := fresh.Get(name)
		if err != nil {
			return err
		}
		var kept []Context
		for _, candidate := range fresh.Contexts {
			if candidate.Name != context.Name {
				kept = append(kept, candidate)
			}
		}
		fresh.Contexts = kept
		if fresh.Current == context.Name {
			fresh.Current = ""
		}
		return fresh.saveLocked()
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// List returns safe views sorted by name.
func (s *ContextStore) List() []ContextView {
	views := make([]ContextView, 0, len(s.Contexts))
	for _, context := range s.Contexts {
		views = append(views, viewOf(context, context.Name == s.Current))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

// Show returns one safe view.
func (s *ContextStore) Show(name string) (ContextView, error) {
	context, err := s.Get(name)
	if err != nil {
		return ContextView{}, err
	}
	return viewOf(context, context.Name == s.Current), nil
}

func viewOf(context Context, current bool) ContextView {
	var workspace *ContextWorkspace
	if context.Workspace != nil {
		copied := *context.Workspace
		workspace = &copied
	}
	return ContextView{
		Name: context.Name, Workspace: workspace, AutoUpgradeNodes: context.AutoUpgradeNodes,
		CreatedAt: context.CreatedAt, UpdatedAt: context.UpdatedAt, Current: current,
		Route: routeOf(context.SSH),
	}
}

func routeOf(route *ContextSSH) string {
	if route == nil {
		return ""
	}
	out := route.User + "@" + route.Address
	if route.WireGuard != nil {
		out += " over WireGuard via " + route.WireGuard.Endpoint
	} else {
		out += " over SSH"
	}
	return out
}
