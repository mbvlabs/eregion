package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// These tests need the explicitly authorized disposable PostgreSQL target
// named by EREGION_TEST_DATABASE_URL. testPool skips them otherwise; they never
// start a database or use an ambient DSN.

func TestIntegrationPlatformWebhookAdmissionReplayAndRotation(t *testing.T) {
	pool, s, ctx := newPlatformWebhookFixture(t)
	first, operationID := enablePlatformWebhook(t, ctx, pool, s)
	assertWebhookSecretNotLeaked(t, ctx, pool, operationID, "webhook-secret-value-1")
	assertWebhookDesiredRevision(t, ctx, pool)
	assertPlatformWebhookReplay(t, ctx, pool, s, first)
	rotation := rotatePlatformWebhook(t, ctx, pool, s)
	assertPlatformWebhookRotationReplay(t, ctx, pool, s, rotation)
	assertPlatformWebhookRotationRequiresSecret(t, ctx, pool, s, rotation)
}

func newPlatformWebhookFixture(t *testing.T) (*pgxpool.Pool, *Service, context.Context) {
	t.Helper()
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
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "webhook-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "webhook-app", map[string]any{
		"name": "webhook",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	return pool, s, ctx
}

func enablePlatformWebhook(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service) (operations.Submission, contract.OperationID) {
	t.Helper()
	first := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/webhook/production", Actor: testActor,
		IdempotencyKey: "webhook-enable-1",
		Input:          json.RawMessage(`{"auto_deploy":true}`),
		WebhookSecret:  []byte("webhook-secret-value-1"),
	}
	result, err := s.SubmitWithReceipt(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replay || !result.WebhookSecretActivated {
		t.Fatalf("first receipt = %+v", result)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "webhook"); got != 1 {
		t.Fatalf("secret revisions after enable = %d", got)
	}
	return first, result.Operation.ID
}

func assertWebhookSecretNotLeaked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, operationID contract.OperationID, secret string) {
	t.Helper()
	var storedInput []byte
	if err := pool.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, operationID).Scan(&storedInput); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedInput), secret) {
		t.Fatal("the transient webhook secret entered the operation input")
	}
	var eventText string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(event::text,' '),'') FROM eregion_operation_events WHERE operation_id=$1`, operationID).Scan(&eventText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(eventText, secret) {
		t.Fatal("the transient webhook secret entered an operation event")
	}
	var riverArgs string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(args::text,' '),'') FROM river_job WHERE args->>'operation_id'=$1`, operationID).Scan(&riverArgs); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(riverArgs, secret) {
		t.Fatal("the transient webhook secret entered River arguments")
	}
}

func assertWebhookDesiredRevision(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var autoDeploy bool
	var webhookName string
	if err := pool.QueryRow(ctx, `SELECT r.auto_deploy,r.webhook_secret_name
        FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='webhook' AND e.name='production' ORDER BY r.number DESC LIMIT 1`).Scan(&autoDeploy, &webhookName); err != nil {
		t.Fatal(err)
	}
	if !autoDeploy || webhookName != platform.DefaultWebhookSecretName {
		t.Fatalf("desired webhook state = auto_deploy %v name %q", autoDeploy, webhookName)
	}
}

