package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func readActionPayload(t *testing.T, pool *pgxpool.Pool, s *Service, op contract.Operation, dst any) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	raw, err := s.actionPayloadTx(ctx, tx, &op, contract.Succeeded)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestIntegrationActionPayloadRevisionAndUnavailable(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "payload-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	op := platformSubmit(t, s, platform.KindAppCreate, "platform", "payload-app", map[string]any{
		"name": "payload", "environments": []map[string]any{
			{"name": "production", "node": "local", "config": map[string]string{"PASSWORD": "do-not-emit"}},
			{"name": "staging", "node": "local"},
		},
	})
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	raw, err := s.actionPayloadTx(ctx, tx, &op, contract.Succeeded)
	if err != nil {
		t.Fatal(err)
	}
	var got revisionPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Availability != "available" || len(got.Revisions) != 2 || got.Revisions[0].Environment != "production" || got.Revisions[1].Environment != "staging" {
		t.Fatalf("revisions: %s", raw)
	}
	if len(got.Revisions[0].ChangedConfigKeys) != 1 || got.Revisions[0].ChangedConfigKeys[0] != "PASSWORD" ||
		len(got.Revisions[0].Processes) == 0 || !got.Revisions[0].ChangedConfigAvailable {
		t.Fatalf("safe revision facts missing: %s", raw)
	}
	if strings.Contains(string(raw), "do-not-emit") {
		t.Fatalf("payload leaked config: %s", raw)
	}
	empty := platformSubmit(t, s, platform.KindAppCreate, "platform", "payload-empty", map[string]any{"name": "emptyapp"})
	var emptyResult revisionPayload
	readActionPayload(t, pool, s, empty, &emptyResult)
	if emptyResult.AppName != "emptyapp" || emptyResult.CurrentApp == nil ||
		emptyResult.CurrentApp.ObservedAt.IsZero() || len(emptyResult.Revisions) != 0 {
		t.Fatalf("app without environments: %+v", emptyResult)
	}
	missing := contract.Operation{ID: op.ID, Kind: platform.KindRetentionPrune}
	raw, err = s.actionPayloadTx(ctx, tx, &missing, contract.Failed)
	if err != nil || !strings.Contains(string(raw), `"availability":"unavailable"`) {
		t.Fatalf("missing retention payload %s: %v", raw, err)
	}
	missing.Kind = platform.KindDeploy
	raw, err = s.actionPayloadTx(ctx, tx, &missing, contract.Failed)
	if err != nil || !strings.Contains(string(raw), `"availability":"unavailable"`) {
		t.Fatalf("missing deployment payload %s: %v", raw, err)
	}
	missing.Kind = platform.KindDefinitionApply
	raw, err = s.actionPayloadTx(ctx, tx, &missing, contract.Failed)
	if err != nil || !strings.Contains(string(raw), `"availability":"unavailable"`) {
		t.Fatalf("missing definition payload %s: %v", raw, err)
	}
}

