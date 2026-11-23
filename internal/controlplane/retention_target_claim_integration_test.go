package controlplane

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/riverqueue/river"
)

func TestIntegrationRetentionTargetClaimFencesAndRetainsUncertainDispatch(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := newPlatformFixture(t, pool, "retention-claim")
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	op, err := s.Submit(ctx, platformSubmission(platform.KindRetentionPrune, "node/"+fixture.node, "retention-claim", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	op, owned, err := s.claimRetention(ctx, op.ID, 1)
	if err != nil || !owned {
		t.Fatalf("claim operation: owned=%v err=%v", owned, err)
	}
	workCtx, claim, err := s.acquireRetentionTargets(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionClaimTargets(t, ctx, s, op, claim, fixture)
	if err := s.settleRetentionTargets(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.retentionWrite(workCtx, func(*entities.StoreTx) error {
		t.Fatal("sealed executor wrote state")
		return nil
	}); !errors.Is(err, errNotOwned) {
		t.Fatalf("stale write error: %v", err)
	}
	_, _, err = s.acquireRetentionTargets(ctx, op)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("uncertain claim should block retry: %v", err)
	}
	assertRetentionClaimSettlement(t, ctx, s, op, claim)
}

func TestIntegrationRetentionRegistryClaimExcludesUpload(t *testing.T) {
	pool, dsn := testPool(t, true)
	fixture := newPlatformFixture(t, pool, "retention-registry")
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	config, err := ParseDatabaseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.Submit(ctx, platformSubmission(platform.KindRetentionPrune, "node/"+fixture.node, "retention-registry", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	op, owned, err := s.claimRetention(ctx, op.ID, 1)
	if err != nil || !owned {
		t.Fatalf("claim operation: owned=%v err=%v", owned, err)
	}
	upload, err := acquireImageUploadClaim(ctx, config.ConnConfig, fixture.app.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.acquireRetentionTargets(ctx, op)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("retention during upload: %v", err)
	}
	upload.Release(ctx)
	_, claim, err := s.acquireRetentionTargets(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	upload, err = acquireImageUploadClaim(ctx, config.ConnConfig, fixture.app.Name)
	if upload != nil {
		upload.Release(ctx)
	}
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("upload during retention: %v", err)
	}
	if err := s.settleRetentionTargets(ctx, claim); err != nil {
		t.Fatal(err)
	}
	upload, err = acquireImageUploadClaim(ctx, config.ConnConfig, fixture.app.Name)
	if err != nil {
		t.Fatal(err)
	}
	upload.Release(ctx)
}

func TestIntegrationRetentionFailedConflictRetainsClaim(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := newPlatformFixture(t, pool, "retention-skip-claim")
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	op, err := s.Submit(ctx, platformSubmission(platform.KindRetentionPrune, "node/"+fixture.node, "retention-skip-claim", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	op, owned, err := s.claimRetention(ctx, op.ID, 1)
	if err != nil || !owned {
		t.Fatalf("claim operation: owned=%v err=%v", owned, err)
	}
	workCtx, claim, err := s.acquireRetentionTargets(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	item := retention.Item{Kind: retention.ItemImage, Reference: "sha256:abc"}
	stepID, _ := retentionRemovalPayload(item)
	exec := engine.Execution{OperationID: op.ID, Attempt: 1, Step: stepID}
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := tx.Steps().Insert(ctx, op.ID, stepID, "input", 1, exec.ExecutionID()); err != nil {
			return err
		}
		_, err := tx.Steps().Fail(ctx, op.ID, stepID, contract.Conflict, "in use")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run := &retention.Run{}
	transport := &retentionFakeTransport{failed: map[string]node.Response{
		string(op.ID) + "/" + stepID: {Status: engine.StatusFailed, Attempt: 1, ExecutionID: exec.ExecutionID(), InputsHash: "input", ErrorCode: string(contract.Conflict)},
	}}
	execution := newRetentionExecution(s, workCtx, op, 1, run, fixture.node, transport)
	execution.handleRemovalError(item, contract.NewFault(contract.Conflict, "in use", ""))
	step, found, err := s.entities.Steps().Get(ctx, op.ID, stepID)
	if err != nil || !found || step.State != "failed" {
		t.Fatalf("step: %+v found=%v err=%v", step, found, err)
	}
	if len(execution.failures) != 1 || len(execution.skippedItems) != 0 {
		t.Fatal("failed conflict was treated as a completed skip")
	}
	if err := s.settleRetentionTargets(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		got, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil || !found || !got.Sealed || got.ID != claim {
			t.Fatalf("ambiguous dispatch claim: %+v found=%v err=%v", got, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRetentionClaimTargets(t *testing.T, ctx context.Context, s *Service, op contract.Operation, claim entities.TargetClaimID, fixture *platformFixture) {
	t.Helper()
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		got, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		want := []string{"env/" + fixture.app.Name + "/" + fixture.env.Name, "node/" + fixture.node, "platform", "registry"}
		if err != nil || !found || !reflect.DeepEqual(got.Targets, want) || got.ID != claim {
			t.Fatalf("claim: %+v found=%v err=%v want targets %v", got, found, err, want)
		}
		other := entities.TargetClaimID{OperationID: "other", Token: "other-token"}
		acquired, err := acquirePlatformTargetClaimTx(ctx, tx, other, []string{want[0]})
		if err != nil || acquired {
			t.Fatalf("conflicting environment acquired=%v err=%v", acquired, err)
		}
		return tx.Steps().Insert(ctx, op.ID, "uncertain", "input", 1, "execution")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertRetentionClaimSettlement(t *testing.T, ctx context.Context, s *Service, op contract.Operation, claim entities.TargetClaimID) {
	t.Helper()
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		got, found, err := tx.UnreleasedTargetClaimByOperation(ctx, op.ID)
		if err != nil || !found || !got.Sealed || got.ID != claim {
			t.Fatalf("uncertain claim: %+v found=%v err=%v", got, found, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		completed, err := tx.Steps().Complete(ctx, op.ID, "uncertain", "execution", nil)
		if err == nil && !completed {
			t.Fatal("could not record durable completion")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.settleRetentionTargets(ctx, claim); err != nil {
		t.Fatal(err)
	}
	_, replacement, err := s.acquireRetentionTargets(ctx, op)
	if err != nil || replacement == claim {
		t.Fatalf("replacement claim=%+v err=%v", replacement, err)
	}
	if err := s.settleRetentionTargets(ctx, replacement); err != nil {
		t.Fatal(err)
	}
}