func currentWebhookRevision(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var revisionID string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='webhook' AND e.name='production'`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	return revisionID
}

func assertPlatformWebhookReplay(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, first operations.Submission) {
	t.Helper()
	// Change the current desired state while keeping the same idempotency key.
	platformSubmitContext(ctx, t, s, platform.KindConfigSet, "env/webhook/production", "webhook-current-change", map[string]any{
		"config": map[string]string{"AFTER": "enable"},
	})
	result, err := s.SubmitWithReceipt(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Replay || result.WebhookSecretActivated {
		t.Fatalf("replay receipt = %+v", result)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "webhook"); got != 1 {
		t.Fatalf("replay added a secret revision: %d", got)
	}
	conflict := first
	conflict.Input = json.RawMessage(`{"auto_deploy":true,"config":{"OTHER":"value"}}`)
	if _, err := s.SubmitWithReceipt(ctx, conflict); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed input error = %v", err)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "webhook"); got != 1 {
		t.Fatalf("conflict added a secret revision: %d", got)
	}
}

func rotatePlatformWebhook(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	s *Service,
) operations.Submission {
	t.Helper()
	beforeRevision := currentWebhookRevision(t, ctx, pool)
	rotation := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/webhook/production", Actor: testActor,
		IdempotencyKey: "webhook-rotate-1",
		Input:          json.RawMessage(`{"rotate_webhook_secret":true}`),
		WebhookSecret:  []byte("webhook-secret-value-2"),
	}
	rotated, err := s.SubmitWithReceipt(ctx, rotation)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Replay || !rotated.WebhookSecretActivated {
		t.Fatalf("rotation receipt = %+v", rotated)
	}
	assertWebhookSecretNotLeaked(t, ctx, pool, rotated.Operation.ID, "webhook-secret-value-2")
	if current := currentWebhookRevision(t, ctx, pool); current != beforeRevision {
		t.Fatalf("rotation changed the current revision %q -> %q", beforeRevision, current)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "webhook"); got != 2 {
		t.Fatalf("secret revisions after rotation = %d", got)
	}
	return rotation
}

func assertPlatformWebhookRotationReplay(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	s *Service,
	rotation operations.Submission,
) {
	t.Helper()
	rotateReplay, err := s.SubmitWithReceipt(ctx, rotation)
	if err != nil {
		t.Fatal(err)
	}
	if !rotateReplay.Replay || rotateReplay.WebhookSecretActivated {
		t.Fatalf("rotation replay = %+v", rotateReplay)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "webhook"); got != 2 {
		t.Fatalf("rotation replay added a secret revision: %d", got)
	}
}

func assertPlatformWebhookRotationRequiresSecret(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	s *Service,
	rotation operations.Submission,
) {
	t.Helper()
	withoutSecret := rotation
	withoutSecret.IdempotencyKey = "webhook-rotate-no-secret"
	withoutSecret.WebhookSecret = nil
	if _, err := s.SubmitWithReceipt(ctx, withoutSecret); err == nil || contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("rotation without a transient secret error = %v", err)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "webhook"); got != 2 {
		t.Fatalf("rejected rotation added a secret revision: %d", got)
	}
}

func TestIntegrationPlatformWebhookConcurrentAdmissionLoser(t *testing.T) {
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
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "webhook-concurrent-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "webhook-concurrent-app", map[string]any{
		"name": "concurrent",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	base := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/concurrent/production", Actor: testActor,
		IdempotencyKey: "webhook-concurrent-key",
		Input:          json.RawMessage(`{"auto_deploy":true}`),
	}
	// Hold a SHARE lock on the operation table. The initial replay SELECT takes
	// only ACCESS SHARE and still runs, while both INSERTs require ROW EXCLUSIVE
	// and block. This makes the loser exercise the ON CONFLICT path rather than
	// returning from its first replay lookup.
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lockReleased := false
	defer func() {
		if !lockReleased {
			_ = lockTx.Rollback(context.Background())
		}
	}()
	if _, err := lockTx.Exec(ctx, `LOCK TABLE eregion_operations IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]operations.SubmissionResult, 2)
	errs := make([]error, 2)
	for index := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			submission := base
			submission.WebhookSecret = []byte("concurrent-secret-" + string(rune('a'+index)))
			results[index], errs[index] = s.SubmitWithReceipt(ctx, submission)
		}(index)
	}
	awaitBlockedWebhookInserts(t, pool, 2)
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	lockReleased = true
	wg.Wait()
	fresh, replay := assertConcurrentWebhookReceipts(t, results, errs)
	if fresh != 1 || replay != 1 {
		t.Fatalf("receipts fresh=%d replay=%d", fresh, replay)
	}
	var operationsCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1 AND target=$2 AND idempotency_key=$3`,
		platform.KindEnvironmentUpdate, "env/concurrent/production", base.IdempotencyKey).Scan(&operationsCount); err != nil {
		t.Fatal(err)
	}
	if operationsCount != 1 {
		t.Fatalf("operations = %d", operationsCount)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "concurrent"); got != 1 {
		t.Fatalf("secret revisions = %d, want exactly the winner", got)
	}
}

func assertConcurrentWebhookReceipts(
	t *testing.T,
	results []operations.SubmissionResult,
	errs []error,
) (fresh, replay int) {
	t.Helper()
	for index := range results {
		if errs[index] != nil {
			t.Fatalf("concurrent submit %d: %v", index, errs[index])
		}
		if results[index].Replay {
			replay++
			if results[index].WebhookSecretActivated {
				t.Fatalf("loser receipt = %+v", results[index])
			}
			continue
		}
		fresh++
		if !results[index].WebhookSecretActivated {
			t.Fatalf("winner receipt = %+v", results[index])
		}
	}
	return fresh, replay
}

func TestIntegrationPlatformWebhookAdmissionRollback(t *testing.T) {
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
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "webhook-rollback-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "webhook-rollback-app", map[string]any{
		"name": "rollback",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	s.beforeSubmissionCommit = func() error {
		return errors.New("injected failure after webhook secret activation")
	}
	submission := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/rollback/production", Actor: testActor,
		IdempotencyKey: "webhook-rollback-key",
		Input:          json.RawMessage(`{"auto_deploy":true}`),
		WebhookSecret:  []byte("rollback-secret-value"),
	}
	if _, err := s.SubmitWithReceipt(ctx, submission); err == nil {
		t.Fatal("rollback injection did not fail admission")
	}
	s.beforeSubmissionCommit = nil
	var operationsCount, revisionsCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, submission.IdempotencyKey).Scan(&operationsCount); err != nil {
		t.Fatal(err)
	}
	if operationsCount != 0 {
		t.Fatalf("rolled-back admission left %d operations", operationsCount)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "rollback"); got != 0 {
		t.Fatalf("rolled-back admission left %d secret revisions", got)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='rollback' AND e.name='production'`).Scan(&revisionsCount); err != nil {
		t.Fatal(err)
	}
	if revisionsCount != 1 {
		t.Fatalf("rolled-back admission left %d environment revisions, want only the create", revisionsCount)
	}
}