func TestIntegrationActionPayloadNodeDomainResource(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	nodeOp := platformSubmit(t, s, platform.KindNodeRegister, "platform", "families-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "families-app", map[string]any{
		"name": "payload", "environments": []map[string]any{{"name": "production", "node": "local"}},
	})
	var n nodePayload
	readActionPayload(t, pool, s, nodeOp, &n)
	if n.Name != "local" || len(n.DeclaredRoles) != 1 || n.CurrentObservation == nil ||
		n.CurrentObservation.EnrollmentState != string(platform.EnrollmentRegistered) ||
		n.CurrentObservation.IdentityFingerprint != "" || n.CurrentObservation.ObservedAt.IsZero() {
		t.Fatalf("registered node falsely verified or missing facts: %+v", n)
	}
	checkOp := platformSubmit(t, s, platform.KindNodeCheck, "node/local", "families-check", map[string]any{})
	var checked nodePayload
	readActionPayload(t, pool, s, checkOp, &checked)
	if checked.Name != "local" || checked.CurrentObservation == nil || checked.CurrentObservation.EnrollmentState != "registered" {
		t.Fatalf("node check: %+v", checked)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET enrollment_state='enrolled',
		enrolled_at=clock_timestamp(),identity_fingerprint='SHA256:observed',last_check_state='ok',
		last_check_at=clock_timestamp(),updated_at=clock_timestamp() WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	readActionPayload(t, pool, s, nodeOp, &n)
	if n.CurrentObservation.EnrollmentState != "enrolled" || n.CurrentObservation.EnrolledAt == nil ||
		n.CurrentObservation.IdentityFingerprint != "SHA256:observed" || n.CurrentObservation.LastCheckAt == nil {
		t.Fatalf("enrolled observation: %+v", n)
	}
	domain := platformSubmit(t, s, platform.KindDomainAdd, "env/payload/production", "families-domain", map[string]any{
		"domain": "app.example.com",
	})
	var d domainPayload
	readActionPayload(t, pool, s, domain, &d)
	if d.CurrentObservation == nil || d.CurrentObservation.Generation != nil || d.CurrentObservation.OwnershipMarker != "" {
		t.Fatalf("manual domain incorrectly has managed DNS facts: %+v", d)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_domains SET desired_revision=3,applied_revision=2,
		route_state='pending',public_state='unknown' WHERE name='app.example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_domain_dns
		(domain_id,provider,zone_id,zone_name,ownership_marker,generation,desired_origins,desired_proxied,state,
		last_operation_id,last_error,applied_generation,cleanup_error,created_at,updated_at)
		SELECT id,'cloudflare','zone-id','example.com','owner-marker',4,ARRAY['203.0.113.1'],true,'pending',
		$1,'',3,'',clock_timestamp(),clock_timestamp() FROM eregion_platform_domains WHERE name='app.example.com'`, domain.ID); err != nil {
		t.Fatal(err)
	}
	d = domainPayload{}
	readActionPayload(t, pool, s, domain, &d)
	if d.Name != "app.example.com" || d.CurrentObservation == nil || d.CurrentObservation.ID == "" ||
		d.CurrentObservation.DesiredRevision != 3 || d.CurrentObservation.AppliedRevision != 2 ||
		d.CurrentObservation.RouteState != "pending" || d.CurrentObservation.Generation == nil ||
		*d.CurrentObservation.Generation != 4 || *d.CurrentObservation.AppliedGeneration != 3 ||
		d.CurrentObservation.OwnershipMarker != "owner-marker" || d.CurrentObservation.LastOperationID != string(domain.ID) ||
		d.CurrentObservation.ObservedAt.IsZero() {
		t.Fatalf("domain: %+v", d)
	}
	resource := platformSubmit(t, s, platform.KindResourceApply, "resource/payload/production/db", "families-resource", map[string]any{
		"image": "postgres:17", "env": map[string]string{"PASSWORD": "payload-secret"},
		"mounts": []map[string]any{{"name": "data", "destination": "/private-secret-path"}},
		"health": map[string]any{"type": "exec", "command": []string{"private-command"}},
	})
	var resourceID string
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_platform_resources WHERE operation_id=$1`, resource.ID).Scan(&resourceID); err != nil {
		t.Fatal(err)
	}
	volumeName := "er-res-" + strings.TrimPrefix(resourceID, "res_") + "-data"
	volumes, err := json.Marshal([]platform.ResourceVolumeStatus{{Name: volumeName, Destination: "/private-secret-path"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state='ready',container_name='resource-container',
		network_name='resource-network',volumes=$2::jsonb,
		health_state='healthy',health_checked_at=clock_timestamp() WHERE operation_id=$1`, resource.ID, volumes); err != nil {
		t.Fatal(err)
	}
	var r resourcePayload
	raw := readActionPayload(t, pool, s, resource, &r)
	if r.CurrentObservation == nil || r.CurrentObservation.State != "ready" || r.CurrentObservation.ContainerName != "resource-container" ||
		r.CurrentObservation.NetworkName != "resource-network" || len(r.CurrentObservation.VolumeNames) != 1 ||
		r.CurrentObservation.VolumeNames[0] != volumeName || r.CurrentObservation.LastOperationID != string(resource.ID) {
		t.Fatalf("resource: %s", raw)
	}
	for _, forbidden := range []string{"payload-secret", "PASSWORD", "/private-secret-path", "private-command"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("resource payload leaked %q: %s", forbidden, raw)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, resource.ID); err != nil {
		t.Fatal(err)
	}
	destroy := platformSubmit(t, s, platform.KindResourceDestroy, "resource/payload/production/db", "families-destroy", map[string]any{
		"confirm": "payload/production/db",
	})
	r = resourcePayload{}
	readActionPayload(t, pool, s, destroy, &r)
	if r.DeleteDataRequested == nil || *r.DeleteDataRequested || r.DataOutcome != "unavailable" || r.CurrentObservation.State != "destroying" {
		t.Fatalf("premature destroy outcome: %+v", r)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state='destroyed',container_name='',
		destroyed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1`, destroy.ID); err != nil {
		t.Fatal(err)
	}
	readActionPayload(t, pool, s, destroy, &r)
	if r.DataOutcome != "retained" || len(r.CurrentObservation.VolumeNames) != 1 {
		t.Fatalf("retained destroy outcome: %+v", r)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, destroy.ID); err != nil {
		t.Fatal(err)
	}
	deleteData := platformSubmit(t, s, platform.KindResourceDestroy, "resource/payload/production/db", "families-delete-data", map[string]any{
		"confirm": "payload/production/db", "delete_data": true,
	})
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state='destroyed',volumes='[]'::jsonb,
		destroyed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1`, deleteData.ID); err != nil {
		t.Fatal(err)
	}
	r = resourcePayload{}
	readActionPayload(t, pool, s, deleteData, &r)
	if r.DeleteDataRequested == nil || !*r.DeleteDataRequested || r.DataOutcome != "delete_completed" || len(r.CurrentObservation.VolumeNames) != 0 {
		t.Fatalf("delete data outcome: %+v", r)
	}
	r = resourcePayload{}
	readActionPayload(t, pool, s, destroy, &r)
	if r.DataOutcome != "unavailable" || r.CurrentObservation.LastOperationID != string(deleteData.ID) {
		t.Fatalf("later operation misattributed to earlier destroy: %+v", r)
	}
}

func TestIntegrationActionPayloadBuildStopTarget(t *testing.T) {
	f := newPointerBuildFixture(t)
	op := platformSubmit(t, f.service, platform.KindBuildStop, "build/"+string(f.build), "payload-stop", map[string]any{})
	var got buildStopPayload
	readActionPayload(t, f.pool, f.service, op, &got)
	if got.TargetBuildID != string(f.build) || got.CurrentObservation == nil ||
		got.CurrentObservation.ID != string(f.build) || got.CurrentObservation.OperationID != string(f.op.ID) ||
		got.CurrentObservation.ObservedAt.IsZero() || got.CurrentObservation.State == "" {
		t.Fatalf("build stop: %+v", got)
	}
}

func TestIntegrationActionPayloadDiagnosticAcknowledgements(t *testing.T) {
	pool, _ := testPool(t, true)
	dir := t.TempDir()
	run := testRuntime(t, pool, 0, dir)
	local, err := node.NewLocal(dir, "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	run.Service.transport = local
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("payload-diagnostic", `{"steps":2,"step_delay_ms":0}`))
	if err != nil {
		t.Fatal(err)
	}
	claimed, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "diagnostic")
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	worker := &testWorker{service: run.Service}
	if err := worker.runStep(ctx, op.ID, 1, 1, operations.TestInput{Steps: 2}); err != nil {
		t.Fatal(err)
	}
	var got diagnosticPayloadResult
	readActionPayload(t, pool, run.Service, claimed, &got)
	if got.RequestedSteps != 2 || got.AcknowledgedSteps != 1 || len(got.Steps) != 1 ||
		got.Steps[0].State != "completed" || got.Steps[0].ExecutionID == "" || got.Steps[0].CompletedAt == nil {
		t.Fatalf("diagnostic: %+v", got)
	}
}

func TestIntegrationActionPayloadProtectionRetention(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "protection-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "protection-app", map[string]any{
		"name": "guard", "environments": []map[string]any{{"name": "preview", "node": "local"}},
	})
	op := platformSubmit(t, s, platform.KindEnvironmentProtect, "env/guard/preview", "protection-on", map[string]any{"protected": true})
	var protect protectionPayload
	readActionPayload(t, pool, s, op, &protect)
	if !protect.DesiredProtected || protect.CurrentObservation == nil || !protect.CurrentObservation.Protected ||
		protect.CurrentObservation.ObservedAt.IsZero() {
		t.Fatalf("protection: %+v", protect)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environments SET protected=false,updated_at=clock_timestamp()
		WHERE id=$1`, protect.CurrentObservation.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	protect = protectionPayload{}
	readActionPayload(t, pool, s, op, &protect)
	if !protect.DesiredProtected || protect.CurrentObservation.Protected {
		t.Fatalf("current protection mistaken for requested state: %+v", protect)
	}
	unprotectID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
		(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
		SELECT $1,kind,target,actor_kind,actor_subject,'unprotect-fixture',input_hash,'{"protected":false}'::jsonb,
		state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code FROM eregion_operations WHERE id=$2`, unprotectID, op.ID); err != nil {
		t.Fatal(err)
	}
	unprotect := op
	unprotect.ID = unprotectID
	protect = protectionPayload{}
	readActionPayload(t, pool, s, unprotect, &protect)
	if protect.DesiredProtected || protect.CurrentObservation == nil || protect.CurrentObservation.Protected {
		t.Fatalf("unprotection: %+v", protect)
	}
	diag, err := s.Submit(ctx, submission("retention-payload-fixture", `{"steps":1}`))
	if err != nil {
		t.Fatal(err)
	}
	s.registerSecretValues(map[string]string{"private": "retention-secret-value"})
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_retention_runs
		(id,operation_id,status,planned_items,removed_items,planned_bytes,reclaimed_bytes,errors,started_at,
		finished_at,cache_bytes,cache_measured_at,cache_measurement_error,gc_generation)
		VALUES ('rtr_fixture',$1,'succeeded_with_skips',
		'[{"kind":"image","reference":"image-1","bytes":10},{"kind":"image_skipped","reference":"image-2","reason":"retention-secret-value"}]',
		'[{"kind":"image","reference":"image-1","bytes":10}]',10,10,
		'[{"stage":"image_skipped","reference":"image-2","message":"failed retention-secret-value"}]',
		clock_timestamp(),clock_timestamp(),20,clock_timestamp(),'cache retention-secret-value',2)`, diag.ID); err != nil {
		t.Fatal(err)
	}
	diag.Kind = platform.KindRetentionPrune
	var retained retentionPayload
	raw := readActionPayload(t, pool, s, diag, &retained)
	if retained.Availability != "available" || retained.PlannedCount != 2 || retained.RemovedCount != 1 ||
		retained.PlannedSkipCount != 1 || retained.SkippedCount != 1 || retained.CacheBytes == nil ||
		*retained.CacheBytes != 20 || retained.CacheMeasuredAt == nil || retained.GCGeneration != 2 ||
		len(retained.Errors) != 1 || retained.Errors[0].Message == "" || strings.Contains(raw, "retention-secret-value") {
		t.Fatalf("retention: %s", raw)
	}
}

func TestIntegrationActionPayloadDeploymentEvidence(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_platform_environments SET serving_deployment_id=$2,
		serving_release_id=$3,health_state='healthy',health_checked_at=clock_timestamp()
		WHERE id=$1`, f.env.ID, f.dep, f.release); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO eregion_operation_steps
		(operation_id,step,inputs_hash,state,attempt,execution_id,started_at,completed_at,error,error_code)
		VALUES ($1,'platform-health-web-v1','hash','completed',1,'health-exec',clock_timestamp(),clock_timestamp(),'',''),
		($1,'platform-public-01-v1','hash','failed',1,'public-exec',clock_timestamp(),clock_timestamp(),'private detail','')`, f.op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO eregion_platform_container_cleanup
		(container_name,environment_id,node,deployment_id,reason,attempts,last_error,created_at,next_attempt_at)
		VALUES ('candidate-cleanup',$1,'local',$2,'candidate',0,'',clock_timestamp(),clock_timestamp())`, f.env.ID, f.dep); err != nil {
		t.Fatal(err)
	}
	var got executionPayload
	raw := readActionPayload(t, f.pool, f.service, f.op, &got)
	if got.DeploymentID != string(f.dep) || got.BuildID != string(f.build) || got.ReleaseID != string(f.release) ||
		got.CurrentObservation == nil || got.CurrentObservation.ServingDeploymentID != string(f.dep) ||
		got.CurrentObservation.CandidateCleanupPending != 1 || len(got.Steps) != 2 ||
		got.Steps[0].State != "completed" || got.Steps[1].State != "failed" || strings.Contains(raw, "private detail") {
		t.Fatalf("deployment evidence: %s", raw)
	}
}

func TestIntegrationActionPayloadFirstDeployAttribution(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "first-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	})
	create := platformSubmit(t, s, platform.KindAppCreate, "platform", "first-with", map[string]any{
		"name": "first", "environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api.git", "ref": submittedGitCommit, "builder": "paketobuildpacks/builder-jammy-base"},
			"domain": "first.example.com", "domain_addresses": []string{"203.0.113.10"},
		}}, "deploy": true,
	})
	plain := platformSubmit(t, s, platform.KindAppCreate, "platform", "first-without", map[string]any{
		"name": "plainfirst", "environments": []map[string]any{{"name": "production", "node": "local"}},
	})
	var initial, noIntent revisionPayload
	readActionPayload(t, pool, s, create, &initial)
	readActionPayload(t, pool, s, plain, &noIntent)
	if len(initial.Revisions) != 1 || initial.Revisions[0].FirstDeployOperationID != string(create.FirstDeployOperationID) ||
		len(noIntent.Revisions) != 1 || noIntent.Revisions[0].FirstDeployOperationID != "" {
		t.Fatalf("initial first deploys: %+v %+v", initial, noIntent)
	}
	for i, revision := range []actionRevision{initial.Revisions[0], noIntent.Revisions[0]} {
		for _, candidate := range []struct{ actor, target string }{
			{"other-actor", "env/" + revision.App + "/" + revision.Environment},
			{create.Actor.Subject, "env/other/production"},
		} {
			fake, err := contract.NewOperationID()
			if err != nil {
				t.Fatal(err)
			}
			// A caller-controlled key and even a fabricated deployment pin
			// cannot replace either the original actor or the exact target.
			if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
			(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
			VALUES ($1,'platform.deploy',$2,'operator',$3,$4,'hash','{}','queued',0,0,1,clock_timestamp(),clock_timestamp(),'')`,
				fake, candidate.target, candidate.actor, "first-deploy:"+revision.ID); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployments
				(id,operation_id,app_id,environment_id,action,release_id,previous_release_id,state,error_code,message,created_at,updated_at,
				completed_at,revision_id,secret_revisions,previous_deployment_id,instances)
				SELECT 'dep_' || substr($1,4),$1,app_id,environment_id,action,release_id,previous_release_id,state,error_code,message,
				created_at,updated_at,completed_at,revision_id,secret_revisions,previous_deployment_id,instances
				FROM eregion_platform_deployments WHERE operation_id=$2`, fake, create.FirstDeployOperationID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	withoutIntent, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	r := noIntent.Revisions[0]
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
		(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
		VALUES ($1,'platform.deploy',$2,$3,$4,$5,'hash','{}','queued',0,0,1,clock_timestamp(),clock_timestamp(),'')`,
		withoutIntent, "env/"+r.App+"/"+r.Environment, plain.Actor.Kind, plain.Actor.Subject, "first-deploy:"+r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployments
		(id,operation_id,app_id,environment_id,action,release_id,state,error_code,message,created_at,updated_at,
		revision_id,secret_revisions,instances)
		SELECT 'dep_' || substr($1,4),$1,$2,$3,action,release_id,state,error_code,message,created_at,updated_at,$4,secret_revisions,instances
		FROM eregion_platform_deployments WHERE operation_id=$5`, withoutIntent, r.AppID, r.EnvironmentID, r.ID, create.FirstDeployOperationID); err != nil {
		t.Fatal(err)
	}
	initial, noIntent = revisionPayload{}, revisionPayload{}
	readActionPayload(t, pool, s, create, &initial)
	readActionPayload(t, pool, s, plain, &noIntent)
	if initial.Revisions[0].FirstDeployOperationID != string(create.FirstDeployOperationID) || noIntent.Revisions[0].FirstDeployOperationID != "" {
		t.Fatalf("caller key spoofed first deploy: %+v %+v", initial, noIntent)
	}
	// Removing the true pin must not let the unrelated candidate take over.
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_deployments SET revision_id=NULL WHERE operation_id=$1`, create.FirstDeployOperationID); err != nil {
		t.Fatal(err)
	}
	initial = revisionPayload{}
	readActionPayload(t, pool, s, create, &initial)
	if initial.Revisions[0].FirstDeployOperationID != "" {
		t.Fatalf("unrelated deployment attributed after real pin disappeared: %+v", initial)
	}
}
