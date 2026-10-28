package contract

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type OperationID string

var operationIDPattern = regexp.MustCompile(`^op_[0-9a-f]{32}$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var executionIDPattern = regexp.MustCompile(`^op_[0-9a-f]{32}:[0-9]+:[a-z0-9]+(-[a-z0-9]+)*-v[0-9]+$`)
var releaseIDPattern = regexp.MustCompile(`^rel_[0-9a-f]{32}$`)

func NewOperationID() (OperationID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return OperationID("op_" + hex.EncodeToString(value[:])), nil
}

func (id OperationID) Validate() error {
	if !operationIDPattern.MatchString(string(id)) {
		return NewFault(ValidationFailed, "invalid operation ID", "Use the opaque ID returned by submission.")
	}
	return nil
}

// Actor is server-resolved identity, not a trusted identity supplied by an API body.
type Actor struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

func (a Actor) Validate() error {
	switch a.Kind {
	case "operator", "ci", "webhook", "system":
	default:
		return NewFault(ValidationFailed, "invalid actor kind", "Use operator, ci, webhook, or system.")
	}
	if strings.TrimSpace(a.Subject) == "" || len(a.Subject) > 256 {
		return NewFault(ValidationFailed, "invalid actor subject", "Resolve a nonempty identity of at most 256 bytes.")
	}
	return nil
}

// OperationAuthority makes a server-resolved Actor an actor-scoped operation
// authority: it may open only operations it submitted. The SSH-authenticated
// operator channel widens this through operations.OperatorAuthority.
func (a Actor) OperationAuthority() (Actor, bool) {
	return a, false
}

type State string

const (
	Queued     State = "queued"
	Running    State = "running"
	Cancelling State = "cancelling"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
	Canceled   State = "cancelled"
)

func (s State) Valid() bool {
	return s == Queued || s == Running || s == Cancelling || s.Terminal()
}

func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Canceled }

// CanTransition rejects terminal rewrites and implicit retries. Retries within an
// operation remain running and increment attempt; retrying terminal work is a new
// operation. Cancellation can race with completion or reach a safe finish boundary.
func CanTransition(from, to State) bool {
	switch from {
	case Queued:
		return to == Running || to == Cancelling || to == Failed
	case Running:
		return to == Cancelling || to == Succeeded || to == Failed
	case Cancelling:
		return to == Canceled || to == Succeeded || to == Failed
	default:
		return false
	}
}

func ValidateIdempotencyKey(key string) error {
	if !keyPattern.MatchString(key) {
		return NewFault(ValidationFailed, "invalid idempotency key", "Use 1–128 ASCII letters, digits, dots, underscores, colons, or hyphens.")
	}
	return nil
}

// Operation is a domain snapshot, not a River job row or a connection status.
type Operation struct {
	ID           OperationID `json:"id"`
	Kind         string      `json:"kind"`
	Target       string      `json:"target"`
	Actor        Actor       `json:"actor"`
	State        State       `json:"state"`
	Attempt      uint32      `json:"attempt"`
	LastSequence uint64      `json:"last_sequence,string"`
	// ErrorCode classifies why a failed operation ended. It is empty for every
	// other state, so a caller never has to parse the message text.
	ErrorCode Code      `json:"error_code,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// FirstDeployOperationID is set on an accepted app or environment create
	// that queued a first deploy in the same transaction. It is empty when the
	// create requested no deploy.
	FirstDeployOperationID OperationID `json:"first_deploy_operation_id,omitempty"`
}

func (o Operation) Validate() error {
	if err := o.ID.Validate(); err != nil {
		return err
	}
	if err := o.Actor.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(o.Kind) == "" || strings.TrimSpace(o.Target) == "" || !o.State.Valid() || o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() || o.LastSequence == 0 || ((o.State == Running || o.State == Succeeded) && o.Attempt == 0) {
		return NewFault(ValidationFailed, "invalid operation snapshot", "Require authoritative identity, state, timestamps, attempt, and accepted-event sequence.")
	}
	if o.ErrorCode != "" {
		if o.State != Failed {
			return NewFault(ValidationFailed, "invalid operation snapshot", "Only a failed operation carries an error code.")
		}
		if !KnownCode(o.ErrorCode) {
			return NewFault(ValidationFailed, "invalid operation snapshot", "Use a known error code.")
		}
	}
	if o.FirstDeployOperationID != "" {
		if err := o.FirstDeployOperationID.Validate(); err != nil {
			return NewFault(ValidationFailed, "invalid first deploy operation ID", "Use the operation ID returned by the accepted create.")
		}
	}
	return nil
}

// Cursor is scoped to an operation. After is exclusive. String-encoded sequence
// numbers survive JavaScript clients without losing integer precision.
type Cursor struct {
	OperationID OperationID `json:"operation_id"`
	After       uint64      `json:"after,string"`
}

type Progress struct {
	Current uint64 `json:"current,string"`
	Total   uint64 `json:"total,string"`
	Unit    string `json:"unit"`
}

