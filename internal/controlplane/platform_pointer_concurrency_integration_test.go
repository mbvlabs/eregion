package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// All tests in this file use testPool's isolated database. They require a
// separately authorized EREGION_TEST_DATABASE_URL and must not run on a
// shared or production database.
type pointerBuildFixture struct {
	service *Service
	pool    *pgxpool.Pool
	op      contract.Operation
	env     platform.Environment
	build   platform.BuildID
	release platform.ReleaseID
	dep     platform.DeploymentID
}

func newPointerBuildFixture(t *testing.T) pointerBuildFixture {
	t.Helper()
	s, pool := gitDeployFixture(t)
	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) {
		return submittedGitCommit, nil
	}
	op := platformSubmit(t, s, platform.KindDeploy, "env/shop/production", "pointer-race-deploy", map[string]any{})
	ctx := context.Background()
	env, err := s.environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	var build platform.BuildID
	var release platform.ReleaseID
	var dep platform.DeploymentID
	if err := pool.QueryRow(ctx, `SELECT b.id,r.id,d.id FROM eregion_platform_builds b
		JOIN eregion_platform_releases r ON r.id=b.release_id AND r.build_id=b.id
		JOIN eregion_platform_deployments d ON d.operation_id=b.operation_id AND d.release_id=r.id
		WHERE b.operation_id=$1`, op.ID).Scan(&build, &release, &dep); err != nil {
		t.Fatal(err)
	}
	return pointerBuildFixture{service: s, pool: pool, op: op, env: env, build: build, release: release, dep: dep}
}

// A waiter is observed in PostgreSQL, rather than inferred from goroutine
// scheduling. Every wait is bounded by the caller's context.
func waitPointerLockWaiter(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queryFragment string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			  AND position($1 in query)>0 AND pid<>pg_backend_pid())`, queryFragment).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no lock waiter for %q: %v", queryFragment, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertNoDeadlock(t *testing.T, err error) {
	t.Helper()
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "40P01" {
		t.Fatalf("PostgreSQL deadlock: %v", err)
	}
}

func TestIntegrationPointerBuildCompletionVersusRevisionPublication(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// The publisher holds the parent before build completion reaches it. The
	// real publisher must still be able to inspect the existing build pointer.
	if _, err := lockPointerEnvironmentTx(ctx, tx, f.env.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.service.markBuildSucceeded(ctx, f.build, f.release, "registry.example/shop@sha256:"+strings.Repeat("a", 64), submittedGitCommit, "sha256:"+strings.Repeat("b", 64))
	}()
	waitPointerLockWaiter(t, ctx, f.pool, "current_revision_id,desired_release_id,applied_release_id")
	revision, err := publishEnvironmentRevisionTx(ctx, tx, f.env, "", testActor, "environment.updated", "pointer concurrency test", f.env.Revision.RevisionState, nil)
	assertNoDeadlock(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	assertNoDeadlock(t, err)
	if err != nil {
		t.Fatal(err)
	}
	var buildState, releaseState, operationState, current string
	if err := f.pool.QueryRow(ctx, `SELECT b.state,r.status,o.state,e.current_revision_id
		FROM eregion_platform_builds b JOIN eregion_platform_releases r ON r.id=b.release_id
		JOIN eregion_operations o ON o.id=b.operation_id JOIN eregion_platform_environments e ON e.id=b.environment_id
		WHERE b.id=$1`, f.build).Scan(&buildState, &releaseState, &operationState, &current); err != nil {
		t.Fatal(err)
	}
	if buildState != "succeeded" || releaseState != "ready" || current != string(revision) || operationState != "queued" {
		t.Fatalf("build=%s release=%s revision=%s operation=%s", buildState, releaseState, current, operationState)
	}
}

func TestIntegrationPointerDiagnosticVersusTerminalFailure(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := f.service.markBuildSucceeded(ctx, f.build, f.release, "registry.example/shop@sha256:"+strings.Repeat("a", 64), submittedGitCommit, "sha256:"+strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1); err != nil || !ok {
		t.Fatalf("claim=%t err=%v", ok, err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	pin, err := resolveDiagnosticExecutionPlacement(ctx, tx, f.op.ID, "local", true)
	if err != nil {
		t.Fatal(err)
	}
	if pin.DeploymentID != f.dep || pin.ReleaseID != f.release || pin.RevisionID != f.env.RevisionID {
		t.Fatalf("unexpected historical pin: %+v", pin)
	}
	done := make(chan error, 1)
	go func() { done <- f.service.failPlatformOperation(ctx, f.op.ID, 1, contract.Internal, "known failure") }()
	waitPointerLockWaiter(t, ctx, f.pool, "current_revision_id,desired_release_id,applied_release_id")
	// The diagnostic's exact child SHARE locks still protect the pinned rows.
	var release platform.ReleaseID
	var revision platform.RevisionID
	if err := tx.QueryRow(ctx, `SELECT release_id,revision_id FROM eregion_platform_deployments WHERE id=$1`, f.dep).Scan(&release, &revision); err != nil || release != pin.ReleaseID || revision != pin.RevisionID {
		t.Fatalf("pin changed: release=%s revision=%s err=%v", release, revision, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	assertNoDeadlock(t, err)
	if err != nil {
		t.Fatal(err)
	}
	var operationState, deploymentState string
	if err := f.pool.QueryRow(ctx, `SELECT o.state,d.state FROM eregion_operations o JOIN eregion_platform_deployments d ON d.operation_id=o.id WHERE o.id=$1`, f.op.ID).Scan(&operationState, &deploymentState); err != nil {
		t.Fatal(err)
	}
	if operationState != "failed" || deploymentState != "failed" {
		t.Fatalf("operation=%s deployment=%s", operationState, deploymentState)
	}
}

func TestIntegrationPointerDiagnosticVersusEnvironmentDeletion(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := f.service.markBuildSucceeded(ctx, f.build, f.release, "registry.example/shop@sha256:"+strings.Repeat("a", 64), submittedGitCommit, "sha256:"+strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Rollback(context.Background())
	pin, err := resolveDiagnosticExecutionPlacement(ctx, diagnostic, f.op.ID, "local", true)
	if err != nil || pin.DeploymentID != f.dep || pin.ReleaseID != f.release || pin.RevisionID != f.env.RevisionID {
		t.Fatalf("historical pin=%+v err=%v", pin, err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer tx.Rollback(context.Background())
		if _, err = appByNameForUpdateTx(ctx, tx, f.env.App); err == nil {
			err = deleteEnvironmentRowsTx(ctx, tx, f.env)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		done <- err
	}()
	waitPointerLockWaiter(t, ctx, f.pool, "FROM eregion_platform_apps WHERE name=$1 FOR UPDATE")
	if err := diagnostic.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		assertNoDeadlock(t, err)
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environments WHERE id=$1`, f.env.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("environment count=%d err=%v", count, err)
	}
}