func TestIntegrationPlatformWebhookAdmissionAndDeletionLockOrder(t *testing.T) {
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
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "webhook-order-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "webhook-order-app", map[string]any{
		"name": "order",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	enable := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/order/production", Actor: testActor,
		IdempotencyKey: "webhook-order-enable",
		Input:          json.RawMessage(`{"auto_deploy":true}`),
		WebhookSecret:  []byte("order-secret-value-1"),
	}
	if _, err := s.SubmitWithReceipt(ctx, enable); err != nil {
		t.Fatal(err)
	}
	var environmentID string
	if err := pool.QueryRow(ctx, `SELECT e.id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='order' AND e.name='production'`).Scan(&environmentID); err != nil {
		t.Fatal(err)
	}
	deleteTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(ctx, deleteTx)
	if _, err := deleteTx.Exec(ctx, `UPDATE eregion_platform_environments SET updated_at=clock_timestamp() WHERE id=$1`, environmentID); err != nil {
		t.Fatal(err)
	}
	rotation := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/order/production", Actor: testActor,
		IdempotencyKey: "webhook-order-rotation",
		Input:          json.RawMessage(`{"rotate_webhook_secret":true}`),
		WebhookSecret:  []byte("order-secret-value-2"),
	}
	submitted := make(chan error, 1)
	go func() {
		_, err := s.SubmitWithReceipt(ctx, rotation)
		submitted <- err
	}()
	time.Sleep(150 * time.Millisecond)
	lockCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	var locked int
	lockErr := deleteTx.QueryRow(lockCtx, `SELECT 1 FROM eregion_secrets
        WHERE app=$1 AND environment=$2 AND name=$3 FOR UPDATE`, "order", "production", platform.DefaultWebhookSecretName).Scan(&locked)
	cancel()
	if lockErr != nil {
		t.Fatalf("deletion secret lock deadlocked with platform webhook admission: %v", lockErr)
	}
	if err := deleteTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-submitted:
		if err != nil {
			t.Fatalf("platform webhook rotation after simulated delete released its locks: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("platform webhook rotation did not finish after simulated delete released its locks")
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "order"); got != 2 {
		t.Fatalf("secret revisions = %d", got)
	}
}

