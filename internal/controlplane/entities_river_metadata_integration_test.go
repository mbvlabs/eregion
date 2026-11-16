package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/riverqueue/river"
)

func markRiverRunning(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID int64, attempt uint32, lastClient string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE river_job
        SET state='running', attempt=$2, attempted_by=ARRAY[$3]::text[], attempted_at=clock_timestamp()
        WHERE id=$1`, jobID, attempt, lastClient); err != nil {
		t.Fatal(err)
	}
}

func insertRiverOperation(t *testing.T, ctx context.Context, store *entities.Store, kind, target, key string) contract.OperationID {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id,
			Key: entities.OperationKey{
				Kind: kind, Target: target, Actor: testActor, IdempotencyKey: key,
			},
			InputHash: "hash-" + key,
			Input:     json.RawMessage(`{}`),
		})
		if err != nil {
			return err
		}
		if !inserted {
			return errors.New("operation insert conflicted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func enqueueRiverPlatformJob(t *testing.T, ctx context.Context, store *entities.Store, key string) (int64, contract.OperationID) {
	t.Helper()
	op := insertRiverOperation(t, ctx, store, "platform.deploy", "env/river/staging", key)
	var jobID int64
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		jobID, err = tx.Jobs().EnqueuePlatform(ctx, contract.Operation{
			ID: op, Kind: "platform.deploy", Target: "env/river/staging", Actor: testActor,
		})
		if err != nil {
			return err
		}
		return tx.Operations().SetJob(ctx, op, jobID)
	}); err != nil {
		t.Fatal(err)
	}
	return jobID, op
}

// TestIntegrationEntitiesRiverMetadata proves the app-owned River state/rescue
// predicates on real jobs and audits the pinned generation DDL against River's
// own migrated schema in a separate isolated database.
func TestIntegrationEntitiesRiverMetadata(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := store.NewJobRuntime(&river.Config{}); err != nil {
		t.Fatal(err)
	}

	const lastClient = "host:client"
	firstOp := checkRiverRunningDiscard(t, ctx, pool, store, lastClient)
	checkRiverRequeueRescheduleSnooze(t, ctx, pool, store, lastClient)
	checkRiverReconcileCandidates(t, ctx, store, firstOp)
}

func checkRiverRunningDiscard(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, lastClient string) contract.OperationID {
	t.Helper()
	firstJob, firstOp := enqueueRiverPlatformJob(t, ctx, store, "river-first")
	markRiverRunning(t, ctx, pool, firstJob, 2, lastClient)
	running, err := store.RiverJobs().ListRunningForQueue(ctx, jobargs.PlatformQueue)
	if err != nil || len(running) != 1 || running[0].ID != firstJob || running[0].Attempt != 2 ||
		len(running[0].AttemptedBy) != 1 || running[0].AttemptedBy[0] != lastClient || running[0].OperationID != firstOp {
		t.Fatalf("running jobs=%+v err=%v", running, err)
	}
	if state, found, err := store.RiverJobs().GetState(ctx, firstJob); err != nil || !found || state != "running" {
		t.Fatalf("state=%q found=%v err=%v", state, found, err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		state, found, err := tx.RiverJobs().GetForUpdate(ctx, firstJob)
		if err != nil || !found || state != "running" {
			t.Fatalf("locked state=%q found=%v err=%v", state, found, err)
		}
		if discarded, err := tx.RiverJobs().DiscardRunning(ctx, entities.RiverRescue{
			ID: firstJob, Queue: jobargs.PlatformQueue, Attempt: 2, LastClient: "other",
		}, "wrong owner"); err != nil || discarded {
			t.Fatalf("wrong-owner discard=%v err=%v", discarded, err)
		}
		if discarded, err := tx.RiverJobs().DiscardRunning(ctx, entities.RiverRescue{
			ID: firstJob, Queue: jobargs.PlatformQueue, Attempt: 2, LastClient: lastClient,
		}, "foundation discard"); err != nil || !discarded {
			t.Fatalf("discard=%v err=%v", discarded, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := store.RiverJobs().GetState(ctx, firstJob); state != "discarded" {
		t.Fatalf("discarded state=%q", state)
	}
	return firstOp
}

func checkRiverRequeueRescheduleSnooze(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, lastClient string) {
	t.Helper()
	secondJob, _ := enqueueRiverPlatformJob(t, ctx, store, "river-second")
	markRiverRunning(t, ctx, pool, secondJob, 1, lastClient)
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		requeued, err := tx.RiverJobs().RequeueRunning(ctx, entities.RiverRescue{
			ID: secondJob, Queue: jobargs.PlatformQueue, Attempt: 1, LastClient: lastClient,
		})
		if err != nil || !requeued {
			t.Fatalf("requeue=%v err=%v", requeued, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := store.RiverJobs().GetState(ctx, secondJob); state != "available" {
		t.Fatalf("requeued state=%q", state)
	}

	thirdJob, _ := enqueueRiverPlatformJob(t, ctx, store, "river-third")
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		rescheduled, err := tx.RiverJobs().Reschedule(ctx, thirdJob, 5)
		if err != nil || !rescheduled {
			t.Fatalf("reschedule=%v err=%v", rescheduled, err)
		}
		if state, found, err := tx.RiverJobs().GetState(ctx, thirdJob); err != nil || !found || state != "scheduled" {
			t.Fatalf("tx reschedule state=%q found=%v err=%v", state, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state, attempt, maxAttempts, scheduled := readRiverSchedule(t, ctx, pool, thirdJob)
	if state != "scheduled" || attempt != 0 || maxAttempts < 2 || !scheduled.After(time.Now()) {
		t.Fatalf("rescheduled state=%s attempt=%d max=%d scheduled=%v", state, attempt, maxAttempts, scheduled)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		snoozed, err := tx.RiverJobs().Snooze(ctx, thirdJob, 3, 7)
		if err != nil || !snoozed {
			t.Fatalf("snooze=%v err=%v", snoozed, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state, attempt, maxAttempts, scheduled = readRiverSchedule(t, ctx, pool, thirdJob)
	if state != "scheduled" || attempt < 3 || maxAttempts < 4 || !scheduled.After(time.Now()) {
		t.Fatalf("snoozed state=%s attempt=%d max=%d scheduled=%v", state, attempt, maxAttempts, scheduled)
	}
}

func readRiverSchedule(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID int64) (string, int, int, time.Time) {
	t.Helper()
	var attempt, maxAttempts int
	var state string
	var scheduled time.Time
	if err := pool.QueryRow(ctx, `SELECT state,attempt,max_attempts,scheduled_at FROM river_job WHERE id=$1`, jobID).
		Scan(&state, &attempt, &maxAttempts, &scheduled); err != nil {
		t.Fatal(err)
	}
	return state, attempt, maxAttempts, scheduled
}

func checkRiverReconcileCandidates(t *testing.T, ctx context.Context, store *entities.Store, firstOp contract.OperationID) {
	t.Helper()
	candidates, err := store.RiverJobs().ListReconcileCandidateOperations(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range candidates {
		if id == firstOp {
			found = true
		}
	}
	if !found {
		t.Fatalf("discarded job operation not in reconcile candidates: %+v", candidates)
	}
	if _, found, err := store.RiverJobs().GetState(ctx, 99999999); err != nil || found {
		t.Fatalf("missing River state found=%v err=%v", found, err)
	}
}

type riverSchema struct {
	columns     [][]string
	enums       []string
	constraints [][]string
	indexes     [][]string
}

func riverMetadata(t *testing.T, ctx context.Context, pool *pgxpool.Pool) riverSchema {
	t.Helper()
	var schema riverSchema

	rows, err := pool.Query(ctx, `SELECT a.attname,
        format_type(a.atttypid,a.atttypmod), a.attnotnull,
        coalesce(pg_get_expr(d.adbin,d.adrelid),'')
        FROM pg_attribute a
        LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
        WHERE a.attrelid='river_job'::regclass AND a.attnum>0 AND NOT a.attisdropped
        ORDER BY a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, typ, def string
		var notNull bool
		if err := rows.Scan(&name, &typ, &notNull, &def); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		schema.columns = append(schema.columns, []string{name, normalizeSQL(typ), boolText(notNull), normalizeSQL(def)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	rows, err = pool.Query(ctx, `SELECT e.enumlabel
        FROM pg_enum e JOIN pg_type t ON t.oid=e.enumtypid
        WHERE t.typname='river_job_state'
        ORDER BY e.enumsortorder`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		schema.enums = append(schema.enums, label)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	rows, err = pool.Query(ctx, `SELECT conname, contype::text, pg_get_constraintdef(oid)
        FROM pg_constraint WHERE conrelid='river_job'::regclass ORDER BY conname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, kind, def string
		if err := rows.Scan(&name, &kind, &def); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		schema.constraints = append(schema.constraints, []string{name, kind, normalizeSQL(def)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	rows, err = pool.Query(ctx, `SELECT indexname, indexdef
        FROM pg_indexes WHERE schemaname='public' AND tablename='river_job' ORDER BY indexname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		schema.indexes = append(schema.indexes, []string{name, normalizeSQL(def)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return schema
}

func normalizeSQL(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// TestIntegrationRiverGenerationSchemaMatchesMigrated compares the final
// river_job metadata produced by River migrations 1-7 with the pinned
// generation-only DDL in a separate isolated database.
func TestIntegrationRiverGenerationSchemaMatchesMigrated(t *testing.T) {
	migrated, _ := testPool(t, true)
	metadata, _ := testPool(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var version int
	if err := migrated.QueryRow(ctx, `SELECT max(version) FROM river_migration WHERE line='main'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 7 {
		t.Fatalf("River migrated version=%d, want 7", version)
	}

	path := filepath.Join("..", "entities", "river", "schema_pinned.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "migrations") {
		t.Fatalf("generation DDL path is inside the application migration tree: %s", path)
	}
	conn, err := metadata.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Conn().PgConn().Exec(ctx, string(raw)).ReadAll(); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	conn.Release()

	migratedRiver := riverMetadata(t, ctx, migrated)
	metadataRiver := riverMetadata(t, ctx, metadata)
	for name, pair := range map[string][2]any{
		"columns":     {migratedRiver.columns, metadataRiver.columns},
		"enums":       {migratedRiver.enums, metadataRiver.enums},
		"constraints": {migratedRiver.constraints, metadataRiver.constraints},
		"indexes":     {migratedRiver.indexes, metadataRiver.indexes},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("%s differ:\nRiver migrated: %#v\npinned DDL:     %#v", name, pair[0], pair[1])
		}
	}

	// No River DDL belongs to the embedded application migration set.
	entries, err := os.ReadDir(filepath.Join("..", "dbschema", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join("..", "dbschema", "migrations", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "river_job") {
			t.Fatalf("application migration %s owns River DDL", entry.Name())
		}
	}
}
