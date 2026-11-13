package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

const webhookIntegrationSecretValue = "0123456789abcdef"

// These integration cases need an explicit disposable PostgreSQL DSN. They are
// skipped unless EREGION_TEST_DATABASE_URL is set, and they never read an
// ambient production DSN or start Docker.
func webhookIntegrationFixture(t *testing.T, pool *pgxpool.Pool, run *Runtime) (api.WebhookAdmission, *vault.Service) {
	t.Helper()
	ctx := context.Background()
	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	revisionID, err := platform.NewEnvironmentRevisionID()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureNode(ctx, pool, "local", []string{"application", "builder", "control-plane"}, []string{"docker", "buildpacks"}, "127.0.0.1:2222"); err != nil {
		t.Fatal(err)
	}
	appName := "shop-" + strings.TrimPrefix(string(envID), "env_")[:8]
	if err := insertFixtureApp(ctx, pool, string(appID), appName, appName); err != nil {
		t.Fatal(err)
	}
	var hostPort int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(host_port),29998)+2 FROM eregion_platform_environments`).Scan(&hostPort); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, string(envID), string(appID), "production", "local", hostPort, hostPort+1, 3000); err != nil {
		t.Fatal(err)
	}
	source, _ := json.Marshal(platform.SourceBinding{URL: "https://github.com/mbvlabs/hello.git", Ref: "main"})
	runtime, _ := json.Marshal(platform.RuntimeSpec{Node: "local", HealthType: "http", HealthPath: "/health", HealthTimeoutSeconds: 30})
	processes, _ := json.Marshal([]platform.ProcessSpec{{Name: "web", Kind: "web", Command: "./app", Replicas: 1, ContainerPort: 3000}})
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environment_revisions
        (id,environment_id,number,actor_kind,actor_subject,operation,summary,source,auto_deploy,
         webhook_secret_name,runtime,processes,config,secret_names,deploy_intent,created_at,retention,resources)
        VALUES($1,$2,1,'system','integration','environment.created','integration fixture',
         $3,true,'WEBHOOK_SECRET',$4,$5,'{}'::jsonb,'{}',false,clock_timestamp(),'{}'::jsonb,ARRAY[]::text[])`, revisionID, envID, source, runtime, processes); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environments SET current_revision_id=$2 WHERE id=$1`, envID, revisionID); err != nil {
		t.Fatal(err)
	}
	domain := appName + ".example.test"
	insertFixtureDomain(t, ctx, pool, domain, string(envID), true, time.Now())
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := vault.NewNamespace(appName, "production", "WEBHOOK_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := secrets.Set(ctx, namespace, []byte(webhookIntegrationSecretValue), 0, contract.Actor{Kind: "system", Subject: "webhook-integration"})
	if err != nil {
		t.Fatal(err)
	}
	record := validWebhookAdmission()
	record.DeliveryIDHash = api.WebhookDeliveryHash("integration-delivery-" + string(envID))
	record.BodySHA256 = api.WebhookBodyHash([]byte("integration body"))
	record.Target = "env/" + appName + "/production"
	record.EnvironmentID = string(envID)
	record.RevisionID = string(revisionID)
	record.SecretRevision = metadata.Revision.Revision
	record.SecretRevisionHash = metadata.Revision.Hash
	record.Repository = "mbvlabs/hello"
	record.Branch = "main"
	record.Commit = strings.Repeat("d", 40)
	return record, secrets
}

func webhookRecordApp(t *testing.T, record api.WebhookAdmission) (string, string) {
	t.Helper()
	target, err := platform.ParseTarget(record.Target)
	if err != nil {
		t.Fatal(err)
	}
	return target.App, target.Environment
}

func newWebhookIntegrationHandler(t *testing.T, pool *pgxpool.Pool, run *Runtime, secrets *vault.Service) *api.PushWebhookHandler {
	t.Helper()
	handler, err := api.NewPushWebhookHandler(api.PushWebhookConfig{
		Resolver: NewPushWebhookResolver(pool, secrets), Admissions: run.WebhookDeliveries,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func webhookIntegrationRequest(appName, envName, deliveryID string, body []byte) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/"+appName+"/"+envName, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	mac := hmac.New(sha256.New, []byte(webhookIntegrationSecretValue))
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return request
}

func webhookIntegrationPushBody(commit string) []byte {
	return []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"deleted":false,"repository":{"full_name":"mbvlabs/hello"}}`, commit))
}

