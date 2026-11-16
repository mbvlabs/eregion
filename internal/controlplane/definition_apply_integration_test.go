package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

var definitionIntegrationActor = contract.Actor{Kind: "operator", Subject: "definition-test"}

type definitionSetupAction struct {
	kind, target, key string
	input             map[string]any
}

func runDefinitionSetupActions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, actions []definitionSetupAction) {
	t.Helper()
	for _, action := range actions {
		op, err := s.Submit(ctx, platformSubmissionAs(definitionIntegrationActor, action.kind, action.target, action.key, action.input))
		if err != nil {
			t.Fatalf("setup %s: %v", action.kind, err)
		}
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatalf("setup %s: %v", action.kind, err)
		}
		var state contract.State
		if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, op.ID).Scan(&state); err != nil || state != contract.Succeeded {
			t.Fatalf("setup %s state=%s err=%v", action.kind, state, err)
		}
	}
}

const integrationDefinition = `version = 1
app = "api"
node = "local"
[automation]
auto_deploy = false
[config]
NEW = "value"
`

// No workers are started: record actions are driven explicitly with the fake
// transport, without executing anything against a node.
func definitionIntegrationFixture(t *testing.T) (*pgxpool.Pool, *Service, api.AppService) {
	t.Helper()
	pool, _ := testPool(t, true)
	ctx := context.Background()
	if _, err := AssignWorkspace(ctx, newTestEntities(t, pool), "main", testRegistryHostIdentity()); err != nil {
		t.Fatal(err)
	}
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: newScriptedDeployTransport(), NodeStateDir: t.TempDir(), LocalNodeName: "local",
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	runDefinitionSetupActions(t, ctx, pool, s, []definitionSetupAction{
		{platform.KindNodeRegister, "platform", "definition-node", map[string]any{"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker}}},
		{platform.KindAppCreate, "platform", "definition-app", map[string]any{"name": "api"}},
		{platform.KindEnvironmentCreate, "app/api", "definition-env", map[string]any{"name": "production", "node": "local", "config": map[string]string{"OLD": "value"}}},
	})
	return pool, s, api.AppService{DefinitionPlanner: s}
}

func TestIntegrationDefinitionSnapshotIgnoresUnrelatedOperation(t *testing.T) {
	_, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	// Leave unrelated platform work queued between approval and admission.
	platformSubmitAs(t, s, definitionIntegrationActor, platform.KindAppCreate, "platform", "unrelated-app", map[string]any{"name": "other"})
	again, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || again.Digest != plan.Digest {
		t.Fatalf("unrelated work changed plan: digest=%s err=%v", again.Digest, err)
	}
	result, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "unrelated-operation-apply", Confirm: "api/production",
	})
	if err != nil || result.Replay || result.Operation.State != contract.Queued {
		t.Fatalf("apply=%+v err=%v", result, err)
	}
}

// TestIntegrationExhaustedDefinitionOrphanReschedulesDrain proves the exhausted
// definition branch reports the committed outcome: the drained parent reports a
// requeue, not a discard, and the same transaction leaves the River job
// scheduled for the drain while the linked child is cancelling.
func TestIntegrationExhaustedDefinitionOrphanReschedulesDrain(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runDefinitionSetupActions(t, ctx, pool, s, []definitionSetupAction{
		{platform.KindNodeRegister, "platform", "drain-node", map[string]any{
			"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
		}},
		{platform.KindAppCreate, "platform", "drain-app", map[string]any{"name": "api"}},
		{platform.KindEnvironmentCreate, "app/api", "drain-env", map[string]any{
			"name": "production", "node": "local", "config": map[string]string{"OLD": "value"},
		}},
	})

	app := api.AppService{DefinitionPlanner: s}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	result, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "drain-orphan", Confirm: "api/production",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := result.Operation.ID
	claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	err = (&platformWorker{service: s}).runDefinitionApply(ctx, claimed, 1)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("first definition step did not wait for its child: %v", err)
	}
	var child contract.OperationID
	if err := pool.QueryRow(ctx, `SELECT child_operation_id FROM eregion_definition_steps
        WHERE parent_operation_id=$1 AND child_operation_id IS NOT NULL ORDER BY ordinal LIMIT 1`, parent).Scan(&child); err != nil {
		t.Fatal(err)
	}
	var childState contract.State
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&childState); err != nil || childState != contract.Queued {
		t.Fatalf("child state=%s err=%v, want queued", childState, err)
	}

	assertExhaustedDefinitionDrain(t, ctx, pool, s, parent, child)
}