func TestIntegrationPointerTerminalOutcomeVersusRevisionPublication(t *testing.T) {
	for _, tc := range []struct {
		name, operationState, buildState, deploymentState string
		cancel                                            bool
	}{
		{name: "failure", operationState: "failed", buildState: "failed", deploymentState: "failed"},
		{name: "cancellation", operationState: "cancelled", buildState: "cancelled", deploymentState: "cancelled", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPointerBuildFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, _, ok, err := f.service.claimPlatform(ctx, f.op.ID, 1); err != nil || !ok {
				t.Fatalf("claim=%t err=%v", ok, err)
			}
			if tc.cancel {
				if _, err := f.service.RequestCancel(ctx, testActor, f.op.ID); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := lockPointerEnvironmentTx(ctx, tx, f.env.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if tc.cancel {
					done <- f.service.cancelPlatform(ctx, f.op.ID, 1, "cancelled at boundary")
				} else {
					done <- f.service.failPlatformOperation(ctx, f.op.ID, 1, contract.Internal, "known failure")
				}
			}()
			waitPointerLockWaiter(t, ctx, f.pool, "current_revision_id,desired_release_id,applied_release_id")
			if _, err := publishEnvironmentRevisionTx(ctx, tx, f.env, "", testActor, "environment.updated", "pointer concurrency test", f.env.Revision.RevisionState, nil); err != nil {
				assertNoDeadlock(t, err)
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				assertNoDeadlock(t, err)
				t.Fatal(err)
			}
			var opState, buildState, depState, releaseState string
			if err := f.pool.QueryRow(ctx, `SELECT o.state,b.state,d.state,r.status
				FROM eregion_operations o JOIN eregion_platform_builds b ON b.operation_id=o.id
				JOIN eregion_platform_deployments d ON d.operation_id=o.id
				JOIN eregion_platform_releases r ON r.id=b.release_id WHERE o.id=$1`, f.op.ID).
				Scan(&opState, &buildState, &depState, &releaseState); err != nil {
				t.Fatal(err)
			}
			if opState != tc.operationState || buildState != tc.buildState || depState != tc.deploymentState || releaseState != "failed" {
				t.Fatalf("operation=%s build=%s deployment=%s release=%s", opState, buildState, depState, releaseState)
			}
			if err := f.service.failPlatformOperation(ctx, f.op.ID, 2, contract.Internal, "stale"); !errors.Is(err, errNotOwned) {
				t.Fatalf("stale attempt changed terminal operation: %v", err)
			}
		})
	}
}