type webhookIntegrationResponse struct {
	Accepted    bool   `json:"accepted"`
	Ambiguous   bool   `json:"ambiguous"`
	OperationID string `json:"operation_id"`
	Message     string `json:"message"`
}

func TestIntegrationPushWebhookResolverReadsFreshSecret(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, secrets := webhookIntegrationFixture(t, pool, run)
	appName, envName := webhookRecordApp(t, record)
	resolver := NewPushWebhookResolver(pool, secrets)
	target, enabled, err := resolver.ResolvePushWebhook(ctx, appName, envName)
	if err != nil || !enabled {
		t.Fatalf("resolve enabled=%v err=%v", enabled, err)
	}
	if target.EnvironmentID != record.EnvironmentID || target.RevisionID != record.RevisionID {
		t.Fatalf("snapshot environment=%q revision=%q", target.EnvironmentID, target.RevisionID)
	}
	if string(target.Secret) != webhookIntegrationSecretValue || target.SecretRevision != record.SecretRevision {
		t.Fatalf("secret identity revision=%d hash=%s", target.SecretRevision, target.SecretRevisionHash)
	}
	namespace, _ := vault.NewNamespace(appName, envName, "WEBHOOK_SECRET")
	rotated, err := secrets.Set(ctx, namespace, []byte("fedcba9876543210"), record.SecretRevision, contract.Actor{Kind: "system", Subject: "webhook-integration"})
	if err != nil {
		t.Fatal(err)
	}
	target, enabled, err = resolver.ResolvePushWebhook(ctx, appName, envName)
	if err != nil || !enabled {
		t.Fatalf("resolve after rotation enabled=%v err=%v", enabled, err)
	}
	if string(target.Secret) != "fedcba9876543210" || target.SecretRevision != rotated.Revision.Revision {
		t.Fatalf("rotated secret identity revision=%d hash=%s", target.SecretRevision, target.SecretRevisionHash)
	}
}