func assertExhaustedDefinitionDrain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, parent, child contract.OperationID) {
	t.Helper()
	var jobID int64
	if err := pool.QueryRow(ctx, `UPDATE river_job
        SET state='running',attempt=$2,max_attempts=$2,attempted_by=ARRAY['old-worker'],attempted_at=clock_timestamp()
        WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1) RETURNING id`, parent, jobargs.PlatformMaxAttempts).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	requeued, discarded, err := s.recoverOrphanedPlatformJob(ctx, orphanedPlatformJob{
		ID: jobID, OperationID: parent,
		Attempt: jobargs.PlatformMaxAttempts, MaxAttempts: jobargs.PlatformMaxAttempts,
		LastClient: "old-worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !requeued || discarded {
		t.Fatalf("definition drain requeued=%t discarded=%t, want requeued only", requeued, discarded)
	}

	var parentState, drainedChildState contract.State
	var failureCode, failureMessage string
	var finished bool
	if err := pool.QueryRow(ctx, `SELECT o.state,r.failure_code,r.failure_message,r.finished_at IS NOT NULL
        FROM eregion_operations o JOIN eregion_definition_runs r ON r.operation_id=o.id
        WHERE o.id=$1`, parent).Scan(&parentState, &failureCode, &failureMessage, &finished); err != nil {
		t.Fatal(err)
	}
	if parentState != contract.Cancelling || finished || failureCode != string(contract.Unavailable) ||
		!strings.Contains(failureMessage, "attempts were exhausted") {
		t.Fatalf("parent state=%s finished=%t code=%q message=%q", parentState, finished, failureCode, failureMessage)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&drainedChildState); err != nil ||
		drainedChildState != contract.Cancelling {
		t.Fatalf("drained child state=%s err=%v, want cancelling", drainedChildState, err)
	}
	var jobState string
	var maxAttempts int
	var finalized, inDrainWindow bool
	if err := pool.QueryRow(ctx, `SELECT state,max_attempts,finalized_at IS NOT NULL,
        (scheduled_at-clock_timestamp()) > interval '1 second' AND (scheduled_at-clock_timestamp()) < interval '4 seconds'
        FROM river_job WHERE id=$1`, jobID).Scan(&jobState, &maxAttempts, &finalized, &inDrainWindow); err != nil {
		t.Fatal(err)
	}
	if jobState != "scheduled" || finalized || maxAttempts != jobargs.PlatformMaxAttempts+1 || !inDrainWindow {
		t.Fatalf("river job state=%s finalized=%t max_attempts=%d scheduled_window=%t", jobState, finalized, maxAttempts, inDrainWindow)
	}
}

func TestIntegrationDefinitionSnapshotRelevantChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"placement", `UPDATE eregion_platform_nodes SET status='offline' WHERE name='local'`},
		{"target protection", `UPDATE eregion_platform_environments SET protected=NOT protected WHERE name='production'`},
		{"secret metadata", `INSERT INTO eregion_secrets(app,environment,name,latest_revision,state,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
		 VALUES('api','production','example',1,'active','operator','test','operator','test',clock_timestamp(),clock_timestamp())`},
		{"policy", `INSERT INTO eregion_definition_dns_policy(singleton,revision,enabled,provider,zone_id,origins,proxy_mode,updated_at)
		 VALUES(true,1,false,NULL,NULL,'{}','dns-only',clock_timestamp()) ON CONFLICT (singleton) DO UPDATE SET revision=eregion_definition_dns_policy.revision+1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, app := definitionIntegrationFixture(t)
			ctx := context.Background()
			input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
			plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
			if err != nil || !plan.CanApply {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			input.PlanNonce = plan.PlanNonce
			if _, err := pool.Exec(ctx, tc.query); err != nil {
				t.Fatal(err)
			}
			_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
				Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "stale", Confirm: "api/production",
			})
			if contract.Classify(err).Code != contract.Conflict {
				t.Fatalf("stale %s accepted: %v", tc.name, err)
			}
			var parents int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_definition_runs`).Scan(&parents); err != nil || parents != 0 {
				t.Fatalf("stale admission left runs=%d err=%v", parents, err)
			}
		})
	}
}

