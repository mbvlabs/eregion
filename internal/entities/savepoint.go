package entities

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// SavepointOperation names the lifecycle step that failed.
type SavepointOperation string

const (
	SavepointBegin    SavepointOperation = "begin"
	SavepointRollback SavepointOperation = "rollback"
	SavepointRelease  SavepointOperation = "release"
)

// SavepointError reports that a nested savepoint could not be opened, rolled
// back or released. A callback error is not wrapped when rollback succeeds: the
// exact callback value is returned in that case. When rollback itself fails, the
// cause is errors.Join(callbackError, rollbackError), so errors.Is still sees
// both while a caller can classify the cleanup failure separately.
type SavepointError struct {
	operation SavepointOperation
	cause     error
}

func (e *SavepointError) Error() string {
	return fmt.Sprintf("entities: savepoint %s failed", e.operation)
}

// Operation reports which savepoint lifecycle step failed.
func (e *SavepointError) Operation() SavepointOperation { return e.operation }

func (e *SavepointError) Unwrap() error { return e.cause }

// transactionState is shared by the root transaction and every nested savepoint
// scope. The first savepoint failure poisons it permanently; a later successful
// rollback cannot clear it.
type transactionState struct {
	poison *SavepointError
	// Conservative across savepoint rollbacks: release must wait for a new
	// root transaction whenever this transaction has sealed the claim.
	sealedClaims map[TargetClaimID]bool
}

func (s *transactionState) record(failure *SavepointError) {
	if s.poison == nil {
		s.poison = failure
	}
}

// WithinSavepoint runs fn inside a nested transaction scope on the caller's
// connection. A callback error with a successful rollback is returned as the
// identical value, leaving the outer transaction usable. A begin, rollback or
// release failure poisons the shared transaction state: the root WithinTx will
// not commit even if the callback returned nil. A panic is rolled back under a
// detached bounded context and re-thrown; cleanup failure still poisons. No SQL
// text and no driver handle cross the boundary.
//
// The scope exists for the future webhook speculative insert (job plus receipt)
// and optional enrichment reads. It does not retry and does not commit the outer
// transaction.
func (tx *StoreTx) WithinSavepoint(ctx context.Context, fn func(*StoreTx) error) error {
	if tx == nil || tx.tx == nil {
		return errors.New("entities: savepoint requires a transaction")
	}
	if tx.state == nil {
		return errors.New("entities: savepoint requires a transaction state")
	}
	if fn == nil {
		return errors.New("entities: a savepoint body is required")
	}
	if tx.state.poison != nil {
		return tx.state.poison
	}
	savepoint, err := tx.tx.Begin(ctx)
	if err != nil {
		failure := &SavepointError{operation: SavepointBegin, cause: err}
		tx.state.record(failure)
		return failure
	}
	closed := false
	rollback := func() error {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		return savepoint.Rollback(rollbackCtx)
	}
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if !closed {
			if rollbackErr := rollback(); rollbackErr != nil {
				tx.state.record(&SavepointError{operation: SavepointRollback, cause: rollbackErr})
			}
			closed = true
		}
		panic(recovered)
	}()
	if err := fn(&StoreTx{
		store: tx.store, ctx: ctx, tx: savepoint, q: query.New(savepoint), state: tx.state,
	}); err != nil {
		closed = true
		if rollbackErr := rollback(); rollbackErr != nil {
			failure := &SavepointError{
				operation: SavepointRollback,
				cause:     errors.Join(err, rollbackErr),
			}
			tx.state.record(failure)
			return failure
		}
		return err
	}
	if tx.state.poison != nil {
		// A nested scope failed and the callback ignored it. Roll this scope
		// back instead of releasing it; the root must not commit.
		closed = true
		_ = rollback()
		return tx.state.poison
	}
	if err := savepoint.Commit(ctx); err != nil {
		closed = true
		failure := &SavepointError{operation: SavepointRelease, cause: err}
		tx.state.record(failure)
		return tx.state.poison
	}
	closed = true
	return nil
}