func TestIntegrationWebhookAdmissionQueuesAtomically(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	admitted, created, err := run.WebhookDeliveries.Admit(ctx, record)
	if err != nil || !created {
		t.Fatalf("Admit created=%v err=%v", created, err)
	}
	if admitted.State != api.WebhookDeliveryAdmitted || admitted.JobID == 0 {
		t.Fatalf("admitted = %+v", admitted)
	}
	var storedJobID int64
	if err := pool.QueryRow(ctx, `SELECT job_id FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&storedJobID); err != nil || storedJobID != admitted.JobID {
		t.Fatalf("receipt job_id=%d admitted=%d err=%v", storedJobID, admitted.JobID, err)
	}
	var queue, state, argHash, argEnvironmentID string
	var argKeys int64
	if err := pool.QueryRow(ctx, `SELECT queue,state,args->>'delivery_id_hash',args->>'environment_id',
        (SELECT count(*) FROM jsonb_object_keys(args)) FROM river_job WHERE id=$1`, admitted.JobID).
		Scan(&queue, &state, &argHash, &argEnvironmentID, &argKeys); err != nil {
		t.Fatal(err)
	}
	if queue != webhookQueue || state != "available" {
		t.Fatalf("job queue=%q state=%q", queue, state)
	}
	if argHash != record.DeliveryIDHash || argEnvironmentID != record.EnvironmentID || argKeys != 2 {
		t.Fatalf("job args environment=%q hash=%q keys=%d", argEnvironmentID, argHash, argKeys)
	}
	var operations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("synchronous operations=%d err=%v", operations, err)
	}
	again, created, err := run.WebhookDeliveries.Admit(ctx, record)
	if err != nil || created || again.State != api.WebhookDeliveryAdmitted {
		t.Fatalf("duplicate created=%v state=%s err=%v", created, again.State, err)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, record.DeliveryIDHash).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}
	var rowText string
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(d)::text FROM eregion_webhook_deliveries d WHERE d.delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&rowText); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"integration body", "0123456789abcdef", "integration-delivery-"} {
		if strings.Contains(rowText, forbidden) {
			t.Fatalf("delivery row contains forbidden raw value %q: %s", forbidden, rowText)
		}
	}
}

func TestIntegrationWebhookAdmissionConflictLeavesNoSpeculativeJob(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	if _, _, err := run.WebhookDeliveries.Admit(ctx, record); err != nil {
		t.Fatal(err)
	}
	conflicting := record
	conflicting.BodySHA256 = api.WebhookBodyHash([]byte("different body"))
	if _, _, err := run.WebhookDeliveries.Admit(ctx, conflicting); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting duplicate error = %v, want conflict", err)
	}
	var jobs, deliveries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, record.DeliveryIDHash).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("conflict left %d speculative jobs: %v", jobs, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("conflict deliveries=%d err=%v", deliveries, err)
	}
}

func TestIntegrationWebhookAdmissionRollsBackWhenQueueFails(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	record.DeliveryIDHash = api.WebhookDeliveryHash("rollback-" + record.EnvironmentID)
	store := NewWebhookDeliveryStore(pool)
	store.enqueue = func(context.Context, pgx.Tx, string, string) (int64, error) {
		return 0, errors.New("injected queue failure")
	}
	if _, _, err := store.Admit(ctx, record); err == nil {
		t.Fatal("admission succeeded without its River job")
	}
	var deliveries, jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, record.DeliveryIDHash).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 || jobs != 0 {
		t.Fatalf("partial admission deliveries=%d jobs=%d", deliveries, jobs)
	}
}

func TestIntegrationWebhookAdmissionRejectsRotatedSecret(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, secrets := webhookIntegrationFixture(t, pool, run)
	record.DeliveryIDHash = api.WebhookDeliveryHash("rotation-" + record.EnvironmentID)
	appName, envName := webhookRecordApp(t, record)
	namespace, _ := vault.NewNamespace(appName, envName, "WEBHOOK_SECRET")
	if _, err := secrets.Set(ctx, namespace, []byte("fedcba9876543210"), record.SecretRevision, contract.Actor{Kind: "system", Subject: "webhook-integration"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run.WebhookDeliveries.Admit(ctx, record); !errors.Is(err, api.ErrWebhookBindingChanged) {
		t.Fatalf("rotation error = %v, want ErrWebhookBindingChanged", err)
	}
	var deliveries, jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("stale admission deliveries=%d err=%v", deliveries, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, record.DeliveryIDHash).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("stale admission left %d speculative jobs: %v", jobs, err)
	}
}

func TestIntegrationWebhookAdmissionSerializesSecretRotation(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, secrets := webhookIntegrationFixture(t, pool, run)
	appName, envName := webhookRecordApp(t, record)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	current, err := run.WebhookDeliveries.bindingCurrentTx(ctx, tx, record)
	if err != nil || !current {
		t.Fatalf("binding snapshot current=%v err=%v", current, err)
	}
	namespace, _ := vault.NewNamespace(appName, envName, "WEBHOOK_SECRET")
	rotated := make(chan error, 1)
	go func() {
		_, err := secrets.Set(ctx, namespace, []byte("fedcba9876543210"), record.SecretRevision, contract.Actor{Kind: "system", Subject: "webhook-integration"})
		rotated <- err
	}()
	select {
	case err := <-rotated:
		t.Fatalf("rotation completed while admission held the secret namespace lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-rotated:
		if err != nil {
			t.Fatalf("rotation after admission rollback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not resume after the admission lock was released")
	}
	if _, _, err := run.WebhookDeliveries.Admit(ctx, record); !errors.Is(err, api.ErrWebhookBindingChanged) {
		t.Fatalf("old proof after rotation error = %v, want ErrWebhookBindingChanged", err)
	}
}

func TestIntegrationWebhookAdmissionAndDeletionLockOrder(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	appName, envName := webhookRecordApp(t, record)
	// Simulate deleteEnvironmentRowsTx: lock the environment row first, then
	// attempt the secret row. Admission must take the same environment-then-
	// secret order or this sequence deadlocks.
	deleteTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(deleteTx)
	if _, err := deleteTx.Exec(ctx, `UPDATE eregion_platform_environments
        SET updated_at=clock_timestamp() WHERE id=$1`, record.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan error, 1)
	go func() {
		_, _, err := run.WebhookDeliveries.Admit(ctx, record)
		admitted <- err
	}()
	time.Sleep(150 * time.Millisecond)
	lockCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	var locked int
	lockErr := deleteTx.QueryRow(lockCtx, `SELECT 1 FROM eregion_secrets
        WHERE app=$1 AND environment=$2 AND name=$3 FOR UPDATE`, appName, envName, "WEBHOOK_SECRET").Scan(&locked)
	cancel()
	if lockErr != nil {
		t.Fatalf("delete's secret lock deadlocked with admission: %v", lockErr)
	}
	if err := deleteTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("admission after simulated delete released its locks: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission did not finish after simulated delete released its locks")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "admitted" {
		t.Fatalf("admission state = %q, want admitted", state)
	}
}

func TestIntegrationWebhookWorkerSubmitsOnce(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	admitted, _, err := run.WebhookDeliveries.Admit(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Service.submitWebhookAdmission(ctx, admitted.EnvironmentID, admitted.DeliveryIDHash); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := run.Service.submitWebhookAdmission(ctx, admitted.EnvironmentID, admitted.DeliveryIDHash); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	var state, operationID string
	if err := pool.QueryRow(ctx, `SELECT state,coalesce(operation_id,'') FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash).Scan(&state, &operationID); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || operationID == "" {
		t.Fatalf("delivery state=%q operation=%q", state, operationID)
	}
	var kind, target, key string
	if err := pool.QueryRow(ctx, `SELECT kind,target,idempotency_key FROM eregion_operations WHERE id=$1`, operationID).Scan(&kind, &target, &key); err != nil {
		t.Fatal(err)
	}
	if kind != platform.KindDeploy || target != record.Target || key != "ghdelivery-"+record.EnvironmentID+"-"+record.DeliveryIDHash {
		t.Fatalf("operation kind=%q target=%q key=%q", kind, target, key)
	}
	var operations, platformJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("operations=%d err=%v", operations, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE queue=$1 AND args->>'operation_id'=$2`, platformQueue, operationID).Scan(&platformJobs); err != nil || platformJobs != 1 {
		t.Fatalf("platform jobs=%d err=%v", platformJobs, err)
	}
}

func TestIntegrationWebhookLegacyReconcileNeverResubmits(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	rawDelivery := "legacy-delivery-" + record.EnvironmentID
	record.DeliveryIDHash = api.WebhookDeliveryHash(rawDelivery)
	record.BodySHA256 = api.WebhookBodyHash([]byte("legacy body"))
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_webhook_deliveries
        (delivery_id_hash,body_sha256,actor_kind,actor_subject,kind,target,state,environment_id,first_seen_at,revision_id,secret_revision,secret_revision_hash,repository,branch,commit_sha,error_code,error_detail,admission_attempts)
        VALUES($1,$2,$3,$4,$5,$6,'pending',$7,clock_timestamp(),'',0,'','','','','','',0)`,
		record.DeliveryIDHash, record.BodySHA256, record.Actor.Kind, record.Actor.Subject, record.Kind, record.Target, record.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	appName, envName := webhookRecordApp(t, record)
	canonical, _ := json.Marshal(platform.DeployInput{App: appName, Environment: envName, Ref: record.Commit})
	operationID, _ := contract.NewOperationID()
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,'succeeded',1,0,0,clock_timestamp(),clock_timestamp(),'')`,
		operationID, record.Kind, record.Target, record.Actor.Kind, record.Actor.Subject,
		"ghdelivery-"+rawDelivery, sha256Hex(canonical), canonical); err != nil {
		t.Fatal(err)
	}
	updated, found, err := run.WebhookDeliveries.ReconcileLegacy(ctx, record, "ghdelivery-"+rawDelivery, canonical)
	if err != nil || !found || updated.OperationID != operationID {
		t.Fatalf("reconcile found=%v operation=%q err=%v", found, updated.OperationID, err)
	}
	var state, linked string
	if err := pool.QueryRow(ctx, `SELECT state,coalesce(operation_id,'') FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&state, &linked); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || linked != string(operationID) {
		t.Fatalf("legacy link state=%q operation=%q", state, linked)
	}
}

