package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/riverqueue/river"
)

func webhookHash(seed string) string {
	return strings.Repeat(seed, 64/len(seed))[:64]
}

const webhookFoundationEnvironmentID = "env-webhook-foundation"

func testWebhookReceiptInsert(seed string, jobID int64) entities.WebhookReceiptInsert {
	return entities.WebhookReceiptInsert{
		DeliveryIDHash: webhookHash(seed), BodySHA256: webhookHash("ab"),
		Actor: testActor,
		Kind:  "platform.deploy", Target: "env/webhook-foundation/staging",
		EnvironmentID: webhookFoundationEnvironmentID, RevisionID: "rev-webhook-foundation",
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

	f := webhookReceiptFixture{pool: pool, store: store, ctx: ctx, environmentID: webhookFoundationEnvironmentID}
	checkWebhookMissingReceipt(t, f)
	receipt := checkWebhookConcurrentAdmission(t, f)
	checkWebhookRollback(t, f)
	checkWebhookDuplicateSavepoint(t, f)
	checkWebhookSubmit(t, f, receipt)
	checkWebhookFail(t, f)
	replacementJob := checkWebhookReassign(t, f)
	checkWebhookNeedingRiver(t, f, replacementJob)
}

type webhookReceiptFixture struct {
	pool          *pgxpool.Pool
	store         *entities.Store
	ctx           context.Context
	environmentID string
}

func checkWebhookMissingReceipt(t *testing.T, f webhookReceiptFixture) {
	t.Helper()
	if missing, found, err := f.store.Webhooks().Get(f.ctx, f.environmentID, webhookHash("00")); err != nil || found || missing != (entities.WebhookReceipt{}) {
		t.Fatalf("missing receipt=%+v found=%v err=%v", missing, found, err)
	}
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		missing, found, err := tx.Webhooks().Lock(f.ctx, f.environmentID, webhookHash("00"))
		if err != nil || found || missing != (entities.WebhookReceipt{}) {
			t.Fatalf("missing locked receipt=%+v found=%v err=%v", missing, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type webhookAdmissionResult struct {
	ok  bool
	err error
}

func checkWebhookConcurrentAdmission(t *testing.T, f webhookReceiptFixture) entities.WebhookReceipt {
	t.Helper()
	if winners := concurrentWebhookAdmission(t, f); winners != 1 {
		t.Fatalf("webhook admission winners=%d, want 1", winners)
	}
	receipt, found, err := f.store.Webhooks().Get(f.ctx, f.environmentID, webhookHash("ef"))
	if err != nil || !found || receipt.State != "admitted" || receipt.JobID <= 0 || receipt.Actor != testActor {
		t.Fatalf("receipt=%+v found=%v err=%v", receipt, found, err)
	}
	requireWebhookDeliveries(t, f, webhookHash("ef"), 1, 1)
	return receipt
}

func concurrentWebhookAdmission(t *testing.T, f webhookReceiptFixture) int {
	t.Helper()
	const workers = 2
	var wg sync.WaitGroup
	results := make(chan webhookAdmissionResult, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := admitWebhookOnce(f)
			results <- webhookAdmissionResult{ok: ok, err: err}
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent webhook admission: %v", result.err)
		}
		if result.ok {
			winners++
		}
	}
	return winners
}

func admitWebhookOnce(f webhookReceiptFixture) (bool, error) {
	err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		jobID, err := tx.Jobs().EnqueueWebhook(f.ctx, f.environmentID, webhookHash("ef"))
		if err != nil {
			return err
		}
		_, ok, err := tx.Webhooks().Admit(f.ctx, testWebhookReceiptInsert("ef", jobID))
		if err != nil {
			return err
		}
		if !ok {
			return errWebhookReplay
		}
		return nil
	})
	if errors.Is(err, errWebhookReplay) {
		return false, nil
	}
	return err == nil, err
}

func checkWebhookRollback(t *testing.T, f webhookReceiptFixture) {
	t.Helper()
	rollbackHash := webhookHash("77")
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		jobID, err := tx.Jobs().EnqueueWebhook(f.ctx, f.environmentID, rollbackHash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(f.ctx, testWebhookReceiptInsert("77", jobID)); err != nil || !ok {
			t.Fatalf("speculative admit ok=%v err=%v", ok, err)
		}
		return errWebhookRollback
	}); !errors.Is(err, errWebhookRollback) {
		t.Fatalf("rollback error=%v", err)
	}
	if _, found, err := f.store.Webhooks().Get(f.ctx, f.environmentID, rollbackHash); err != nil || found {
		t.Fatalf("rolled-back receipt found=%v err=%v", found, err)
	}
	requireWebhookJobs(t, f, rollbackHash, 0)
}

