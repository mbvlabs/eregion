package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/riverqueue/river"
)

func webhookHash(seed string) string {
	return strings.Repeat(seed, 64/len(seed))[:64]
}

func testWebhookReceiptInsert(seed, environmentID string, jobID int64) entities.WebhookReceiptInsert {
	return entities.WebhookReceiptInsert{
		DeliveryIDHash: webhookHash(seed), BodySHA256: webhookHash("ab"),
		Actor: testActor,
		Kind:  "platform.deploy", Target: "env/webhook-foundation/staging",
		EnvironmentID: environmentID, RevisionID: "rev-webhook-foundation",
		SecretRevision: 3, SecretRevisionHash: webhookHash("cd"),
		Repository: "example/repo", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567",
		JobID: jobID,
	}
}

// TestIntegrationEntitiesWebhookReceipts covers receipt insertion, replay
// dedup, row locking, submit/fail/reassign and the River job association join.
func TestIntegrationEntitiesWebhookReceipts(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := store.NewJobRuntime(&river.Config{}); err != nil {
		t.Fatal(err)
	}

	const environmentID = "env-webhook-foundation"
	if missing, found, err := store.Webhooks().Get(ctx, environmentID, webhookHash("00")); err != nil || found || missing != (entities.WebhookReceipt{}) {
		t.Fatalf("missing receipt=%+v found=%v err=%v", missing, found, err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		missing, found, err := tx.Webhooks().Lock(ctx, environmentID, webhookHash("00"))
		if err != nil || found || missing != (entities.WebhookReceipt{}) {
			t.Fatalf("missing locked receipt=%+v found=%v err=%v", missing, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	const workers = 2
	var wg sync.WaitGroup
	inserted := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
				jobID, err := tx.Jobs().EnqueueWebhook(ctx, environmentID, webhookHash("ef"))
				if err != nil {
					return err
				}
				_, ok, err := tx.Webhooks().Admit(ctx, testWebhookReceiptInsert("ef", environmentID, jobID))
				if err != nil {
					return err
				}
				inserted <- ok
				if !ok {
					return errWebhookReplay
				}
				return nil
			})
			if err != nil && !errors.Is(err, errWebhookReplay) {
				inserted <- false
			}
		}()
	}
	wg.Wait()
	close(inserted)
	winners := 0
	for ok := range inserted {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("webhook admission winners=%d, want 1", winners)
	}
	receipt, found, err := store.Webhooks().Get(ctx, environmentID, webhookHash("ef"))
	if err != nil || !found || receipt.State != "admitted" || receipt.JobID <= 0 || receipt.Actor != testActor {
		t.Fatalf("receipt=%+v found=%v err=%v", receipt, found, err)
	}
	var receipts, jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_webhook_deliveries WHERE environment_id=$1 AND delivery_id_hash=$2`, environmentID, webhookHash("ef")).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("receipt count=%d err=%v", receipts, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'environment_id'=$1 AND args->>'delivery_id_hash'=$2`, environmentID, webhookHash("ef")).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("job count=%d err=%v", jobs, err)
	}

	// A failed transaction leaves neither receipt nor River job.
	rollbackHash := webhookHash("77")
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		jobID, err := tx.Jobs().EnqueueWebhook(ctx, environmentID, rollbackHash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(ctx, testWebhookReceiptInsert("77", environmentID, jobID)); err != nil || !ok {
			t.Fatalf("speculative admit ok=%v err=%v", ok, err)
		}
		return errWebhookRollback
	}); !errors.Is(err, errWebhookRollback) {
		t.Fatalf("rollback error=%v", err)
	}
	if _, found, err := store.Webhooks().Get(ctx, environmentID, rollbackHash); err != nil || found {
		t.Fatalf("rolled-back receipt found=%v err=%v", found, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'environment_id'=$1 AND args->>'delivery_id_hash'=$2`, environmentID, rollbackHash).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("job count after rollback=%d err=%v", jobs, err)
	}

	// A duplicate admission inside a savepoint rolls back only its speculative
	// receipt and real River job; the outer transaction remains usable and
	// commits its marker.
	dupHash := webhookHash("aa")
	var dupJob int64
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		dupJob, err = tx.Jobs().EnqueueWebhook(ctx, environmentID, dupHash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(ctx, testWebhookReceiptInsert("aa", environmentID, dupJob)); err != nil || !ok {
			t.Fatalf("duplicate base admit ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		scopeErr := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
			speculativeJob, err := nested.Jobs().EnqueueWebhook(ctx, environmentID, dupHash)
			if err != nil {
				return err
			}
			_, inserted, err := nested.Webhooks().Admit(ctx, testWebhookReceiptInsert("aa", environmentID, speculativeJob))
			if err != nil {
				return err
			}
			if inserted {
				return fmt.Errorf("duplicate receipt was inserted")
			}
			return errWebhookReplay
		})
		if scopeErr != errWebhookReplay {
			return scopeErr
		}
		// The outer transaction is still usable.
		if err := tx.Retention().SetGCPending(ctx, true); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dupReceipt, found, err := store.Webhooks().Get(ctx, environmentID, dupHash)
	if err != nil || !found || dupReceipt.JobID != dupJob {
		t.Fatalf("duplicate receipt=%+v found=%v err=%v", dupReceipt, found, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, dupHash).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("duplicate delivery jobs=%d err=%v", jobs, err)
	}
	if pending, err := store.Retention().GCPending(ctx); err != nil || !pending {
		t.Fatalf("outer marker pending=%v err=%v", pending, err)
	}

	// Lock, submit and fail transitions.
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		locked, found, err := tx.Webhooks().Lock(ctx, environmentID, webhookHash("ef"))
		if err != nil || !found || locked.JobID != receipt.JobID {
			t.Fatalf("locked=%+v found=%v err=%v", locked, found, err)
		}
		submitted, err := tx.Webhooks().Submit(ctx, environmentID, webhookHash("ef"), mustOperationID(t), "admitted")
		if err != nil || !submitted {
			t.Fatalf("submit=%v err=%v", submitted, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	secondHash := webhookHash("88")
	var secondJob int64
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		secondJob, err = tx.Jobs().EnqueueWebhook(ctx, environmentID, secondHash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(ctx, testWebhookReceiptInsert("88", environmentID, secondJob)); err != nil || !ok {
			t.Fatalf("second admit ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		failed, err := tx.Webhooks().Fail(ctx, environmentID, secondHash, "precondition_failed", "sanitized failure")
		if err != nil || !failed {
			t.Fatalf("fail=%v err=%v", failed, err)
		}
		if failed, err := tx.Webhooks().Fail(ctx, environmentID, secondHash, "internal", "again"); err != nil || failed {
			t.Fatalf("second fail=%v err=%v", failed, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Reassign uses the admitted state and increments attempts.
	thirdHash := webhookHash("99")
	var thirdJob int64
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		thirdJob, err = tx.Jobs().EnqueueWebhook(ctx, environmentID, thirdHash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(ctx, testWebhookReceiptInsert("99", environmentID, thirdJob)); err != nil || !ok {
			t.Fatalf("third admit ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	replacement, found, err := store.RiverJobs().GetState(ctx, thirdJob)
	if err != nil || !found || replacement != "available" {
		t.Fatalf("initial river state=%q found=%v err=%v", replacement, found, err)
	}
	var replacementJob int64
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		replacementJob, err = tx.Jobs().EnqueueWebhook(ctx, environmentID, thirdHash)
		if err != nil {
			return err
		}
		reassigned, err := tx.Webhooks().ReassignJob(ctx, environmentID, thirdHash, replacementJob)
		if err != nil || !reassigned {
			t.Fatalf("reassign=%v err=%v", reassigned, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	receipt, _, _ = store.Webhooks().Get(ctx, environmentID, thirdHash)
	if receipt.JobID != replacementJob || receipt.AdmissionAttempts != 2 {
		t.Fatalf("reassigned receipt=%+v", receipt)
	}

	// A terminal River state makes the admitted receipt actionable.
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state='completed', finalized_at=clock_timestamp() WHERE id=$1`, replacementJob); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.Webhooks().ListNeedingRiver(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundThird := false
	for _, candidate := range candidates {
		if candidate.EnvironmentID == environmentID && candidate.DeliveryIDHash == thirdHash {
			foundThird = true
		}
	}
	if !foundThird {
		t.Fatalf("receipt needing River not listed: %+v", candidates)
	}
	if state, found, err := store.RiverJobs().GetState(ctx, replacementJob); err != nil || !found || state != "completed" {
		t.Fatalf("terminal state=%q found=%v err=%v", state, found, err)
	}
}

var (
	errWebhookReplay   = fmt.Errorf("webhook replay")
	errWebhookRollback = fmt.Errorf("webhook rollback")
)
