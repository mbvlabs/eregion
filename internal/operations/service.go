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
}

type EventPage struct {
	Events []contract.Event `json:"events"`
	Cursor contract.Cursor  `json:"cursor"`
}

// Service implementations must atomically commit domain state and River insertion.
// Get/Events return authorized state, never raw queue rows. Events are ascending,
// After-exclusive and bounded by limit. An expired cursor is an explicit error.
// A terminal viewer consumes this interface, not PostgreSQL, River, or SSH.
type Service interface {
	Submit(context.Context, Submission) (contract.Operation, error)
	Get(context.Context, Authority, contract.OperationID) (contract.Operation, error)
	Events(context.Context, Authority, contract.Cursor, int) (EventPage, error)
	RequestCancel(context.Context, Authority, contract.OperationID) (contract.Operation, error)
}