func TestIntegrationWebhookLegacyMissingOperationIsAmbiguous(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	rawDelivery := "legacy-missing-" + record.EnvironmentID
	record.DeliveryIDHash = api.WebhookDeliveryHash(rawDelivery)
	record.BodySHA256 = api.WebhookBodyHash([]byte("legacy missing body"))
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_webhook_deliveries
        (delivery_id_hash,body_sha256,actor_kind,actor_subject,kind,target,state,environment_id,first_seen_at,revision_id,secret_revision,secret_revision_hash,repository,branch,commit_sha,error_code,error_detail,admission_attempts)
        VALUES($1,$2,$3,$4,$5,$6,'pending',$7,clock_timestamp(),'',0,'','','','','','',0)`,
		record.DeliveryIDHash, record.BodySHA256, record.Actor.Kind, record.Actor.Subject, record.Kind, record.Target, record.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	appName, envName := webhookRecordApp(t, record)
	canonical, _ := json.Marshal(platform.DeployInput{App: appName, Environment: envName, Ref: record.Commit})
	updated, found, err := run.WebhookDeliveries.ReconcileLegacy(ctx, record, "ghdelivery-"+rawDelivery, canonical)
	if err != nil || found || updated.State != api.WebhookDeliveryPending {
		t.Fatalf("missing legacy operation found=%v state=%s err=%v", found, updated.State, err)
	}
	var operations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("ambiguous reconciliation created %d operations: %v", operations, err)
	}
}

func TestIntegrationWebhookHTTPLegacyPendingRetainsOperation(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, secrets := webhookIntegrationFixture(t, pool, run)
	appName, envName := webhookRecordApp(t, record)
	rawDelivery := "legacy-http-retained-" + strings.ReplaceAll(record.EnvironmentID, "_", "-")
	body := webhookIntegrationPushBody(record.Commit)
	record.DeliveryIDHash = api.WebhookDeliveryHash(rawDelivery)
	record.BodySHA256 = api.WebhookBodyHash(body)
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_webhook_deliveries
        (delivery_id_hash,body_sha256,actor_kind,actor_subject,kind,target,state,environment_id,first_seen_at,revision_id,secret_revision,secret_revision_hash,repository,branch,commit_sha,error_code,error_detail,admission_attempts)
        VALUES($1,$2,$3,$4,$5,$6,'pending',$7,clock_timestamp(),'',0,'','','','','','',0)`,
		record.DeliveryIDHash, record.BodySHA256, record.Actor.Kind, record.Actor.Subject, record.Kind, record.Target, record.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(platform.DeployInput{App: appName, Environment: envName, Ref: record.Commit})
	operationID, _ := contract.NewOperationID()
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,'succeeded',1,0,0,clock_timestamp(),clock_timestamp(),'')`,
		operationID, record.Kind, record.Target, record.Actor.Kind, record.Actor.Subject,
		"ghdelivery-"+rawDelivery, sha256Hex(canonical), canonical); err != nil {
		t.Fatal(err)
	}
	handler := newWebhookIntegrationHandler(t, pool, run, secrets)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, webhookIntegrationRequest(appName, envName, rawDelivery, body))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	var response webhookIntegrationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OperationID != string(operationID) || response.Ambiguous || !response.Accepted {
		t.Fatalf("response = %+v", response)
	}
	var state, linked string
	if err := pool.QueryRow(ctx, `SELECT state,coalesce(operation_id,'') FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&state, &linked); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || linked != string(operationID) {
		t.Fatalf("legacy link state=%q operation=%q", state, linked)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, record.DeliveryIDHash).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("legacy HTTP redelivery left %d speculative jobs: %v", jobs, err)
	}
}