func TestIntegrationDefinitionSnapshotIgnoresUnrelatedNode(t *testing.T) {
	pool, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	op := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindNodeRegister, "platform", "other-node", map[string]any{
		"name": "other", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
		t.Fatal(err)
	}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET status='offline' WHERE name='other'`); err != nil {
		t.Fatal(err)
	}
	again, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || again.Digest != plan.Digest {
		t.Fatalf("unrelated node changed plan: digest=%s err=%v", again.Digest, err)
	}
	_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "unrelated-node-apply", Confirm: "api/production",
	})
	if err != nil {
		t.Fatalf("unrelated node blocked apply: %v", err)
	}
}

func TestIntegrationDefinitionPlannedNodeAndCredentialFences(t *testing.T) {
	pool, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	op := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindNodeRegister, "platform", "planned-node", map[string]any{
		"name": "next", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
		t.Fatal(err)
	}
	op = platformSubmitAs(t, s, definitionIntegrationActor, platform.KindNodeRegister, "platform", "planned-builder", map[string]any{
		"name": "builder", "roles": []string{platform.RoleBuilder}, "capabilities": []string{platform.CapabilityBuildpacks},
	})
	if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_provider_credentials
		(provider,zone_id,zone_name,latest_revision,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
		VALUES('cloudflare','zone','example.com',1,'operator','test','operator','test',clock_timestamp(),clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_definition_dns_policy(singleton,revision,enabled,provider,zone_id,origins,proxy_mode,updated_at)
		VALUES(true,1,false,'cloudflare','zone','{}','dns-only',clock_timestamp())
		ON CONFLICT (singleton) DO UPDATE SET revision=1,enabled=false,provider='cloudflare',zone_id='zone'`); err != nil {
		t.Fatal(err)
	}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: strings.Replace(integrationDefinition, `node = "local"`, "node = \"next\"\nbuild_node = \"builder\"", 1)}
	for _, mutation := range []string{
		`UPDATE eregion_platform_nodes SET status='offline' WHERE name='next'`,
		`UPDATE eregion_platform_nodes SET status='offline' WHERE name='builder'`,
		`UPDATE eregion_provider_credentials SET latest_revision=latest_revision+1 WHERE provider='cloudflare' AND zone_id='zone'`,
	} {
		input.PlanNonce = ""
		plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
		if err != nil || !plan.CanApply {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
		input.PlanNonce = plan.PlanNonce
		if _, err := pool.Exec(ctx, mutation); err != nil {
			t.Fatal(err)
		}
		_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
			Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "planned-fence", Confirm: "api/production",
		})
		if contract.Classify(err).Code != contract.Conflict {
			t.Fatalf("changed placement or credential accepted: %v", err)
		}
	}
}

func TestIntegrationDefinitionRefusesActiveSameAppWork(t *testing.T) {
	_, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	platformSubmitAs(t, s, definitionIntegrationActor, platform.KindConfigSet, "env/api/production", "active-config", map[string]any{"config": map[string]string{"OTHER": "value"}})
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "active-work", Confirm: "api/production",
	})
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("active same-app work accepted: %v", err)
	}
}

func TestIntegrationDefinitionHostnameConflictAfterPlan(t *testing.T) {
	pool, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	for _, action := range []struct {
		kind, target, key string
		input             map[string]any
	}{
		{platform.KindAppCreate, "platform", "other-app", map[string]any{"name": "other"}},
		{platform.KindEnvironmentCreate, "app/other", "other-env", map[string]any{"name": "production", "node": "local"}},
	} {
		op := platformSubmitAs(t, s, definitionIntegrationActor, action.kind, action.target, action.key, action.input)
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition + `
[[environments.production.domains]]
name = "shared.example.com"
primary = true
`}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	platformSubmitAs(t, s, definitionIntegrationActor, platform.KindDomainAdd, "env/other/production", "claim-host", map[string]any{"domain": "shared.example.com", "primary": true})
	_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "hostname-conflict", Confirm: "api/production",
	})
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("cross-app hostname accepted: %v", err)
	}
	var runs, owners int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_definition_runs), (SELECT count(*) FROM eregion_platform_domains WHERE name='shared.example.com')`).Scan(&runs, &owners); err != nil || runs != 0 || owners != 1 {
		t.Fatalf("runs=%d owners=%d err=%v", runs, owners, err)
	}
}

func TestIntegrationDefinitionConcurrentReplay(t *testing.T) {
	pool, _, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	apply := api.DefinitionApplyInput{Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "concurrent-replay", Confirm: "api/production"}
	var wg sync.WaitGroup
	results := make([]contract.OperationID, 2)
	replays := make([]bool, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := app.ApplyDefinition(ctx, definitionIntegrationActor, apply)
			results[i], replays[i], errs[i] = result.Operation.ID, result.Replay, err
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0] == "" || results[0] != results[1] || replays[0] == replays[1] {
		t.Fatalf("results=%v replays=%v errors=%v", results, replays, errs)
	}
	var runs, parents int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_definition_runs), (SELECT count(*) FROM eregion_operations WHERE kind=$1)`, platform.KindDefinitionApply).Scan(&runs, &parents); err != nil || runs != 1 || parents != 1 {
		t.Fatalf("runs=%d parents=%d err=%v", runs, parents, err)
	}
}

