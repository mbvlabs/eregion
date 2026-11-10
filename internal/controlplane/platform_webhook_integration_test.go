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
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// These tests need the explicitly authorized disposable PostgreSQL target
// named by EREGION_TEST_DATABASE_URL. testPool skips them otherwise; they never
// start a database or use an ambient DSN.

func TestIntegrationPlatformWebhookAdmissionReplayAndRotation(t *testing.T) {
	pool, _ := testPool(t, true)
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
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
			"source": map[string]any{"kind": "git", "url": "https://github.com/acme/api", "ref": "main"},
		}},
	})

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
	var storedInput []byte
	if err := pool.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, result.Operation.ID).Scan(&storedInput); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedInput), "webhook-secret-value-1") {
		t.Fatal("the transient webhook secret entered the operation input")
	}
	var eventText string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(event::text,' '),'') FROM eregion_operation_events WHERE operation_id=$1`, result.Operation.ID).Scan(&eventText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(eventText, "webhook-secret-value-1") {
		t.Fatal("the transient webhook secret entered an operation event")
	}
	var riverArgs string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(args::text,' '),'') FROM river_job WHERE args->>'operation_id'=$1`, result.Operation.ID).Scan(&riverArgs); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(riverArgs, "webhook-secret-value-1") {
		t.Fatal("the transient webhook secret entered River arguments")
	}
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
	var revisionID string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='webhook' AND e.name='production'`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	if got := webhookSecretRevisionCount(t, pool, "webhook", "production", platform.DefaultWebhookSecretName); got != 1 {
		t.Fatalf("secret revisions after enable = %d", got)
	}

	// Change the current desired state while keeping the same idempotency key.
	platformSubmit(t, s, platform.KindConfigSet, "env/webhook/production", "webhook-current-change", map[string]any{
		"config": map[string]string{"AFTER": "enable"},
	})
	result, err = s.SubmitWithReceipt(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Replay || result.WebhookSecretActivated {
		t.Fatalf("replay receipt = %+v", result)
	}
	if got := webhookSecretRevisionCount(t, pool, "webhook", "production", platform.DefaultWebhookSecretName); got != 1 {
		t.Fatalf("replay added a secret revision: %d", got)
	}

	conflict := first
	conflict.Input = json.RawMessage(`{"auto_deploy":true,"config":{"OTHER":"value"}}`)
	if _, err := s.SubmitWithReceipt(ctx, conflict); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed input error = %v", err)
	}
	if got := webhookSecretRevisionCount(t, pool, "webhook", "production", platform.DefaultWebhookSecretName); got != 1 {
		t.Fatalf("conflict added a secret revision: %d", got)
	}

	// Rotation-only is a real admitted operation but must not fabricate an
	// unchanged desired-state revision.
	var beforeRevision string
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='webhook' AND e.name='production'`).Scan(&beforeRevision); err != nil {
		t.Fatal(err)
	}
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
	var rotatedInput []byte
	if err := pool.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, rotated.Operation.ID).Scan(&rotatedInput); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rotatedInput), "webhook-secret-value-2") {
		t.Fatal("the rotated transient webhook secret entered the operation input")
	}
	if err := pool.QueryRow(ctx, `SELECT current_revision_id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='webhook' AND e.name='production'`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	if revisionID != beforeRevision {
		t.Fatalf("rotation changed the current revision %q -> %q", beforeRevision, revisionID)
	}
	if got := webhookSecretRevisionCount(t, pool, "webhook", "production", platform.DefaultWebhookSecretName); got != 2 {
		t.Fatalf("secret revisions after rotation = %d", got)
	}
	rotateReplay, err := s.SubmitWithReceipt(ctx, rotation)
	if err != nil {
		t.Fatal(err)
	}
	if !rotateReplay.Replay || rotateReplay.WebhookSecretActivated {
		t.Fatalf("rotation replay = %+v", rotateReplay)
	}
	if got := webhookSecretRevisionCount(t, pool, "webhook", "production", platform.DefaultWebhookSecretName); got != 2 {
		t.Fatalf("rotation replay added a secret revision: %d", got)
	}
	withoutSecret := rotation
	withoutSecret.IdempotencyKey = "webhook-rotate-no-secret"
	withoutSecret.WebhookSecret = nil
	if _, err := s.SubmitWithReceipt(ctx, withoutSecret); err == nil || contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("rotation without a transient secret error = %v", err)
	}
	if got := webhookSecretRevisionCount(t, pool, "webhook", "production", platform.DefaultWebhookSecretName); got != 2 {
		t.Fatalf("rejected rotation added a secret revision: %d", got)
	}
}