// HealthObservation is a structured probe result. It is emitted beside the
// human-readable message so clients never have to parse log text to learn what
// was probed, how many attempts it took, and what deadline applied.
type HealthObservation struct {
	Target         string `json:"target,omitempty"`
	Type           string `json:"type,omitempty"`
	Passed         bool   `json:"passed"`
	Checks         int    `json:"checks,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

type Event struct {
	OperationID OperationID `json:"operation_id"`
	Sequence    uint64      `json:"sequence,string"`
	At          time.Time   `json:"at"`
	Kind        string      `json:"kind"`
	Attempt     uint32      `json:"attempt"`
	State       State       `json:"state,omitempty"`
	Stage       string      `json:"stage,omitempty"`
	ExecutionID string      `json:"execution_id,omitempty"`
	// Step names the durable node step an observation belongs to. It is present
	// on log and warning events emitted by the platform worker, so build and
	// health lines are attributed without guessing from the last step.
	Step string `json:"step,omitempty"`
	// ReleaseID names the release this event is about. A successful deployment
	// and a traffic-switch acknowledgement name the release that serves; a
	// failed deployment names the release that failed.
	ReleaseID string `json:"release_id,omitempty"`
	// ServingReleaseID names the release the environment actually serves after
	// the observation, when the worker can prove it.
	ServingReleaseID string `json:"serving_release_id,omitempty"`
	// DurationMS is the measured duration of one completed step.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	// Health is a structured probe observation.
	Health *HealthObservation `json:"health,omitempty"`
	// ErrorCode classifies a failed terminal state. It is only valid on a
	// state event that reports the failed state.
	ErrorCode Code      `json:"error_code,omitempty"`
	Message   string    `json:"message,omitempty"`
	Progress  *Progress `json:"progress,omitempty"`
}

func (e Event) Validate() error {
	if err := e.OperationID.Validate(); err != nil {
		return err
	}
	if e.Sequence == 0 || e.At.IsZero() {
		return NewFault(ValidationFailed, "event requires a sequence and timestamp", "Sequences start at 1 within each operation.")
	}
	switch e.Kind {
	case "state":
		if !e.State.Valid() {
			return NewFault(ValidationFailed, "invalid event state", "Use a defined operation state.")
		}
	case "step", "log", "warning", "progress":
		if e.State != "" {
			return NewFault(ValidationFailed, "only state events carry operation state", "Emit a separate state event.")
		}
	default:
		return NewFault(ValidationFailed, "invalid event kind", "Use state, step, log, warning, or progress.")
	}
	if e.Kind == "step" && strings.TrimSpace(e.Stage) == "" {
		return NewFault(ValidationFailed, "step event requires a stage", "Name the observed execution stage.")
	}
	if e.ExecutionID != "" && (e.Kind != "step" || !executionIDPattern.MatchString(e.ExecutionID)) {
		return NewFault(ValidationFailed, "invalid execution identity", "Only step events may carry a durable node execution identity.")
	}
	if e.ErrorCode != "" {
		if e.Kind != "state" || e.State != Failed {
			return NewFault(ValidationFailed, "invalid error code", "Only a failed state event carries an error code.")
		}
		if !KnownCode(e.ErrorCode) {
			return NewFault(ValidationFailed, "invalid error code", "Use a known error code.")
		}
	}
	if e.Kind == "progress" {
		if e.Progress == nil || e.Progress.Total == 0 || e.Progress.Current > e.Progress.Total || strings.TrimSpace(e.Progress.Unit) == "" {
			return NewFault(ValidationFailed, "invalid measured progress", "Provide a unit and a known, positive total; do not estimate overall deployment progress.")
		}
	} else if e.Progress != nil {
		return NewFault(ValidationFailed, "only progress events carry measurements", "Emit a separate progress event.")
	}
	if e.ReleaseID != "" && !releaseIDPattern.MatchString(e.ReleaseID) {
		return NewFault(ValidationFailed, "invalid event release identity", "Use the release ID returned by inventory.")
	}
	if e.ServingReleaseID != "" && !releaseIDPattern.MatchString(e.ServingReleaseID) {
		return NewFault(ValidationFailed, "invalid serving release identity", "Use the release ID returned by inventory.")
	}
	if e.Step != "" && (len(e.Step) > 128 || strings.ContainsAny(e.Step, " \t\r\n\x00")) {
		return NewFault(ValidationFailed, "invalid event step", "Name one durable node step without whitespace.")
	}
	if e.DurationMS != nil && *e.DurationMS < 0 {
		return NewFault(ValidationFailed, "invalid event duration", "Step durations cannot be negative.")
	}
	if e.Health != nil {
		if strings.ContainsAny(e.Health.Target, "\r\n\x00") || strings.ContainsAny(e.Health.Detail, "\r\n\x00") {
			return NewFault(ValidationFailed, "invalid health observation", "Health targets and details cannot contain control characters.")
		}
		if e.Health.Checks < 0 || e.Health.TimeoutSeconds < 0 {
			return NewFault(ValidationFailed, "invalid health observation", "Probe counts and deadlines cannot be negative.")
		}
	}
	return nil
}

func ValidateTransition(from, to State) error {
	if !CanTransition(from, to) {
		return NewFault(Conflict, fmt.Sprintf("cannot transition operation from %s to %s", from, to), "Refresh the authoritative operation before changing it.")
	}
	return nil
}