func TestIntegrationDefinitionAdmissionReplayAndFences(t *testing.T) {
	pool, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply || !definitionHex256(plan.PlanNonce) || !definitionHex256(plan.Digest) {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if plan.ConfirmTarget != "api/production" {
		t.Fatalf("config removal confirmation=%q", plan.ConfirmTarget)
	}
	input.PlanNonce = plan.PlanNonce
	again, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || again.Digest != plan.Digest || again.PlanNonce != plan.PlanNonce {
		t.Fatalf("replan=%+v err=%v", again, err)
	}
	apply := api.DefinitionApplyInput{Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "definition-apply"}
	baseline := readDefinitionAdmissionCounts(t, ctx, pool)
	var servingBefore string
	var releasesBefore, buildsBefore, deploymentsBefore, lifecycleBefore int
	if err := pool.QueryRow(ctx, `SELECT (SELECT coalesce(e.serving_release_id,'') FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='api' AND e.name='production'), (SELECT count(*) FROM eregion_platform_releases), (SELECT count(*) FROM eregion_platform_builds), (SELECT count(*) FROM eregion_platform_deployments), (SELECT count(*) FROM eregion_operations WHERE kind IN ($1,$2,$3))`, platform.KindBuildStop, platform.KindBuildRetry, platform.KindDeploy).Scan(&servingBefore, &releasesBefore, &buildsBefore, &deploymentsBefore, &lifecycleBefore); err != nil {
		t.Fatal(err)
	}
	testDefinitionAdmissionRejections(t, ctx, pool, app, apply, baseline)
	apply.Confirm = "api/production"
	accepted, err := app.ApplyDefinition(ctx, definitionIntegrationActor, apply)
	if err != nil || accepted.Replay || accepted.Operation.State != contract.Queued {
		t.Fatalf("accept=%+v err=%v", accepted, err)
	}
	assertDefinitionAdmissionJournal(t, ctx, pool, accepted.Operation.ID, apply.Confirm)
	assertDefinitionAdmissionFences(t, ctx, pool, s)
	testDefinitionAdmissionReplay(t, ctx, pool, app, apply, accepted.Operation.ID)
	claimed, _, ok, err := s.claimPlatform(ctx, accepted.Operation.ID, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%t err=%v", ok, err)
	}
	testDefinitionAdmissionWorker(t, ctx, pool, s, accepted.Operation.ID, claimed)
	var servingAfter string
	var releasesAfter, buildsAfter, deploymentsAfter, lifecycleAfter int
	if err := pool.QueryRow(ctx, `SELECT (SELECT coalesce(e.serving_release_id,'') FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='api' AND e.name='production'), (SELECT count(*) FROM eregion_platform_releases), (SELECT count(*) FROM eregion_platform_builds), (SELECT count(*) FROM eregion_platform_deployments), (SELECT count(*) FROM eregion_operations WHERE kind IN ($1,$2,$3))`, platform.KindBuildStop, platform.KindBuildRetry, platform.KindDeploy).Scan(&servingAfter, &releasesAfter, &buildsAfter, &deploymentsAfter, &lifecycleAfter); err != nil || servingAfter != servingBefore || releasesAfter != releasesBefore || buildsAfter != buildsBefore || deploymentsAfter != deploymentsBefore || lifecycleAfter != lifecycleBefore {
		t.Fatalf("unexpected deployment effects serving=%q releases=%d builds=%d deployments=%d lifecycle=%d err=%v", servingAfter, releasesAfter, buildsAfter, deploymentsAfter, lifecycleAfter, err)
	}
}

// The DNS guard locks the journal before checking the child kind. An ordinary
// environment-update child therefore probes its lock ordering without granting
// provider authorization or contacting a DNS provider.
func TestIntegrationDefinitionDNSGuardDoesNotBlockParentPoll(t *testing.T) {
	pool, s, app := definitionIntegrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply || plan.ConfirmTarget != "api/production" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	accepted, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "definition-lock-order", Confirm: plan.ConfirmTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _, ok, err := s.claimPlatform(ctx, accepted.Operation.ID, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%t err=%v", ok, err)
	}
	var snooze *river.JobSnoozeError
	if err := (&platformWorker{service: s}).runDefinitionApply(ctx, claimed, 1); !errors.As(err, &snooze) {
		t.Fatalf("child admission did not snooze: %v", err)
	}
	var child contract.OperationID
	if err := pool.QueryRow(ctx, `SELECT child_operation_id FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=0 AND kind=$2`, accepted.Operation.ID, platform.KindEnvironmentUpdate).Scan(&child); err != nil {
		t.Fatal(err)
	}

	assertDefinitionDNSGuardLockOrder(t, ctx, pool, accepted.Operation.ID)
}

func lockDefinitionDNSGuardPoll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parent contract.OperationID) (pgx.Tx, int) {
	t.Helper()
	pollTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pollPID int
	if err := pollTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pollPID); err != nil {
		t.Fatal(err)
	}
	var parentID string
	if err := pollTx.QueryRow(ctx, `SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, parent).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	return pollTx, pollPID
}

func assertDefinitionDNSGuardLockOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parent contract.OperationID) {
	t.Helper()
	// Model the parent poll's first lock, then verify the guard is actually
	// waiting on that backend before attempting the remaining poll locks.
	pollTx, pollPID := lockDefinitionDNSGuardPoll(t, ctx, pool, parent)
	defer rollback(ctx, pollTx)
	guardTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var guardPID int
	if err := guardTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&guardPID); err != nil {
		rollback(ctx, guardTx)
		t.Fatal(err)
	}
	guardResult := make(chan error, 1)
	guardDone := make(chan struct{})
	go func() {
		defer close(guardDone)
		defer rollback(ctx, guardTx)
		// The first lock of the poll guard is the parent operation row. Model
		// that exact statement here, then fail closed as the typed loader does
		// once it observes a non-DNS child.
		var id string
		err := guardTx.QueryRow(ctx, `SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, parent).Scan(&id)
		if err == nil {
			err = contract.NewFault(contract.Conflict, "definition DNS child is not a DNS operation", "Inspect the journal.")
		}
		guardResult <- err
	}()
	defer func() {
		rollback(ctx, pollTx)
		<-guardDone
	}()

	waitCtx, stopWaiting := context.WithTimeout(ctx, 5*time.Second)
	defer stopWaiting()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(waitCtx, `SELECT $1 = ANY(pg_blocking_pids($2))`, pollPID, guardPID).Scan(&blocked); err != nil {
			t.Fatalf("inspect guard wait: %v", err)
		}
		if blocked {
			break
		}
		select {
		case err := <-guardResult:
			t.Fatalf("guard returned before waiting on parent: %v", err)
		case <-waitCtx.Done():
			t.Fatalf("guard did not wait on parent: %v", waitCtx.Err())
		case <-ticker.C:
		}
	}
	if _, err := pollTx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		t.Fatal(err)
	}
	var runID, stepID string
	if err := pollTx.QueryRow(ctx, `SELECT operation_id FROM eregion_definition_runs WHERE operation_id=$1 FOR UPDATE`, parent).Scan(&runID); err != nil {
		t.Fatalf("parent poll could not lock run while guard waited: %v", err)
	}
	if err := pollTx.QueryRow(ctx, `SELECT parent_operation_id FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=0 FOR UPDATE`, parent).Scan(&stepID); err != nil {
		t.Fatalf("parent poll could not lock step while guard waited: %v", err)
	}
	if err := pollTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-guardResult:
		if contract.Classify(err).Code != contract.Conflict {
			t.Fatalf("non-domain child should fail closed after acquiring locks: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("guard did not finish after parent released locks: %v", ctx.Err())
	}
}

