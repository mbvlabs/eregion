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
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
)

const workspaceStoreSchema = 2

var accessNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// WorkspaceAccess is one local key/route record for a workspace. Name is the
// historical private access key used for the on-disk credential directory; it
// is never a public target. Workspace is the verified portable identity.
type WorkspaceAccess struct {
	Name string `json:"name"`
	// SSH reaches the node through a pinned SSH tunnel inside the embedded
	// userspace WireGuard tunnel.
	SSH *WorkspaceSSH `json:"ssh,omitempty"`
	// Workspace is the verified portable installation identity. Nil means an
	// explicitly unconfigured legacy access record; it is never guessed from
	// the local access key or node name.
	Workspace *WorkspaceIdentity `json:"workspace,omitempty"`
	// CredentialDir is a store-relative private directory. Empty means the
	// historical servers/<Name> path.
	CredentialDir    string    `json:"credential_dir,omitempty"`
	AutoUpgradeNodes bool      `json:"auto_upgrade_nodes,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// WorkspaceIdentity is the portable non-secret identity verified against the
// node before publication.
type WorkspaceIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// WorkspaceSSH is the SSH route of a workspace access record. Identity is empty
// when ssh-agent or the default ~/.ssh keys authenticate.
type WorkspaceSSH struct {
	Address    string              `json:"address"`
	User       string              `json:"user"`
	Identity   string              `json:"identity,omitempty"`
	KnownHosts string              `json:"known_hosts"`
	WireGuard  *WorkspaceWireGuard `json:"wireguard,omitempty"`
}

// WorkspaceWireGuard is the operator peer of a node's wg0.
type WorkspaceWireGuard struct {
	PrivateKeyFile string `json:"private_key_file"`
	Address        string `json:"address"`
	NodeAddress    string `json:"node_address"`
	NodePublicKey  string `json:"node_public_key"`
	Endpoint       string `json:"endpoint"`
}

// WorkspaceView is the safe, one-entry-per-authoritative-ID list/show shape.
type WorkspaceView struct {
	ID               string    `json:"id,omitempty"`
	Name             string    `json:"name,omitempty"`
	Current          bool      `json:"current"`
	Configured       bool      `json:"configured"`
	Ambiguous        bool      `json:"ambiguous,omitempty"`
	Conflict         string    `json:"conflict,omitempty"`
	AccessCount      int       `json:"access_count,omitempty"`
	AutoUpgradeNodes bool      `json:"auto_upgrade_nodes,omitempty"`
	Route            string    `json:"route,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

// WorkspaceStore is the private on-disk local access store. The JSON field that
// historically held contexts remains the sole storage format.
type WorkspaceStore struct {
	Schema  int               `json:"schema"`
	Current string            `json:"current,omitempty"`
	Access  []WorkspaceAccess `json:"contexts"`
	Path    string            `json:"-"`
	Now     func() time.Time  `json:"-"`
}

// DefaultWorkspacePath follows XDG and can be overridden explicitly for tests.
func DefaultWorkspacePath() (string, error) {
	if err := CheckObsoleteWorkspaceEnvironment(); err != nil {
		return "", err
	}
	if path := os.Getenv("EREGION_WORKSPACE_FILE"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "eregion", "contexts.json"), nil
}

func CheckObsoleteWorkspaceEnvironment() error {
	for _, name := range []string{"EREGION_CONTEXT", "EREGION_CONTEXT_FILE"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			continue
		}
		fault := contract.NewFault(contract.ValidationFailed,
			name+" is obsolete and is not used for workspace selection or storage",
			"Unset "+name+". Use --workspace or EREGION_WORKSPACE for selection and EREGION_WORKSPACE_FILE for the private store.")
		fault.Details = map[string]any{"obsolete_environment": name}
		return fault
	}
	return nil
}

