package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/entities"
)

type classificationCallbackError struct{ cause error }

func (e *classificationCallbackError) Error() string { return "classification callback error" }
func (e *classificationCallbackError) Unwrap() error { return e.cause }

// Begin failures, callback failures and commit failures stay distinguishable
// through the typed entities API. A write made before an active context is
// canceled must not commit, and the borrowed pool must remain usable.
func TestIntegrationEntitiesBeginCallbackAndCancellationClassification(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx := context.Background()

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := store.WithinTx(canceled, func(*entities.StoreTx) error {
		t.Fatal("canceled transaction body ran")
		return nil
	})
	var beginErr *entities.BeginError
	if err == nil || !errors.As(err, &beginErr) || !entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
		t.Fatalf("canceled begin = %T %v", err, err)
	}

	sentinel := errors.New("callback sentinel")
	callbackErr := &classificationCallbackError{cause: sentinel}
	err = store.WithinTx(ctx, func(*entities.StoreTx) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) || !errors.Is(err, sentinel) {
		t.Fatalf("callback error identity = %v, want exact callback error", err)
	}
	var typed *classificationCallbackError
	if !errors.As(err, &typed) || typed != callbackErr {
		t.Fatalf("callback error type = %T %v", err, err)
	}
	if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
		t.Fatalf("callback error was classified as a transaction boundary: %v", err)
	}

	writeCtx, cancelWrite := context.WithCancel(ctx)
	var writeErr error
	err = store.WithinTx(writeCtx, func(tx *entities.StoreTx) error {
		writeErr = tx.Audit().Record(writeCtx, auditEntry("cancel-after-write"))
		if writeErr != nil {
			return writeErr
		}
		cancelWrite()
		return nil
	})
	if writeErr != nil {
		t.Fatalf("write before cancellation: %v", writeErr)
	}
	var commitErr *entities.CommitError
	if !entities.IsCommitFailure(err) || !errors.As(err, &commitErr) {
		t.Fatalf("canceled commit = %T %v", err, err)
	}
	if entities.IsDefiniteRollback(err) {
		t.Fatalf("canceled commit was classified as definite rollback: %v", err)
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("canceled commit retained %d audit rows, %v", rows, err)
	}
	if err := store.Audit().Record(ctx, auditEntry("after-cancel")); err != nil {
		t.Fatalf("pool was not reusable after canceled commit: %v", err)
	}
	var first int64
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_integration_audit WHERE entry->>'action'='after-cancel'`).Scan(&first); err != nil || first != 1 {
		t.Fatalf("identity after canceled commit = %d, %v; want 1", first, err)
	}
}

// Definite rollback is exercised through a real PostgreSQL aborted transaction.
// The deferred-trigger subtest exercises the current conservative unknown-outcome
// classifier branch: PostgreSQL rejected the transaction at COMMIT, but pgx does
// not expose a definite-rollback signal for that path. It does not prove a lost
// COMMIT response or genuine wire ambiguity.
func TestIntegrationEntitiesCommitFailureClassification(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx := context.Background()

	t.Run("definite rollback from aborted transaction", func(t *testing.T) {
		checkAbortedCommitClassification(t, ctx, pool, store)
	})
	t.Run("deferred trigger at commit is conservative unknown", func(t *testing.T) {
		checkDeferredCommitClassification(t, ctx, pool, store)
	})

	if err := store.Audit().Record(ctx, auditEntry("after-commit-failures")); err != nil {
		t.Fatalf("pool was not reusable after commit failures: %v", err)
	}
}

func checkAbortedCommitClassification(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store) {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION eregion_test_aborted_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'aborted audit transaction'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER eregion_test_aborted_audit BEFORE INSERT ON eregion_integration_audit
		FOR EACH ROW EXECUTE FUNCTION eregion_test_aborted_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS eregion_test_aborted_audit ON eregion_integration_audit`)
		_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS eregion_test_aborted_audit()`)
	})

	var swallowed error
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		swallowed = tx.Audit().Record(ctx, auditEntry("aborted"))
		if swallowed == nil {
			return errors.New("audit insert unexpectedly succeeded")
		}
		// Swallow the statement error so COMMIT observes an aborted
		// transaction and PostgreSQL returns the ROLLBACK command tag.
		return nil
	})
	if swallowed == nil {
		t.Fatal("real PG trigger did not abort the transaction")
	}
	var commitErr *entities.CommitError
	if !entities.IsCommitFailure(err) || !errors.As(err, &commitErr) {
		t.Fatalf("aborted commit = %T %v", err, err)
	}
	if !entities.IsDefiniteRollback(err) || !commitErr.DefiniteRollback() {
		t.Fatalf("aborted commit was not a definite rollback: %v", err)
	}
	if !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("aborted commit did not preserve the real PG rollback signal: %v", err)
	}
	requireAuditRows(t, ctx, pool, 0)
}

func checkDeferredCommitClassification(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store) {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION eregion_test_deferred_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'deferred audit commit failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER eregion_test_deferred_audit
		AFTER INSERT ON eregion_integration_audit
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION eregion_test_deferred_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS eregion_test_deferred_audit ON eregion_integration_audit`)
		_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS eregion_test_deferred_audit()`)
	})

	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		return tx.Audit().Record(ctx, auditEntry("deferred-commit"))
	})
	var commitErr *entities.CommitError
	if !entities.IsCommitFailure(err) || !errors.As(err, &commitErr) {
		t.Fatalf("deferred commit = %T %v", err, err)
	}
	if entities.IsDefiniteRollback(err) {
		t.Fatalf("deferred commit was incorrectly classified as definite rollback: %v", err)
	}
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("deferred commit unexpectedly exposed the definite-rollback signal: %v", err)
	}
	requireAuditRows(t, ctx, pool, 0)
}

func requireAuditRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int64) {
	t.Helper()
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit`).Scan(&rows); err != nil || rows != want {
		t.Fatalf("audit rows = %d, %v; want %d", rows, err, want)
	}
}