type definitionAdmissionCounts struct {
	operations, jobs, events, runs, steps, linked int
}

func readDefinitionAdmissionCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) definitionAdmissionCounts {
	t.Helper()
	var c definitionAdmissionCounts
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations), (SELECT count(*) FROM river_job), (SELECT count(*) FROM eregion_operation_events), (SELECT count(*) FROM eregion_definition_runs), (SELECT count(*) FROM eregion_definition_steps), (SELECT count(*) FROM eregion_definition_steps WHERE child_operation_id IS NOT NULL)`).Scan(&c.operations, &c.jobs, &c.events, &c.runs, &c.steps, &c.linked); err != nil {
		t.Fatal(err)
	}
	return c
}

func testDefinitionAdmissionRejections(t *testing.T, ctx context.Context, pool *pgxpool.Pool, app api.AppService, apply api.DefinitionApplyInput, baseline definitionAdmissionCounts) {
	t.Helper()
	for _, tc := range []struct {
		name string
		edit func(*api.DefinitionApplyInput)
		code contract.Code
	}{
		{"missing digest", func(v *api.DefinitionApplyInput) { v.ApprovedDigest = "" }, contract.ValidationFailed},
		{"wrong digest", func(v *api.DefinitionApplyInput) { v.ApprovedDigest = strings.Repeat("0", 64) }, contract.Conflict},
		{"missing confirmation", func(v *api.DefinitionApplyInput) {}, contract.InputRequired},
		{"wrong confirmation", func(v *api.DefinitionApplyInput) { v.Confirm = "api/staging" }, contract.InputRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := apply
			tc.edit(&bad)
			if _, err := app.ApplyDefinition(ctx, definitionIntegrationActor, bad); contract.Classify(err).Code != tc.code {
				t.Fatalf("error=%v, want %s", err, tc.code)
			}
			var parents, runs, steps, resources int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations WHERE kind=$1), (SELECT count(*) FROM eregion_definition_runs), (SELECT count(*) FROM eregion_definition_steps), (SELECT count(*) FROM eregion_platform_resources)`, platform.KindDefinitionApply).Scan(&parents, &runs, &steps, &resources); err != nil || parents != 0 || runs != 0 || steps != 0 || resources != 0 {
				t.Fatalf("partial admission parents=%d runs=%d steps=%d resources=%d err=%v", parents, runs, steps, resources, err)
			}
			if got := readDefinitionAdmissionCounts(t, ctx, pool); got != baseline {
				t.Fatalf("rejected admission changed durable counts: got %+v baseline %+v", got, baseline)
			}
		})
	}
}

