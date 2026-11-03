package controlplane

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationWorkerRescuesThisHostsOrphanedPlatformJobs simulates a worker
// SIGKILL: platform jobs and their operations are left running under previous
// client IDs on this host. A new worker must re-queue jobs with attempts left,
// discard an exhausted one with a terminal operation failure, record both
// recoveries as operation events, and leave another host's job alone.
func TestIntegrationWorkerRescuesThisHostsOrphanedPlatformJobs(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	submitter := testRuntime(t, pool, 0)
	s := submitter.Service

	rescued := platformSubmit(t, s, platform.KindAppCreate, platform.PlatformTarget().String(), "rescue-orphan",
		map[string]any{"name": "rescued"})
	exhausted := platformSubmit(t, s, platform.KindAppCreate, platform.PlatformTarget().String(), "rescue-exhausted",
		map[string]any{"name": "exhausted"})
	foreign := platformSubmit(t, s, platform.KindAppCreate, platform.PlatformTarget().String(), "rescue-foreign",
		map[string]any{"name": "foreign"})

	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	hostPrefix := workerHostPrefix(host)
	setRunning := func(op contract.Operation, clientID string, attempt int32) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='running', attempt=$2 WHERE id=$1`, op.ID, attempt); err != nil {
			t.Fatal(err)
		}
		tag, err := pool.Exec(ctx, `UPDATE river_job SET state='running', attempt=$2,
            attempted_at=clock_timestamp(), attempted_by=ARRAY[$3]
            WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, op.ID, attempt, clientID)
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("operation %s has no runnable River job", op.ID)
		}
	}
	setRunning(rescued, hostPrefix+"previous-start", 1)
	setRunning(exhausted, hostPrefix+"previous-start", platformMaxAttempts)
	setRunning(foreign, workerHostPrefix("another-host")+"live", 1)

	run := testRuntime(t, pool, 1)
	if id := run.Jobs.ID(); !strings.HasPrefix(id, hostPrefix) {
		t.Fatalf("worker client ID %q does not carry this host's prefix %q", id, hostPrefix)
	}
	if state := riverJobState(t, pool, rescued.ID); state != "available" {
		t.Fatalf("this host's orphaned job was not re-queued before start: state %q", state)
	}
	if state := riverJobState(t, pool, exhausted.ID); state != "discarded" {
		t.Fatalf("this host's exhausted job was not discarded before start: state %q", state)
	}
	var finalized bool
	var errors int
	if err := pool.QueryRow(ctx, `SELECT finalized_at IS NOT NULL, coalesce(array_length(errors,1),0)
        FROM river_job WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, exhausted.ID).Scan(&finalized, &errors); err != nil {
		t.Fatal(err)
	}
	if !finalized || errors == 0 {
		t.Fatalf("discarded job finalized=%t errors=%d, want both", finalized, errors)
	}
	exhaustedOp, err := run.Service.Get(ctx, testActor, exhausted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exhaustedOp.State != contract.Failed || exhaustedOp.ErrorCode != contract.Unavailable {
		t.Fatalf("exhausted operation state=%s code=%s, want failed/unavailable", exhaustedOp.State, exhaustedOp.ErrorCode)
	}
	if state := riverJobState(t, pool, foreign.ID); state != "running" {
		t.Fatalf("another host's running job was re-queued: state %q", state)
	}

	startWorkers(t, run)
	await(t, 15*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, rescued.ID)
		return err == nil && current.State == contract.Succeeded
	})
	current, err := run.Service.Get(ctx, testActor, rescued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Attempt != 2 {
		t.Fatalf("rescued attempt = %d, want 2", current.Attempt)
	}
	awaitEvent := func(operationID contract.OperationID, want string) {
		t.Helper()
		page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: operationID}, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			if event.Kind == "warning" && strings.Contains(event.Message, want) {
				return
			}
		}
		t.Fatalf("operation %s has no warning event containing %q", operationID, want)
	}
	awaitEvent(rescued.ID, "Recovered orphaned platform job")
	awaitEvent(exhausted.ID, "Discarded platform job")

	if state := riverJobState(t, pool, foreign.ID); state != "running" {
		t.Fatalf("another host's job changed state to %q", state)
	}
	other, err := run.Service.Get(ctx, testActor, foreign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.State != contract.Running {
		t.Fatalf("another host's operation state = %s, want running", other.State)
	}
}

// TestIntegrationWorkerRescueRechecksTheObservedOwner changes the last
// attempted_by entry between the scan and the update: the conditional update
// must leave the job running rather than stealing it from its new owner.
func TestIntegrationWorkerRescueRechecksTheObservedOwner(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	run := testRuntime(t, pool, 0)
	op := platformSubmit(t, run.Service, platform.KindAppCreate, platform.PlatformTarget().String(), "rescue-race",
		map[string]any{"name": "race"})
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='running', attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `UPDATE river_job SET state='running', attempt=1,
        attempted_at=clock_timestamp(), attempted_by=ARRAY['client-a']
        WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1) RETURNING id`, op.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET attempted_by=ARRAY['client-b'] WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	moved, discarded, err := run.Service.recoverOrphanedPlatformJob(ctx, orphanedPlatformJob{
		ID: jobID, OperationID: op.ID, Attempt: 1, MaxAttempts: platformMaxAttempts, LastClient: "client-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved || discarded {
		t.Fatalf("a job whose owner changed was recovered: moved=%t discarded=%t", moved, discarded)
	}
	if state := riverJobState(t, pool, op.ID); state != "running" {
		t.Fatalf("job state = %q, want running", state)
	}
	var attemptedBy []string
	if err := pool.QueryRow(ctx, `SELECT attempted_by FROM river_job WHERE id=$1`, jobID).Scan(&attemptedBy); err != nil {
		t.Fatal(err)
	}
	if len(attemptedBy) != 1 || attemptedBy[0] != "client-b" {
		t.Fatalf("attempted_by = %q, want the new owner", attemptedBy)
	}
}

func riverJobState(t *testing.T, pool *pgxpool.Pool, id contract.OperationID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM river_job WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
