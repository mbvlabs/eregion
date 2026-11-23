package controlplane

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
)

type claimFixture struct {
	t    *testing.T
	ctx  context.Context
	a, b *entities.Store
}

func claimID(op, token string) entities.TargetClaimID {
	return entities.TargetClaimID{OperationID: contract.OperationID(op), Token: token}
}

func (f claimFixture) acquire(s *entities.Store, owner entities.TargetClaimID, targets ...string) bool {
	f.t.Helper()
	var ok bool
	err := s.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		ok, err = tx.AcquireTargetClaim(f.ctx, owner, targets)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f claimFixture) transition(s *entities.Store, owner entities.TargetClaimID, action string) bool {
	f.t.Helper()
	var ok bool
	err := s.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		switch action {
		case "lock":
			ok, err = tx.LockActiveTargetClaim(f.ctx, owner)
		case "seal":
			ok, err = tx.SealTargetClaim(f.ctx, owner)
		case "release":
			ok, err = tx.ReleaseTargetClaim(f.ctx, owner)
		default:
			f.t.Fatalf("unknown action %q", action)
		}
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f claimFixture) inspect(token string) (entities.TargetClaim, bool) {
	f.t.Helper()
	var claim entities.TargetClaim
	var found bool
	err := f.b.WithinReadTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		claim, found, err = tx.TargetClaim(f.ctx, token)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return claim, found
}

func (f claimFixture) byOperation(op contract.OperationID) (entities.TargetClaim, bool) {
	f.t.Helper()
	var claim entities.TargetClaim
	var found bool
	err := f.b.WithinReadTx(f.ctx, func(tx *entities.StoreTx) error {
		var err error
		claim, found, err = tx.UnreleasedTargetClaimByOperation(f.ctx, op)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return claim, found
}

// This runs against independent transaction-pooled clients using the restricted
// runtime role, after migration by the separate session-pooled role.
func checkTargetClaims(t *testing.T, ctx context.Context, pool, other *pgxpool.Pool, reassign func()) {
	t.Helper()
	a, err := entities.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	b, err := entities.NewStore(other)
	if err != nil {
		t.Fatal(err)
	}
	f := claimFixture{t: t, ctx: ctx, a: a, b: b}
	f.checkRollbackAndValidation()
	f.checkLifecycle(reassign)
	f.checkTargetGuard()
	f.checkConcurrentClaims(false)
	f.checkConcurrentClaims(true)
}

func (f claimFixture) checkTargetGuard() {
	f.t.Helper()
	owner := claimID("guard-operation", "guard-token")
	if !f.acquire(f.a, owner, "build/old") {
		f.t.Fatal("guard acquisition failed")
	}
	check := func(id entities.TargetClaimID, target string, want bool) {
		f.t.Helper()
		err := f.b.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
			got, err := tx.LockActiveTargetClaimForTarget(f.ctx, id, target)
			if err == nil && got != want {
				f.t.Errorf("guard %v %q = %v, want %v", id, target, got, want)
			}
			return err
		})
		if err != nil {
			f.t.Fatal(err)
		}
	}
	check(owner, "build/old", true)
	check(owner, "build/new", false)
	check(claimID("guard-operation", "wrong-token"), "build/old", false)
	check(claimID("wrong-operation", "guard-token"), "build/old", false)
	if !f.transition(f.a, owner, "seal") {
		f.t.Fatal("guard seal failed")
	}
	check(owner, "build/old", false)
	if !f.transition(f.a, owner, "release") {
		f.t.Fatal("guard release failed")
	}
	check(owner, "build/old", false)
}