// LoadWorkspaceStore reads path. A missing store is empty. An unsafe file mode
// is rejected rather than silently trusted.
func LoadWorkspaceStore(path string) (*WorkspaceStore, error) {
	if err := CheckObsoleteWorkspaceEnvironment(); err != nil {
		return nil, err
	}
	if path == "" {
		var err error
		path, err = DefaultWorkspacePath()
		if err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "cannot locate the operator config directory", "Set EREGION_WORKSPACE_FILE to an explicit private file.")
		}
	}
	store := &WorkspaceStore{Schema: workspaceStoreSchema, Path: path, Now: time.Now}
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return store, nil
	}
	if runtime.GOOS != "windows" && mode&0o077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "the workspace store is readable by group or others", "Run chmod 600 "+path+" before using it.")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(store); err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the workspace store is invalid", "Fix or move "+path+" aside; the file contains credentials and is never printed.")
	}
	if store.Schema != workspaceStoreSchema {
		return nil, contract.NewFault(contract.PreconditionFailed, "the workspace store has an unsupported schema", "Use a compatible eregion binary or move the file aside.")
	}
	store.Path, store.Now = path, time.Now
	seen := map[string]bool{}
	for _, access := range store.Access {
		if err := validateWorkspaceAccess(access); err != nil {
			return nil, err
		}
		if seen[access.Name] {
			return nil, contract.NewFault(contract.PreconditionFailed, "the workspace store has duplicate private access records", "Repair or move the store aside; names are private storage keys, not workspace identities.")
		}
		seen[access.Name] = true
	}
	return store, nil
}

func validateWorkspaceAccess(access WorkspaceAccess) error {
	if !accessNamePattern.MatchString(access.Name) {
		return contract.NewFault(contract.ValidationFailed, "invalid private access name", "Use lowercase letters, digits, hyphens, or underscores.")
	}
	return validateWorkspaceIdentity(access.Workspace)
}

func validateWorkspaceIdentity(workspace *WorkspaceIdentity) error {
	if workspace == nil {
		return nil
	}
	if err := controlplane.ValidateWorkspaceID(workspace.ID); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "the workspace access record has an invalid workspace ID", "Import a current protected export; do not edit the workspace store by hand.")
	}
	if err := controlplane.ValidateWorkspaceName(workspace.Name); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "the workspace access record has an invalid workspace name", "Import a current protected export; do not edit the workspace store by hand.")
	}
	return nil
}

// ValidateAccessName checks a private storage name before local state is written.
func ValidateAccessName(name string) error {
	if !accessNamePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid private access name "+name, "Use lowercase letters, digits, hyphens, or underscores, starting with a letter or digit.")
	}
	return nil
}