func TestIntegrationPlatformWebhookCredentialCollisionRollsBackActivation(t *testing.T) {
	pool, run, _ := newWebhookIntegrationRuntime(t, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "webhook-collision-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "webhook-collision-app", map[string]any{
		"name": "collision",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{
				"url": "https://github.com/acme/api", "ref": "main",
				"credential_secret_name": "GIT_READ_TOKEN",
			},
		}},
	})
	enable := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/collision/production", Actor: testActor,
		IdempotencyKey: "webhook-collision-enable",
		Input:          json.RawMessage(`{"auto_deploy":true}`),
		WebhookSecret:  []byte("collision-secret-value-1"),
	}
	if _, err := s.SubmitWithReceipt(ctx, enable); err != nil {
		t.Fatal(err)
	}
	var currentRevision string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='collision' AND e.name='production'`).Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	var revisionsBefore, riverBefore, operationsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='collision' AND e.name='production'`).Scan(&revisionsBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&riverBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operationsBefore); err != nil {
		t.Fatal(err)
	}
	collision := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/collision/production", Actor: testActor,
		IdempotencyKey: "webhook-credential-collision",
		Input: json.RawMessage(`{
            "source":{"url":"https://github.com/acme/api","ref":"main",
                      "credential_secret_name":"EREGION_WEBHOOK_SECRET"},
            "auto_deploy":true,
            "webhook_secret_name":"EREGION_WEBHOOK_SECRET",
            "rotate_webhook_secret":true
        }`),
		WebhookSecret: []byte("collision-secret-value-2"),
	}
	if _, err := s.SubmitWithReceipt(ctx, collision); err == nil || contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("credential/webhook-name collision error = %v", err)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "collision"); got != 1 {
		t.Fatalf("collision left %d webhook secret revisions, want the original", got)
	}
	var operationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, collision.IdempotencyKey).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if operationCount != 0 {
		t.Fatalf("collision left %d operations", operationCount)
	}
	var revisionsAfter, riverAfter, operationsAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='collision' AND e.name='production'`).Scan(&revisionsAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&riverAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operationsAfter); err != nil {
		t.Fatal(err)
	}
	if revisionsAfter != revisionsBefore || riverAfter != riverBefore || operationsAfter != operationsBefore {
		t.Fatalf("collision changed rows: revisions %d->%d river %d->%d operations %d->%d",
			revisionsBefore, revisionsAfter, riverBefore, riverAfter, operationsBefore, operationsAfter)
	}
	var currentAfter string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='collision' AND e.name='production'`).Scan(&currentAfter); err != nil {
		t.Fatal(err)
	}
	if currentAfter != currentRevision {
		t.Fatalf("collision changed current revision %q -> %q", currentRevision, currentAfter)
	}
}

