package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"

	"github.com/mbvlabs/eregion/internal/secrets"
)

// ErrInputsMismatch means an existing effect was recorded under other inputs.
// Retrying the same step cannot make it converge.
var ErrInputsMismatch = errors.New("step inputs mismatch")

// ErrPermanent marks a step Check/Apply failure that a retry cannot fix. Steps
// wrap it to report a non-retryable precondition failure.
var ErrPermanent = errors.New("permanent step failure")

// AttemptUnknown is the documented sentinel for "the attempt that applied this
// effect is not known". A step that keeps a durable record of the applying
// attempt reports it through CheckResult.AppliedAttempt; a step that cannot
// leaves the sentinel in place instead of guessing.
const AttemptUnknown uint32 = 0

// Step is one versioned, idempotent unit of node convergence.
type Step interface {
	ID() string
	Describe() string
	// Inputs returns canonical bytes of everything Apply depends on. Secrets must
	// be represented with HashSecret, never in plaintext.
	Inputs() ([]byte, error)
	// Check must be read-only and cheap. It reports whether the node already
	// matches the step.
	Check(context.Context, Runtime) (CheckResult, error)
	// Apply performs the mutation. It must be safe to call after an interrupted
	// attempt; Check decides whether it is needed.
	Apply(context.Context, Runtime, Reporter) (ApplyResult, error)
	// CancelBoundary says where a cancellation request may safely interrupt this
	// step. It does not authorise killing a process mid-effect.
	CancelBoundary() Boundary
}

// Boundary is where a cancellation request may take effect for a step.
type Boundary string

const (
	// BoundaryBeforeApply may skip Apply without a side effect.
	BoundaryBeforeApply Boundary = "before_apply"
	// BoundaryAfterApply lets an in-flight Apply finish and be acknowledged.
	BoundaryAfterApply Boundary = "after_apply"
	// BoundaryNever must finish; reserve it for short, atomic effects.
	BoundaryNever Boundary = "never"
)

func (b Boundary) Valid() bool {
	return b == BoundaryBeforeApply || b == BoundaryAfterApply || b == BoundaryNever
}

// CheckResult says whether the node already matches the step.
type CheckResult struct {
	Complete bool
	Detail   string
	// AppliedAttempt is the attempt that actually applied the effect, when the
	// step can report it from a durable record. AttemptUnknown means the step
	// did not record it; the engine then keeps the attempt it knows.
	AppliedAttempt uint32
}

// ApplyResult is what Apply changed.
type ApplyResult struct {
	Notes    []string
	Warnings []string
	// Result is stored in the durable effect record. It must not contain secrets.
	Result any
}

// Runtime is what a step may use while it runs.
type Runtime struct {
	DryRun  bool
	Secrets *secrets.Registry
	// SecretValues are transient request-scoped values for the current effect.
	// They are never included in step inputs, the node journal, or events; steps
	// that use them must pass them only to the child process environment.
	SecretValues map[string]string
	// Root is the node's private state directory. Steps that write node-local
	// diagnostic state must stay beneath it.
	Root string
}

// Reporter receives redacted step and log events. It may be nil.
type Reporter func(Event)

// Event is an engine-level observation. The node adapter maps it to its own
// transport event; the engine never emits ANSI or raw terminal text.
type Event struct {
	Kind    string
	Step    string
	Message string
}

func (r Reporter) emit(event Event) {
	if r != nil {
		r(event)
	}
}

// IDPattern is the versioned step ID format, for example diagnostic-step-01-v1.
var IDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-v[0-9]+$`)

// InputsHash is sha256(stepID || 0x00 || inputs) in hex.
func InputsHash(stepID string, inputs []byte) string {
	hash := sha256.New()
	hash.Write([]byte(stepID))
	hash.Write([]byte{0})
	hash.Write(inputs)
	return hex.EncodeToString(hash.Sum(nil))
}

// HashSecret is the canonical representation of a secret input. It is a change
// detector, not encryption or a password store: callers must not rely on it to
// make low-entropy secrets unguessable.
func HashSecret(value string) string {
	hash := sha256.Sum256([]byte("eregion/inputs/v1\x00" + value))
	return "sha256:" + hex.EncodeToString(hash[:])
}
