package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// Each stale write waits behind the successor's operation lock. This exercises
// the real persistence boundary, including the check/write transaction gap.
type resourceAttemptCase struct {
	name  string
	want  string
	write func(uint32) error
}

func TestIntegrationResourceAttemptFencesBookkeeping(t *testing.T) {
	m := newManagedResourceTest(t)
	ctx := context.Background()
	target := "resource/api/production/db"
	op := platformSubmit(t, m.service, platform.KindResourceApply, target, "attempt-fence", managedResourceInput(false, true))
	platformAwait(t, m.service, op, contract.Succeeded)
	resource, err := m.service.resourceByName(ctx, "api", "production", "db")
	if err != nil {
		t.Fatal(err)
	}
	plan := platform.ResourcePlan{Container: "successor-container", Network: "successor-network"}
	plan.Spec.Image = "successor-image"
	volumes := []platform.ResourceVolumeStatus{{Name: "successor-volume", Destination: "/data"}}
	cases := []resourceAttemptCase{
		{"state", `"state":"destroying"`, func(attempt uint32) error {
			return m.service.markResourceState(ctx, op.ID, attempt, target, platform.ResourceDestroying)
		}},
		{"volumes", `"name": "successor-volume"`, func(attempt uint32) error {
			_, err := m.service.persistOwnedResourceVolumes(ctx, op.ID, attempt, resource.ID, volumes)
			return err
		}},
		{"container", `"container_name":"successor-container"`, func(attempt uint32) error {
			return m.service.recordResourceContainer(ctx, op.ID, attempt, resource.ID, plan, "successor-id", "successor-digest", volumes)
		}},
		{"failure", `"state":"degraded"`, func(attempt uint32) error {
			m.service.recordOwnedResourceFailure(ctx, op.ID, attempt, "api", "production", "db", errors.New("attempt failure"))
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertResourceAttemptFence(t, m, op.ID, resource.ID, tc)
		})
	}
}

func assertResourceAttemptFence(t *testing.T, m *managedResourceTest, id contract.OperationID, resource platform.ResourceID, tc resourceAttemptCase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := m.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE eregion_operations SET attempt=2 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	const snapshot = `SELECT row_to_json(r)::text FROM eregion_platform_resources r WHERE id=$1`
	var before string
	if err := tx.QueryRow(ctx, snapshot, resource).Scan(&before); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tc.write(1) }()
	waitPointerLockWaiter(t, ctx, m.pool, "SELECT id, kind, target, actor_kind")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; tc.name != "failure" && !errors.Is(err, errNotOwned) {
		t.Fatalf("stale write: got %v, want errNotOwned", err)
	}
	var after string
	if err := m.pool.QueryRow(ctx, snapshot, resource).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("stale write changed resource:\nbefore %s\nafter  %s", before, after)
	}
	if err := tc.write(2); err != nil {
		t.Fatalf("current attempt: %v", err)
	}
	if err := m.pool.QueryRow(ctx, snapshot, resource).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before == after || !strings.Contains(after, tc.want) {
		t.Fatalf("current attempt resource missing %s: %s", tc.want, after)
	}
}
