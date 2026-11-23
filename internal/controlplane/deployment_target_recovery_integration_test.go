package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
)

func TestIntegrationSealedDeploymentCannotSetRouteIntent(t *testing.T) {
	fixture := newDeploymentRetryFixture(t)
	fixture.deploy(t)
	ctx := fixture.ctx
	op, dep, env, owner := fixture.prepareOwnedCandidate(t, "route-fence", "registry.example/live:fence", "")
	s := fixture.service
	f := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	claim := entities.TargetClaimID{OperationID: op.ID, Token: "1/route-fence"}
	if !f.acquire(s.entities, claim, op.Target) {
		t.Fatal("reserve deployment target")
	}
	workCtx := context.WithValue(ctx, buildTargetContextKey{}, buildTargetExecutor{claim: claim, target: op.Target})
	owner, err := s.setDeploymentOwnerRoute(workCtx, owner, dep, deploymentOwnerVerifying, 1)
	if err != nil {
		t.Fatalf("live owner: %v", err)
	}
	if !f.transition(s.entities, claim, "seal") {
		t.Fatal("seal deployment target")
	}
	if _, err := s.setDeploymentOwnerRoute(workCtx, owner, owner.RestoreDeploymentID, deploymentOwnerRestoring, 1); !errors.Is(err, errNotOwned) {
		t.Fatalf("sealed executor changed route intent: %v", err)
	}
	if current, found := ownerForFixture(t, fixture, env); !found || current.RouteTarget != dep || current.Phase != deploymentOwnerVerifying {
		t.Fatalf("stored route intent changed: %+v", current)
	}
}

func TestIntegrationSettledDeploymentClaimReleasedByReconcile(t *testing.T) {
	fixture := newDeploymentRetryFixture(t)
	s := fixture.service
	ctx := fixture.ctx
	op, _, _, _ := fixture.prepareOwnedCandidate(t, "settled", "registry.example/live:settled", "")
	f := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	claim := entities.TargetClaimID{OperationID: op.ID, Token: "1/settled"}
	if !f.acquire(s.entities, claim, op.Target) {
		t.Fatal("reserve completed operation target")
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE eregion_operations SET state=$2 WHERE id=$1`, op.ID, contract.Succeeded); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileSettledTargets(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.inspect(claim.Token); got.Released {
		t.Fatal("released before the River job ended")
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE river_job SET state='completed',finalized_at=clock_timestamp()
		WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileSettledTargets(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.inspect(claim.Token); !got.Released {
		t.Fatal("reconciler left the completed operation's target reserved")
	}
}

func TestIntegrationDeploymentRecoveryReservation(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	f := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	old := claimID("deployment-operation", "1/original")
	target := "env/app/live"
	if !f.acquire(s.entities, old, target) {
		t.Fatal("could not reserve old executor")
	}
	if _, _, acquired, err := s.acquireDeploymentRecoveryTarget(ctx, "app", "live"); err != nil || acquired {
		t.Fatalf("recovery raced active executor: acquired=%v err=%v", acquired, err)
	}
	if !f.transition(s.entities, old, "seal") {
		t.Fatal("could not seal old executor")
	}
	if _, _, acquired, err := s.acquireDeploymentRecoveryTarget(ctx, "app", "live"); err != nil || acquired {
		t.Fatalf("recovery raced uncertain sealed executor: acquired=%v err=%v", acquired, err)
	}
	if !f.transition(s.entities, old, "release") {
		t.Fatal("could not release old executor")
	}
	_, claim, acquired, err := s.acquireDeploymentRecoveryTarget(ctx, "app", "live")
	if err != nil || !acquired {
		t.Fatalf("recovery reservation: acquired=%v err=%v", acquired, err)
	}
	if f.acquire(s.entities, claimID("other", "other"), target) {
		t.Fatal("executor entered during recovery")
	}
	wrong := entities.TargetClaimID{OperationID: claim.OperationID, Token: "wrong-generation"}
	if err := s.settleDeploymentRecoveryTarget(ctx, wrong, true); err != nil {
		t.Fatal(err)
	}
	if f.acquire(s.entities, claimID("other", "other"), target) {
		t.Fatal("wrong generation released reservation")
	}
	if err := s.settleDeploymentRecoveryTarget(ctx, claim, false); err != nil {
		t.Fatal(err)
	}
	got, found := f.inspect(claim.Token)
	if !found || !got.Sealed || got.Released {
		t.Fatalf("uncertain recovery claim: %+v found=%v", got, found)
	}
	if f.acquire(s.entities, claimID("other", "other"), target) {
		t.Fatal("uncertain recovery released reservation")
	}
}

func TestIntegrationDeploymentRecoveryReservationRelease(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	_, claim, acquired, err := s.acquireDeploymentRecoveryTarget(ctx, "app", "live")
	if err != nil || !acquired {
		t.Fatalf("recovery reservation: acquired=%v err=%v", acquired, err)
	}
	if err := s.settleDeploymentRecoveryTarget(ctx, claim, true); err != nil {
		t.Fatal(err)
	}
	f := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	got, found := f.inspect(claim.Token)
	if !found || !got.Sealed || !got.Released {
		t.Fatalf("settled recovery claim: %+v found=%v", got, found)
	}
	if !f.acquire(s.entities, claimID("next", "next"), "env/app/live") {
		t.Fatal("successful recovery did not release environment")
	}
}

func TestIntegrationDeploymentOwnerRecoveryDefersForExecutor(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := newPlatformFixture(t, pool, "owner-recovery-claim")
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployment_owners
		(environment_id, phase, created_at, updated_at)
		VALUES ($1, 'ambiguous', clock_timestamp(), clock_timestamp())`, fixture.env.ID); err != nil {
		t.Fatal(err)
	}
	f := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	old := claimID("old-deployment", "old-execution")
	if !f.acquire(s.entities, old, "env/"+fixture.app.Name+"/"+fixture.env.Name) {
		t.Fatal("could not acquire executor claim")
	}
	for _, sealed := range []bool{false, true} {
		if sealed && !f.transition(s.entities, old, "seal") {
			t.Fatal("could not seal executor")
		}
		if err := s.reconcileDeploymentOwners(ctx); err != nil {
			t.Fatal(err)
		}
		var phase string
		if err := pool.QueryRow(ctx, `SELECT phase FROM eregion_platform_deployment_owners WHERE environment_id=$1`, fixture.env.ID).Scan(&phase); err != nil || phase != "ambiguous" {
			t.Fatalf("owner changed with sealed=%v: phase=%q err=%v", sealed, phase, err)
		}
		got, found := f.inspect(old.Token)
		if !found || got.Released || got.Sealed != sealed {
			t.Fatalf("executor changed by recovery: %+v found=%v", got, found)
		}
	}
}
