// Package operations defines the shared application boundary for future API,
// client, and terminal adapters. It deliberately has no database or queue dependency.
package operations

import (
	"context"
	"encoding/json"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Submission is internal service input. Authentication resolves Actor before this
// boundary; never trust an actor claimed in a remote request body. Input must be
// validated by the operation kind and must reference secrets, not embed them.
type Submission struct {
	Kind           string
	Target         string
	Actor          contract.Actor
	IdempotencyKey string
	Input          json.RawMessage
	Confirmation   string
	// HumanFactor is a transient TOTP code carried by the node transport into
	// the submission transaction. It is never persisted in the operation input,
	// events or River arguments. HTTP submissions leave it empty.
	HumanFactor string `json:"-"`
	// WebhookSecret is transient request data for an environment update that
	// enables or rotates push-to-deploy. It is sealed into the vault inside the
	// winning submission transaction and never enters operation input, its
	// canonical hash, events or River arguments. HTTP submissions leave it empty.
	WebhookSecret []byte `json:"-"`
}

// SubmissionResult reports the authoritative admission outcome. Replay is true
// only when an existing operation owns the idempotency key; callers must not
// perform first-submission effects such as hook installation in that case.
type SubmissionResult struct {
	Operation              contract.Operation `json:"operation"`
	Replay                 bool               `json:"replay,omitempty"`
	WebhookSecretActivated bool               `json:"webhook_secret_activated,omitempty"`
}

type EventPage struct {
	Events []contract.Event `json:"events"`
	Cursor contract.Cursor  `json:"cursor"`
}

// Service implementations must atomically commit domain state and River insertion
// and return the authoritative admission receipt from that transaction.
// Get/Events return authorized state, never raw queue rows. Events are ascending,
// After-exclusive and bounded by limit. An expired cursor is an explicit error.
// A terminal viewer consumes this interface, not PostgreSQL, River, or SSH.
type Service interface {
	Submit(context.Context, Submission) (contract.Operation, error)
	SubmitWithReceipt(context.Context, Submission) (SubmissionResult, error)
	Get(context.Context, Authority, contract.OperationID) (contract.Operation, error)
	Events(context.Context, Authority, contract.Cursor, int) (EventPage, error)
	RequestCancel(context.Context, Authority, contract.OperationID) (contract.Operation, error)
}
