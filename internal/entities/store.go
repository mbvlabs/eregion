// Package entities owns typed persistence for the control plane's durable
// aggregates. A Store borrows the process database pool; it never closes it and
// it exposes no raw pool, transaction, query or driver handle.
package entities

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// rollbackTimeout bounds the cleanup of a transaction whose caller context may
// already be canceled or exhausted. Rollback itself must not inherit that
// cancellation, or a failed request would pin its connection indefinitely.
const rollbackTimeout = 2 * time.Second

// Store is the typed persistence boundary for one process. The pool is
// borrowed: the caller owns its lifetime, configuration and shutdown.
type Store struct {
	pool *pgxpool.Pool
	// jobs is the one optional River insertion client. It is constructed by
	// NewJobRuntime and never started here.
	jobsMu sync.RWMutex
	jobs   *jobRuntime
}

// NewStore borrows an existing pool. It performs no query and never closes the
// pool. A nil pool is rejected rather than deferred to the first transaction.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("entities: a database pool is required")
	}
	return &Store{pool: pool}, nil
}

// StoreTx is one transaction owned by Store.WithinTx. Domain methods are
// defined on typed views of this value; no raw transaction, query interface or
// driver callback is exported.
type StoreTx struct {
	store *Store
	ctx   context.Context
	tx    pgx.Tx
	q     *query.Queries
	state *transactionState
}

// BeginError reports that WithinTx could not start a transaction. It is
// distinct from a callback or commit failure, so callers can classify storage
// availability without inspecting the driver.
type BeginError struct{ cause error }

func (e *BeginError) Error() string { return "entities: transaction begin failed" }
func (e *BeginError) Unwrap() error { return e.cause }

// CommitError reports a failed transaction commit. It is returned only after
// the callback returned nil, so a callback error is never presented as a commit
// failure. DefiniteRollback is true only when the driver confirmed the
// transaction did not commit.
type CommitError struct {
	cause            error
	definiteRollback bool
}

func (e *CommitError) Error() string          { return "entities: transaction commit failed" }
func (e *CommitError) Unwrap() error          { return e.cause }
func (e *CommitError) DefiniteRollback() bool { return e.definiteRollback }

// IsBeginFailure reports whether err came from starting a Store transaction.
func IsBeginFailure(err error) bool {
	var begin *BeginError
	return errors.As(err, &begin)
}

// IsCommitFailure reports whether err came from committing a Store transaction.
// Callback errors and begin errors never match.
func IsCommitFailure(err error) bool {
	var commit *CommitError
	return errors.As(err, &commit)
}

// IsDefiniteRollback reports whether a commit failure is known to have left the
// transaction uncommitted. Any other failure has an unknown outcome.
func IsDefiniteRollback(err error) bool {
	var commit *CommitError
	return errors.As(err, &commit) && commit.definiteRollback
}

// WithinReadTx begins one repeatable-read, read-only transaction and runs the
// callback with its typed view. It is the consistent-snapshot form for
// multi-statement application reads. Rollback runs on every non-commit exit
// under a detached bounded context. There is no automatic retry.
func (s *Store) WithinReadTx(ctx context.Context, fn func(*StoreTx) error) error {
	if fn == nil {
		return errors.New("entities: a transaction body is required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return &BeginError{cause: err}
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	state := &transactionState{}
	if err := fn(&StoreTx{store: s, ctx: ctx, tx: tx, q: query.New(tx), state: state}); err != nil {
		return err
	}
	if state.poison != nil {
		return state.poison
	}
	if err := tx.Commit(ctx); err != nil {
		return &CommitError{cause: err, definiteRollback: errors.Is(err, pgx.ErrTxCommitRollback)}
	}
	committed = true
	return nil
}

// WithinTx begins, commits and rolls back one transaction. Rollback runs on
// every non-commit exit, including a panic, under a fresh bounded context, and
// a callback error is returned unchanged. Begin and commit failures are typed so
// callers can distinguish them from callback errors without importing a driver.
// There is no automatic retry.
func (s *Store) WithinTx(ctx context.Context, fn func(*StoreTx) error) error {
	if fn == nil {
		return errors.New("entities: a transaction body is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return &BeginError{cause: err}
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	state := &transactionState{}
	if err := fn(&StoreTx{store: s, ctx: ctx, tx: tx, q: query.New(tx), state: state}); err != nil {
		return err
	}
	// A nested savepoint failure poisons the shared transaction state. The
	// callback may have ignored the scope error; the root must not commit.
	if state.poison != nil {
		return state.poison
	}
	if err := tx.Commit(ctx); err != nil {
		return &CommitError{cause: err, definiteRollback: errors.Is(err, pgx.ErrTxCommitRollback)}
	}
	committed = true
	return nil
}