func TestIntegrationWebhookHTTPLegacyPendingMissingReportsAmbiguous(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, secrets := webhookIntegrationFixture(t, pool, run)
	appName, envName := webhookRecordApp(t, record)
	rawDelivery := "legacy-http-missing-" + strings.ReplaceAll(record.EnvironmentID, "_", "-")
	body := webhookIntegrationPushBody(record.Commit)
	record.DeliveryIDHash = api.WebhookDeliveryHash(rawDelivery)
	record.BodySHA256 = api.WebhookBodyHash(body)
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_webhook_deliveries
        (delivery_id_hash,body_sha256,actor_kind,actor_subject,kind,target,state,environment_id,first_seen_at,revision_id,secret_revision,secret_revision_hash,repository,branch,commit_sha,error_code,error_detail,admission_attempts)
        VALUES($1,$2,$3,$4,$5,$6,'pending',$7,clock_timestamp(),'',0,'','','','','','',0)`,
		record.DeliveryIDHash, record.BodySHA256, record.Actor.Kind, record.Actor.Subject, record.Kind, record.Target, record.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	handler := newWebhookIntegrationHandler(t, pool, run, secrets)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, webhookIntegrationRequest(appName, envName, rawDelivery, body))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	var response webhookIntegrationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Ambiguous || response.OperationID != "" || !response.Accepted {
		t.Fatalf("response = %+v", response)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, record.DeliveryIDHash).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("legacy ambiguous state = %q, want pending", state)
	}
	var operations, jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, "ghdelivery-"+rawDelivery).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("ambiguous HTTP redelivery created %d operations: %v", operations, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, record.DeliveryIDHash).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("ambiguous HTTP redelivery left %d speculative jobs: %v", jobs, err)
	}
}

func TestIntegrationWebhookMaintenanceRequeuesMissingJobAndFailsDiscarded(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	admitted, _, err := run.WebhookDeliveries.Admit(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_webhook_deliveries SET job_id=999999 WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.reconcileWebhookAdmissions(ctx); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT job_id FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if jobID == 999999 || jobID == 0 {
		t.Fatalf("missing job was not requeued: %d", jobID)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state='discarded', finalized_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.reconcileWebhookAdmissions(ctx); err != nil {
		t.Fatal(err)
	}
	var state, code string
	if err := pool.QueryRow(ctx, `SELECT state,error_code FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != string(contract.Unavailable) {
		t.Fatalf("discarded job receipt state=%q code=%q", state, code)
	}
}

