package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/vault"
)

// These tests deliberately stop at admission: no host effects or River workers
// are needed to inspect the values committed by the real application service.
func TestIntegrationDatabasePolicyAdmissionDefaults(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	submit := func(kind, target, key string, input any) contract.Operation {
		t.Helper()
		op, err := s.Submit(ctx, platformSubmission(kind, target, key, input))
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return op
	}
	node := submit(platform.KindNodeRegister, "platform", "policy-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	app := submit(platform.KindAppCreate, "platform", "policy-app", map[string]any{
		"name": "policy", "environments": []map[string]any{
			{"name": "production", "node": "local", "domain": "policy.example.com"},
			{"name": "staging", "node": "local"},
			{"name": "preview", "node": "local"},
		},
	})
	extra := submit(platform.KindEnvironmentCreate, "app/policy", "policy-extra", map[string]any{
		"name": "review", "node": "local",
	})
	resource := submit(platform.KindResourceApply, "resource/policy/production/cache", "policy-resource", map[string]any{
		"image": "redis:7", "health": map[string]any{"type": "exec", "command": []string{"true"}},
	})
	for _, op := range []contract.Operation{node, app, extra, resource} {
		var ok bool
		err := pool.QueryRow(ctx, `SELECT state='queued' AND attempt=0 AND completed_steps=0 AND last_sequence=1
			AND error_code='' AND job_id IS NOT NULL AND created_at IS NOT NULL AND updated_at IS NOT NULL
			FROM eregion_operations WHERE id=$1`, op.ID).Scan(&ok)
		if err != nil || !ok {
			t.Fatalf("operation %s initial state: ok=%v err=%v", op.ID, ok, err)
		}
	}
	checks := []struct{ name, query string }{
		{"node", `SELECT address='' AND status='registered' AND enrollment_state='registered'
			AND enrollment_error='' AND transport='local' AND build_concurrency=1 AND build_user=''
			AND NOT wg_enabled AND wg_state='none' AND last_check_state='unknown'
			FROM eregion_platform_nodes WHERE name='local'`},
		{"app", `SELECT image_repository=$1 FROM eregion_platform_apps WHERE name='policy'`},
		{"domain", `SELECT is_primary AND desired_revision=1 AND applied_revision=0 AND route_state='pending'
			AND route_error='' AND dns_state='pending' AND cardinality(dns_addresses)=0
			AND cardinality(expected_addresses)=0 AND dns_error='' AND certificate_state='pending'
			AND certificate_error='' AND public_state='pending' AND public_error=''
			AND check_path='' AND observed_at IS NULL FROM eregion_platform_domains WHERE name='policy.example.com'`},
		{"resource", `SELECT state='pending' AND health_state='unknown' AND health_detail=''
			AND image='redis:7' AND image_id='' AND image_digest='' AND container_name='' AND network_name=''
			AND volumes='[]'::jsonb AND error_code='' AND error='' AND destroyed_at IS NULL
			FROM eregion_platform_resources WHERE name='cache'`},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			var ok bool
			var err error
			if check.name == "app" {
				err = pool.QueryRow(ctx, check.query, "policy").Scan(&ok)
			} else {
				err = pool.QueryRow(ctx, check.query).Scan(&ok)
			}
			if err != nil || !ok {
				t.Fatalf("initial state: ok=%v err=%v", ok, err)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		protected bool
		op        contract.Operation
	}{{"production", true, app}, {"staging", true, app}, {"preview", false, app}, {"review", false, extra}} {
		t.Run(tc.name, func(t *testing.T) {
			var ok bool
			err := pool.QueryRow(ctx, `SELECT e.protected=$2 AND e.bind_address=$3 AND e.health_type='http'
				AND e.health_path=$4 AND e.health_timeout_seconds=$5 AND e.env='{}'::jsonb
				AND e.secret_refs='{}'::jsonb AND e.secret_revision=0 AND e.health_state='unknown'
				AND e.health_detail='' AND e.health_checked_at IS NULL AND e.desired_release_id IS NULL
				AND e.applied_release_id IS NULL AND e.serving_release_id IS NULL
				AND r.number=1 AND r.operation_id=$6 AND r.actor_kind=$7 AND r.actor_subject=$8
				AND NOT r.auto_deploy AND NOT r.deploy_intent AND cardinality(r.secret_names)=0
				AND cardinality(r.resources)=0
				FROM eregion_platform_environments e JOIN eregion_platform_environment_revisions r
				ON r.id=e.current_revision_id AND r.environment_id=e.id WHERE e.name=$1`,
				tc.name, tc.protected, "127.0.0.1", "/", 30,
				tc.op.ID, testActor.Kind, testActor.Subject).Scan(&ok)
			if err != nil || !ok {
				t.Fatalf("environment and revision: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestIntegrationDatabasePolicyInvalidAdmissionIsAtomic(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name, kind, target string
		input              any
	}{
		{"node-role", platform.KindNodeRegister, "platform", map[string]any{"name": "badnode", "roles": []string{"invented"}}},
		{"health-type", platform.KindAppCreate, "platform", map[string]any{"name": "badhealth", "environments": []map[string]any{{"name": "production", "node": "local", "health": map[string]any{"type": "invented"}}}}},
		{"process-kind", platform.KindAppCreate, "platform", map[string]any{"name": "badprocess", "environments": []map[string]any{{"name": "production", "node": "local", "processes": []map[string]any{{"name": "worker", "kind": "invented"}}}}}},
		{"bind-format", platform.KindAppCreate, "platform", map[string]any{"name": "badbind", "environments": []map[string]any{{"name": "production", "node": "local", "bind": "not-an-ip"}}}},
		{"port-bounds", platform.KindAppCreate, "platform", map[string]any{"name": "badport", "environments": []map[string]any{{"name": "production", "node": "local", "container_port": 65536}}}},
		{"resource-spec", platform.KindResourceApply, "resource/policy/production/cache", map[string]any{"image": "redis:7", "health": map[string]any{"type": "invented"}}},
		{"domain-proxy-type", platform.KindDomainAdd, "env/policy/production", map[string]any{"domain": "bad.example.com", "dns_mode": "cloudflare", "dns_proxied": "invented"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before, after [4]int
			count := func(out *[4]int) {
				t.Helper()
				if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations),
					(SELECT count(*) FROM river_job), (SELECT count(*) FROM eregion_platform_apps),
					(SELECT count(*) FROM eregion_platform_resources)`).Scan(&out[0], &out[1], &out[2], &out[3]); err != nil {
					t.Fatal(err)
				}
			}
			count(&before)
			_, err := s.Submit(ctx, platformSubmission(tc.kind, tc.target, "invalid-"+tc.name, tc.input))
			if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
				t.Fatalf("expected validation failure, got %v", err)
			}
			count(&after)
			if before != after {
				t.Fatalf("invalid admission changed row counts: before=%v after=%v", before, after)
			}
		})
	}
}

func TestIntegrationDatabasePolicyPartialAppCreationRollsBack(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Submit(ctx, platformSubmission(platform.KindNodeRegister, "platform", "node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})); err != nil {
		t.Fatal(err)
	}
	counts := func() [5]int {
		t.Helper()
		var result [5]int
		err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations),
			(SELECT count(*) FROM river_job), (SELECT count(*) FROM eregion_platform_apps),
			(SELECT count(*) FROM eregion_platform_environments),
			(SELECT count(*) FROM eregion_platform_environment_revisions)`).
			Scan(&result[0], &result[1], &result[2], &result[3], &result[4])
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := counts()
	_, err := s.Submit(ctx, platformSubmission(platform.KindAppCreate, "platform", "partial-app", map[string]any{
		"name": "partial", "environments": []map[string]any{
			{"name": "production", "node": "local"},
			{"name": "staging", "node": "missing"},
		},
	}))
	if err == nil || contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("missing second node: %v", err)
	}
	if after := counts(); before != after {
		t.Fatalf("partial creation committed: before=%v after=%v", before, after)
	}
}

func TestIntegrationDatabasePolicyCorruptResourceFailsClosed(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, item := range []struct {
		kind, target, key string
		input             any
	}{
		{platform.KindNodeRegister, "platform", "node", map[string]any{"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker}}},
		{platform.KindAppCreate, "platform", "app", map[string]any{"name": "policy", "environments": []map[string]any{{"name": "production", "node": "local"}}}},
		{platform.KindResourceApply, "resource/policy/production/cache", "resource", map[string]any{"image": "redis:7", "health": map[string]any{"type": "exec", "command": []string{"true"}}}},
	} {
		if _, err := s.Submit(ctx, platformSubmission(item.kind, item.target, item.key, item.input)); err != nil {
			t.Fatalf("%s: %v", item.key, err)
		}
	}
	query := platform.InventoryQuery{Collection: "resources", App: "policy", Environment: "production", Limit: 10}
	if _, err := s.Inventory(ctx, testActor, query); err != nil {
		t.Fatalf("valid resource inventory: %v", err)
	}
	for _, tc := range []struct{ name, column, bad string }{
		{"state", "state", "'invented'"},
		{"health", "health_state", "'invented'"},
		{"spec", "spec", `'{"image":"","health":{"type":"invented"}}'::jsonb`},
		{"volume-ownership", "volumes", `'[{"name":"er-res-other-data","destination":"/data"}]'::jsonb`},
		{"volume-null", "volumes", `'null'::jsonb`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Preserve and restore the service-produced value for each independent case.
			var original string
			if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT %s::text FROM eregion_platform_resources WHERE name='cache'", tc.column)).Scan(&original); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, fmt.Sprintf("UPDATE eregion_platform_resources SET %s=%s WHERE name='cache'", tc.column, tc.bad)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := pool.Exec(ctx, fmt.Sprintf("UPDATE eregion_platform_resources SET %s=$1 WHERE name='cache'", tc.column), original); err != nil {
					t.Errorf("restore %s: %v", tc.column, err)
				}
			})
			_, err := s.Inventory(ctx, testActor, query)
			if err == nil || contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("corrupt %s inventory: %v", tc.name, err)
			}
		})
	}
	if _, err := s.Inventory(ctx, testActor, query); err != nil {
		t.Fatalf("restored resource inventory: %v", err)
	}
}

func TestIntegrationDatabasePolicyGitDeploymentAndStoredSecretIntegrity(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	submit := func(kind, target, key string, input any) contract.Operation {
		t.Helper()
		op, err := s.Submit(ctx, platformSubmission(kind, target, key, input))
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return op
	}
	submit(platform.KindNodeRegister, "platform", "node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	})
	submit(platform.KindAppCreate, "platform", "app", map[string]any{
		"name": "deploypolicy", "environments": []map[string]any{{
			"name": "production", "node": "local", "container_port": 8080,
			"domain": "deploypolicy.example.com",
			"source": map[string]any{"url": "https://github.com/acme/app.git", "ref": "main", "builder": "paketobuildpacks/builder-jammy-base"},
		}},
	})
	// Only the external git lookup is replaced; admission and all rows are real.
	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) {
		return "0123456789abcdef0123456789abcdef01234567", nil
	}
	op := submit(platform.KindDeploy, "env/deploypolicy/production", "deploy", map[string]any{})
	var envID platform.EnvironmentID
	var releaseID platform.ReleaseID
	var deploymentID platform.DeploymentID
	var ok bool
	err := pool.QueryRow(ctx, `SELECT e.id,r.id,d.id,
		r.status='pending' AND r.image_id='' AND r.error_code='' AND r.error=''
		AND r.created_at IS NOT NULL AND r.completed_at IS NULL AND r.pruned_at IS NULL
		AND r.source->>'type'='git' AND r.source->>'ref'='main' AND r.build IS NOT NULL
		AND r.commit_sha='0123456789abcdef0123456789abcdef01234567'
		AND b.state='pending' AND b.error_code='' AND b.error='' AND b.started_at IS NULL
		AND b.completed_at IS NULL AND b.created_at IS NOT NULL AND b.commit_sha=r.commit_sha
		AND b.revision_id=e.current_revision_id AND b.release_id=r.id
		AND d.action='deploy' AND d.state='pending' AND d.error_code='' AND d.message=''
		AND d.secret_revisions='{}'::jsonb AND d.instances='[]'::jsonb
		AND d.previous_release_id IS NULL AND d.previous_deployment_id IS NULL
		AND d.completed_at IS NULL AND d.revision_id=e.current_revision_id
		AND e.desired_release_id=r.id
		FROM eregion_platform_releases r
		JOIN eregion_platform_builds b ON b.id=r.build_id
		JOIN eregion_platform_deployments d ON d.operation_id=r.operation_id
		JOIN eregion_platform_environments e ON e.id=r.environment_id
		WHERE r.operation_id=$1`, op.ID).Scan(&envID, &releaseID, &deploymentID, &ok)
	if err != nil || !ok {
		t.Fatalf("git admission defaults: ok=%v err=%v", ok, err)
	}

	// Exercise internal services against the real release and operation, not
	// rows made solely to satisfy the command/cleanup foreign keys.
	digest := platform.ReleaseCommandDigest(platform.CommandSpec{Argv: []string{"bin/migrate"}, TimeoutSeconds: 120})
	if err := s.startReleaseCommandRun(ctx, releaseID, digest, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state='running' AND error_code='' AND error=''
		AND started_at IS NOT NULL AND completed_at IS NULL
		FROM eregion_platform_release_command_runs WHERE release_id=$1 AND command_digest=$2`,
		releaseID, digest).Scan(&ok); err != nil || !ok {
		t.Fatalf("release command initial row: ok=%v err=%v", ok, err)
	}
	if err := s.finishReleaseCommandRun(ctx, releaseID, digest, op.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state='succeeded' AND error_code='' AND error=''
		AND completed_at>=started_at FROM eregion_platform_release_command_runs
		WHERE release_id=$1 AND command_digest=$2`, releaseID, digest).Scan(&ok); err != nil || !ok {
		t.Fatalf("release command completion: ok=%v err=%v", ok, err)
	}
	if err := s.recordContainerCleanup(ctx, "eregion-policy-old", envID, "local", deploymentID, "previous",
		contract.NewFault(contract.PreconditionFailed, "cleanup failed", "retry")); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT attempts=1 AND last_error<>'' AND created_at IS NOT NULL
		AND next_attempt_at>created_at AND deployment_id=$1 AND reason='previous'
		FROM eregion_platform_container_cleanup WHERE container_name='eregion-policy-old'`, deploymentID).Scan(&ok); err != nil || !ok {
		t.Fatalf("cleanup initial row: ok=%v err=%v", ok, err)
	}

	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := vault.NewNamespace("deploypolicy", "production", "API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := v.Store().Set(ctx, tx, ns, []byte("integration-secret-value"), 0, testActor); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT s.latest_revision=1 AND s.state='active' AND s.archived_at IS NULL
		AND s.created_by_kind=$1 AND s.created_by_subject=$2 AND s.updated_by_kind=$1 AND s.updated_by_subject=$2
		AND s.created_at IS NOT NULL AND s.updated_at>=s.created_at AND r.revision=1
		AND r.key_id<>'' AND length(r.ciphertext)>0 AND length(r.ciphertext_sha256)=64
		AND length(r.revision_hash)=64 AND r.operation_id='' AND r.created_by_kind=$1
		AND r.created_by_subject=$2 AND r.created_at IS NOT NULL
		FROM eregion_secrets s JOIN eregion_secret_revisions r USING(app,environment,name)
		WHERE s.app='deploypolicy' AND s.environment='production' AND s.name='API_KEY'`,
		testActor.Kind, testActor.Subject).Scan(&ok); err != nil || !ok {
		t.Fatalf("vault metadata: ok=%v err=%v", ok, err)
	}
	scope, err := vault.NewProviderScope("cloudflare", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	providerTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(providerTx)
	if _, err := v.Store().SetProviderCredential(ctx, providerTx, scope, "example.com",
		[]byte("integration-provider-value"), 0, testActor); err != nil {
		t.Fatal(err)
	}
	if err := providerTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT p.latest_revision=1 AND p.zone_name='example.com'
		AND p.created_by_kind=$1 AND p.updated_by_subject=$2 AND p.created_at IS NOT NULL
		AND p.updated_at>=p.created_at AND r.revision=1 AND r.key_id<>'' AND length(r.ciphertext)>0
		AND length(r.ciphertext_sha256)=64 AND length(r.revision_hash)=64
		AND r.created_by_kind=$1 AND r.created_by_subject=$2 AND r.created_at IS NOT NULL
		FROM eregion_provider_credentials p JOIN eregion_provider_credential_revisions r USING(provider,zone_id)
		WHERE p.provider='cloudflare' AND p.zone_id=$3`, testActor.Kind, testActor.Subject, scope.ZoneID).Scan(&ok); err != nil || !ok {
		t.Fatalf("provider credential metadata: ok=%v err=%v", ok, err)
	}
	audit, err := integrations.NewPostgresAudit(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"policy.first", "policy.second"} {
		if err := audit.Record(ctx, integrations.AuditEntry{At: time.Now().UTC(), Actor: testActor.Subject,
			Action: action, Target: "deploypolicy/production", Outcome: "succeeded"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*)=2 AND min(id)=1 AND max(id)=2
		AND bool_and(recorded_at IS NOT NULL AND entry->>'outcome'='succeeded')
		FROM eregion_integration_audit`).Scan(&ok); err != nil || !ok {
		t.Fatalf("integration audit identities: ok=%v err=%v", ok, err)
	}
	// A snapshot identity is populated from an actual encrypted vault revision.
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_deployments
		SET secret_revisions=(SELECT jsonb_build_object('API_KEY',jsonb_build_object(
			'revision',revision,'hash',revision_hash,'key_id',key_id,'created_at',created_at))
			FROM eregion_secret_revisions WHERE app='deploypolicy' AND environment='production' AND name='API_KEY' AND revision=1)
		WHERE id=$1`, deploymentID); err != nil {
		t.Fatal(err)
	}
	query := platform.InventoryQuery{Collection: "deployments", App: "deploypolicy", Environment: "production", Limit: 10}
	if _, err := s.Inventory(ctx, testActor, query); err != nil {
		t.Fatalf("vault-produced identity rejected: %v", err)
	}
	var original []byte
	if err := pool.QueryRow(ctx, `SELECT secret_revisions FROM eregion_platform_deployments WHERE id=$1`, deploymentID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, expression string }{
		{"zero-revision", `jsonb_set(secret_revisions,'{API_KEY,revision}','0'::jsonb)`},
		{"bad-hash", `jsonb_set(secret_revisions,'{API_KEY,hash}','"bad"'::jsonb)`},
		{"bad-key-id", `jsonb_set(secret_revisions,'{API_KEY,key_id}','""'::jsonb)`},
		{"bad-name", `jsonb_build_object('bad-name',secret_revisions->'API_KEY')`},
		{"null-map", `'null'::jsonb`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `UPDATE eregion_platform_deployments SET secret_revisions=`+tc.expression+` WHERE id=$1`, deploymentID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := pool.Exec(ctx, `UPDATE eregion_platform_deployments SET secret_revisions=$2 WHERE id=$1`, deploymentID, original); err != nil {
					t.Errorf("restore snapshot: %v", err)
				}
			})
			_, err := s.Inventory(ctx, testActor, query)
			if err == nil || contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("corrupt snapshot must fail closed: %v", err)
			}
		})
	}
	if _, err := s.Inventory(ctx, testActor, query); err != nil {
		t.Fatalf("restored snapshot: %v", err)
	}
}

func TestIntegrationDatabasePolicyRetentionSettings(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	want := retention.Policy{KeepReleases: 10, KeepNewerThanDays: 14, ArtifactGraceDays: 14,
		BuildpackCacheMaxBytes: 10737418240, BuildpackCacheMaxPercent: 80}
	got, err := s.ServerRetentionPolicy(ctx)
	if err != nil || got != want {
		t.Fatalf("unstored defaults: got=%+v err=%v", got, err)
	}
	if err := s.SetServerRetentionPolicy(ctx, got); err != nil {
		t.Fatal(err)
	}
	var ok bool
	err = pool.QueryRow(ctx, `SELECT singleton AND NOT retention_gc_pending AND updated_at IS NOT NULL
		AND retention='{"keep_releases":10,"keep_newer_than_days":14,"artifact_grace_days":14,
		"buildpack_cache_max_bytes":10737418240,"buildpack_cache_max_percent":80}'::jsonb
		FROM eregion_server_settings`).Scan(&ok)
	if err != nil || !ok {
		t.Fatalf("stored settings: ok=%v err=%v", ok, err)
	}
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY singleton),'[]'::jsonb)::text
			FROM eregion_server_settings s`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	beforeSettings := snapshot()
	var beforeOps, beforeJobs int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations),
		(SELECT count(*) FROM river_job)`).Scan(&beforeOps, &beforeJobs); err != nil {
		t.Fatal(err)
	}
	bad := got
	bad.KeepReleases = 0
	if err := s.SetServerRetentionPolicy(ctx, bad); err == nil || contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("invalid retention policy: %v", err)
	}
	if after := snapshot(); after != beforeSettings {
		t.Fatalf("invalid policy changed settings: before=%s after=%s", beforeSettings, after)
	}
	var afterOps, afterJobs int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations),
		(SELECT count(*) FROM river_job)`).Scan(&afterOps, &afterJobs); err != nil {
		t.Fatal(err)
	}
	if beforeOps != afterOps || beforeJobs != afterJobs {
		t.Fatalf("invalid policy changed queue counts: operations %d -> %d, jobs %d -> %d",
			beforeOps, afterOps, beforeJobs, afterJobs)
	}
}

