package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// These tests exercise the production Service and Runtime wiring: the Service
// owns the typed entities store, and every write in one submission commits or
// rolls back with its domain rows, secret revision, River job and event.
// They use the real PostgreSQL target named by EREGION_TEST_DATABASE_URL and
// construct no transport, HTTP server, Docker command or node emulator.

func cutoverSubmission(kind, target, key, input string) operations.Submission {
	return operations.Submission{
		Kind: kind, Target: target, Actor: testActor,
		IdempotencyKey: key, Input: json.RawMessage(input),
	}
}

func cutoverCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

func cutoverRequireCount(t *testing.T, pool *pgxpool.Pool, want int, query string, args ...any) {
	t.Helper()
	if got := cutoverCount(t, pool, query, args...); got != want {
		t.Fatalf("count %q = %d, want %d", query, got, want)
	}
}

func cutoverMaxConnsOnePool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	config, err := ParseDatabaseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIntegrationCutoverRegisterExtensionsComposition(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)

	// Extension registration is a mandatory production composition step and
	// must fail closed without the typed entities store.
	if _, err := RegisterExtensions(ExtensionConfig{}); err == nil {
		t.Fatal("extension registration accepted a nil entities store")
	}

	extensions, err := RegisterExtensions(ExtensionConfig{
		Entities:     store,
		AccessAdmins: []string{"access-admin"},
		DNSAdmins:    []string{"dns-admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if extensions.Access == nil {
		t.Fatal("production extension composition did not build the typed access controller")
	}
	if extensions.DomainDNS == nil {
		t.Fatal("production extension composition did not build the domain DNS planner")
	}
	for _, kind := range []string{
		integrations.KindAccessGrant,
		integrations.KindAccessRevoke,
		integrations.KindDNSReconcile,
	} {
		if _, found := platform.Default().Lookup(kind); !found {
			t.Fatalf("production extension composition did not register %s", kind)
		}
	}
}

func TestIntegrationCutoverPreviewExplicitArtifactMaxConnsOne(t *testing.T) {
	_, dsn := testPool(t, true)
	// One connection proves the preview never borrows a second pool connection
	// while its repeatable-read snapshot is open.
	pool := cutoverMaxConnsOnePool(t, dsn)
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service

	platformSubmit(t, s, platform.KindNodeRegister, "platform", "preview-node", map[string]any{
		"name":         "local",
		"roles":        []string{platform.RoleApplication},
		"capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "preview-app", map[string]any{
		"name": "cutoverpreview",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"domain": "preview.example.test", "domain_primary": true,
		}},
	})
	normalized, err := platform.Default().Normalize(
		platform.KindDeploy,
		"env/cutoverpreview/production",
		json.RawMessage(`{"image_tag":"v1"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	previewCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	preview, err := s.Preview(previewCtx, testActor, normalized, "")
	if err != nil {
		t.Fatalf("explicit-artifact preview with maxConns=1: %v", err)
	}
	if !preview.DryRun || len(preview.Steps) == 0 || !strings.Contains(preview.Summary, "v1") {
		t.Fatalf("explicit-artifact preview = %+v", preview)
	}
}

func TestIntegrationCutoverBuildAdmissionMaxConnsOne(t *testing.T) {
	_, dsn := testPool(t, true)
	pool := cutoverMaxConnsOnePool(t, dsn)
	store := newTestEntities(t, pool)
	run, err := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()

	platformSubmit(t, s, platform.KindNodeRegister, "platform", "build-node", map[string]any{
		"name":         "local",
		"roles":        []string{platform.RoleApplication},
		"capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "build-app", map[string]any{
		"name": "cutoverbuild",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/cutover", "ref": "main"},
		}},
	})
	var appID, envID, revisionID string
	if err := pool.QueryRow(ctx, `SELECT a.id,e.id,coalesce(e.current_revision_id,'')
		FROM eregion_platform_apps a JOIN eregion_platform_environments e ON e.app_id=a.id
		WHERE a.name='cutoverbuild' AND e.name='production'`).Scan(&appID, &envID, &revisionID); err != nil {
		t.Fatal(err)
	}
	const commit = "0123456789abcdef0123456789abcdef01234567"
	source := `{"type":"git","url":"https://github.com/acme/cutover","ref":"main"}`
	insertOperation := func(id contract.OperationID, target, key string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
			(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,error_code,created_at,updated_at)
			VALUES($1,'platform.deploy',$2,$3,$4,$5,'fixture','{}'::jsonb,'running',1,0,0,'',clock_timestamp(),clock_timestamp())`,
			id, target, testActor.Kind, testActor.Subject, key); err != nil {
			t.Fatalf("insert fixture operation: %v", err)
		}
	}
	insertBuild := func(id platform.BuildID, operationID contract.OperationID, releaseID platform.ReleaseID, state string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_builds
			(id,operation_id,app_id,environment_id,revision_id,release_id,source,commit_sha,state,error_code,error,created_at,started_at)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7::jsonb,$8,$9,'','',clock_timestamp(),clock_timestamp())`,
			id, operationID, appID, envID, revisionID, releaseID, source, commit, state); err != nil {
			t.Fatalf("insert fixture build: %v", err)
		}
	}

	stopOpID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	stopBuildID, err := platform.NewBuildID()
	if err != nil {
		t.Fatal(err)
	}
	insertOperation(stopOpID, "build/"+string(stopBuildID), "fixture-stop-"+string(stopBuildID))
	insertBuild(stopBuildID, stopOpID, "", "running")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if _, err := s.Submit(stopCtx, platformSubmission(platform.KindBuildStop, "build/"+string(stopBuildID), "cutover-build-stop", map[string]any{})); err != nil {
		t.Fatalf("build stop admission with maxConns=1: %v", err)
	}

	retryOpID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	releaseOpID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	retryReleaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	retryBuildID, err := platform.NewBuildID()
	if err != nil {
		t.Fatal(err)
	}
	insertOperation(releaseOpID, "env/cutoverbuild/production", "fixture-retry-release")
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_releases
		(id,operation_id,app_id,environment_id,image,image_id,source,build,command,status,commit_sha,error_code,error,created_at,completed_at,build_id)
		VALUES($1,$2,$3,$4,'','','{}'::jsonb,NULL,NULL,'failed',$5,'','',clock_timestamp(),clock_timestamp(),NULL)`,
		retryReleaseID, releaseOpID, appID, envID, commit); err != nil {
		t.Fatalf("insert fixture release: %v", err)
	}
	insertOperation(retryOpID, "build/"+string(retryBuildID), "fixture-retry-"+string(retryBuildID))
	insertBuild(retryBuildID, retryOpID, retryReleaseID, "failed")
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer retryCancel()
	if _, err := s.Submit(retryCtx, platformSubmission(platform.KindBuildRetry, "build/"+string(retryBuildID), "cutover-build-retry", map[string]any{})); err != nil {
		t.Fatalf("build retry admission with maxConns=1: %v", err)
	}
	cutoverRequireCount(t, pool, 3, `SELECT count(*) FROM eregion_platform_builds`)
	cutoverRequireCount(t, pool, 2, `SELECT count(*) FROM eregion_platform_releases`)
}