func TestIntegrationWebhookMaintenanceFiltersActionableBeforeLimit(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	for index := 0; index < 101; index++ {
		healthy := record
		healthy.DeliveryIDHash = api.WebhookDeliveryHash(fmt.Sprintf("healthy-%d-%s", index, record.EnvironmentID))
		healthy.BodySHA256 = api.WebhookBodyHash([]byte(fmt.Sprintf("healthy body %d", index)))
		if _, _, err := run.WebhookDeliveries.Admit(ctx, healthy); err != nil {
			t.Fatal(err)
		}
	}
	broken := record
	broken.DeliveryIDHash = api.WebhookDeliveryHash("broken-" + record.EnvironmentID)
	broken.BodySHA256 = api.WebhookBodyHash([]byte("broken body"))
	admitted, _, err := run.WebhookDeliveries.Admit(ctx, broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_webhook_deliveries SET job_id=999999 WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.reconcileWebhookAdmissions(ctx); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT job_id FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if jobID == 999999 || jobID == 0 {
		t.Fatalf("broken receipt after 101 healthy older receipts was not requeued: %d", jobID)
	}
}

func TestIntegrationWebhookMaintenanceRechecksCurrentJobUnderLock(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	admitted, _, err := run.WebhookDeliveries.Admit(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_webhook_deliveries SET job_id=999999 WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		wait.Go(func() {
			failures <- run.Service.reconcileWebhookAdmission(ctx, admitted.EnvironmentID, admitted.DeliveryIDHash)
		})
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var jobs, attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, admitted.DeliveryIDHash).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT admission_attempts FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	// One original job plus exactly one requeued job; the second concurrent
	// pass must observe the replacement job and leave it alone.
	if jobs != 2 || attempts != 2 {
		t.Fatalf("concurrent reconciliation jobs=%d attempts=%d, want 2/2", jobs, attempts)
	}
	if err := run.Service.reconcileWebhookAdmission(ctx, admitted.EnvironmentID, admitted.DeliveryIDHash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'delivery_id_hash'=$1`, admitted.DeliveryIDHash).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 2 {
		t.Fatalf("healthy replacement was requeued again: jobs=%d", jobs)
	}
}

func TestIntegrationWebhookWorkerRejectsRebindAndDeletedEnvironment(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	record, _ := webhookIntegrationFixture(t, pool, run)
	admitted, _, err := run.WebhookDeliveries.Admit(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	newRevisionID, _ := platform.NewEnvironmentRevisionID()
	otherSource, _ := json.Marshal(platform.SourceBinding{URL: "https://github.com/mbvlabs/other.git", Ref: "main"})
	runtime, _ := json.Marshal(platform.RuntimeSpec{Node: "local", HealthType: "http", HealthPath: "/health", HealthTimeoutSeconds: 30})
	processes, _ := json.Marshal([]platform.ProcessSpec{{Name: "web", Kind: "web", Command: "./app", Replicas: 1, ContainerPort: 3000}})
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environment_revisions
        (id,environment_id,number,actor_kind,actor_subject,operation,summary,source,auto_deploy,
         webhook_secret_name,runtime,processes,config,secret_names,deploy_intent,created_at,retention,resources)
        SELECT $1,environment_id,2,'system','integration','environment.updated','rebind',$2,true,
         'WEBHOOK_SECRET',$3,$4,'{}'::jsonb,'{}',false,clock_timestamp(),'{}'::jsonb,ARRAY[]::text[]
        FROM eregion_platform_environment_revisions WHERE id=$5`, newRevisionID, otherSource, runtime, processes, record.RevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_environments SET current_revision_id=$2 WHERE id=$1`, record.EnvironmentID, newRevisionID); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.submitWebhookAdmission(ctx, admitted.EnvironmentID, admitted.DeliveryIDHash); err != nil {
		t.Fatal(err)
	}
	var state, code string
	if err := pool.QueryRow(ctx, `SELECT state,error_code FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, admitted.DeliveryIDHash).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != string(contract.PreconditionFailed) {
		t.Fatalf("rebind state=%q code=%q", state, code)
	}

	second, _ := webhookIntegrationFixture(t, pool, run)
	second.DeliveryIDHash = api.WebhookDeliveryHash("deleted-" + second.EnvironmentID)
	secondAdmitted, _, err := run.WebhookDeliveries.Admit(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_platform_environments WHERE id=$1`, second.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if err := run.Service.submitWebhookAdmission(ctx, secondAdmitted.EnvironmentID, secondAdmitted.DeliveryIDHash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state,error_code FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, secondAdmitted.DeliveryIDHash).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != string(contract.PreconditionFailed) {
		t.Fatalf("deleted state=%q code=%q", state, code)
	}
}