func TestIntegrationPlatformWebhookConcurrentAdmissionLoser(t *testing.T) {
	pool, _ := testPool(t, true)
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
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
			"source": map[string]any{"kind": "git", "url": "https://github.com/acme/api", "ref": "main"},
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
	fresh, replay := 0, 0
	for index := range results {
		if errs[index] != nil {
			t.Fatalf("concurrent submit %d: %v", index, errs[index])
		}
		if results[index].Replay {
			replay++
			if results[index].WebhookSecretActivated {
				t.Fatalf("loser receipt = %+v", results[index])
			}
		} else {
			fresh++
			if !results[index].WebhookSecretActivated {
				t.Fatalf("winner receipt = %+v", results[index])
			}
		}
	}
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
	if got := webhookSecretRevisionCount(t, pool, "concurrent", "production", platform.DefaultWebhookSecretName); got != 1 {
		t.Fatalf("secret revisions = %d, want exactly the winner", got)
	}
}

func TestIntegrationPlatformWebhookAdmissionRollback(t *testing.T) {
	pool, _ := testPool(t, true)
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
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
			"source": map[string]any{"kind": "git", "url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	originalEnqueue := s.enqueuePlatform
	s.enqueuePlatform = func(ctx context.Context, tx pgx.Tx, op contract.Operation) (int64, error) {
		if _, err := originalEnqueue(ctx, tx, op); err != nil {
			return 0, err
		}
		return 0, errors.New("injected failure after webhook secret activation")
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
	s.enqueuePlatform = originalEnqueue
	var operationsCount, revisionsCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, submission.IdempotencyKey).Scan(&operationsCount); err != nil {
		t.Fatal(err)
	}
	if operationsCount != 0 {
		t.Fatalf("rolled-back admission left %d operations", operationsCount)
	}
	if got := webhookSecretRevisionCount(t, pool, "rollback", "production", platform.DefaultWebhookSecretName); got != 0 {
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
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, LocalNodeName: "local", NodeStateDir: t.TempDir(), Vault: secrets,
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
			"source": map[string]any{"kind": "git", "url": "https://github.com/acme/api", "ref": "main"},
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
	defer rollback(deleteTx)
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
	if got := webhookSecretRevisionCount(t, pool, "order", "production", platform.DefaultWebhookSecretName); got != 2 {
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
				"kind": "git", "url": "https://github.com/acme/api", "ref": "main",
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
            "source":{"kind":"git","url":"https://github.com/acme/api","ref":"main",
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
	if got := webhookSecretRevisionCount(t, pool, "collision", "production", platform.DefaultWebhookSecretName); got != 1 {
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
			"source": map[string]any{"kind": "git", "url": "https://github.com/acme/api", "ref": "main"},
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
	if got := webhookSecretRevisionCount(t, pool, "httpwebhook", "production", platform.DefaultWebhookSecretName); got != 1 {
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
			"source": map[string]any{"kind": "git", "url": "https://github.com/acme/api", "ref": "main"},
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
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: concurrency, TestOnly: true, LocalNodeName: "local",
		NodeStateDir: t.TempDir(), Vault: secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	return pool, run, secrets
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

func webhookSecretRevisionCount(t *testing.T, pool *pgxpool.Pool, app, environment, name string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_secret_revisions
        WHERE app=$1 AND environment=$2 AND name=$3`, app, environment, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
