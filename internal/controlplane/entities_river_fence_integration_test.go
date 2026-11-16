package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/riverqueue/river"
)

// TestIntegrationEntitiesRiverOwnerFenceMatrix exercises the app-owned
// conditional discard/requeue predicates for every owner/queue/attempt/state
// fence. False matrices must leave the observed row byte-for-byte unchanged.
func TestIntegrationEntitiesRiverOwnerFenceMatrix(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := store.NewJobRuntime(&river.Config{}); err != nil {
		t.Fatal(err)
	}

	cases := []riverFenceCase{
		{name: "wrong queue", queue: "other-queue", attempt: 2, state: "running", owner: "host:client"},
		{name: "wrong attempt", queue: jobargs.PlatformQueue, attempt: 3, state: "running", owner: "host:client"},
		{name: "wrong owner", queue: jobargs.PlatformQueue, attempt: 2, state: "running", owner: "other:client"},
		{name: "wrong state", queue: jobargs.PlatformQueue, attempt: 2, state: "scheduled", owner: "host:client"},
		{name: "empty owner", queue: jobargs.PlatformQueue, attempt: 2, state: "running", emptyOwner: true},
		{name: "exact owner", queue: jobargs.PlatformQueue, attempt: 2, state: "running", owner: "host:client", want: true},
	}

	for _, method := range []string{"discard", "requeue"} {
		for index, test := range cases {
			t.Run(method+"/"+test.name, func(t *testing.T) {
				checkRiverFenceCase(t, ctx, pool, store, method, index, test)
			})
		}
	}
}

type riverFenceCase struct {
	name       string
	queue      string
	attempt    uint32
	state      string
	owner      string
	emptyOwner bool
	want       bool
}

func checkRiverFenceCase(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, method string, index int, test riverFenceCase) {
	t.Helper()
	jobID, _ := enqueueRiverPlatformJob(t, ctx, store, fmt.Sprintf("%s-%d", method, index))
	markRiverRunning(t, ctx, pool, jobID, 2, "host:client")
	if test.state != "running" {
		if _, err := pool.Exec(ctx, `UPDATE river_job SET state=$2 WHERE id=$1`, jobID, test.state); err != nil {
			t.Fatal(err)
		}
	}
	if test.emptyOwner {
		if _, err := pool.Exec(ctx, `UPDATE river_job SET attempted_by=ARRAY[]::text[] WHERE id=$1`, jobID); err != nil {
			t.Fatal(err)
		}
	}
	beforeState, beforeAttempt, beforeOwner := readRiverFenceRow(t, ctx, pool, jobID)
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		rescue := entities.RiverRescue{ID: jobID, Queue: test.queue, Attempt: test.attempt, LastClient: test.owner}
		var ok bool
		var err error
		if method == "discard" {
			ok, err = tx.RiverJobs().DiscardRunning(ctx, rescue, "fence matrix")
		} else {
			ok, err = tx.RiverJobs().RequeueRunning(ctx, rescue)
		}
		if err != nil {
			return err
		}
		if ok != test.want {
			t.Fatalf("%s result=%v, want %v", method, ok, test.want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	afterState, afterAttempt, afterOwner := readRiverFenceRow(t, ctx, pool, jobID)
	if !test.want {
		if afterState != beforeState || afterAttempt != beforeAttempt || !sameStrings(afterOwner, beforeOwner) {
			t.Fatalf("false fence changed row: before=%s/%d/%v after=%s/%d/%v",
				beforeState, beforeAttempt, beforeOwner, afterState, afterAttempt, afterOwner)
		}
		return
	}
	wantState := "discarded"
	if method == "requeue" {
		wantState = "available"
	}
	if afterState != wantState {
		t.Fatalf("winning %s state=%s, want %s", method, afterState, wantState)
	}
}

func readRiverFenceRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID int64) (string, int, []string) {
	t.Helper()
	var state string
	var attempt int
	var owner []string
	if err := pool.QueryRow(ctx, `SELECT state,attempt,attempted_by FROM river_job WHERE id=$1`, jobID).
		Scan(&state, &attempt, &owner); err != nil {
		t.Fatal(err)
	}
	return state, attempt, owner
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