func assertDefinitionAdmissionJournal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parent contract.OperationID, confirm string) {
	t.Helper()
	var runs, steps, children int
	var kind string
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_definition_runs), (SELECT count(*) FROM eregion_definition_steps WHERE parent_operation_id=$1), (SELECT count(*) FROM eregion_definition_steps WHERE parent_operation_id=$1 AND child_operation_id IS NOT NULL), kind, input FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=0`, parent).Scan(&runs, &steps, &children, &kind, &raw); err != nil {
		t.Fatal(err)
	}
	var patch platform.EnvironmentUpdateInput
	if err := json.Unmarshal(raw, &patch); err != nil || runs != 1 || steps != 1 || children != 0 || kind != platform.KindEnvironmentUpdate || patch.Confirm != confirm {
		t.Fatalf("journal runs=%d steps=%d children=%d kind=%s patch=%+v err=%v", runs, steps, children, kind, patch, err)
	}
	var jobs, events int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations o JOIN river_job j ON j.id=o.job_id WHERE o.id=$1), (SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1)`, parent).Scan(&jobs, &events); err != nil || jobs != 1 || events == 0 {
		t.Fatalf("parent job=%d events=%d err=%v", jobs, events, err)
	}
}

func assertDefinitionAdmissionFences(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service) {
	t.Helper()
	for _, action := range []struct {
		kind, target, key string
		input             map[string]any
	}{
		{platform.KindConfigSet, "env/api/production", "blocked-config", map[string]any{"config": map[string]string{"OTHER": "v"}}},
		{platform.KindAppDelete, "app/api", "blocked-delete", map[string]any{"confirm": "api"}},
	} {
		_, err := s.Submit(ctx, platformSubmissionAs(definitionIntegrationActor, action.kind, action.target, action.key, action.input))
		if contract.Classify(err).Code != contract.Conflict {
			t.Fatalf("%s fence: %v", action.kind, err)
		}
	}
	store := NewDefinitionDNSPolicyStore(newTestEntities(t, pool))
	if _, err := store.SetDefinitionDNSPolicy(ctx, definitionIntegrationActor, api.DefinitionDNSPolicyInput{}); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("policy fence: %v", err)
	}
}

