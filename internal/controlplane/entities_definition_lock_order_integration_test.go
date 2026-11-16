package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationEntitiesDefinitionLockOrderConflict exercises only the
// parent -> run prefix of the documented caller order. It does not acquire the
// step or child. A competing worker blocked at the first parent lock fails with
// SQLSTATE 55P03; this does not claim the typed run method acquires the parent
// by itself.
func TestIntegrationEntitiesDefinitionLockOrderConflict(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	parent := insertDefinitionOperation(t, store, platform.KindDefinitionApply, "env/lock-order/staging", "lock-order-parent", json.RawMessage(`{}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		return tx.DefinitionRuns().Insert(ctx, entities.DefinitionRunInsert{
			OperationID: parent, PlanNonce: "nonce-lock-order", EnvironmentID: "env-lock-order",
			Approved: json.RawMessage(`{}`),
		})
	}); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, found, err := tx.Operations().Lock(ctx, parent); err != nil || !found {
				return err
			}
			close(ready)
			<-release
			if _, found, err := tx.DefinitionRuns().Lock(ctx, parent); err != nil || !found {
				return err
			}
			return nil
		})
	}()
	<-ready
	assertDefinitionParentLockBlocked(t, ctx, pool, parent)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A fresh caller following parent -> run still succeeds after the holder
	// commits; the test proves serialization at the first lock, not a universal
	// reverse-order guarantee.
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, found, err := tx.Operations().Lock(ctx, parent); err != nil || !found {
			return err
		}
		if _, found, err := tx.DefinitionRuns().Lock(ctx, parent); err != nil || !found {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertDefinitionParentLockBlocked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parent contract.OperationID) {
	t.Helper()
	blocked, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Exec(ctx, `SET LOCAL lock_timeout = '250ms'`); err != nil {
		_ = blocked.Rollback(ctx)
		t.Fatal(err)
	}
	var id contract.OperationID
	err = blocked.QueryRow(ctx, `SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, parent).Scan(&id)
	if err == nil {
		_ = blocked.Rollback(ctx)
		t.Fatalf("competing parent lock unexpectedly succeeded")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		_ = blocked.Rollback(ctx)
		t.Fatalf("competing parent lock error=%v, want SQLSTATE 55P03", err)
	}
	if rollbackErr := blocked.Rollback(ctx); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
}
