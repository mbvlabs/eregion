package controlplane

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func wantViolation(t *testing.T, err error, reason string) {
	t.Helper()
	got, ok := IsPlacementViolation(err)
	if !ok || got != reason {
		t.Fatalf("want a placement_violation with reason %q, got ok=%v reason=%q err=%v", reason, ok, got, err)
	}
	fault := contract.Classify(err)
	if fault.Code != contract.PreconditionFailed || !strings.HasPrefix(fault.Message, "placement_violation: ") {
		t.Fatalf("a violation is a structured precondition_failed: %+v", fault)
	}
	if fault.Details["reason"] != reason || fault.Details["node"] == "" || fault.Hint == "" {
		t.Fatalf("a violation names its reason, node, and a hint: %+v", fault)
	}
}

// TestIntegrationBuildSlotsBoundConcurrentBuildsPerNode proves the per-node build
// concurrency limit across callers, and that a released slot admits the next.
func TestIntegrationBuildSlotsBoundConcurrentBuildsPerNode(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	for _, name := range []string{"build-1", "build-2", "build-3"} {
		if err := insertFixtureNode(ctx, pool, name, []string{platform.RoleBuilder}, []string{platform.CapabilityBuildpacks}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET build_concurrency=2 WHERE name IN ('build-1','build-2')`); err != nil {
		t.Fatal(err)
	}
	oldPoll, oldWait := buildSlotPoll, buildSlotWait
	buildSlotPoll, buildSlotWait = 20*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { buildSlotPoll, buildSlotWait = oldPoll, oldWait })

	first, err := s.acquireBuildSlot(ctx, "build-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.acquireBuildSlot(ctx, "build-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	var queued sync.WaitGroup
	queued.Add(1)
	_, err = s.acquireBuildSlot(ctx, "build-1", queued.Done)
	wantViolation(t, err, ReasonBuildCapacity)
	queued.Wait()

	other, err := s.acquireBuildSlot(ctx, "build-2", nil)
	if err != nil {
		t.Fatalf("slots are per node: %v", err)
	}
	other()

	buildSlotWait = 5 * time.Second
	got := make(chan func(), 1)
	go func() {
		release, err := s.acquireBuildSlot(ctx, "build-1", nil)
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- release
	}()
	select {
	case <-got:
		t.Fatal("a build started while every slot was busy")
	case <-time.After(150 * time.Millisecond):
	}
	first()
	select {
	case release := <-got:
		release()
	case <-time.After(3 * time.Second):
		t.Fatal("a released slot did not admit the waiting build")
	}
	second()

	one, err := s.acquireBuildSlot(ctx, "build-3", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := s.acquireBuildSlot(cancelCtx, "build-3", nil); err == nil {
		t.Fatal("a node with one slot ran two builds")
	}
	one()
}
