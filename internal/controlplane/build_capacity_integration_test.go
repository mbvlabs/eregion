package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func (s *Service) claimBuildCapacity(ctx context.Context, id contract.OperationID, attempt uint32, node string) (bool, error) {
	exec := engine.Execution{OperationID: id, Attempt: attempt, Step: deploymentPackStep}
	already, err := s.beginBuildDispatch(ctx, id, attempt, node, "", "fixture", exec.ExecutionID())
	var snooze *river.JobSnoozeError
	if errors.As(err, &snooze) {
		return false, nil
	}
	return !already && err == nil, err
}

// This exercises the real operation fence and PostgreSQL transactions, without
// a node dispatcher or build-command substitute.
func TestIntegrationDurableBuildCapacity(t *testing.T) { //nolint:gocognit,gocyclo // One ordered scenario checks concurrent admission and recovery.
	pool, _ := testPool(t, true)
	ctx := context.Background()
	s := testRuntime(t, pool, 0).Service
	if err := insertFixtureNode(ctx, pool, "builder", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureNode(ctx, pool, "other", []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET build_concurrency=2 WHERE name='builder'`); err != nil {
		t.Fatal(err)
	}
	const total = 8
	ids := make([]contract.OperationID, total)
	for i := range ids {
		op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
			Actor: testActor, IdempotencyKey: fmt.Sprintf("capacity-%d", i), Input: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = op.ID
		if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
			t.Fatalf("claim attempt: %v %v", ok, err)
		}
	}
	results := make(chan contract.OperationID, total)
	errs := make(chan error, total)
	var group sync.WaitGroup
	for _, id := range ids {
		group.Add(1)
		go func() {
			defer group.Done()
			ok, err := s.claimBuildCapacity(ctx, id, 1, "builder")
			if err != nil {
				errs <- err
			} else if ok {
				results <- id
			}
		}()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var held []contract.OperationID
	for id := range results {
		held = append(held, id)
	}
	if len(held) != 2 {
		t.Fatalf("concurrent admissions = %d, want 2", len(held))
	}
	if ok, err := s.claimBuildCapacity(ctx, held[0], 1, "other"); err == nil || ok {
		t.Fatalf("existing claim moved builders = %v, %v", ok, err)
	}
	// A new pool connection/session sees the same reservations. Reducing the
	// limit cannot create room while the old claims exceed the new limit.
	pool.Reset()
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET build_concurrency=1 WHERE name='builder'`); err != nil {
		t.Fatal(err)
	}
	var blocked contract.OperationID
	for _, id := range ids {
		if id != held[0] && id != held[1] {
			blocked = id
			break
		}
	}
	if ok, err := s.claimBuildCapacity(ctx, blocked, 1, "builder"); err != nil || ok {
		t.Fatalf("admission after session reset = %v, %v", ok, err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, held[0], 2, "test"); err != nil || !ok {
		t.Fatalf("retry attempt: %v %v", ok, err)
	}
	if ok, err := s.claimBuildCapacity(ctx, held[0], 2, "builder"); err != nil || !ok {
		t.Fatalf("recover own claim = %v, %v", ok, err)
	}
	if err := s.releaseBuildCapacity(ctx, held[0], 1, "builder"); !errors.Is(err, errNotOwned) {
		t.Fatalf("stale release = %v", err)
	}
	if err := s.releaseBuildCapacity(ctx, held[1], 1, "builder"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.claimBuildCapacity(ctx, blocked, 1, "builder"); err != nil || ok {
		t.Fatalf("admission at reduced limit = %v, %v", ok, err)
	}
	if err := s.releaseBuildCapacity(ctx, held[0], 2, "builder"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.claimBuildCapacity(ctx, blocked, 1, "builder"); err != nil || !ok {
		t.Fatalf("admission after release = %v, %v", ok, err)
	}
	assertBuildCapacityTransportUnavailable(t, s, blocked, held[0])
}

func assertBuildCapacityTransportUnavailable(t *testing.T, s *Service, blocked, other contract.OperationID) {
	t.Helper()
	ctx := context.Background()
	// Exercise the production build path with an unavailable transport. No
	// node or command is emulated, and an error must not release capacity.
	w := &platformWorker{service: s}
	_, err := w.executeBuildPack(ctx, blocked, 1, platform.Environment{Node: "builder"},
		platform.Release{}, platform.BuildRecord{}, nil, platform.PackPayload{}, nil)
	if err == nil {
		t.Fatal("missing transport unexpectedly completed a build")
	}
	if ok, err := s.claimBuildCapacity(ctx, other, 2, "builder"); err != nil || ok {
		t.Fatalf("admission after unavailable transport = %v, %v", ok, err)
	}
}