func TestIntegrationCutoverManagedDomainAdmissionMaxConnsOne(t *testing.T) {
	_, dsn := testPool(t, true)
	pool := cutoverMaxConnsOnePool(t, dsn)
	store := newTestEntities(t, pool)
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	vaultService, err := newTestVaultService(t, pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	credentials := NewCloudflareCredentialStore(vaultService)
	domainDNS, err := integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones: credentials, Credentials: credentials,
		Authorizer: integrations.NewOperatorDNSAuthorizer(), Audit: store.Audit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, RegistryEndpoint: "127.0.0.1:5000", DomainDNS: domainDNS,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()

	const zoneID = "0123456789abcdef0123456789abcdef"
	scope, err := vault.NewProviderScope(vault.ProviderCloudflare, zoneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := vaultService.Store().SetProviderCredential(ctx, tx.Providers(), scope, "example.com", []byte("cutover-provider-token"), 0, contract.Actor{Kind: "operator", Subject: testActor.Subject})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "domain-node", map[string]any{
		"name":         "local",
		"roles":        []string{platform.RoleApplication},
		"capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "domain-app", map[string]any{
		"name": "cutoverdns",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
		}},
	})
	actor := contract.Actor{Kind: "operator", Subject: testActor.Subject}
	domainCtx, domainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer domainCancel()
	op, err := s.Submit(domainCtx, operations.Submission{
		Kind: platform.KindDomainAdd, Target: "env/cutoverdns/production", Actor: actor,
		IdempotencyKey: "cutover-managed-domain",
		Input:          json.RawMessage(`{"domain":"app.example.com","primary":true,"dns_mode":"cloudflare","dns_zone":"example.com","dns_origins":["8.8.8.8"],"dns_proxied":false}`),
	})
	if err != nil {
		t.Fatalf("managed-domain admission with maxConns=1: %v", err)
	}
	if op.Kind != platform.KindDomainAdd {
		t.Fatalf("managed-domain operation = %+v", op)
	}
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_platform_domains WHERE name='app.example.com'`)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_platform_domain_dns x JOIN eregion_platform_domains d ON d.id=x.domain_id WHERE d.name='app.example.com' AND x.zone_id=$1 AND x.state='pending'`, zoneID)
}

