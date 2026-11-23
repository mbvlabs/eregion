package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

// Exercise the real worker handoff and PostgreSQL claim fence without
// dispatching a simulated node or provider.
func TestIntegrationDeploymentVerifiedHandoff(t *testing.T) {
	for _, tc := range []deploymentHandoffCase{
		{"completed", "completed", "verifying", "", true, true},
		{"no-evidence", "", "verifying", "", true, false},
		{"lost-reply", "running", "verifying", "", true, false},
		{"provider-error", "failed", "verifying", "", true, false},
		{"no-verification", "completed", "verifying", "", false, false},
		{"restore-may-be-running", "completed", "restoring", "", true, false},
		{"serving-reassertion-missing", "completed", "serving", "", true, false},
		{"serving-reassertion-completed", "completed", "serving", "completed", true, true},
		{"serving-reassertion-running", "completed", "serving", "running", true, false},
		{"serving-reassertion-failed", "completed", "serving", "failed", true, false},
		{"serving-reassertion-wrong-input", "completed", "serving", "wrong-input", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkDeploymentHandoff(t, tc)
		})
	}
}

func TestIntegrationDeploymentPreRecordHandoffRetry(t *testing.T) {
	for name, failLookup := range map[string]bool{"insert": false, "lookup": true} {
		t.Run(name, func(t *testing.T) {
			fixture, _, env := newDeployedRecordingFixture(t)
			previous := previousDeploymentFor(t, fixture, env)
			instances, op := seedPreRecordFailure(t, fixture, previous, failLookup)
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err == nil {
				t.Fatal("deployment succeeded without durable previous cleanup")
			}
			repairPreRecordFailure(t, fixture, previous, instances, failLookup)
			var snooze *river.JobSnoozeError
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); !errors.As(err, &snooze) {
				t.Fatalf("verified handoff should snooze: %v", err)
			}
			if err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1); err != nil {
				t.Fatalf("retry after verified handoff: %v", err)
			}
			assertOperationState(t, fixture, op.ID, contract.Succeeded, "pre-record retry")
			assertOwnerReleased(t, fixture, env, "pre-record retry")
		})
	}
}

type deploymentHandoffCase struct {
	name, route, phase, reassert string
	public                       bool
	released                     bool
}

func checkDeploymentHandoff(t *testing.T, tc deploymentHandoffCase) {
	t.Helper()
	s, id, target := seedDeploymentHandoff(t, tc)
	ctx := t.Context()
	claims := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	claim := entities.TargetClaimID{OperationID: id, Token: "1/original"}
	if !claims.acquire(s.entities, claim, target) {
		t.Fatal("could not reserve target")
	}
	err := runPlatformAttempt(ctx, s, id, 2)
	var snooze *river.JobSnoozeError
	if tc.released && !errors.As(err, &snooze) {
		t.Fatalf("verified handoff: %v", err)
	}
	if !tc.released && (err == nil || errors.As(err, &snooze)) {
		t.Fatalf("ambiguous handoff: %v", err)
	}
	got, found := claims.inspect(claim.Token)
	if !found || !got.Sealed || got.Released != tc.released {
		t.Fatalf("claim after retry: %+v found=%v", got, found)
	}
	contender := entities.TargetClaimID{OperationID: contract.OperationID("contender"), Token: "next"}
	if acquired := claims.acquire(s.entities, contender, target); acquired != tc.released {
		t.Fatalf("contender acquired=%v, want %v", acquired, tc.released)
	}
}

func seedDeploymentHandoff(t *testing.T, tc deploymentHandoffCase) (*Service, contract.OperationID, string) {
	return seedDeploymentHandoffWithLocal(t, tc, false)
}

func seedDeploymentHandoffWithLocal(t *testing.T, tc deploymentHandoffCase, local bool) (*Service, contract.OperationID, string) {
	t.Helper()
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "handoff")
	if local {
		if _, err := AssignWorkspace(t.Context(), newTestEntities(t, pool), "test", testRegistryHostIdentityFor("local", t.TempDir())); err != nil {
			t.Fatal(err)
		}
	}
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	release := seedArtifactRelease(t, pool, s, f.app.Name, f.env.Name, "registry.example/handoff:one")
	target := "env/" + f.app.Name + "/" + f.env.Name
	id := mustNewOperationID(t)
	dep := platform.DeploymentID("dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
				(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
				VALUES($1,'platform.deploy',$2,'operator','tester','handoff','hash',jsonb_build_object('release_id',$3::text),'running',1,0,0,clock_timestamp(),clock_timestamp(),'')`, id, target, release.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployments
				(id,operation_id,app_id,environment_id,action,release_id,revision_id,state,error_code,message,created_at,updated_at,secret_revisions,instances)
				VALUES($1,$2,$3,$4,'deploy',$5,$6,'running','','',clock_timestamp(),clock_timestamp(),'{}','[]')`,
		dep, id, f.app.ID, f.env.ID, release.ID, f.revision.ID); err != nil {
		t.Fatal(err)
	}
	routeTarget := dep
	if tc.phase == "prepared" {
		routeTarget = ""
	}
	insertDeploymentOwner(t, pool, f.env, deploymentOwner{
		EnvironmentID: f.env.ID, OperationID: id, DeploymentID: dep,
		Attempt: 1, Phase: deploymentOwnerPhase(tc.phase), RouteTarget: routeTarget,
	})
	if tc.route != "" {
		errorText := ""
		if tc.route == "failed" {
			errorText = "provider error"
		}
		insertRouteStep(t, pool, id, tc.route, errorText)
	}
	if tc.public {
		insertCompletedStep(t, pool, id, deploymentPublicStep, strings.Repeat("b", 64))
	}
	if tc.phase == "serving" {
		if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environments SET serving_release_id=$2, serving_deployment_id=$3 WHERE id=$1`, f.env.ID, release.ID, dep); err != nil {
			t.Fatal(err)
		}
	}
	seedServingReassertion(t, pool, id, dep, tc.reassert)
	// These are control-plane steps, not node execution identities.
	if _, err := pool.Exec(ctx, `UPDATE eregion_operation_steps SET execution_id='' WHERE operation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	return s, id, target
}

func seedServingReassertion(t *testing.T, pool *pgxpool.Pool, id contract.OperationID, dep platform.DeploymentID, state string) {
	t.Helper()
	if state == "" {
		return
	}
	raw, err := json.Marshal(map[string]any{"deployment_id": dep})
	if err != nil {
		t.Fatal(err)
	}
	hash := engine.InputsHash(deploymentServingRouteStep, raw)
	if state == "wrong-input" {
		hash = strings.Repeat("c", 64)
	}
	insertCompletedStep(t, pool, id, deploymentServingRouteStep, hash)
	if state == "running" || state == "failed" {
		if _, err := pool.Exec(t.Context(), `UPDATE eregion_operation_steps SET state=$3 WHERE operation_id=$1 AND step=$2`, id, deploymentServingRouteStep, state); err != nil {
			t.Fatal(err)
		}
	}
}