func TestIntegrationPointerDeletionWinsAgainstBuildCompletion(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := appByNameForUpdateTx(ctx, tx, f.env.App); err != nil {
		t.Fatal(err)
	}
	if err := deleteEnvironmentRowsTx(ctx, tx, f.env); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.service.markBuildSucceeded(ctx, f.build, f.release, "registry.example/shop@sha256:"+strings.Repeat("a", 64), submittedGitCommit, "sha256:"+strings.Repeat("b", 64))
	}()
	waitPointerLockWaiter(t, ctx, f.pool, "SELECT id FROM eregion_platform_apps WHERE id=$1 FOR KEY SHARE")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	assertNoDeadlock(t, err)
	if err == nil {
		t.Fatal("build completed after its environment was deleted")
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environments WHERE id=$1`, f.env.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("environment count=%d err=%v", count, err)
	}
}

func TestIntegrationPointerBuildCompletionWinsAgainstDeletion(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var locked string
	if err := blocker.QueryRow(ctx, `SELECT id FROM eregion_platform_builds WHERE id=$1 FOR UPDATE`, f.build).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	built := make(chan error, 1)
	go func() {
		built <- f.service.markBuildSucceeded(ctx, f.build, f.release, "registry.example/shop@sha256:"+strings.Repeat("a", 64), submittedGitCommit, "sha256:"+strings.Repeat("b", 64))
	}()
	// Completion owns the parent gates and waits on this child barrier.
	waitPointerLockWaiter(t, ctx, f.pool, "FROM eregion_platform_builds WHERE id=$1 AND app_id=$2")
	deleted := make(chan error, 1)
	go func() {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			deleted <- err
			return
		}
		defer tx.Rollback(context.Background())
		if _, err = appByNameForUpdateTx(ctx, tx, f.env.App); err == nil {
			err = deleteEnvironmentRowsTx(ctx, tx, f.env)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		deleted <- err
	}()
	waitPointerLockWaiter(t, ctx, f.pool, "FROM eregion_platform_apps WHERE name=$1 FOR UPDATE")
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-built; err != nil {
		assertNoDeadlock(t, err)
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		assertNoDeadlock(t, err)
		t.Fatal(err)
	}
	var environments, builds, releases int
	if err := f.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM eregion_platform_environments WHERE id=$1),
		(SELECT count(*) FROM eregion_platform_builds WHERE id=$2),
		(SELECT count(*) FROM eregion_platform_releases WHERE id=$3)`,
		f.env.ID, f.build, f.release).Scan(&environments, &builds, &releases); err != nil || environments != 0 || builds != 0 || releases != 0 {
		t.Fatalf("remaining env=%d build=%d release=%d err=%v", environments, builds, releases, err)
	}
}