func checkWebhookDuplicateSavepoint(t *testing.T, f webhookReceiptFixture) {
	t.Helper()
	dupHash := webhookHash("aa")
	dupJob := seedDuplicateWebhook(t, f, dupHash)
	probeDuplicateWebhookSavepoint(t, f, dupHash)
	dupReceipt, found, err := f.store.Webhooks().Get(f.ctx, f.environmentID, dupHash)
	if err != nil || !found || dupReceipt.JobID != dupJob {
		t.Fatalf("duplicate receipt=%+v found=%v err=%v", dupReceipt, found, err)
	}
	requireWebhookJobs(t, f, dupHash, 1)
	if pending, err := f.store.Retention().GCPending(f.ctx); err != nil || !pending {
		t.Fatalf("outer marker pending=%v err=%v", pending, err)
	}
}

func seedDuplicateWebhook(t *testing.T, f webhookReceiptFixture, hash string) int64 {
	t.Helper()
	var jobID int64
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		jobID, err = tx.Jobs().EnqueueWebhook(f.ctx, f.environmentID, hash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(f.ctx, testWebhookReceiptInsert("aa", jobID)); err != nil || !ok {
			t.Fatalf("duplicate base admit ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func probeDuplicateWebhookSavepoint(t *testing.T, f webhookReceiptFixture, hash string) {
	t.Helper()
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		scopeErr := tx.WithinSavepoint(f.ctx, func(nested *entities.StoreTx) error {
			speculativeJob, err := nested.Jobs().EnqueueWebhook(f.ctx, f.environmentID, hash)
			if err != nil {
				return err
			}
			_, inserted, err := nested.Webhooks().Admit(f.ctx, testWebhookReceiptInsert("aa", speculativeJob))
			if err != nil {
				return err
			}
			if inserted {
				return fmt.Errorf("duplicate receipt was inserted")
			}
			return errWebhookReplay
		})
		if !errors.Is(scopeErr, errWebhookReplay) {
			return scopeErr
		}
		// The outer transaction is still usable.
		return tx.Retention().SetGCPending(f.ctx, true)
	}); err != nil {
		t.Fatal(err)
	}
}

func checkWebhookSubmit(t *testing.T, f webhookReceiptFixture, receipt entities.WebhookReceipt) {
	t.Helper()
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		locked, found, err := tx.Webhooks().Lock(f.ctx, f.environmentID, webhookHash("ef"))
		if err != nil || !found || locked.JobID != receipt.JobID {
			t.Fatalf("locked=%+v found=%v err=%v", locked, found, err)
		}
		submitted, err := tx.Webhooks().Submit(f.ctx, f.environmentID, webhookHash("ef"), mustOperationID(t), "admitted")
		if err != nil || !submitted {
			t.Fatalf("submit=%v err=%v", submitted, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func checkWebhookFail(t *testing.T, f webhookReceiptFixture) {
	t.Helper()
	secondHash := webhookHash("88")
	var secondJob int64
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		secondJob, err = tx.Jobs().EnqueueWebhook(f.ctx, f.environmentID, secondHash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(f.ctx, testWebhookReceiptInsert("88", secondJob)); err != nil || !ok {
			t.Fatalf("second admit ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		failed, err := tx.Webhooks().Fail(f.ctx, f.environmentID, secondHash, "precondition_failed", "sanitized failure")
		if err != nil || !failed {
			t.Fatalf("fail=%v err=%v", failed, err)
		}
		if failed, err := tx.Webhooks().Fail(f.ctx, f.environmentID, secondHash, "internal", "again"); err != nil || failed {
			t.Fatalf("second fail=%v err=%v", failed, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func checkWebhookReassign(t *testing.T, f webhookReceiptFixture) int64 {
	t.Helper()
	thirdHash := webhookHash("99")
	var thirdJob int64
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		thirdJob, err = tx.Jobs().EnqueueWebhook(f.ctx, f.environmentID, thirdHash)
		if err != nil {
			return err
		}
		if _, ok, err := tx.Webhooks().Admit(f.ctx, testWebhookReceiptInsert("99", thirdJob)); err != nil || !ok {
			t.Fatalf("third admit ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	replacement, found, err := f.store.RiverJobs().GetState(f.ctx, thirdJob)
	if err != nil || !found || replacement != "available" {
		t.Fatalf("initial river state=%q found=%v err=%v", replacement, found, err)
	}
	var replacementJob int64
	if err := f.store.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		replacementJob, err = tx.Jobs().EnqueueWebhook(f.ctx, f.environmentID, thirdHash)
		if err != nil {
			return err
		}
		reassigned, err := tx.Webhooks().ReassignJob(f.ctx, f.environmentID, thirdHash, replacementJob)
		if err != nil || !reassigned {
			t.Fatalf("reassign=%v err=%v", reassigned, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	receipt, _, _ := f.store.Webhooks().Get(f.ctx, f.environmentID, thirdHash)
	if receipt.JobID != replacementJob || receipt.AdmissionAttempts != 2 {
		t.Fatalf("reassigned receipt=%+v", receipt)
	}
	return replacementJob
}

func checkWebhookNeedingRiver(t *testing.T, f webhookReceiptFixture, replacementJob int64) {
	t.Helper()
	thirdHash := webhookHash("99")
	// A terminal River state makes the admitted receipt actionable.
	if _, err := f.pool.Exec(f.ctx, `UPDATE river_job SET state='completed', finalized_at=clock_timestamp() WHERE id=$1`, replacementJob); err != nil {
		t.Fatal(err)
	}
	candidates, err := f.store.Webhooks().ListNeedingRiver(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundThird := false
	for _, candidate := range candidates {
		if candidate.EnvironmentID == f.environmentID && candidate.DeliveryIDHash == thirdHash {
			foundThird = true
		}
	}
	if !foundThird {
		t.Fatalf("receipt needing River not listed: %+v", candidates)
	}
	if state, found, err := f.store.RiverJobs().GetState(f.ctx, replacementJob); err != nil || !found || state != "completed" {
		t.Fatalf("terminal state=%q found=%v err=%v", state, found, err)
	}
}

func requireWebhookDeliveries(t *testing.T, f webhookReceiptFixture, hash string, wantReceipts, wantJobs int) {
	t.Helper()
	var receipts, jobs int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM eregion_webhook_deliveries WHERE environment_id=$1 AND delivery_id_hash=$2`, f.environmentID, hash).Scan(&receipts); err != nil || receipts != wantReceipts {
		t.Fatalf("receipt count=%d err=%v; want %d", receipts, err, wantReceipts)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM river_job WHERE args->>'environment_id'=$1 AND args->>'delivery_id_hash'=$2`, f.environmentID, hash).Scan(&jobs); err != nil || jobs != wantJobs {
		t.Fatalf("job count=%d err=%v; want %d", jobs, err, wantJobs)
	}
}

func requireWebhookJobs(t *testing.T, f webhookReceiptFixture, hash string, want int) {
	t.Helper()
	var jobs int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, hash).Scan(&jobs); err != nil || jobs != want {
		t.Fatalf("delivery jobs=%d err=%v; want %d", jobs, err, want)
	}
}

var (
	errWebhookReplay   = fmt.Errorf("webhook replay")
	errWebhookRollback = fmt.Errorf("webhook rollback")
)
