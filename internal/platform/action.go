package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	KindAppCreate         = "platform.app.create"
	KindNodeRegister      = "platform.node.register"
	KindNodeCheck         = "platform.node.check"
	KindEnvironmentCreate = "platform.environment.create"
	KindReleaseCreate     = "platform.release.create"
	KindDeploy            = "platform.deploy"
	KindRollback          = "platform.rollback"
	KindDomainAdd         = "platform.domain.add"
	KindDomainRemove      = "platform.domain.remove"
)

// MaxInputBytes bounds one action input. The API enforces the same value at the
// HTTP boundary.
const MaxInputBytes = 64 * 1024

// ActionInput is one decoded, validated action input.
type ActionInput interface {
	Target() Target
	Preview() any
}

// Normalizer decodes strict JSON for one canonical target.
type Normalizer func(Target, json.RawMessage) (ActionInput, error)

// Event is one best-effort progress observation from an extension executor. The
// control-plane worker redacts and persists it as an operation event.
type Event struct {
	Level   string
	Stage   string
	Message string
}

// Execution is one accepted action attempt. Input is canonical and must never
// contain plaintext secrets.
type Execution struct {
	OperationID contract.OperationID
	Attempt     uint32
	Kind        string
	Target      string
	Actor       contract.Actor
	Input       json.RawMessage
	Report      func(Event)
}

// Result is a successful extension executor outcome. Data is informational and
// must be secret-free.
type Result struct {
	Message string
	Data    any
}