func TestIntegrationCutoverProductionSubmissionAtomicity(t *testing.T) {
	pool, _ := testPool(t, true)
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()

	platformSubmit(t, s, platform.KindNodeRegister, "platform", "cutover-node", map[string]any{
		"name":         "local",
		"roles":        []string{platform.RoleApplication},
		"capabilities": []string{platform.CapabilityDocker},
	})
	baselineJobs := cutoverCount(t, pool, `SELECT count(*) FROM river_job`)
	baselineEvents := cutoverCount(t, pool, `SELECT count(*) FROM eregion_operation_events`)

	const appKey = "cutover-app-create"
	const appInput = `{"name":"cutover","environments":[{"name":"production","node":"local","domain":"cutover.example.test","domain_primary":true,"source":{"url":"https://github.com/acme/cutover","ref":"main"}}]}`
	appSubmission := cutoverSubmission(platform.KindAppCreate, platform.PlatformTarget().String(), appKey, appInput)

	// The seam runs after the app, environment, revision, domain, River job and
	// queued event are all written. Any one surviving would prove a partial
	// commit.
	s.beforeSubmissionCommit = func() error {
		return errors.New("injected production cutover rollback")
	}
	if _, err := s.Submit(ctx, appSubmission); err == nil {
		t.Fatal("injected submission rollback succeeded")
	}
	cutoverRequireCount(t, pool, 0, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, appKey)
	cutoverRequireCount(t, pool, 0, `SELECT count(*) FROM eregion_platform_apps WHERE name='cutover'`)
	cutoverRequireCount(t, pool, 0, `SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='cutover'`)
	cutoverRequireCount(t, pool, 0, `SELECT count(*) FROM eregion_platform_environment_revisions r JOIN eregion_platform_environments e ON e.id=r.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='cutover'`)
	cutoverRequireCount(t, pool, 0, `SELECT count(*) FROM eregion_platform_domains WHERE name='cutover.example.test'`)
	cutoverRequireCount(t, pool, baselineJobs, `SELECT count(*) FROM river_job`)
	cutoverRequireCount(t, pool, baselineEvents, `SELECT count(*) FROM eregion_operation_events`)
	s.beforeSubmissionCommit = nil

	result, err := s.SubmitWithReceipt(ctx, appSubmission)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replay || result.WebhookSecretActivated {
		t.Fatalf("first cutover receipt = %+v", result)
	}
	if result.Operation.State != contract.Queued {
		t.Fatalf("admitted operation state = %s", result.Operation.State)
	}
	op := result.Operation
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1 AND state='queued'`, appKey)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_platform_apps WHERE name='cutover'`)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_platform_domains WHERE name='cutover.example.test' AND is_primary`)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1`, op.ID)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM river_job WHERE args->>'operation_id'=$1`, op.ID)
	cutoverRequireCount(t, pool, baselineJobs+1, `SELECT count(*) FROM river_job`)
	cutoverRequireCount(t, pool, baselineEvents+1, `SELECT count(*) FROM eregion_operation_events`)

	var boundJobID int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(job_id,0) FROM eregion_operations WHERE id=$1`, op.ID).Scan(&boundJobID); err != nil {
		t.Fatal(err)
	}
	if boundJobID <= 0 {
		t.Fatalf("admitted operation has no River job binding: %d", boundJobID)
	}

	// The application boundary, not the entities mapper, evaluates policy from
	// the exact primary-domain DNS fact. The fresh environment has a primary
	// domain with pending DNS and no ready release.
	wantDeployability := func(t *testing.T, env platform.Environment) {
		t.Helper()
		if env.Deployability.DNS != string(platform.DNSPending) {
			t.Fatalf("deployability DNS = %q, want pending", env.Deployability.DNS)
		}
		if env.Deployability.Deployable {
			t.Fatal("fresh environment with no ready release is deployable")
		}
		foundArtifact := false
		for _, missing := range env.Deployability.Missing {
			if missing == "artifact" {
				foundArtifact = true
			}
		}
		if !foundArtifact {
			t.Fatalf("deployability missing = %v, want artifact", env.Deployability.Missing)
		}
	}
	env, err := s.environment(ctx, "cutover", "production")
	if err != nil {
		t.Fatal(err)
	}
	wantDeployability(t, env)
	inventory, err := s.inventoryEnvironments(ctx, platform.InventoryQuery{
		App: "cutover", Environment: "production", Limit: 10,
	})
	if err != nil || len(inventory) != 1 {
		t.Fatalf("inventory environments = %d, %v", len(inventory), err)
	}
	wantDeployability(t, inventory[0])

	// The production Service read/follow projection sees the committed event.
	got, err := s.Get(ctx, testActor, op.ID)
	if err != nil || got.ID != op.ID || got.State != contract.Queued {
		t.Fatalf("Get after admission = %+v, %v", got, err)
	}
	page, err := s.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Sequence != 1 || page.Events[0].State != contract.Queued {
		t.Fatalf("follow projection = %+v", page.Events)
	}

	// A replayed submission returns the same operation and adds no domain,
	// River or event row.
	operationsBefore := cutoverCount(t, pool, `SELECT count(*) FROM eregion_operations`)
	replay, err := s.SubmitWithReceipt(ctx, appSubmission)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay || replay.Operation.ID != op.ID {
		t.Fatalf("replay receipt = %+v", replay)
	}
	cutoverRequireCount(t, pool, operationsBefore, `SELECT count(*) FROM eregion_operations`)
	cutoverRequireCount(t, pool, baselineJobs+1, `SELECT count(*) FROM river_job`)
	cutoverRequireCount(t, pool, baselineEvents+1, `SELECT count(*) FROM eregion_operation_events`)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_platform_domains WHERE name='cutover.example.test'`)

	// The same durable admission key with different inputs is a conflict, not a
	// second application or a rewritten operation.
	conflict := cutoverSubmission(platform.KindAppCreate, platform.PlatformTarget().String(), appKey,
		strings.Replace(appInput, `"name":"cutover"`, `"name":"cutover-conflict"`, 1))
	if _, err := s.SubmitWithReceipt(ctx, conflict); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting replay error = %v", err)
	}
	cutoverRequireCount(t, pool, operationsBefore, `SELECT count(*) FROM eregion_operations`)
	cutoverRequireCount(t, pool, 0, `SELECT count(*) FROM eregion_platform_apps WHERE name='cutover-conflict'`)
	cutoverRequireCount(t, pool, baselineJobs+1, `SELECT count(*) FROM river_job`)
	cutoverRequireCount(t, pool, baselineEvents+1, `SELECT count(*) FROM eregion_operation_events`)
}

func TestIntegrationCutoverWebhookSecretRollback(t *testing.T) {
	pool, _ := testPool(t, true)
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := newTestVaultService(t, pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Vault: secrets, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()

	platformSubmit(t, s, platform.KindNodeRegister, "platform", "cutover-secret-node", map[string]any{
		"name":         "local",
		"roles":        []string{platform.RoleApplication},
		"capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "cutover-secret-app", map[string]any{
		"name": "cutoversecret",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/cutover", "ref": "main"},
		}},
	})

	const enableKey = "cutover-secret-enable"
	enable := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/cutoversecret/production", Actor: testActor,
		IdempotencyKey: enableKey,
		Input:          json.RawMessage(`{"auto_deploy":true}`),
		WebhookSecret:  []byte("cutover-enable-secret"),
	}
	enabled, err := s.SubmitWithReceipt(ctx, enable)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.Replay || !enabled.WebhookSecretActivated {
		t.Fatalf("enable receipt = %+v", enabled)
	}
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_secret_revisions WHERE app='cutoversecret' AND environment='production' AND name=$1`, platform.DefaultWebhookSecretName)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1 AND state='queued'`, enableKey)

	baselineJobs := cutoverCount(t, pool, `SELECT count(*) FROM river_job`)
	baselineEvents := cutoverCount(t, pool, `SELECT count(*) FROM eregion_operation_events`)
	baselineRevisions := cutoverCount(t, pool, `SELECT count(*) FROM eregion_platform_environment_revisions r JOIN eregion_platform_environments e ON e.id=r.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='cutoversecret' AND e.name='production'`)

	const rotateKey = "cutover-secret-rotate-rollback"
	rotate := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/cutoversecret/production", Actor: testActor,
		IdempotencyKey: rotateKey,
		Input:          json.RawMessage(`{"rotate_webhook_secret":true}`),
		WebhookSecret:  []byte("cutover-rotate-secret-rollback"),
	}
	s.beforeSubmissionCommit = func() error {
		return errors.New("injected secret rotation rollback")
	}
	if _, err := s.SubmitWithReceipt(ctx, rotate); err == nil {
		t.Fatal("injected secret rotation rollback succeeded")
	}
	s.beforeSubmissionCommit = nil
	cutoverRequireCount(t, pool, 0, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, rotateKey)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_secret_revisions WHERE app='cutoversecret' AND environment='production' AND name=$1`, platform.DefaultWebhookSecretName)
	cutoverRequireCount(t, pool, baselineRevisions, `SELECT count(*) FROM eregion_platform_environment_revisions r JOIN eregion_platform_environments e ON e.id=r.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='cutoversecret' AND e.name='production'`)
	cutoverRequireCount(t, pool, baselineJobs, `SELECT count(*) FROM river_job`)
	cutoverRequireCount(t, pool, baselineEvents, `SELECT count(*) FROM eregion_operation_events`)

	const rotateKeyOK = "cutover-secret-rotate"
	rotate.IdempotencyKey = rotateKeyOK
	rotate.WebhookSecret = []byte("cutover-rotate-secret")
	rotated, err := s.SubmitWithReceipt(ctx, rotate)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Replay || !rotated.WebhookSecretActivated {
		t.Fatalf("rotation receipt = %+v", rotated)
	}
	cutoverRequireCount(t, pool, 2, `SELECT count(*) FROM eregion_secret_revisions WHERE app='cutoversecret' AND environment='production' AND name=$1`, platform.DefaultWebhookSecretName)
	cutoverRequireCount(t, pool, baselineRevisions, `SELECT count(*) FROM eregion_platform_environment_revisions r JOIN eregion_platform_environments e ON e.id=r.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='cutoversecret' AND e.name='production'`)
	cutoverRequireCount(t, pool, baselineJobs+1, `SELECT count(*) FROM river_job`)
	cutoverRequireCount(t, pool, baselineEvents+1, `SELECT count(*) FROM eregion_operation_events`)
	cutoverRequireCount(t, pool, 1, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1`, rotated.Operation.ID)
	page, err := s.Events(ctx, testActor, contract.Cursor{OperationID: rotated.Operation.ID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Sequence != 1 || page.Events[0].State != contract.Queued {
		t.Fatalf("rotation follow projection = %+v", page.Events)
	}
}
