package host

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
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
	// AutoUpgradeNodes installs this CLI's build on a node whose build differs.
	AutoUpgradeNodes bool      `json:"auto_upgrade_nodes,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
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
	Name             string    `json:"name"`
	AutoUpgradeNodes bool      `json:"auto_upgrade_nodes,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Current          bool      `json:"current"`
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
	return nil
}

// Save writes the store atomically with mode 0600.
func (s *ContextStore) Save() error {
	if s.Path == "" {
		return errors.New("context store has no path")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	s.Schema = contextSchema
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(s.Path, append(raw, '\n'), 0o600)
}

// SaveContext adds or replaces a context and makes it current. Login imports
// one from the server init handoff.
func (s *ContextStore) SaveContext(context Context) (ContextView, error) {
	context.Name = strings.TrimSpace(context.Name)
	if !contextNamePattern.MatchString(context.Name) {
		return ContextView{}, contract.NewFault(contract.ValidationFailed, "invalid context name", "Use lowercase letters, digits, hyphens, or underscores.")
	}
	if context.SSH == nil {
		return ContextView{}, contract.NewFault(contract.ValidationFailed, "the context has no SSH route", "Import a server init handoff or re-run server init.")
	}
	now := s.Now().UTC()
	if context.CreatedAt.IsZero() {
		context.CreatedAt = now
	}
	context.UpdatedAt = now
	found := false
	for index := range s.Contexts {
		if s.Contexts[index].Name == context.Name {
			context.CreatedAt = s.Contexts[index].CreatedAt
			s.Contexts[index] = context
			found = true
			break
		}
	}
	if !found {
		s.Contexts = append(s.Contexts, context)
	}
	sort.Slice(s.Contexts, func(i, j int) bool { return s.Contexts[i].Name < s.Contexts[j].Name })
	s.Current = context.Name
	if err := s.Save(); err != nil {
		return ContextView{}, err
	}
	return viewOf(context, true), nil
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
	context, err := s.Get(name)
	if err != nil {
		return ContextView{}, err
	}
	now := s.Now().UTC()
	for index := range s.Contexts {
		if s.Contexts[index].Name == context.Name {
			s.Contexts[index].UpdatedAt = now
			context = s.Contexts[index]
			break
		}
	}
	s.Current = context.Name
	if err := s.Save(); err != nil {
		return ContextView{}, err
	}
	return viewOf(context, true), nil
}

// Remove deletes a context and clears Current when needed.
func (s *ContextStore) Remove(name string) (bool, error) {
	context, err := s.Get(name)
	if err != nil {
		return false, err
	}
	var kept []Context
	for _, candidate := range s.Contexts {
		if candidate.Name != context.Name {
			kept = append(kept, candidate)
		}
	}
	s.Contexts = kept
	if s.Current == context.Name {
		s.Current = ""
	}
	if err := s.Save(); err != nil {
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
	return ContextView{
		Name: context.Name, AutoUpgradeNodes: context.AutoUpgradeNodes,
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
