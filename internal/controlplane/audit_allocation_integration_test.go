package controlplane

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
)

func auditEntry(action string) integrations.AuditEntry {
	return integrations.AuditEntry{
		At: time.Now().UTC(), Actor: testActor.Subject, Action: action,
		Target: "integration/audit", Outcome: "succeeded",
	}
}

// Uses testPool's explicitly authorized isolated database, never a shared DB.
func TestIntegrationAuditConcurrentAllocation(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx := context.Background()
	const count = 12
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.Audit().Record(ctx, auditEntry("concurrent"))
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n, first, last int64
	if err := pool.QueryRow(ctx, `SELECT count(*),min(id),max(id) FROM eregion_integration_audit`).Scan(&n, &first, &last); err != nil {
		t.Fatal(err)
	}
	if n != count || first != 1 || last != count {
		t.Fatalf("count=%d first=%d last=%d", n, first, last)
	}
	var distinct int64
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT id) FROM eregion_integration_audit`).Scan(&distinct); err != nil || distinct != count {
		t.Fatalf("distinct audit identities=%d err=%v", distinct, err)
	}
}

// A rolled-back transaction releases its allocated identity, exhaustion is
// refused without a partial row, and cancellation leaves the pool usable.
func TestIntegrationAuditRollbackExhaustionAndCancellation(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx := context.Background()

	callbackErr := errors.New("audit transaction rollback")
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := tx.Audit().Record(ctx, auditEntry("rolled-back")); err != nil {
			return err
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v, want %v", err, callbackErr)
	}
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback retained %d audit rows, %v", count, err)
	}

	if err := store.Audit().Record(ctx, auditEntry("after-rollback")); err != nil {
		t.Fatal(err)
	}
	var first int64
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_integration_audit`).Scan(&first); err != nil || first != 1 {
		t.Fatalf("identity after rollback = %d, %v; want 1", first, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO eregion_integration_audit(id,entry,recorded_at)
		VALUES($1,'{}'::jsonb,clock_timestamp())`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := store.Audit().Record(ctx, auditEntry("exhausted")); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("exhausted allocation = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE id<>$1`, int64(math.MaxInt64)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("exhaustion left %d ordinary audit rows, %v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE entry->>'action'='exhausted'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("exhaustion wrote %d rows, %v", count, err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM eregion_integration_audit WHERE id=$1`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := store.Audit().Record(ctx, auditEntry("after-exhaustion")); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_integration_audit WHERE entry->>'action'='after-exhaustion'`).Scan(&first); err != nil || first != 2 {
		t.Fatalf("identity after exhaustion = %d, %v; want 2", first, err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Audit().Record(canceled, auditEntry("canceled")); err == nil {
		t.Fatal("canceled audit context opened a transaction")
	}
	if err := store.Audit().Record(ctx, auditEntry("after-cancellation")); err != nil {
		t.Fatalf("pool was poisoned by cancellation: %v", err)
	}
}

// A persisted negative identity is invalid state: allocation must fail closed
// without inserting a row, and remain usable after the corrupt row is removed.
func TestIntegrationAuditNegativeAllocatorGuard(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO eregion_integration_audit(id,entry,recorded_at)
		VALUES(-5,'{}'::jsonb,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if err := store.Audit().Record(ctx, auditEntry("negative")); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("negative allocator guard = %v", err)
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("negative guard retained %d rows, %v", rows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE entry->>'action'='negative'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("negative guard wrote %d rows, %v", rows, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_integration_audit WHERE id=-5`); err != nil {
		t.Fatal(err)
	}
	if err := store.Audit().Record(ctx, auditEntry("after-negative")); err != nil {
		t.Fatalf("pool was not reusable after negative guard: %v", err)
	}
	var first int64
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_integration_audit WHERE entry->>'action'='after-negative'`).Scan(&first); err != nil || first != 1 {
		t.Fatalf("identity after negative guard = %d, %v; want 1", first, err)
	}
}
