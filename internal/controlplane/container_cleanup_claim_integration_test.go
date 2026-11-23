package controlplane

import (
	"errors"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/entities"
)

func TestIntegrationCleanupClaimSettlement(t *testing.T) {
	pool, _ := testPool(t, true)
	s := &Service{entities: newTestEntities(t, pool)}
	ctx := t.Context()
	row := containerCleanupRow{App: "app", Environment: "prod", ContainerName: "leak", CreatedAt: time.Now()}
	release, err := s.lockTargets(ctx, []string{"env/app/prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireCleanupClaim(ctx, row); !errors.Is(err, errCleanupDeferred) {
		release()
		t.Fatalf("legacy target lock did not gate acquisition: %v", err)
	}
	release()
	claim, err := s.acquireCleanupClaim(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireCleanupClaim(ctx, row); !errors.Is(err, errCleanupDeferred) {
		t.Fatalf("concurrent acquisition: %v", err)
	}
	s.settleCleanupClaim(ctx, claim, true)
	next, err := s.acquireCleanupClaim(ctx, row)
	if err != nil {
		t.Fatalf("acquisition after settled cleanup: %v", err)
	}
	s.settleCleanupClaim(ctx, next, false)
	if _, err := s.acquireCleanupClaim(ctx, row); !errors.Is(err, errCleanupDeferred) {
		t.Fatalf("uncertain cleanup did not retain claim: %v", err)
	}
	var stored entities.TargetClaim
	if err := s.entities.WithinReadTx(ctx, func(tx *entities.StoreTx) error {
		var found bool
		var err error
		stored, found, err = tx.TargetClaim(ctx, next.Token)
		if err == nil && !found {
			t.Fatal("uncertain claim disappeared")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !stored.Sealed || stored.Released {
		t.Fatalf("uncertain claim = %+v", stored)
	}
}
