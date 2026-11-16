package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

	checkJobRuntimeStartup(t, ctx, pool, store)

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
	jobID := seedAtomicOperationSecret(t, ctx, store, id, key, namespace, keyID, ciphertext)
	checkCommittedAtomicOperation(t, ctx, pool, store, id, namespace, jobID)
	checkRejectedAtomicOperation(t, ctx, pool, store, ring)
}

func checkJobRuntimeStartup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store) {
	t.Helper()
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
}

func seedAtomicOperationSecret(
	t *testing.T,
	ctx context.Context,
	store *entities.Store,
	id contract.OperationID,
	key entities.OperationKey,
	namespace vault.Namespace,
	keyID string,
	ciphertext []byte,
) int64 {
	t.Helper()
	var jobID int64
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id, Key: key, InputHash: "atomic-hash",
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
		return tx.Operations().SetJob(ctx, id, jobID)
	}); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func checkCommittedAtomicOperation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, id contract.OperationID, namespace vault.Namespace, jobID int64) {
	t.Helper()
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
}

func checkRejectedAtomicOperation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, ring *vault.KeyRing) {
	t.Helper()
	rejected, rejectedNamespace := seedRejectedAtomicOperation(t, ctx, store, ring)
	checkRejectedAtomicRolledBack(t, ctx, pool, store, rejected, rejectedNamespace)
}

func seedRejectedAtomicOperation(t *testing.T, ctx context.Context, store *entities.Store, ring *vault.KeyRing) (contract.OperationID, vault.Namespace) {
	t.Helper()
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
	return rejected, rejectedNamespace
}

func checkRejectedAtomicRolledBack(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, rejected contract.OperationID, rejectedNamespace vault.Namespace) {
	t.Helper()
	if _, found, err := store.Operations().Get(ctx, rejected); err != nil || found {
		t.Fatalf("rejected operation found=%v err=%v", found, err)
	}
	if _, found, err := store.Secrets().SecretMetadata(ctx, rejectedNamespace); err != nil || found {
		t.Fatalf("rejected secret found=%v err=%v", found, err)
	}
	var jobs int
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
		if err := runDiscardedDedupSavepoint(t, ctx, tx, speculative, key, input, discarded); err != nil {
			return err
		}
		if err := seedAcceptedDedup(t, ctx, tx, accepted, key, input); err != nil {
			return err
		}
		checkDuplicateDedupSavepoint(t, ctx, tx, accepted, key, input)
		expectDedupPanic(t, ctx, tx, phantom, input)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	checkDedupResults(t, ctx, pool, store, speculative, accepted, phantom)
}

func runDiscardedDedupSavepoint(t *testing.T, ctx context.Context, tx *entities.StoreTx, speculative contract.OperationID, key entities.OperationKey, input json.RawMessage, discarded error) error {
	t.Helper()
	err := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
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
	})
	if !errors.Is(err, discarded) {
		t.Fatalf("speculative savepoint error = %v", err)
	}
	return nil
}

func seedAcceptedDedup(t *testing.T, ctx context.Context, tx *entities.StoreTx, accepted contract.OperationID, key entities.OperationKey, input json.RawMessage) error {
	t.Helper()
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
	_, err = tx.Jobs().EnqueueDiagnostic(ctx, accepted)
	return err
}

func checkDuplicateDedupSavepoint(t *testing.T, ctx context.Context, tx *entities.StoreTx, accepted contract.OperationID, key entities.OperationKey, input json.RawMessage) {
	t.Helper()
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
}

func expectDedupPanic(t *testing.T, ctx context.Context, tx *entities.StoreTx, phantom contract.OperationID, input json.RawMessage) {
	t.Helper()
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
}

func checkDedupResults(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, speculative, accepted, phantom contract.OperationID) {
	t.Helper()
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
	enqueueTypedJobs(t, ctx, store, op, id)
	checkTypedJobRows(t, ctx, pool, id)
}

func enqueueTypedJobs(t *testing.T, ctx context.Context, store *entities.Store, op contract.Operation, id contract.OperationID) {
	t.Helper()
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
}

type typedJobRow struct {
	kind, queue string
	maxAttempts int
	args        []byte
}

func checkTypedJobRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id contract.OperationID) {
	t.Helper()
	got := readTypedJobRows(t, ctx, pool)
	if len(got) != 3 {
		t.Fatalf("typed job count = %d", len(got))
	}
	want := []typedJobRow{
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

func readTypedJobRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []typedJobRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT kind,queue,max_attempts,args FROM river_job ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []typedJobRow
	for rows.Next() {
		var row typedJobRow
		if err := rows.Scan(&row.kind, &row.queue, &row.maxAttempts, &row.args); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
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

	checkJobRuntimeUninitialized(t, ctx)
	runJobRuntimeLifecycle(t, ctx, store)
}

func checkJobRuntimeUninitialized(t *testing.T, ctx context.Context) {
	t.Helper()
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
}

func runJobRuntimeLifecycle(t *testing.T, ctx context.Context, store *entities.Store) {
	t.Helper()
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
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stopCancel()
		_ = jobRuntime.StopAndCancel(stopCtx)
	})

	first := mustOperationID(t)
	second := mustOperationID(t)
	insertLifecycleOperation(t, ctx, store, first, "lifecycle-first")
	insertLifecycleOperation(t, ctx, store, second, "lifecycle-second")

	if err := jobRuntime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	enqueueLifecycleJob(t, ctx, store, first)
	requireLifecycleWorkerRun(t, ctx, worker, first)
	if err := jobRuntime.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	if err := jobRuntime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	enqueueLifecycleJob(t, ctx, store, second)
	requireLifecycleWorkerRun(t, ctx, worker, second)
	if err := jobRuntime.StopAndCancel(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
}

func insertLifecycleOperation(t *testing.T, ctx context.Context, store *entities.Store, id contract.OperationID, key string) {
	t.Helper()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
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
	}); err != nil {
		t.Fatal(err)
	}
}

func enqueueLifecycleJob(t *testing.T, ctx context.Context, store *entities.Store, id contract.OperationID) {
	t.Helper()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Jobs().EnqueueDiagnostic(ctx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func requireLifecycleWorkerRun(t *testing.T, ctx context.Context, worker *lifecycleDiagnosticWorker, want contract.OperationID) {
	t.Helper()
	select {
	case got := <-worker.ran:
		if got != want {
			t.Fatalf("worker ran %s, want %s", got, want)
		}
	case <-ctx.Done():
		t.Fatalf("worker did not run job %s: %v", want, ctx.Err())
	}
}