// Save writes the store atomically with mode 0600. Individual mutations use
// withMutation so concurrent login, workspace-use and removal share one lock
// and reload the latest on-disk state before changing it.
func (s *WorkspaceStore) Save() error {
	if s.Path == "" {
		return errors.New("workspace store has no path")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return engine.WithFileLock(s.Path+".lock", s.saveLocked)
}

func (s *WorkspaceStore) saveLocked() error {
	s.Schema = workspaceStoreSchema
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(s.Path, append(raw, '\n'), 0o600)
}

func (s *WorkspaceStore) withMutation(change func(*WorkspaceStore) error) error {
	if s.Path == "" {
		return errors.New("workspace store has no path")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return engine.WithFileLock(s.Path+".lock", func() error {
		fresh, err := LoadWorkspaceStore(s.Path)
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

// WorkspaceGroup is one authoritative workspace ID and all its local access
// records. It is internal; public views never expose record aliases.
type WorkspaceGroup struct {
	ID       string
	Name     string
	Access   []WorkspaceAccess
	Conflict string
}

func (s *WorkspaceStore) Groups() []WorkspaceGroup {
	byID := map[string]*WorkspaceGroup{}
	order := make([]string, 0)
	for _, access := range s.Access {
		if access.Workspace == nil {
			continue
		}
		group, ok := byID[access.Workspace.ID]
		if !ok {
			group = &WorkspaceGroup{ID: access.Workspace.ID, Name: access.Workspace.Name}
			byID[access.Workspace.ID] = group
			order = append(order, access.Workspace.ID)
		} else if group.Name != "" && group.Name != access.Workspace.Name {
			group.Name = ""
			group.Conflict = "workspace ID has records with different names"
		}
		group.Access = append(group.Access, access)
	}
	nameIDs := map[string]map[string]bool{}
	for _, group := range byID {
		if group.Name == "" {
			continue
		}
		if nameIDs[group.Name] == nil {
			nameIDs[group.Name] = map[string]bool{}
		}
		nameIDs[group.Name][group.ID] = true
	}
	for _, ids := range nameIDs {
		if len(ids) < 2 {
			continue
		}
		for id := range ids {
			group := byID[id]
			if group.Conflict == "" {
				group.Conflict = "workspace name is used by multiple workspace IDs"
			}
		}
	}
	groups := make([]WorkspaceGroup, 0, len(order))
	for _, id := range order {
		groups = append(groups, *byID[id])
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Name == groups[j].Name {
			return groups[i].ID < groups[j].ID
		}
		if groups[i].Name == "" {
			return false
		}
		if groups[j].Name == "" {
			return true
		}
		return groups[i].Name < groups[j].Name
	})
	return groups
}

func (s *WorkspaceStore) ListWorkspaces() []WorkspaceView {
	groups := s.Groups()
	views := make([]WorkspaceView, 0, len(groups))
	for _, group := range groups {
		view := WorkspaceView{
			ID: group.ID, Name: group.Name, Configured: true,
			Conflict: group.Conflict, AccessCount: len(group.Access),
		}
		selected, selectErr := chooseAccess(s, group)
		if selectErr == nil {
			view.Current = selected.Name == s.Current
			view.Route = RouteOf(selected.SSH)
			view.AutoUpgradeNodes = selected.AutoUpgradeNodes
			view.CreatedAt, view.UpdatedAt = selected.CreatedAt, selected.UpdatedAt
		} else if group.Conflict == "" && len(group.Access) > 1 {
			view.Ambiguous = true
		}
		views = append(views, view)
	}
	return views
}

func chooseAccess(store *WorkspaceStore, group WorkspaceGroup) (WorkspaceAccess, error) {
	if group.Conflict != "" {
		return WorkspaceAccess{}, workspaceGroupConflict(group)
	}
	if len(group.Access) == 0 {
		return WorkspaceAccess{}, contract.NewFault(contract.NotFound, "workspace "+group.ID+" has no local access", "Run eregion login with a current protected export.")
	}
	for _, access := range group.Access {
		if access.Name == store.Current {
			return access, nil
		}
	}
	if len(group.Access) == 1 {
		return group.Access[0], nil
	}
	return WorkspaceAccess{}, workspaceAmbiguous(group)
}

func workspaceGroupConflict(group WorkspaceGroup) error {
	fault := contract.NewFault(contract.Conflict,
		"workspace "+group.ID+" has conflicting local access records",
		"Refresh and replace the canonical workspace with eregion login --replace "+group.Name+", or remove the workspace by ID.")
	fault.Details = map[string]any{"workspace_id": group.ID, "workspace": group.Name}
	return fault
}

func workspaceAmbiguous(group WorkspaceGroup) error {
	fault := contract.NewFault(contract.Conflict,
		"workspace "+group.ID+" has multiple local access records and no current record",
		"Select it with eregion workspace use "+group.ID+", or refresh and replace it with eregion login --replace "+group.Name+".")
	fault.Details = map[string]any{"workspace_id": group.ID, "workspace": group.Name}
	return fault
}

// ResolveWorkspace resolves a public NAME_OR_ID to exactly one local access
// record. An empty selector uses the saved current workspace. It never picks an
// arbitrary alias or newest record.
func (s *WorkspaceStore) ResolveWorkspace(selector string) (WorkspaceAccess, WorkspaceGroup, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		if s.Current == "" {
			return WorkspaceAccess{}, WorkspaceGroup{}, contract.NewFault(contract.InputRequired, "no current workspace is selected", "Pass --workspace NAME, set EREGION_WORKSPACE, or run eregion workspace use NAME_OR_ID.")
		}
		access, ok := s.accessByName(s.Current)
		if !ok {
			return WorkspaceAccess{}, WorkspaceGroup{}, contract.NewFault(contract.PreconditionFailed, "the saved current workspace access is missing", "Run eregion workspace use NAME_OR_ID to select a saved workspace.")
		}
		if access.Workspace == nil {
			return WorkspaceAccess{}, WorkspaceGroup{}, contract.NewFault(contract.PreconditionFailed, "the current access record has no verified workspace identity", "Run eregion apply --assign-workspace NAME on the node, or log in with a current protected export.")
		}
		group, err := s.groupByID(access.Workspace.ID)
		if err != nil {
			return WorkspaceAccess{}, WorkspaceGroup{}, err
		}
		chosen, err := chooseAccess(s, group)
		return chosen, group, err
	}
	if strings.HasPrefix(selector, "ws_") {
		group, err := s.groupByID(selector)
		if err != nil {
			return WorkspaceAccess{}, WorkspaceGroup{}, err
		}
		chosen, err := chooseAccess(s, group)
		return chosen, group, err
	}
	if err := controlplane.ValidateWorkspaceName(selector); err != nil {
		return WorkspaceAccess{}, WorkspaceGroup{}, contract.NewFault(contract.ValidationFailed, "invalid workspace selector "+selector, "Use a verified workspace name or workspace ID (ws_…); local aliases are not selectors.")
	}
	groups := s.groupsByName(selector)
	switch len(groups) {
	case 0:
		fault := contract.NewFault(contract.NotFound, "workspace "+selector+" was not found", "List saved workspaces with eregion workspace list.")
		fault.Details = map[string]any{"workspace": selector}
		return WorkspaceAccess{}, WorkspaceGroup{}, fault
	case 1:
		chosen, err := chooseAccess(s, groups[0])
		return chosen, groups[0], err
	default:
		fault := contract.NewFault(contract.Conflict, "workspace name "+selector+" is used by multiple workspace IDs", "Pass the exact workspace ID to disambiguate; names never select an arbitrary workspace.")
		ids := make([]string, 0, len(groups))
		for _, group := range groups {
			ids = append(ids, group.ID)
		}
		fault.Details = map[string]any{"workspace": selector, "workspace_ids": ids}
		return WorkspaceAccess{}, WorkspaceGroup{}, fault
	}
}

func (s *WorkspaceStore) groupsByName(name string) []WorkspaceGroup {
	var out []WorkspaceGroup
	for _, group := range s.Groups() {
		if group.Name == name {
			out = append(out, group)
		}
	}
	return out
}

func (s *WorkspaceStore) groupByID(id string) (WorkspaceGroup, error) {
	for _, group := range s.Groups() {
		if group.ID == id {
			return group, nil
		}
	}
	fault := contract.NewFault(contract.NotFound, "workspace "+id+" was not found", "List saved workspaces with eregion workspace list.")
	fault.Details = map[string]any{"workspace_id": id}
	return WorkspaceGroup{}, fault
}

// AccessByName returns one private access record by its historical storage key.
func (s *WorkspaceStore) AccessByName(name string) (WorkspaceAccess, bool) {
	return s.accessByName(name)
}

func (s *WorkspaceStore) accessByName(name string) (WorkspaceAccess, bool) {
	for _, access := range s.Access {
		if access.Name == name {
			return access, true
		}
	}
	return WorkspaceAccess{}, false
}

// CurrentAccess returns the saved current access record even when it has no
// verified workspace identity. Only the bounded alpha.6 assignment flow may use
// an unconfigured record.
func (s *WorkspaceStore) CurrentAccess() (WorkspaceAccess, error) {
	if s.Current == "" {
		return WorkspaceAccess{}, contract.NewFault(contract.NotFound, "no current workspace access is saved", "Run eregion login or server init first.")
	}
	access, ok := s.accessByName(s.Current)
	if !ok {
		return WorkspaceAccess{}, contract.NewFault(contract.PreconditionFailed, "the saved current workspace access is missing", "Run eregion workspace use NAME_OR_ID to select a saved workspace.")
	}
	return access, nil
}

// ShowWorkspace returns the one-entry public view for NAME_OR_ID. An exact ID
// can address a group with a name conflict.
func (s *WorkspaceStore) ShowWorkspace(selector string) (WorkspaceView, error) {
	selector = strings.TrimSpace(selector)
	groups := s.Groups()
	var group *WorkspaceGroup
	if strings.HasPrefix(selector, "ws_") {
		for index := range groups {
			if groups[index].ID == selector {
				group = &groups[index]
				break
			}
		}
	} else if selector != "" {
		matching := s.groupsByName(selector)
		if len(matching) == 1 {
			group = &matching[0]
		} else if len(matching) > 1 {
			return WorkspaceView{}, contract.NewFault(contract.Conflict, "workspace name "+selector+" is used by multiple workspace IDs", "Pass the exact workspace ID.")
		}
	} else {
		access, err := s.CurrentAccess()
		if err != nil {
			return WorkspaceView{}, err
		}
		if access.Workspace == nil {
			return WorkspaceView{}, contract.NewFault(contract.PreconditionFailed, "the current access record has no verified workspace identity", "Run eregion apply --assign-workspace NAME on the node, or log in with a current protected export.")
		}
		for index := range groups {
			if groups[index].ID == access.Workspace.ID {
				group = &groups[index]
				break
			}
		}
	}
	if group == nil {
		return WorkspaceView{}, contract.NewFault(contract.NotFound, "workspace "+selector+" was not found", "List saved workspaces with eregion workspace list.")
	}
	for _, view := range s.ListWorkspaces() {
		if view.ID == group.ID {
			return view, nil
		}
	}
	return WorkspaceView{}, contract.NewFault(contract.NotFound, "workspace "+selector+" was not found", "List saved workspaces with eregion workspace list.")
}

// SelectInitializationAccess selects the exact private access record written by
// the bounded server-init flow, including before it has a verified workspace
// identity. It is not a public alias selector; the pending route must match.
func (s *WorkspaceStore) SelectInitializationAccess(name string, expected *WorkspaceSSH) error {
	return s.withMutation(func(fresh *WorkspaceStore) error {
		access, ok := fresh.accessByName(name)
		if !ok {
			return contract.NewFault(contract.NotFound, "the initialization access record is missing", "Resume server init or discard the pending state.")
		}
		if expected != nil && !SameWorkspaceRoute(access.SSH, expected) {
			return contract.NewFault(contract.Conflict,
				"the initialization access route changed while resuming",
				"Discard the unfinished init and start again; the pending route no longer matches the saved access.")
		}
		fresh.Current = access.Name
		fresh.Access = append([]WorkspaceAccess(nil), fresh.Access...)
		return fresh.saveLocked()
	})
}

// UseWorkspace selects a workspace's resolved local access record.
func (s *WorkspaceStore) UseWorkspace(selector string) (WorkspaceView, error) {
	var view WorkspaceView
	err := s.withMutation(func(fresh *WorkspaceStore) error {
		access, group, err := fresh.ResolveWorkspace(selector)
		if err != nil {
			return err
		}
		fresh.Current = access.Name
		now := fresh.Now().UTC()
		for index := range fresh.Access {
			if fresh.Access[index].Name == access.Name {
				fresh.Access[index].UpdatedAt = now
			}
		}
		if err := fresh.saveLocked(); err != nil {
			return err
		}
		view = fresh.viewOfGroup(group.ID)
		return nil
	})
	return view, err
}

func (s *WorkspaceStore) viewOfGroup(id string) WorkspaceView {
	for _, view := range s.ListWorkspaces() {
		if view.ID == id {
			return view
		}
	}
	return WorkspaceView{}
}

// RemovedAccess describes a local record removed by workspace removal.
type RemovedAccess struct {
	Name          string
	CredentialDir string
	SSH           *WorkspaceSSH
}

// RemoveWorkspace removes every local access record for one workspace ID. It
// never changes the database identity, application state or server.
func (s *WorkspaceStore) RemoveWorkspace(selector string) (WorkspaceView, []RemovedAccess, error) {
	return s.removeWorkspace(selector, false)
}

// RemoveWorkspaceWithCleanup removes the records and then attempts credential
// directory cleanup under the same store lock. Directories that are shared,
// nested, symlinked, non-existent, or otherwise not provably owned are retained.
func (s *WorkspaceStore) RemoveWorkspaceWithCleanup(selector string, beforeCleanup func([]RemovedAccess)) (WorkspaceView, []RemovedAccess, []string, error) {
	var retained []string
	var view WorkspaceView
	var removed []RemovedAccess
	err := s.withMutation(func(fresh *WorkspaceStore) error {
		var err error
		view, removed, err = fresh.removeFromLocked(selector)
		if err != nil {
			return err
		}
		if beforeCleanup != nil {
			beforeCleanup(removed)
		}
		retained = fresh.cleanupCredentialDirs(removed)
		return nil
	})
	return view, removed, retained, err
}

func (s *WorkspaceStore) removeWorkspace(selector string, _ bool) (WorkspaceView, []RemovedAccess, error) {
	var view WorkspaceView
	var removed []RemovedAccess
	err := s.withMutation(func(fresh *WorkspaceStore) error {
		var err error
		view, removed, err = fresh.removeFromLocked(selector)
		return err
	})
	return view, removed, err
}

func (s *WorkspaceStore) removeFromLocked(selector string) (WorkspaceView, []RemovedAccess, error) {
	var view WorkspaceView
	var removed []RemovedAccess
	selector = strings.TrimSpace(selector)
	var group WorkspaceGroup
	var err error
	if strings.HasPrefix(selector, "ws_") {
		group, err = s.groupByID(selector)
	} else {
		groups := s.groupsByName(selector)
		switch len(groups) {
		case 0:
			return WorkspaceView{}, nil, contract.NewFault(contract.NotFound, "workspace "+selector+" was not found", "List saved workspaces with eregion workspace list.")
		case 1:
			group = groups[0]
		default:
			return WorkspaceView{}, nil, contract.NewFault(contract.Conflict, "workspace name "+selector+" is used by multiple workspace IDs", "Pass the exact workspace ID to remove.")
		}
	}
	if err != nil {
		return WorkspaceView{}, nil, err
	}
	view = s.viewOfGroup(group.ID)
	kept := make([]WorkspaceAccess, 0, len(s.Access))
	for _, access := range s.Access {
		if access.Workspace != nil && access.Workspace.ID == group.ID {
			removed = append(removed, RemovedAccess{Name: access.Name, CredentialDir: access.CredentialDir, SSH: access.SSH})
			continue
		}
		kept = append(kept, access)
	}
	s.Access = kept
	if s.Current != "" {
		if _, ok := s.accessByName(s.Current); !ok {
			s.Current = ""
		}
	}
	if err := s.saveLocked(); err != nil {
		return WorkspaceView{}, nil, err
	}
	return view, removed, nil
}

func (s *WorkspaceStore) cleanupCredentialDirs(removed []RemovedAccess) []string {
	if len(removed) == 0 {
		return nil
	}
	candidates := make([]string, len(removed))
	for index, record := range removed {
		dir, err := s.CredentialDirFor(WorkspaceAccess{Name: record.Name, CredentialDir: record.CredentialDir})
		if err != nil {
			continue
		}
		candidates[index] = dir
	}
	retained := make([]string, 0)
	for index, record := range removed {
		dir := candidates[index]
		if dir == "" {
			retained = append(retained, record.Name)
			continue
		}
		if _, err := os.Lstat(dir); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				retained = append(retained, record.Name)
			}
			continue
		}
		if !s.provablyOwnedCredentialDir(dir, candidates) {
			retained = append(retained, record.Name)
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			retained = append(retained, record.Name)
		}
	}
	return retained
}

func (s *WorkspaceStore) provablyOwnedCredentialDir(candidate string, otherCandidates []string) bool {
	serversRoot := filepath.Join(filepath.Dir(s.Path), "servers")
	rootAbs, err := filepath.Abs(serversRoot)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil || candidateAbs == rootAbs || !withinPath(rootAbs, candidateAbs) {
		return false
	}
	// Any symlink in the candidate or its ancestors makes ownership ambiguous.
	realCandidate, err := filepath.EvalSymlinks(candidateAbs)
	if err != nil || filepath.Clean(realCandidate) != filepath.Clean(candidateAbs) {
		return false
	}
	refs := make([]string, 0)
	for _, access := range s.Access {
		if dir, err := s.CredentialDirFor(access); err == nil {
			refs = append(refs, dir)
		} else {
			return false
		}
		if access.SSH != nil {
			refs = append(refs, access.SSH.Identity, access.SSH.KnownHosts)
			if access.SSH.WireGuard != nil {
				refs = append(refs, access.SSH.WireGuard.PrivateKeyFile)
			}
		}
	}
	for _, ref := range refs {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		refAbs, err := filepath.Abs(ref)
		if err != nil {
			return false
		}
		if pathsOverlap(candidateAbs, refAbs) {
			return false
		}
		if realRef, err := filepath.EvalSymlinks(refAbs); err == nil {
			if pathsOverlap(realCandidate, realRef) {
				return false
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	for _, other := range otherCandidates {
		if filepath.Clean(other) == filepath.Clean(candidate) {
			continue
		}
		otherAbs, err := filepath.Abs(other)
		if err != nil || pathsOverlap(candidateAbs, otherAbs) {
			return false
		}
	}
	return true
}

func withinPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathsOverlap(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	return left == right || withinPath(left, right) || withinPath(right, left)
}

// SetWorkspaceIdentity updates one saved access record's identity by its
// private access name and freezes the expected route.
func (s *WorkspaceStore) SetWorkspaceIdentity(name string, identity *WorkspaceIdentity, expected *WorkspaceSSH) (WorkspaceView, error) {
	var view WorkspaceView
	err := s.withMutation(func(fresh *WorkspaceStore) error {
		if err := validateWorkspaceIdentity(identity); err != nil {
			return err
		}
		for index := range fresh.Access {
			if fresh.Access[index].Name != name {
				continue
			}
			if expected != nil && !SameWorkspaceRoute(fresh.Access[index].SSH, expected) {
				fault := contract.NewFault(contract.Conflict,
					"workspace access "+name+" changed while the apply was running",
					"Reload the workspace and retry; identity was not written over the replacement.")
				fault.Details = map[string]any{"workspace_access": name}
				return fault
			}
			if fresh.Access[index].Workspace != nil && !sameWorkspaceIdentity(fresh.Access[index].Workspace, identity) {
				return workspaceIdentityConflict(name, fresh.Access[index].Workspace, identity)
			}
			if identity != nil {
				for _, other := range fresh.Access {
					if other.Name == name || other.Workspace == nil {
						continue
					}
					if other.Workspace.Name == identity.Name && other.Workspace.ID != identity.ID {
						return workspaceAccessNameConflict(identity.Name, other)
					}
				}
			}
			if identity == nil {
				fresh.Access[index].Workspace = nil
			} else {
				copied := *identity
				fresh.Access[index].Workspace = &copied
			}
			fresh.Access[index].UpdatedAt = fresh.Now().UTC()
			if err := fresh.saveLocked(); err != nil {
				return err
			}
			view = fresh.viewOfAccess(fresh.Access[index])
			return nil
		}
		return contract.NewFault(contract.NotFound, "workspace access "+name+" was not found", "Run eregion login or server init first.")
	})
	return view, err
}

// PublishWorkspace atomically installs one verified access record for id. When
// replace is set, all existing records for that ID are removed in the same
// locked write; unrelated records and their current selection are preserved.
func (s *WorkspaceStore) PublishWorkspace(id, name string, access WorkspaceAccess, replace bool) (WorkspaceView, error) {
	var view WorkspaceView
	err := s.withMutation(func(fresh *WorkspaceStore) error {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
			return contract.NewFault(contract.ValidationFailed, "the workspace identity is incomplete", "Use a current protected export with both the workspace name and ID.")
		}
		access.Name = strings.TrimSpace(access.Name)
		if access.Name == "" {
			access.Name = name
		}
		if !accessNamePattern.MatchString(access.Name) {
			return contract.NewFault(contract.ValidationFailed, "the workspace name is not usable as a private access key", "Use a validated workspace name.")
		}
		access.Workspace = &WorkspaceIdentity{ID: id, Name: name}
		if err := validateWorkspaceAccess(access); err != nil {
			return err
		}
		existingGroups := fresh.Groups()
		replaced := false
		for _, group := range existingGroups {
			if group.ID == id {
				for _, existing := range group.Access {
					if existing.Workspace != nil && existing.Workspace.Name != name {
						return workspaceGroupNameConflict(group, name)
					}
				}
				if !replace {
					fault := contract.NewFault(contract.Conflict,
						"workspace "+name+" is already saved on this machine",
						"Pass --replace "+name+" to replace its local access records with the verified export.")
					fault.Details = map[string]any{"workspace": name, "workspace_id": id}
					return fault
				}
				replaced = true
			}
		}
		for _, existing := range fresh.Access {
			if existing.Workspace != nil && existing.Workspace.ID != id && existing.Workspace.Name == name {
				fault := contract.NewFault(contract.Conflict,
					"workspace name "+name+" is already used by workspace "+existing.Workspace.ID,
					"A workspace name identifies one immutable workspace ID. Import the export for that workspace, or remove the conflicting local access explicitly.")
				fault.Details = map[string]any{"workspace": name, "workspace_id": existing.Workspace.ID, "incoming_workspace_id": id}
				return fault
			}
		}
		if replaced {
			kept := make([]WorkspaceAccess, 0, len(fresh.Access))
			for _, existing := range fresh.Access {
				if existing.Workspace != nil && existing.Workspace.ID == id {
					continue
				}
				kept = append(kept, existing)
			}
			fresh.Access = kept
		}
		for _, existing := range fresh.Access {
			if existing.Name == access.Name {
				if existing.Workspace == nil && replace {
					// An unconfigured legacy access record may be replaced by a
					// verified canonical record; its old credentials are untouched.
					continue
				}
				return contract.NewFault(contract.Conflict,
					"the private workspace access key is already in use",
					"Remove the conflicting local access explicitly; names are never arbitrary aliases for another workspace.")
			}
		}
		kept := make([]WorkspaceAccess, 0, len(fresh.Access))
		for _, existing := range fresh.Access {
			if existing.Name == access.Name {
				continue
			}
			kept = append(kept, existing)
		}
		fresh.Access = append(kept, access)
		sort.Slice(fresh.Access, func(i, j int) bool { return fresh.Access[i].Name < fresh.Access[j].Name })
		fresh.Current = access.Name
		if err := fresh.saveLocked(); err != nil {
			return err
		}
		view = fresh.viewOfAccess(access)
		return nil
	})
	return view, err
}

// PublishUnconfiguredAccess writes a bounded legacy access record without
// inferring any workspace identity from its private name.
func (s *WorkspaceStore) PublishUnconfiguredAccess(access WorkspaceAccess) (WorkspaceView, error) {
	var view WorkspaceView
	err := s.withMutation(func(fresh *WorkspaceStore) error {
		access.Name = strings.TrimSpace(access.Name)
		if err := validateWorkspaceAccess(access); err != nil {
			return err
		}
		if access.Workspace != nil {
			return contract.NewFault(contract.Internal, "an unconfigured access publication carried a workspace identity", "Report the command and binary version.")
		}
		for _, existing := range fresh.Access {
			if existing.Name == access.Name {
				return contract.NewFault(contract.Conflict,
					"a local access record with this private key already exists",
					"Refresh the protected export to login with a verified workspace identity.")
			}
		}
		fresh.Access = append(fresh.Access, access)
		sort.Slice(fresh.Access, func(i, j int) bool { return fresh.Access[i].Name < fresh.Access[j].Name })
		fresh.Current = access.Name
		if err := fresh.saveLocked(); err != nil {
			return err
		}
		view = fresh.viewOfAccess(access)
		return nil
	})
	return view, err
}

func (s *WorkspaceStore) viewOfAccess(access WorkspaceAccess) WorkspaceView {
	if access.Workspace != nil {
		return s.viewOfGroup(access.Workspace.ID)
	}
	return WorkspaceView{Configured: false, Current: access.Name == s.Current, AccessCount: 1, Route: RouteOf(access.SSH), AutoUpgradeNodes: access.AutoUpgradeNodes, CreatedAt: access.CreatedAt, UpdatedAt: access.UpdatedAt}
}

func sameWorkspaceIdentity(existing, incoming *WorkspaceIdentity) bool {
	if existing == nil || incoming == nil {
		return existing == nil && incoming == nil
	}
	return existing.ID == incoming.ID && existing.Name == incoming.Name
}

func workspaceIdentityConflict(name string, existing, incoming *WorkspaceIdentity) error {
	fault := contract.NewFault(contract.Conflict,
		"workspace access "+name+" is already assigned to a different workspace",
		"The workspace ID and name are immutable locally. Refresh the export or remove the local access explicitly; identity is never overwritten.")
	details := map[string]any{"workspace_access": name}
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

func workspaceAccessNameConflict(name string, other WorkspaceAccess) error {
	fault := contract.NewFault(contract.Conflict,
		"workspace name "+name+" is already used by workspace "+other.Workspace.ID,
		"A workspace name identifies one immutable workspace. Import the export for the existing workspace, or remove the conflicting local access explicitly.")
	fault.Details = map[string]any{"workspace": name, "workspace_id": other.Workspace.ID, "workspace_access": other.Name}
	return fault
}

func workspaceGroupNameConflict(group WorkspaceGroup, incoming string) error {
	fault := contract.NewFault(contract.Conflict,
		"workspace "+group.ID+" has a different saved name",
		"The workspace ID and name are immutable. Remove the local access by ID and refresh the correct protected export.")
	fault.Details = map[string]any{"workspace_id": group.ID, "saved_workspace": group.Name, "incoming_workspace": incoming}
	return fault
}

// SameWorkspaceRoute reports exact route/identity equality without exposing key
// material.
func SameWorkspaceRoute(left, right *WorkspaceSSH) bool {
	return reflect.DeepEqual(left, right)
}

// CredentialDirFor returns the store-relative private credential directory for
// an access record. Existing records use their historical private Name.
func (s *WorkspaceStore) CredentialDirFor(access WorkspaceAccess) (string, error) {
	dir := access.CredentialDir
	if dir == "" {
		dir = filepath.Join("servers", access.Name)
	}
	clean := filepath.Clean(dir)
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", contract.NewFault(contract.PreconditionFailed, "the workspace access directory is invalid", "Repair or move the workspace store aside; it is never printed.")
	}
	base := filepath.Dir(s.Path)
	return filepath.Join(base, clean), nil
}

// CredentialDirName returns a store-relative unique private directory name for
// a verified workspace publication.
func (s *WorkspaceStore) CredentialDirName(id string) (string, error) {
	if s.Path == "" {
		return "", errors.New("workspace store has no path")
	}
	base := filepath.Join("servers", "workspace-"+strings.TrimPrefix(id, "ws_"))
	dir := filepath.Dir(s.Path)
	for suffix := 0; ; suffix++ {
		candidate := base
		if suffix > 0 {
			candidate = base + "-" + strconv.Itoa(suffix)
		}
		_, err := os.Lstat(filepath.Join(dir, candidate))
		switch {
		case err == nil:
			continue
		case errors.Is(err, os.ErrNotExist):
			return candidate, nil
		default:
			return "", err
		}
	}
}

func (s *WorkspaceStore) AccessNames() []string {
	names := make([]string, 0, len(s.Access))
	for _, access := range s.Access {
		names = append(names, access.Name)
	}
	return names
}

// RouteOf renders a safe route label.
func RouteOf(route *WorkspaceSSH) string {
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