// Executor runs one accepted action. Extension packages register one; built-in
// kinds leave it nil and the control-plane worker owns execution.
type Executor interface {
	Execute(context.Context, Execution) (Result, error)
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(context.Context, Execution) (Result, error)

func (f ExecutorFunc) Execute(ctx context.Context, exec Execution) (Result, error) {
	return f(ctx, exec)
}

// Definition is one durable action kind.
type Definition struct {
	Kind      string
	Summary   string
	Normalize Normalizer
	Executor  Executor
}

// Normalized is a validated action with canonical input bytes.
type Normalized struct {
	Kind      string
	Target    Target
	Input     ActionInput
	Canonical json.RawMessage
	Summary   string
	def       Definition
}

// Executor returns the extension executor, or nil for a built-in action.
func (n Normalized) Executor() Executor { return n.def.Executor }

// InventoryProvider adds one read-only collection.
type InventoryProvider interface {
	Collection() string
	Read(context.Context, InventoryQuery) (any, error)
}

// Registry holds the action and inventory definitions used by this process.
type Registry struct {
	mu        sync.RWMutex
	defs      map[string]Definition
	inventory map[string]InventoryProvider
}

var defaultRegistry = newBuiltinRegistry()

// Default returns the process-wide registry. Extension packages add to it during
// package initialization, before the API or worker starts.
func Default() *Registry { return defaultRegistry }

// NewRegistry returns an independent registry with the built-in actions. Tests
// use it to avoid sharing extension state.
func NewRegistry() *Registry { return newBuiltinRegistry() }

func newBuiltinRegistry() *Registry {
	r := &Registry{defs: make(map[string]Definition), inventory: make(map[string]InventoryProvider)}
	registerBuiltins(r)
	return r
}

// Register adds an extension action to the process-wide registry.
func Register(def Definition) error { return defaultRegistry.Register(def) }

// RegisterInventory adds an extension inventory collection to the process-wide
// registry.
func RegisterInventory(provider InventoryProvider) error {
	return defaultRegistry.RegisterInventory(provider)
}

// Register adds one action. A public registration must carry an executor: a
// missing executor would silently leave accepted work unexecuted.
func (r *Registry) Register(def Definition) error {
	if def.Kind == "" || def.Normalize == nil {
		return contract.NewFault(contract.ValidationFailed, "invalid action registration", "Provide a kind and a normalizer.")
	}
	if def.Executor == nil {
		return contract.NewFault(contract.ValidationFailed, "extension action has no executor", "Register an executor so accepted work cannot be left unexecuted.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.defs[def.Kind]; exists {
		return contract.NewFault(contract.Conflict, "action kind is already registered", "Choose a unique platform.<action> kind.")
	}
	r.defs[def.Kind] = def
	return nil
}

func (r *Registry) registerBuiltin(def Definition) {
	if def.Kind == "" || def.Normalize == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.defs[def.Kind]; exists {
		return
	}
	r.defs[def.Kind] = def
}

func (r *Registry) RegisterInventory(provider InventoryProvider) error {
	if provider == nil || provider.Collection() == "" {
		return contract.NewFault(contract.ValidationFailed, "invalid inventory provider", "Provide a collection name and a read function.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.inventory[provider.Collection()]; exists {
		return contract.NewFault(contract.Conflict, "inventory collection is already registered", "Choose a unique collection name.")
	}
	r.inventory[provider.Collection()] = provider
	return nil
}

// Lookup returns one action definition.
func (r *Registry) Lookup(kind string) (Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.defs[kind]
	return def, ok
}

// InventoryProvider returns one extension inventory provider.
func (r *Registry) InventoryProvider(collection string) (InventoryProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.inventory[collection]
	return provider, ok
}

// Normalize validates a kind, canonical target, and input object. Unknown
// fields and trailing JSON are rejected.
func (r *Registry) Normalize(kind, targetText string, raw json.RawMessage) (Normalized, error) {
	def, ok := r.Lookup(kind)
	if !ok {
		return Normalized{}, contract.NewFault(contract.ValidationFailed, "unknown platform action kind", "Use a documented platform.<action> kind; inspect the platform contract.")
	}
	if len(raw) > MaxInputBytes {
		return Normalized{}, contract.NewFault(contract.ValidationFailed, "platform input is too large", "Limit action input to 64 KiB.")
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	target, err := ParseTarget(targetText)
	if err != nil {
		return Normalized{}, err
	}
	input, err := def.Normalize(target, raw)
	if err != nil {
		return Normalized{}, err
	}
	if input == nil {
		return Normalized{}, contract.NewFault(contract.ValidationFailed, "platform action produced no input", "This is an internal registration error; report the binary version.")
	}
	resolved := input.Target()
	if resolved.String() != target.String() {
		return Normalized{}, contract.NewFault(contract.ValidationFailed, "target does not match the action input", "Use the canonical target described by the platform contract.")
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return Normalized{}, contract.NewFault(contract.Internal, "could not canonicalize platform input", "This is an internal error; do not resubmit accepted work.")
	}
	return Normalized{Kind: kind, Target: target, Input: input, Canonical: canonical, Summary: def.Summary, def: def}, nil
}

// IsKind reports whether kind is a registered platform action.
func IsKind(kind string) bool {
	_, ok := defaultRegistry.Lookup(kind)
	return ok
}

func strictDecode(raw json.RawMessage, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid platform input", "Use the documented object fields and value ranges; unknown fields are rejected.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return contract.NewFault(contract.ValidationFailed, "platform input has trailing data", "Provide exactly one JSON object.")
	}
	return nil
}

func validateEnv(values map[string]string, label string) error {
	if len(values) > 64 {
		return contract.NewFault(contract.ValidationFailed, label+" has too many entries", "Use at most 64 configuration keys.")
	}
	total := 0
	for key, value := range values {
		if !envKeyPattern.MatchString(key) {
			return contract.NewFault(contract.ValidationFailed, "invalid "+label+" key", "Use 1–128 ASCII letters, digits, and underscores; do not start with a digit.")
		}
		if len(value) > 4096 {
			return contract.NewFault(contract.ValidationFailed, label+" value is too long", "Limit one configuration value to 4096 bytes.")
		}
		for _, r := range value {
			if r == 0 || r == '\n' || r == '\r' {
				return contract.NewFault(contract.ValidationFailed, "invalid "+label+" value", "Configuration values cannot contain NUL or line breaks.")
			}
		}
		total += len(key) + len(value)
	}
	if total > 32*1024 {
		return contract.NewFault(contract.ValidationFailed, label+" is too large", "Limit all configuration keys and values to 32 KiB.")
	}
	return nil
}

func sortedEnvKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func invalid(field, hint string) error {
	return contract.NewFault(contract.ValidationFailed, "invalid "+field, hint)
}

func describeErr(kind string, err error) string {
	return fmt.Sprintf("%s: %v", kind, err)
}
