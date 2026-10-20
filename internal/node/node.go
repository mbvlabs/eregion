// Package node is the node-dispatch boundary. The local transport runs the same
// request shape in-process; pinned SSH and userspace WireGuard transports can be
// added later without changing the worker that calls Transport.
package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Method is one node request.
type Method string

const (
	MethodRun    Method = "run"
	MethodStatus Method = "status"
)

// Request is transport-independent. Secrets are transient and must never be
// written to River arguments, operation events, or the node journal.
type Request struct {
	Method    Method            `json:"method"`
	RequestID string            `json:"request_id"`
	Execution engine.Execution  `json:"execution"`
	Payload   json.RawMessage   `json:"payload,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
}

// Event is one redacted node observation.
type Event struct {
	Kind        string `json:"kind"`
	Step        string `json:"step"`
	ExecutionID string `json:"execution_id,omitempty"`
	Message     string `json:"message,omitempty"`
}

// Response is the final node result. A transport error is separate: a domain
// failure returns a Response with Status failed.
type Response struct {
	Status         engine.Status `json:"status"`
	ExecutionID    string        `json:"execution_id,omitempty"`
	Attempt        uint32        `json:"attempt,omitempty"`
	AppliedAttempt uint32        `json:"applied_attempt,omitempty"`
	InputsHash     string        `json:"inputs_hash,omitempty"`
	Executed       bool          `json:"executed"`
	Retryable      bool          `json:"retryable"`
	InputsMismatch bool          `json:"inputs_mismatch,omitempty"`
	// ErrorCode classifies a failed step in a machine-readable way, so a caller
	// never has to parse the message text to tell a precondition failure from
	// an unexpected one.
	ErrorCode string          `json:"error_code,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

// Transport moves one request to a node and streams its events. Implementations
// must return a transport error only when the request outcome is unknown; a
// completed but failing step is a Response.
type Transport interface {
	Name() string
	Call(context.Context, Request, func(Event)) (Response, error)
}

// Local runs requests in-process through the node dispatcher.
type Local struct {
	dispatcher *Dispatcher
	name       string
}

func NewLocal(stateDir, nodeName string, registry *secrets.Registry) (*Local, error) {
	dispatcher, err := NewDispatcher(stateDir, nodeName, registry)
	if err != nil {
		return nil, err
	}
	return &Local{dispatcher: dispatcher, name: nodeName}, nil
}

// DefaultStateDir returns the shared node state directory. Every worker process
// on one host must use the same value; production services set it explicitly.
func DefaultStateDir() string {
	if value := os.Getenv("EREGION_NODE_STATE_DIR"); value != "" {
		return value
	}
	if value := os.Getenv("XDG_STATE_HOME"); value != "" {
		return filepath.Join(value, "eregion", "node")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "eregion-node")
	}
	return filepath.Join(home, ".local", "state", "eregion", "node")
}

func (*Local) Name() string { return "local" }

// NodeIdentity is the configured local node name. Platform placement compares
// this authenticated runtime identity with the node record; a hostname is never
// used for placement.
func (l *Local) NodeIdentity() string { return l.name }

func (l *Local) SetHooks(hooks Hooks) { l.dispatcher.SetHooks(hooks) }

func (l *Local) Call(ctx context.Context, request Request, onEvent func(Event)) (Response, error) {
	return l.dispatcher.Call(ctx, request, onEvent)
}