func TestIntegrationPlatformWebhookHTTPRotationWithoutTransientRejected(t *testing.T) {
	pool, run, _ := newWebhookIntegrationRuntime(t, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "webhook-http-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "webhook-http-app", map[string]any{
		"name": "httpwebhook",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	enable := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/httpwebhook/production", Actor: testActor,
		IdempotencyKey: "webhook-http-enable",
		Input:          json.RawMessage(`{"auto_deploy":true}`),
		WebhookSecret:  []byte("http-secret-value-1"),
	}
	if _, err := s.SubmitWithReceipt(ctx, enable); err != nil {
		t.Fatal(err)
	}
	var currentRevision string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='httpwebhook' AND e.name='production'`).Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	var revisionsBefore, riverBefore, operationsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='httpwebhook' AND e.name='production'`).Scan(&revisionsBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&riverBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operationsBefore); err != nil {
		t.Fatal(err)
	}
	app := api.NewAppService(s, nil, nil)
	normalized, err := api.NormalizeAction(platform.KindEnvironmentUpdate, "env/httpwebhook/production",
		json.RawMessage(`{"rotate_webhook_secret":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.SubmitNormalizedWithFactor(ctx, testActor, normalized, "webhook-http-rotate", false, ""); err == nil ||
		contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("HTTP rotation without transient secret error = %v", err)
	}
	if got := webhookSecretRevisionCount(ctx, t, pool, "httpwebhook"); got != 1 {
		t.Fatalf("HTTP rejection changed webhook secret revisions: %d", got)
	}
	var currentAfter string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='httpwebhook' AND e.name='production'`).Scan(&currentAfter); err != nil {
		t.Fatal(err)
	}
	if currentAfter != currentRevision {
		t.Fatalf("HTTP rejection changed current revision %q -> %q", currentRevision, currentAfter)
	}
	var revisionsAfter, riverAfter, operationsAfter, keyOperations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='httpwebhook' AND e.name='production'`).Scan(&revisionsAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&riverAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operationsAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, "webhook-http-rotate").Scan(&keyOperations); err != nil {
		t.Fatal(err)
	}
	if revisionsAfter != revisionsBefore || riverAfter != riverBefore || operationsAfter != operationsBefore || keyOperations != 0 {
		t.Fatalf("HTTP rejection changed rows: revisions %d->%d river %d->%d operations %d->%d key %d",
			revisionsBefore, revisionsAfter, riverBefore, riverAfter, operationsBefore, operationsAfter, keyOperations)
	}
}

func TestIntegrationPlatformWebhookRotationOnlyWorkerSemantics(t *testing.T) {
	pool, run, _ := newWebhookIntegrationRuntime(t, 1)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "webhook-worker-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "webhook-worker-app", map[string]any{
		"name": "workerhook",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	enable := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/workerhook/production", Actor: testActor,
		IdempotencyKey: "webhook-worker-enable",
		Input:          json.RawMessage(`{"auto_deploy":true}`),
		WebhookSecret:  []byte("worker-secret-value-1"),
	}
	enableResult, err := s.SubmitWithReceipt(ctx, enable)
	if err != nil {
		t.Fatal(err)
	}
	var revisionBefore string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='workerhook' AND e.name='production'`).Scan(&revisionBefore); err != nil {
		t.Fatal(err)
	}
	var revisionsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='workerhook' AND e.name='production'`).Scan(&revisionsBefore); err != nil {
		t.Fatal(err)
	}
	rotation := operations.Submission{
		Kind: platform.KindEnvironmentUpdate, Target: "env/workerhook/production", Actor: testActor,
		IdempotencyKey: "webhook-worker-rotate",
		Input:          json.RawMessage(`{"rotate_webhook_secret":true}`),
		WebhookSecret:  []byte("worker-secret-value-2"),
	}
	rotateResult, err := s.SubmitWithReceipt(ctx, rotation)
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	platformAwait(t, s, enableResult.Operation, contract.Succeeded)
	platformAwait(t, s, rotateResult.Operation, contract.Succeeded)

	var revisionAfter string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='workerhook' AND e.name='production'`).Scan(&revisionAfter); err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("rotation worker changed current revision %q -> %q", revisionBefore, revisionAfter)
	}
	var revisionsAfter, rotationRevisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions r
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='workerhook' AND e.name='production'`).Scan(&revisionsAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environment_revisions WHERE operation_id=$1`, rotateResult.Operation.ID).Scan(&rotationRevisions); err != nil {
		t.Fatal(err)
	}
	if revisionsAfter != revisionsBefore || rotationRevisions != 0 {
		t.Fatalf("rotation worker revisions = %d (before %d), owned = %d", revisionsAfter, revisionsBefore, rotationRevisions)
	}
	var changeKind, changeSummary string
	if err := pool.QueryRow(ctx, `SELECT kind,summary FROM eregion_platform_changes WHERE operation_id=$1`, rotateResult.Operation.ID).
		Scan(&changeKind, &changeSummary); err != nil {
		t.Fatal(err)
	}
	if changeKind != "environment.webhook.rotated" || !strings.Contains(changeSummary, "desired state is unchanged") {
		t.Fatalf("rotation change = %q / %q", changeKind, changeSummary)
	}
	var storedSecret string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(desired::text,' '),'') FROM eregion_platform_changes WHERE operation_id=$1`, rotateResult.Operation.ID).Scan(&storedSecret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedSecret, "worker-secret-value-2") {
		t.Fatal("rotation change stored the transient secret")
	}
	var operationState string
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, rotateResult.Operation.ID).Scan(&operationState); err != nil {
		t.Fatal(err)
	}
	if operationState != string(contract.Succeeded) {
		t.Fatalf("rotation operation state = %q", operationState)
	}
}

func newWebhookIntegrationRuntime(t *testing.T, concurrency int) (*pgxpool.Pool, *Runtime, *vault.Service) {
	t.Helper()
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
		Concurrency: concurrency, TestOnly: true, LocalNodeName: "local",
		NodeStateDir: t.TempDir(), Vault: secrets, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	return pool, run, secrets
}

// These tests use testPool's explicit disposable-DSN gate; no database or
// container is started by the test suite.
func TestIntegrationWebhookNameOnlyAdmissionAndReplay(t *testing.T) {
	pool, run, secrets := newWebhookIntegrationRuntime(t, 0)
	s := run.Service
	ctx := context.Background()
	seedWebhookNameFixture(t, ctx, s, secrets)
	request := func(key, input string) operations.Submission {
		return operations.Submission{Kind: platform.KindEnvironmentUpdate, Target: "env/nametest/production", Actor: testActor, IdempotencyKey: key, Input: json.RawMessage(input)}
	}
	if _, err := s.SubmitWithReceipt(ctx, request("name-enable", `{"auto_deploy":true,"webhook_secret_name":"ACTIVE"}`)); err != nil {
		t.Fatal(err)
	}
	assertWebhookNameRejections(t, ctx, pool, s, request, true)
	if _, err := s.SubmitWithReceipt(ctx, request("name-disable", `{"auto_deploy":false}`)); err != nil {
		t.Fatal(err)
	}
	assertWebhookNameRejections(t, ctx, pool, s, request, false)
	assertWebhookNameOnlyReplay(t, ctx, pool, s, secrets, request)
}

func seedWebhookNameFixture(t *testing.T, ctx context.Context, s *Service, secrets *vault.Service) {
	t.Helper()
	platformSubmitContext(ctx, t, s, platform.KindNodeRegister, "platform", "name-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmitContext(ctx, t, s, platform.KindAppCreate, "platform", "name-app", map[string]any{
		"name": "nametest", "environments": []map[string]any{{"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"}}},
	})
	for _, name := range []string{"ACTIVE", "ARCHIVED"} {
		ns, err := vault.NewNamespace("nametest", "production", name)
		if err != nil {
			t.Fatal(err)
		}
		err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, err := secrets.Store().Set(ctx, tx.Secrets(), ns, []byte("test-secret-value"), 0, testActor); err != nil {
				return err
			}
			if name == "ARCHIVED" {
				if _, err := secrets.Store().Archive(ctx, tx.Secrets(), ns, 1, testActor); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type webhookNameCounts struct {
	revision                                     string
	revisions, operations, jobs, secretRevisions int
}

func readWebhookNameCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) webhookNameCounts {
	t.Helper()
	var c webhookNameCounts
	err := pool.QueryRow(ctx, `SELECT e.current_revision_id,
		(SELECT count(*) FROM eregion_platform_environment_revisions r WHERE r.environment_id=e.id),
		(SELECT count(*) FROM eregion_operations), (SELECT count(*) FROM river_job),
		(SELECT count(*) FROM eregion_secret_revisions WHERE app='nametest' AND environment='production')
		FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
		WHERE a.name='nametest' AND e.name='production'`).Scan(&c.revision, &c.revisions, &c.operations, &c.jobs, &c.secretRevisions)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func assertWebhookNameRejections(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	s *Service,
	request func(key, input string) operations.Submission,
	enabled bool,
) {
	t.Helper()
	for _, name := range []string{"MISSING", "ARCHIVED"} {
		before := readWebhookNameCounts(t, ctx, pool)
		key := fmt.Sprintf("name-reject-%t-%s", enabled, name)
		_, err := s.SubmitWithReceipt(ctx, request(key, fmt.Sprintf(`{"webhook_secret_name":%q}`, name)))
		if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
			t.Fatalf("enabled=%t name=%s error=%v", enabled, name, err)
		}
		if after := readWebhookNameCounts(t, ctx, pool); after != before {
			t.Fatalf("rejected name changed rows: before=%+v after=%+v", before, after)
		}
	}
}

func assertWebhookNameOnlyReplay(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	s *Service,
	secrets *vault.Service,
	request func(key, input string) operations.Submission,
) {
	t.Helper()
	// Commit a name-only update while disabled, then archive the name. An
	// idempotent replay must be returned before validating current vault state.
	ns, _ := vault.NewNamespace("nametest", "production", "ACTIVE")
	other, _ := vault.NewNamespace("nametest", "production", "OTHER")
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := secrets.Store().Set(ctx, tx.Secrets(), other, []byte("other-secret-value"), 0, testActor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	replay := request("name-only-replay", `{"webhook_secret_name":"OTHER"}`)
	first, err := s.SubmitWithReceipt(ctx, replay)
	if err != nil || first.Replay {
		t.Fatalf("first name update=%+v err=%v", first, err)
	}
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := secrets.Store().Archive(ctx, tx.Secrets(), other, 1, testActor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before := readWebhookNameCounts(t, ctx, pool)
	again, err := s.SubmitWithReceipt(ctx, replay)
	if err != nil || !again.Replay || again.Operation.ID != first.Operation.ID || again.WebhookSecretActivated {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	if after := readWebhookNameCounts(t, ctx, pool); after != before {
		t.Fatalf("replay changed rows: before=%+v after=%+v", before, after)
	}
	metadata, err := secrets.Store().Get(ctx, other)
	if err != nil || metadata.State != vault.StateArchived || metadata.LatestRevision != 1 {
		t.Fatalf("replay changed archived secret: %+v %v", metadata, err)
	}
	metadata, err = secrets.Store().Get(ctx, ns)
	if err != nil || metadata.State != vault.StateActive || metadata.LatestRevision != 1 {
		t.Fatalf("replay changed prior secret: %+v %v", metadata, err)
	}
}

func TestIntegrationWebhookAdmissionSerializesArchive(t *testing.T) {
	pool, run, secrets := newWebhookIntegrationRuntime(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ns := seedArchiveWebhookFixture(t, ctx, run, secrets)
	assertAdmissionBeforeArchive(t, ctx, pool, run, secrets, ns)
	assertArchiveBeforeAdmission(t, ctx, pool, run, secrets, ns)
}

func seedArchiveWebhookFixture(
	t *testing.T,
	ctx context.Context,
	run *Runtime,
	secrets *vault.Service,
) vault.Namespace {
	t.Helper()
	platformSubmitContext(ctx, t, run.Service, platform.KindNodeRegister, "platform", "archive-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmitContext(ctx, t, run.Service, platform.KindAppCreate, "platform", "archive-app", map[string]any{
		"name": "archivetest", "environments": []map[string]any{{"name": "production", "node": "local",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"}}},
	})
	ns, err := vault.NewNamespace("archivetest", "production", "HOOK")
	if err != nil {
		t.Fatal(err)
	}
	err = run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := secrets.Store().Set(ctx, tx.Secrets(), ns, []byte("secret-value"), 0, testActor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func awaitWebhookLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fragment string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
			AND state='active' AND wait_event_type='Lock' AND query ILIKE '%' || $1 || '%'`, fragment).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lock waiter for %q", fragment)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertAdmissionBeforeArchive(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	run *Runtime,
	secrets *vault.Service,
	ns vault.Namespace,
) {
	t.Helper()
	// Admission's namespace lock remains held until its caller commits.
	admissionLocked := make(chan struct{})
	admissionRelease := make(chan struct{})
	admissionDone := make(chan error, 1)
	go func() {
		admissionDone <- run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
			name := "HOOK"
			input := &platform.EnvironmentUpdateInput{App: "archivetest", Environment: "production", WebhookSecretName: &name}
			state := platform.RevisionState{WebhookSecretName: name}
			if activated, err := run.Service.activateWebhookSecretTx(ctx, tx, testActor, input, state, nil); err != nil || activated {
				return errors.New("admission validation failed")
			}
			close(admissionLocked)
			<-admissionRelease
			return nil
		})
	}()
	<-admissionLocked
	archiveDone := make(chan error, 1)
	go func() {
		archiveDone <- run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
			_, err := secrets.Store().Archive(ctx, tx.Secrets(), ns, 1, testActor)
			return err
		})
	}()
	awaitWebhookLock(t, ctx, pool, "FROM eregion_secrets")
	close(admissionRelease)
	if err := <-admissionDone; err != nil {
		t.Fatalf("admission validation: %v", err)
	}
	if err := <-archiveDone; err != nil {
		t.Fatalf("archive after admission commit: %v", err)
	}
}