func TestIntegrationDatabasePolicyHTTPResourceAdmission(t *testing.T) {
	pool, _ := testPool(t, true)
	s := testRuntime(t, pool, 0).Service
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, item := range []struct {
		kind, target, key string
		input             any
	}{
		{platform.KindNodeRegister, "platform", "http-node", map[string]any{"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker}}},
		{platform.KindAppCreate, "platform", "http-app", map[string]any{"name": "httpapp", "environments": []map[string]any{{"name": "production", "node": "local"}}}},
	} {
		if _, err := s.Submit(ctx, platformSubmission(item.kind, item.target, item.key, item.input)); err != nil {
			t.Fatalf("%s: %v", item.key, err)
		}
	}
	const target = "resource/httpapp/production/cache"
	token := strings.Repeat("d", 64)
	hash := sha256.Sum256([]byte(token))
	auth, err := api.NewAuth([]api.Principal{{
		Actor: testActor, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: []string{"operations:submit"},
		ActionGrants: []api.ActionGrant{{Kind: platform.KindResourceApply, Target: target}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.NewHandler(auth, s, func(ctx context.Context) error { return Ready(ctx, pool) }))
	defer server.Close()
	client, err := api.NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	counts := func() [3]int {
		t.Helper()
		var c [3]int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM eregion_operations),
			(SELECT count(*) FROM river_job), (SELECT count(*) FROM eregion_platform_resources)`).
			Scan(&c[0], &c[1], &c[2]); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"invalid-health", `{"image":"redis:7","health":{"type":"invented","command":["true"]}}`, false},
		{"valid", `{"image":"redis:7","health":{"type":"exec","command":["true"]}}`, true},
		{"invalid-update", `{"image":"redis:7","health":{"type":"invented","command":["true"]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := counts()
			snapshot := func() string {
				t.Helper()
				var raw string
				if err := pool.QueryRow(ctx, `SELECT coalesce((SELECT to_jsonb(r)::text
					FROM eregion_platform_resources r WHERE r.name='cache'),'')`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			beforeResource := snapshot()
			raw, err := client.SubmitAction(ctx, platform.KindResourceApply, target, json.RawMessage(tc.input), "http-"+tc.name, false)
			if !tc.valid {
				if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
					t.Fatalf("HTTP invalid health: %v", err)
				}
				if after := counts(); before != after {
					t.Fatalf("invalid HTTP admission committed: before=%v after=%v", before, after)
				}
				if after := snapshot(); beforeResource != after {
					t.Fatalf("invalid HTTP admission changed existing resource: before=%s after=%s", beforeResource, after)
				}
				return
			}
			if err != nil {
				t.Fatalf("authorized HTTP admission: %v", err)
			}
			var op contract.Operation
			if err := json.Unmarshal(raw, &op); err != nil || op.State != contract.Queued {
				t.Fatalf("HTTP response=%s err=%v", raw, err)
			}
			after := counts()
			if after != [3]int{before[0] + 1, before[1] + 1, before[2] + 1} {
				t.Fatalf("valid HTTP admission counts before=%v after=%v", before, after)
			}
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT state='pending' AND health_state='unknown' AND volumes='[]'::jsonb
				AND image_id='' AND error_code='' AND operation_id=$1 FROM eregion_platform_resources WHERE name='cache'`,
				op.ID).Scan(&ok); err != nil || !ok {
				t.Fatalf("HTTP resource defaults: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestIntegrationDatabasePolicyDeploymentWorkerTransitions(t *testing.T) {
	f := newDeploymentRetryFixture(t) // testPool gates and migrates before fixture setup.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release := f.createRelease("policy-worker", "registry.example/live:policy")
	op, err := f.service.Submit(ctx, platformSubmission(platform.KindDeploy, "env/live/production", "policy-worker",
		map[string]any{"release_id": release}))
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	err = f.pool.QueryRow(ctx, `SELECT d.state='pending' AND d.error_code='' AND d.message=''
		AND d.completed_at IS NULL AND d.instances='[]'::jsonb AND o.state='queued' AND o.attempt=0
		FROM eregion_platform_deployments d JOIN eregion_operations o ON o.id=d.operation_id
		WHERE d.operation_id=$1`, op.ID).Scan(&ok)
	if err != nil || !ok {
		t.Fatalf("pre-worker state: ok=%v err=%v", ok, err)
	}
	if err := runPlatformAttempt(ctx, f.service, op.ID, 1); err != nil {
		t.Fatalf("deployment worker: %v", err)
	}
	err = f.pool.QueryRow(ctx, `SELECT d.state='succeeded' AND d.error_code='' AND d.completed_at IS NOT NULL
		AND o.state='succeeded' AND o.attempt=1 AND r.status='ready' AND r.error_code=''
		AND e.serving_deployment_id=d.id AND e.serving_release_id=r.id
		FROM eregion_platform_deployments d JOIN eregion_operations o ON o.id=d.operation_id
		JOIN eregion_platform_releases r ON r.id=d.release_id
		JOIN eregion_platform_environments e ON e.id=d.environment_id
		WHERE d.operation_id=$1`, op.ID).Scan(&ok)
	if err != nil || !ok {
		t.Fatalf("post-worker state: ok=%v err=%v", ok, err)
	}
}

func TestIntegrationDatabasePolicyRetentionWorkerRunLifecycle(t *testing.T) {
	f := newRetentionFixture(t) // fake node effects only; durable planning and execution use PostgreSQL.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	op, err := f.service.Submit(ctx, platformSubmission(platform.KindRetentionPrune, "node/local", "policy-retention", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	var sawRunning bool
	f.transport.onRun = func(string) {
		if sawRunning {
			return
		}
		var ok bool
		err := f.pool.QueryRow(ctx, `SELECT status='running' AND reclaimed_bytes=0
			AND removed_items='[]'::jsonb AND errors='[]'::jsonb AND finished_at IS NULL
			AND cache_measurement_error='' AND gc_generation=0 AND started_at IS NOT NULL
			FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).Scan(&ok)
		if err != nil || !ok {
			t.Errorf("initial running retention row: ok=%v err=%v", ok, err)
		}
		sawRunning = true
	}
	if err := (&retentionWorker{service: f.service}).Work(ctx, retentionWorkerJob(op.ID, 1, 8)); err != nil {
		t.Fatalf("retention worker: %v", err)
	}
	if !sawRunning {
		t.Fatal("worker did not execute a node effect")
	}
	var ok bool
	err = f.pool.QueryRow(ctx, `SELECT r.status='succeeded' AND r.finished_at>=r.started_at
		AND r.errors='[]'::jsonb AND r.planned_bytes=100 AND r.reclaimed_bytes=100
		AND o.state='succeeded' AND o.attempt=1
		FROM eregion_retention_runs r JOIN eregion_operations o ON o.id=r.operation_id
		WHERE r.operation_id=$1`, op.ID).Scan(&ok)
	if err != nil || !ok {
		t.Fatalf("completed retention run: ok=%v err=%v", ok, err)
	}
}
