package operations

import (
	"github.com/mbvlabs/eregion/internal/contract"
)

// Authority selects which operation snapshots, events and cancellations a
// caller may open. A transport resolves it; it is never read from request
// input. A server-resolved Actor is actor-scoped, while OperatorAuthority
// widens reads and cancellation to every operation on the node.
type Authority interface {
	OperationAuthority() (contract.Actor, bool)
}

type operatorAuthority struct {
	actor contract.Actor
}

// OperatorAuthority grants the SSH-authenticated operator access to every
// operation, including CI tokens, the GitHub webhook and other operators'
// submissions.
func OperatorAuthority(actor contract.Actor) Authority {
	return operatorAuthority{actor: actor}
}

func (a operatorAuthority) OperationAuthority() (contract.Actor, bool) {
	return a.actor, true
}

// ResolveAuthority validates an authority and returns its actor and whether it
// may open operations submitted by another actor.
func ResolveAuthority(authority Authority) (contract.Actor, bool, error) {
	if authority == nil {
		return contract.Actor{}, false, contract.NewFault(contract.InputRequired, "operation authority is required", "Resolve an authenticated actor before opening an operation.")
	}
	actor, all := authority.OperationAuthority()
	if err := actor.Validate(); err != nil {
		return contract.Actor{}, false, err
	}
	return actor, all, nil
}