func TestIntegrationPointerRetentionAndAdmissionOrdering(t *testing.T) {
	t.Run("prune first refuses deployment", func(t *testing.T) {
		f := newDeploymentRetryFixture(t)
		release := f.createRelease("prune-first", "registry.example/live:prune")
		pruned, err := f.service.markRetentionRelease(f.ctx, string(release))
		if err != nil || !pruned {
			t.Fatalf("prune=%t err=%v", pruned, err)
		}
		var jobsBefore int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM river_job`).Scan(&jobsBefore); err != nil {
			t.Fatal(err)
		}
		_, err = f.service.Submit(f.ctx, platformSubmission(platform.KindDeploy, "env/live/production", "pruned-deploy", map[string]any{"release_id": release}))
		assertNoDeadlock(t, err)
		fault := contract.Classify(err)
		if fault.Code != contract.PreconditionFailed || !strings.Contains(fault.Message, "release "+string(release)+" for live/production was pruned at ") {
			t.Fatalf("expected release-specific pruned refusal, got %v", err)
		}
		var operations, deployments, jobs int
		if err := f.pool.QueryRow(f.ctx, `SELECT
			(SELECT count(*) FROM eregion_operations WHERE kind=$1 AND actor_kind=$2 AND actor_subject=$3 AND target=$4 AND idempotency_key=$5),
			(SELECT count(*) FROM eregion_platform_deployments d JOIN eregion_operations o ON o.id=d.operation_id
			 WHERE o.kind=$1 AND o.actor_kind=$2 AND o.actor_subject=$3 AND o.target=$4 AND o.idempotency_key=$5),
			(SELECT count(*) FROM river_job)`,
			platform.KindDeploy, testActor.Kind, testActor.Subject, "env/live/production", "pruned-deploy").Scan(&operations, &deployments, &jobs); err != nil || operations != 0 || deployments != 0 || jobs != jobsBefore {
			t.Fatalf("rejected submission operations=%d deployments=%d jobs before=%d after=%d err=%v", operations, deployments, jobsBefore, jobs, err)
		}
	})
	t.Run("admission first protects release", func(t *testing.T) {
		f := newDeploymentRetryFixture(t)
		release := f.createRelease("admit-first", "registry.example/live:admit")
		op := platformSubmit(t, f.service, platform.KindDeploy, "env/live/production", "protected-deploy", map[string]any{"release_id": release})
		pruned, err := f.service.markRetentionRelease(f.ctx, string(release))
		if err != nil || pruned {
			t.Fatalf("prune=%t err=%v", pruned, err)
		}
		var state string
		var prunedAt *time.Time
		if err := f.pool.QueryRow(f.ctx, `SELECT o.state,r.pruned_at FROM eregion_operations o JOIN eregion_platform_releases r ON r.id=$2 WHERE o.id=$1`, op.ID, release).Scan(&state, &prunedAt); err != nil || state != "queued" || prunedAt != nil {
			t.Fatalf("operation=%s pruned=%v err=%v", state, prunedAt, err)
		}
	})
}

func TestIntegrationPointerHistoryParentGateVersusApplicationDeletion(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	history, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Rollback(context.Background())
	var eventsBefore int
	if err := history.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1`, f.op.ID).Scan(&eventsBefore); err != nil || eventsBefore == 0 {
		t.Fatalf("operation events before deletion=%d err=%v", eventsBefore, err)
	}
	if err := insertPlatformChange(ctx, history, &f.op, "test.history", f.env.App, f.env.Name, "history parent gate", nil, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer tx.Rollback(context.Background())
		app, err := appByNameForUpdateTx(ctx, tx, f.env.App)
		if err == nil {
			err = deleteEnvironmentRowsTx(ctx, tx, f.env)
		}
		if err == nil {
			err = deleteApplicationRowsTx(ctx, tx, app)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		done <- err
	}()
	waitPointerLockWaiter(t, ctx, f.pool, "FROM eregion_platform_apps WHERE name=$1 FOR UPDATE")
	if err := history.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		assertNoDeadlock(t, err)
		t.Fatal(err)
	}
	var appCount, eventsAfter int
	var operationState string
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_platform_apps WHERE name=$1),state,
		(SELECT count(*) FROM eregion_operation_events WHERE operation_id=$2) FROM eregion_operations WHERE id=$2`, f.env.App, f.op.ID).Scan(&appCount, &operationState, &eventsAfter); err != nil || appCount != 0 || operationState != "queued" || eventsAfter != eventsBefore {
		t.Fatalf("remaining app=%d operation=%s events before=%d after=%d err=%v", appCount, operationState, eventsBefore, eventsAfter, err)
	}
}

func TestIntegrationPointerInvalidTargetsRollback(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	platformSubmit(t, f.service, platform.KindEnvironmentCreate, "app/shop", "pointer-staging", map[string]any{
		"name": "staging", "node": "local", "domain": "staging.example.com",
	})
	platformSubmit(t, f.service, platform.KindAppCreate, "platform", "pointer-other-app", map[string]any{"name": "other"})
	platformSubmit(t, f.service, platform.KindEnvironmentCreate, "app/other", "pointer-other-env", map[string]any{
		"name": "production", "node": "local", "domain": "other.example.com",
		"source": map[string]any{"url": "https://github.com/acme/other.git", "ref": "main"},
	})
	staging, err := f.service.environment(ctx, "shop", "staging")
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.service.environment(ctx, "other", "production")
	if err != nil {
		t.Fatal(err)
	}
	stagingRelease := seedArtifactRelease(t, f.service, "shop", "staging", "registry.example/shop:staging").ID
	otherRelease := seedArtifactRelease(t, f.service, "other", "production", "registry.example/other:base").ID
	sameEnvRelease := seedArtifactRelease(t, f.service, "shop", "production", "registry.example/shop:other").ID
	stagingOp := platformSubmit(t, f.service, platform.KindDeploy, "env/shop/staging", "pointer-staging-deploy", map[string]any{"release_id": stagingRelease})
	otherOp := platformSubmit(t, f.service, platform.KindDeploy, "env/other/production", "pointer-other-deploy", map[string]any{})
	var stagingDeployment platform.DeploymentID
	var otherBuild platform.BuildID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM eregion_platform_deployments WHERE operation_id=$1`, stagingOp.ID).Scan(&stagingDeployment); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id FROM eregion_platform_builds WHERE operation_id=$1`, otherOp.ID).Scan(&otherBuild); err != nil {
		t.Fatal(err)
	}
	var original string
	if err := f.pool.QueryRow(ctx, `SELECT health_detail FROM eregion_platform_environments WHERE id=$1`, f.env.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	type invalidCase struct {
		name string
		run  func(pgx.Tx, string) error
	}
	cases := []invalidCase{
		{"current missing", func(tx pgx.Tx, _ string) error {
			return lockRevisionPointerTx(ctx, tx, f.env.ID, "erv_ffffffffffffffffffffffffffffffff")
		}},
		{"current cross environment", func(tx pgx.Tx, _ string) error {
			return lockRevisionPointerTx(ctx, tx, f.env.ID, staging.RevisionID)
		}},
		{"current cross application", func(tx pgx.Tx, _ string) error {
			return lockRevisionPointerTx(ctx, tx, f.env.ID, other.RevisionID)
		}},
		{"desired missing", func(tx pgx.Tx, app string) error {
			return lockReleasePointerTx(ctx, tx, app, f.env.ID, "rel_ffffffffffffffffffffffffffffffff")
		}},
		{"desired cross environment", func(tx pgx.Tx, app string) error {
			return lockReleasePointerTx(ctx, tx, app, f.env.ID, stagingRelease)
		}},
		{"desired cross application", func(tx pgx.Tx, app string) error {
			return lockReleasePointerTx(ctx, tx, app, f.env.ID, otherRelease)
		}},
		{"shared serving/applied release validator: missing", func(tx pgx.Tx, _ string) error {
			return lockServingPointersTx(ctx, tx, f.env.ID, "rel_ffffffffffffffffffffffffffffffff", f.dep)
		}},
		{"serving deployment missing", func(tx pgx.Tx, _ string) error {
			return lockServingPointersTx(ctx, tx, f.env.ID, f.release, "dep_ffffffffffffffffffffffffffffffff")
		}},
		{"serving pair mismatch", func(tx pgx.Tx, _ string) error {
			return lockServingPointersTx(ctx, tx, f.env.ID, sameEnvRelease, f.dep)
		}},
		{"serving deployment cross environment", func(tx pgx.Tx, _ string) error {
			return lockServingPointersTx(ctx, tx, f.env.ID, f.release, stagingDeployment)
		}},
		{"build missing", func(tx pgx.Tx, _ string) error {
			return setReleaseBuildPointerTx(ctx, tx, f.env.ID, f.release, "bld_ffffffffffffffffffffffffffffffff")
		}},
		{"build reciprocal mismatch", func(tx pgx.Tx, _ string) error {
			return setReleaseBuildPointerTx(ctx, tx, f.env.ID, sameEnvRelease, f.build)
		}},
		{"build cross application", func(tx pgx.Tx, _ string) error {
			return setReleaseBuildPointerTx(ctx, tx, f.env.ID, f.release, otherBuild)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			appID, err := lockPointerEnvironmentTx(ctx, tx, f.env.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments SET health_detail='rollback sentinel' WHERE id=$1`, f.env.ID); err != nil {
				t.Fatal(err)
			}
			err = tc.run(tx, appID)
			assertNoDeadlock(t, err)
			var fault *contract.Fault
			if !errors.As(err, &fault) || fault.Code != contract.Unavailable || fault.Message != pointerIntegrityFault().Error() || fault.Cause != nil {
				t.Fatalf("expected pointer-integrity fault without cause, got %v", err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			var actual string
			if err := f.pool.QueryRow(ctx, `SELECT health_detail FROM eregion_platform_environments WHERE id=$1`, f.env.ID).Scan(&actual); err != nil || actual != original {
				t.Fatalf("rollback detail=%q original=%q err=%v", actual, original, err)
			}
		})
	}
}