func testDefinitionAdmissionReplay(t *testing.T, ctx context.Context, pool *pgxpool.Pool, app api.AppService, apply api.DefinitionApplyInput, acceptedID contract.OperationID) {
	t.Helper()
	// Simulate a changed snapshot after a lost response. Replay must not replan.
	if _, err := pool.Exec(ctx, `UPDATE eregion_definition_dns_policy SET revision=revision+1 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	beforeReplay := readDefinitionAdmissionCounts(t, ctx, pool)
	replay, err := app.ApplyDefinition(ctx, definitionIntegrationActor, apply)
	if err != nil || !replay.Replay || replay.Operation.ID != acceptedID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if got := readDefinitionAdmissionCounts(t, ctx, pool); got != beforeReplay {
		t.Fatalf("replay changed durable counts: got %+v before %+v", got, beforeReplay)
	}
	changed := apply
	changed.ApprovedDigest = strings.Repeat("a", 64)
	if _, err := app.ApplyDefinition(ctx, definitionIntegrationActor, changed); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed replay: %v", err)
	}
	// Restore the test-only snapshot mutation before exercising the worker.
	if _, err := pool.Exec(ctx, `UPDATE eregion_definition_dns_policy SET revision=revision-1 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
}

func testDefinitionAdmissionWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, parent contract.OperationID, claimed contract.Operation) {
	t.Helper()
	w := &platformWorker{service: s}
	// Both calls snooze: the first creates the child and the second observes it.
	for _, phase := range []string{"child admission", "active child"} {
		var snooze *river.JobSnoozeError
		if err := w.runDefinitionApply(ctx, claimed, 1); !errors.As(err, &snooze) {
			t.Fatalf("%s did not snooze: %v", phase, err)
		}
	}
	var next, linked, matching int
	if err := pool.QueryRow(ctx, `SELECT r.next_step, count(s.child_operation_id), count(c.id) FILTER (WHERE c.kind=s.kind AND c.target=s.target AND c.input=s.input) FROM eregion_definition_runs r JOIN eregion_definition_steps s ON s.parent_operation_id=r.operation_id LEFT JOIN eregion_operations c ON c.id=s.child_operation_id WHERE r.operation_id=$1 GROUP BY r.next_step`, parent).Scan(&next, &linked, &matching); err != nil || next != 0 || linked != 1 || matching != 1 {
		t.Fatalf("child cursor=%d linked=%d matching=%d err=%v", next, linked, matching, err)
	}
	var child contract.OperationID
	if err := pool.QueryRow(ctx, `SELECT child_operation_id FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=0`, parent).Scan(&child); err != nil {
		t.Fatal(err)
	}
	if err := runPlatformAttempt(ctx, s, child, 1); err != nil {
		t.Fatalf("execute environment update child: %v", err)
	}
	var childState contract.State
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&childState); err != nil || childState != contract.Succeeded {
		t.Fatalf("child state=%s err=%v", childState, err)
	}
	var snooze *river.JobSnoozeError
	if err := w.runDefinitionApply(ctx, claimed, 1); !errors.As(err, &snooze) {
		t.Fatalf("completed child poll did not advance and snooze: %v", err)
	}
	if err := w.runDefinitionApply(ctx, claimed, 1); err != nil {
		t.Fatalf("finish parent: %v", err)
	}
	var parentState contract.State
	var finished bool
	var stepCount, reservations int
	if err := pool.QueryRow(ctx, `SELECT o.state,r.finished_at IS NOT NULL,r.next_step,(SELECT count(*) FROM eregion_definition_steps WHERE parent_operation_id=r.operation_id),(SELECT count(*) FROM eregion_definition_runs WHERE environment_id=r.environment_id AND finished_at IS NULL) FROM eregion_definition_runs r JOIN eregion_operations o ON o.id=r.operation_id WHERE r.operation_id=$1`, parent).Scan(&parentState, &finished, &next, &stepCount, &reservations); err != nil || parentState != contract.Succeeded || !finished || next != stepCount || reservations != 0 {
		t.Fatalf("parent=%s finished=%t cursor=%d steps=%d reservations=%d err=%v", parentState, finished, next, stepCount, reservations, err)
	}
}