type archiveWebhookCounts struct {
	revision                    string
	revisions, operations, jobs int
}

func readArchiveWebhookCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) archiveWebhookCounts {
	t.Helper()
	var c archiveWebhookCounts
	if err := pool.QueryRow(ctx, `SELECT e.current_revision_id,
		(SELECT count(*) FROM eregion_platform_environment_revisions r WHERE r.environment_id=e.id),
		(SELECT count(*) FROM eregion_operations), (SELECT count(*) FROM river_job)
		FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
		WHERE a.name='archivetest' AND e.name='production'`).Scan(&c.revision, &c.revisions, &c.operations, &c.jobs); err != nil {
		t.Fatal(err)
	}
	return c
}

func assertArchiveBeforeAdmission(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	run *Runtime,
	secrets *vault.Service,
	ns vault.Namespace,
) {
	t.Helper()
	// Restore, then archive first inside an uncommitted transaction. The real
	// submission must wait, observe archived state, and roll back completely.
	err := run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := secrets.Store().Set(ctx, tx.Secrets(), ns, []byte("secret-value-2"), 1, testActor)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveLocked := make(chan struct{})
	archiveRelease := make(chan struct{})
	archive := make(chan error, 1)
	go func() {
		archive <- run.Service.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, err := secrets.Store().Archive(ctx, tx.Secrets(), ns, 2, testActor); err != nil {
				return err
			}
			close(archiveLocked)
			<-archiveRelease
			return nil
		})
	}()
	<-archiveLocked
	before := readArchiveWebhookCounts(t, ctx, pool)
	submitted := make(chan error, 1)
	go func() {
		_, err := run.Service.SubmitWithReceipt(ctx, operations.Submission{
			Kind: platform.KindEnvironmentUpdate, Target: "env/archivetest/production", Actor: testActor,
			IdempotencyKey: "archive-wins", Input: json.RawMessage(`{"auto_deploy":true,"webhook_secret_name":"HOOK"}`),
		})
		submitted <- err
	}()
	awaitWebhookLock(t, ctx, pool, "FROM eregion_secrets")
	close(archiveRelease)
	if err := <-archive; err != nil {
		t.Fatal(err)
	}
	if err := <-submitted; err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("archived name admission error: %v", err)
	}
	if after := readArchiveWebhookCounts(t, ctx, pool); after != before {
		t.Fatalf("rejected admission left rows: before=%+v after=%+v", before, after)
	}
}

func awaitBlockedWebhookInserts(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked int
		if err := pool.QueryRow(context.Background(), `
            SELECT count(*) FROM pg_stat_activity
            WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'
              AND query ILIKE '%INSERT INTO eregion_operations%'`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("observed %d blocked webhook operation inserts, want %d", blocked, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func webhookSecretRevisionCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, app string) int {
	t.Helper()
	const environment = "production"
	const name = platform.DefaultWebhookSecretName
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_secret_revisions
        WHERE app=$1 AND environment=$2 AND name=$3`, app, environment, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