func (f claimFixture) checkRollbackAndValidation() {
	f.t.Helper()
	rollback := errors.New("rollback claim")
	err := f.a.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		ok, err := tx.AcquireTargetClaim(f.ctx, claimID("rollback-op", "rollback-token"), []string{"rollback-target"})
		if err != nil || !ok {
			f.t.Fatalf("rollback setup: %v %v", ok, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		f.t.Fatalf("rollback: %v", err)
	}
	if _, found := f.inspect("rollback-token"); found || !f.acquire(f.b, claimID("rollback-op", "rollback-token"), "rollback-target") {
		f.t.Fatal("rolled back acquisition persisted")
	}
	err = f.a.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
		for _, targets := range [][]string{nil, {"x", "x"}, {" ", "x"}} {
			if ok, err := tx.AcquireTargetClaim(f.ctx, claimID("invalid", "invalid"), targets); err == nil || ok {
				f.t.Fatalf("invalid targets %q: %v %v", targets, ok, err)
			}
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f claimFixture) checkLifecycle(reassign func()) {
	f.t.Helper()
	owner := claimID("op-1", "claim-1")
	if !f.acquire(f.a, owner, "b", "a") || !f.transition(f.a, owner, "lock") {
		f.t.Fatal("initial claim or active lock failed")
	}
	if f.acquire(f.b, claimID("op-2", "claim-1"), "other") || f.transition(f.b, claimID("op-2", "claim-1"), "lock") || f.transition(f.b, claimID("op-2", "claim-1"), "seal") || f.transition(f.b, owner, "release") {
		f.t.Fatal("foreign token use or unsealed release succeeded")
	}
	if f.acquire(f.b, claimID("op-1", "other-generation"), "free") {
		f.t.Fatal("operation acquired a second unreleased claim")
	}
	if f.acquire(f.b, claimID("op-2", "claim-2"), "0-c", "b") || !f.acquire(f.b, claimID("op-2", "claim-2"), "0-c") {
		f.t.Fatal("overlap succeeded or conflict left partial claim")
	}
	reassign()
	claim, found := f.inspect(owner.Token)
	byOperation, foundOperation := f.byOperation(owner.OperationID)
	if !found || !foundOperation || !reflect.DeepEqual(claim, byOperation) || claim.ID != owner || claim.Sealed || claim.Released || !reflect.DeepEqual(claim.Targets, []string{"a", "b"}) {
		f.t.Fatalf("claim after backend reassignment: %+v found=%v by operation=%+v found=%v", claim, found, byOperation, foundOperation)
	}
	if f.acquire(f.b, claimID("op-3", "claim-3"), "a") {
		f.t.Fatal("backend reassignment lost claim")
	}
	f.checkSealWaits(owner)
	f.checkRelease(owner)
}

func (f claimFixture) checkRelease(owner entities.TargetClaimID) {
	f.t.Helper()
	if f.transition(f.a, owner, "seal") || f.transition(f.b, owner, "lock") {
		f.t.Fatal("seal did not fence guarded dispatch")
	}
	claim, found := f.byOperation(owner.OperationID)
	if !found || claim.ID != owner || !claim.Sealed || f.acquire(f.b, claimID("op-1", "other-generation"), "free") {
		f.t.Fatal("sealed claim not discoverable or operation could reacquire")
	}
	if f.acquire(f.b, claimID("op-3", "claim-3"), "a") || f.transition(f.b, claimID("op-2", "claim-1"), "release") {
		f.t.Fatal("sealed claim lost ownership or foreign release succeeded")
	}
	// Production must prove quiescence between these committed transactions.
	if !f.transition(f.a, owner, "release") || !f.acquire(f.b, claimID("op-3", "claim-3"), "a", "b") {
		f.t.Fatal("release or replacement acquisition failed")
	}
	if _, found := f.byOperation(owner.OperationID); found {
		f.t.Fatal("released claim returned by operation lookup")
	}
	if f.transition(f.a, owner, "release") || f.acquire(f.a, owner, "free") || f.acquire(f.a, claimID("op-4", "claim-4"), "a") {
		f.t.Fatal("stale generation bypassed the replacement")
	}
	claim, found = f.inspect(owner.Token)
	if !found || !claim.Sealed || !claim.Released || len(claim.Targets) != 0 {
		f.t.Fatalf("released tombstone: %+v found=%v", claim, found)
	}
	if !f.acquire(f.a, claimID("op-1", "other-generation"), "free") {
		f.t.Fatal("released operation could not acquire a new generation")
	}
}

func (f claimFixture) checkSealWaits(owner entities.TargetClaimID) {
	f.t.Helper()
	locked, unlock := make(chan struct{}), make(chan struct{})
	lockResult := make(chan error, 1)
	go func() {
		lockResult <- f.a.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
			ok, err := tx.LockActiveTargetClaimForTarget(f.ctx, owner, "a")
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("active claim missing")
			}
			close(locked)
			<-unlock
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-lockResult:
		f.t.Fatalf("lock setup: %v", err)
	case <-f.ctx.Done():
		f.t.Fatal(f.ctx.Err())
	}
	sealResult := make(chan error, 1)
	go func() {
		sealResult <- f.b.WithinTx(f.ctx, func(tx *entities.StoreTx) error {
			ok, err := tx.SealTargetClaim(f.ctx, owner)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("seal failed")
			}
			if ok, err := tx.ReleaseTargetClaim(f.ctx, owner); err == nil || ok {
				return errors.New("release before seal commit succeeded")
			}
			return nil
		})
	}()
	select {
	case err := <-sealResult:
		close(unlock)
		f.t.Fatalf("seal did not wait for active lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(unlock)
	if err := <-lockResult; err != nil {
		f.t.Fatal(err)
	}
	if err := <-sealResult; err != nil {
		f.t.Fatal(err)
	}
}

func (f claimFixture) checkConcurrentClaims(sameOperation bool) {
	f.t.Helper()
	const count = 16
	var wg sync.WaitGroup
	results := make(chan bool, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store, targets := f.a, []string{"race-a", "race-b"}
			op, token := fmt.Sprintf("race-op-%d", i), fmt.Sprintf("race-token-%d", i)
			if sameOperation {
				op, token = "shared-race-op", fmt.Sprintf("operation-race-token-%d", i)
				targets = []string{fmt.Sprintf("operation-race-target-%d", i)}
			} else if i%2 != 0 {
				store, targets = f.b, []string{"race-b", "race-a"}
			}
			if sameOperation && i%2 != 0 {
				store = f.b
			}
			results <- f.acquire(store, claimID(op, token), targets...)
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		f.t.Fatalf("concurrent winners = %d, want 1 (same operation=%v, context=%v)", winners, sameOperation, f.ctx.Err())
	}
}
