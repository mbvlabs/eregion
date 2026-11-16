package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/riverqueue/river"
)

// TestIntegrationEntitiesAtomicOperationSecretRiverRollback proves one
// transaction spanning the operation aggregate, a vault revision and a real
// River insertion. The committed scope becomes visible together; the rejected
// scope leaves no operation, event, secret or job behind. The River client is
// constructed on the same Store pool and is never started.
func TestIntegrationEntitiesAtomicOperationSecretRiverRollback(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Enqueue before initialization fails explicitly; there is no fallback
	// client and no raw insert path.
	uninitialized := mustOperationID(t)
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Jobs().EnqueueDiagnostic(ctx, uninitialized)
		return err
	}); !errors.Is(err, entities.ErrJobRuntimeUninitialized) {
		t.Fatalf("uninitialized enqueue error = %v", err)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("river jobs before initialization = %d err %v", jobs, err)
	}

	if _, err := store.NewJobRuntime(&river.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NewJobRuntime(&river.Config{}); !errors.Is(err, entities.ErrJobRuntimeInitialized) {
		t.Fatalf("second job runtime error = %v", err)
	}

	id := mustOperationID(t)
	key := entities.OperationKey{
		Kind: "platform.deploy", Target: "env/demo/staging", Actor: testActor,
		IdempotencyKey: "atomic-foundation",
	}
	namespace := vault.Namespace{App: "demo", Environment: "staging", Name: "WEBHOOK_SECRET"}
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, keyID, err := ring.Seal(namespace, 1, []byte("atomic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	inputsHash := "atomic-hash"
	jobID := int64(0)

	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id, Key: key, InputHash: inputsHash,
			Input: json.RawMessage(`{"app":"demo","environment":"staging"}`),
		})
		if err != nil || !inserted {
			t.Fatalf("atomic insert = %v err %v", inserted, err)
		}
		op, found, err := tx.Operations().Lock(ctx, id)
		if err != nil || !found {
			t.Fatalf("atomic lock found=%v err=%v", found, err)
		}
		if _, _, err := tx.Operations().ClaimAttempt(ctx, id, 1); err != nil {
			return err
		}
		op.Attempt = 1
		if err := tx.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "step", Stage: "atomic", Message: "atomic foundation started",
		}); err != nil {
			return err
		}
		if err := tx.Secrets().InsertSecretNamespace(ctx, namespace, testActor); err != nil {
			t.Fatalf("atomic secret namespace = %v", err)
		}
		if err := tx.Secrets().InsertSecretRevision(ctx, namespace, vault.SecretRevisionInput{
			Namespace: namespace, Revision: 1, KeyID: keyID, Ciphertext: ciphertext,
			CiphertextSHA256: vault.CiphertextSHA256(ciphertext),
			RevisionHash:     vault.RevisionHash(namespace, 1, keyID, ciphertext),
			Actor:            testActor,
		}); err != nil {
			t.Fatalf("atomic secret revision = %v", err)
		}
		jobID, err = tx.Jobs().EnqueueDiagnostic(ctx, id)
		if err != nil {
			return err
		}
		if err := tx.Operations().SetJob(ctx, id, jobID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if jobID == 0 {
		t.Fatalf("committed job has no identity")
	}
	op, found, err := store.Operations().Get(ctx, id)
	if err != nil || !found || op.LastSequence != 1 || op.Attempt != 1 || op.State != contract.Queued {
		t.Fatalf("committed operation = %+v found=%v err=%v", op, found, err)
	}
	if _, found, err := store.Secrets().SecretMetadata(ctx, namespace); err != nil || !found {
		t.Fatalf("committed secret found=%v err=%v", found, err)
	}
	events, err := store.Operations().Events(ctx, contract.Cursor{OperationID: id}, op.LastSequence, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("committed events = %d err %v", len(events), err)
	}
	var kind, queue string
	if err := pool.QueryRow(ctx, `SELECT kind,queue FROM river_job WHERE id=$1`, jobID).Scan(&kind, &queue); err != nil {
		t.Fatal(err)
	}
	if kind != "diagnostic_test" || queue != jobargs.DiagnosticQueue {
		t.Fatalf("committed job kind=%s queue=%s", kind, queue)
	}

	// A rejected transaction leaves none of the four writes behind.
	rejected := mustOperationID(t)
	rejectedNamespace := vault.Namespace{App: "demo", Environment: "staging", Name: "REJECTED_TOKEN"}
	rollbackCause := errors.New("foundation rollback")
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: rejected,
			Key: entities.OperationKey{
				Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
				IdempotencyKey: "atomic-rejected",
			},
			InputHash: "rejected-hash", Input: json.RawMessage(`{"steps":1}`),
		})
		if err != nil || !inserted {
			t.Fatalf("rejected insert = %v err %v", inserted, err)
		}
		op, found, err := tx.Operations().Lock(ctx, rejected)
		if err != nil || !found {
			t.Fatalf("rejected lock found=%v err=%v", found, err)
		}
		if err := tx.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "log", Stage: "atomic", Message: "this event must roll back",
		}); err != nil {
			return err
		}
		if err := tx.Secrets().InsertSecretNamespace(ctx, rejectedNamespace, testActor); err != nil {
			t.Fatalf("rejected secret namespace = %v", err)
		}
		rejectedCiphertext, rejectedKeyID, err := ring.Seal(rejectedNamespace, 1, []byte("rejected-secret"))
		if err != nil {
			return err
		}
		if err := tx.Secrets().InsertSecretRevision(ctx, rejectedNamespace, vault.SecretRevisionInput{
			Namespace: rejectedNamespace, Revision: 1, KeyID: rejectedKeyID, Ciphertext: rejectedCiphertext,
			CiphertextSHA256: vault.CiphertextSHA256(rejectedCiphertext),
			RevisionHash:     vault.RevisionHash(rejectedNamespace, 1, rejectedKeyID, rejectedCiphertext),
			Actor:            testActor,
		}); err != nil {
			return err
		}
		if _, err := tx.Jobs().EnqueueDiagnostic(ctx, rejected); err != nil {
			return err
		}
		return rollbackCause
	}); !errors.Is(err, rollbackCause) {
		t.Fatalf("rollback error = %v, want the callback error unchanged", err)
	}

	if _, found, err := store.Operations().Get(ctx, rejected); err != nil || found {
		t.Fatalf("rejected operation found=%v err=%v", found, err)
	}
	if _, found, err := store.Secrets().SecretMetadata(ctx, rejectedNamespace); err != nil || found {
		t.Fatalf("rejected secret found=%v err=%v", found, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("river jobs after rollback = %d err %v", jobs, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1`, rejected).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("rejected events = %d err %v", jobs, err)
	}
}

// TestIntegrationEntitiesSavepointDedup proves the nested savepoint scope: a
// speculative admission and its River job can be discarded while the outer
// transaction continues, and the deduplicating admission binds exactly one
// operation to one job.
func TestIntegrationEntitiesSavepointDedup(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := store.NewJobRuntime(&river.Config{}); err != nil {
		t.Fatal(err)
	}
	key := entities.OperationKey{
		Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
		IdempotencyKey: "savepoint-dedup",
	}
	discarded := errors.New("discard speculative admission")
	accepted := mustOperationID(t)
	speculative := mustOperationID(t)
	phantom := mustOperationID(t)
	input := json.RawMessage(`{"steps":1}`)

	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		// First speculative admission is rolled back with its River job.
		if err := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
			inserted, err := nested.Operations().Insert(ctx, entities.OperationInsert{
				ID: speculative, Key: key, InputHash: "speculative-hash", Input: input,
			})
			if err != nil || !inserted {
				t.Fatalf("speculative insert = %v err %v", inserted, err)
			}
			if _, err := nested.Jobs().EnqueueDiagnostic(ctx, speculative); err != nil {
				return err
			}
			return discarded
		}); !errors.Is(err, discarded) {
			t.Fatalf("speculative savepoint error = %v", err)
		}

		// The outer transaction is still usable and the discarded admission is
		// invisible, so the accepted attempt admits the same key.
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: accepted, Key: key, InputHash: "speculative-hash", Input: input,
		})
		if err != nil || !inserted {
			t.Fatalf("accepted insert = %v err %v", inserted, err)
		}
		op, found, err := tx.Operations().Lock(ctx, accepted)
		if err != nil || !found {
			t.Fatalf("accepted lock found=%v err=%v", found, err)
		}
		if err := tx.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "state", State: contract.Queued, Message: "accepted after a discarded speculative scope",
		}); err != nil {
			return err
		}
		if _, err := tx.Jobs().EnqueueDiagnostic(ctx, accepted); err != nil {
			return err
		}

		// A duplicate admission inside a released savepoint observes the row its
		// own savepoint can read but writes nothing new. Returning nil keeps it.
		if err := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
			inserted, err := nested.Operations().Insert(ctx, entities.OperationInsert{
				ID: mustOperationID(t), Key: key, InputHash: "other-hash", Input: input,
			})
			if err != nil {
				return err
			}
			if inserted {
				t.Fatalf("duplicate admission inside the savepoint inserted a row")
			}
			identity, found, err := nested.Operations().Identity(ctx, key)
			if err != nil || !found || identity.ID != accepted || identity.InputHash != "speculative-hash" {
				t.Fatalf("nested identity = %+v found=%v err=%v", identity, found, err)
			}
			return nil
		}); err != nil {
			t.Fatalf("released savepoint error = %v", err)
		}

		// A write-then-panic rolls the savepoint back before it escapes; the
		// outer transaction can still commit.
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatalf("savepoint panic did not propagate")
				}
			}()
			_ = tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
				inserted, err := nested.Operations().Insert(ctx, entities.OperationInsert{
					ID: phantom,
					Key: entities.OperationKey{
						Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
						IdempotencyKey: "savepoint-panic",
					},
					InputHash: "panic-hash", Input: input,
				})
				if err != nil || !inserted {
					t.Fatalf("panic marker insert = %v err %v", inserted, err)
				}
				panic("savepoint panic")
			})
		}()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.Operations().Get(ctx, speculative); err != nil || found {
		t.Fatalf("discarded operation found=%v err=%v", found, err)
	}
	op, found, err := store.Operations().Get(ctx, accepted)
	if err != nil || !found || op.LastSequence != 1 {
		t.Fatalf("accepted operation = %+v found=%v err=%v", op, found, err)
	}
	if _, found, err := store.Operations().Get(ctx, phantom); err != nil || found {
		t.Fatalf("panic marker found=%v err=%v", found, err)
	}
	var operationCount, jobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operationCount); err != nil || operationCount != 1 {
		t.Fatalf("operation count = %d err %v", operationCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind=$1`, "diagnostic_test").Scan(&jobCount); err != nil || jobCount != 1 {
		t.Fatalf("diagnostic job count = %d err %v", jobCount, err)
	}
}

// TestIntegrationEntitiesJobRuntimeTypedEnqueues proves all four typed
// insertion methods use the neutral argument types, queue names and attempt
// bounds on the private transaction.
func TestIntegrationEntitiesJobRuntimeTypedEnqueues(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := store.NewJobRuntime(&river.Config{}); err != nil {
		t.Fatal(err)
	}
	id := mustOperationID(t)
	op := contract.Operation{
		ID: id, Kind: "platform.deploy", Target: "env/demo/staging", Actor: testActor,
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Jobs().EnqueuePlatform(ctx, op); err != nil {
			return err
		}
		if _, err := tx.Jobs().EnqueueRetention(ctx, id); err != nil {
			return err
		}
		if _, err := tx.Jobs().EnqueueWebhook(ctx, "env-one", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT kind,queue,max_attempts,args FROM river_job ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type jobRow struct {
		kind, queue string
		maxAttempts int
		args        []byte
	}
	var got []jobRow
	for rows.Next() {
		var row jobRow
		if err := rows.Scan(&row.kind, &row.queue, &row.maxAttempts, &row.args); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("typed job count = %d", len(got))
	}
	want := []jobRow{
		{kind: "platform_action", queue: jobargs.PlatformQueue, maxAttempts: jobargs.PlatformMaxAttempts},
		{kind: "platform_retention", queue: jobargs.MaintenanceQueue, maxAttempts: jobargs.PlatformMaxAttempts},
		{kind: "webhook_admission", queue: jobargs.WebhookQueue, maxAttempts: jobargs.PlatformMaxAttempts},
	}
	for index := range want {
		if got[index].kind != want[index].kind || got[index].queue != want[index].queue || got[index].maxAttempts != want[index].maxAttempts {
			t.Fatalf("job %d = %+v, want %+v", index, got[index], want[index])
		}
	}
	var platform jobargs.PlatformAction
	if err := json.Unmarshal(got[0].args, &platform); err != nil {
		t.Fatal(err)
	}
	if platform.OperationID != id || platform.ActionKind != "platform.deploy" {
		t.Fatalf("platform args = %+v", platform)
	}
	var retention jobargs.Retention
	if err := json.Unmarshal(got[1].args, &retention); err != nil || retention.OperationID != id {
		t.Fatalf("retention args = %+v err %v", retention, err)
	}
	var webhook jobargs.WebhookAdmission
	if err := json.Unmarshal(got[2].args, &webhook); err != nil ||
		webhook.EnvironmentID != "env-one" || webhook.DeliveryIDHash == "" {
		t.Fatalf("webhook args = %+v err %v", webhook, err)
	}
}

type lifecycleDiagnosticWorker struct {
	river.WorkerDefaults[jobargs.DiagnosticTest]
	ran chan contract.OperationID
}

func (w *lifecycleDiagnosticWorker) Work(_ context.Context, job *river.Job[jobargs.DiagnosticTest]) error {
	w.ran <- job.Args.OperationID
	return nil
}

// TestIntegrationEntitiesJobRuntimeLifecycle starts and stops a real River
// client on an isolated database with an actual jobargs worker. It never starts
// a production client.
func TestIntegrationEntitiesJobRuntimeLifecycle(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var uninitialized entities.JobRuntime
	if err := uninitialized.Start(ctx); !errors.Is(err, entities.ErrJobRuntimeUninitialized) {
		t.Fatalf("uninitialized start error = %v", err)
	}
	if err := uninitialized.Stop(ctx); !errors.Is(err, entities.ErrJobRuntimeUninitialized) {
		t.Fatalf("uninitialized stop error = %v", err)
	}
	if err := uninitialized.StopAndCancel(ctx); !errors.Is(err, entities.ErrJobRuntimeUninitialized) {
		t.Fatalf("uninitialized hard-stop error = %v", err)
	}

	workers := river.NewWorkers()
	worker := &lifecycleDiagnosticWorker{ran: make(chan contract.OperationID, 2)}
	river.AddWorker(workers, worker)
	jobRuntime, err := store.NewJobRuntime(&river.Config{
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			jobargs.DiagnosticQueue: {MaxWorkers: 1},
		},
		TestOnly: true,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = jobRuntime.StopAndCancel(stopCtx)
	})

	insertOperation := func(id contract.OperationID, key string) error {
		return store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
				ID: id,
				Key: entities.OperationKey{
					Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
					IdempotencyKey: key,
				},
				InputHash: key, Input: json.RawMessage(`{"steps":1}`),
			})
			if err != nil {
				return err
			}
			if !inserted {
				t.Fatalf("operation %s was not inserted", id)
			}
			return nil
		})
	}
	enqueue := func(id contract.OperationID) error {
		return store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			_, err := tx.Jobs().EnqueueDiagnostic(ctx, id)
			return err
		})
	}

	first := mustOperationID(t)
	second := mustOperationID(t)
	if err := insertOperation(first, "lifecycle-first"); err != nil {
		t.Fatal(err)
	}
	if err := insertOperation(second, "lifecycle-second"); err != nil {
		t.Fatal(err)
	}

	if err := jobRuntime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := enqueue(first); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-worker.ran:
		if got != first {
			t.Fatalf("worker ran %s, want %s", got, first)
		}
	case <-ctx.Done():
		t.Fatalf("worker did not run the first job: %v", ctx.Err())
	}
	if err := jobRuntime.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	if err := jobRuntime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := enqueue(second); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-worker.ran:
		if got != second {
			t.Fatalf("worker ran %s, want %s", got, second)
		}
	case <-ctx.Done():
		t.Fatalf("worker did not run the second job: %v", ctx.Err())
	}
	if err := jobRuntime.StopAndCancel(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
}
