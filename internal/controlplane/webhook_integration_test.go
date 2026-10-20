package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const integrationWebhookSecret = "integration-webhook-secret-0123456789"

func signedIntegrationWebhook(t *testing.T, body []byte, delivery string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", delivery)
	mac := hmac.New(sha256.New, []byte(integrationWebhookSecret))
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return request
}

// TestIntegrationWebhookDeliverySurvivesOperationRetention proves the durable
// delivery table is the replay guard, not operation idempotency. After the
// operation and its idempotency row are deleted, the same signed delivery is
// answered from the delivery record and creates nothing.
func TestIntegrationWebhookDeliverySurvivesOperationRetention(t *testing.T) {
	pool, dsn := testPool(t, true)
	keyFile := writeKeyFile(t)
	nodeDir := t.TempDir()
	ctx := context.Background()
	cp := startControlPlane(t, dsn, keyFile, nodeDir, 2, false)
	store := NewWebhookDeliveryStore(pool)
	handler, err := api.NewWebhookHandler(api.WebhookConfig{
		Secret:     []byte(integrationWebhookSecret),
		Allowed:    []api.AllowedPush{{Repository: "Acme/API", Branch: "main", App: "api", Environment: "production"}},
		Kind:       platform.KindAppCreate,
		Target:     func(api.WebhookEvent) (string, error) { return "platform", nil },
		Input:      func(api.WebhookEvent) (json.RawMessage, error) { return json.RawMessage(`{"name":"webhookapp"}`), nil },
		Deliveries: store,
	}, cp.run.Service)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"ref": "refs/heads/main", "after": strings.Repeat("1", 40), "deleted": false, "forced": false,
		"repository": map[string]any{"full_name": "Acme/API"},
		"sender":     map[string]any{"login": "octocat"},
	})
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, signedIntegrationWebhook(t, body, "integration-delivery-1"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d body=%s", first.Code, first.Body.String())
	}
	var accepted struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil || accepted.OperationID == "" {
		t.Fatalf("first response = %s %v", first.Body.String(), err)
	}
	await(t, 20*time.Second, func() bool {
		var state string
		err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, accepted.OperationID).Scan(&state)
		return err == nil && state == "succeeded"
	})

	// Simulate operation retention: remove the operation, its events/steps, and
	// the idempotency row the old design relied on.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DELETE FROM eregion_platform_changes WHERE operation_id=$1`,
		`DELETE FROM eregion_operation_events WHERE operation_id=$1`,
		`DELETE FROM eregion_operation_steps WHERE operation_id=$1`,
		`DELETE FROM eregion_operations WHERE id=$1`,
	} {
		if _, err := tx.Exec(ctx, statement, accepted.OperationID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE id=$1`, accepted.OperationID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("operation retention simulation failed: %d %v", remaining, err)
	}

	// Replay the exact same delivery after retention.
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, signedIntegrationWebhook(t, body, "integration-delivery-1"))
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d body=%s", replay.Code, replay.Body.String())
	}
	var replayed struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.OperationID != accepted.OperationID {
		t.Fatalf("replay created a new operation: %s then %s", accepted.OperationID, replayed.OperationID)
	}
	var operations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("replay after retention created an operation: %d %v", operations, err)
	}

	// Same delivery ID with different signed bytes is a conflict.
	different := append(append([]byte{}, body...), '\n')
	conflict := httptest.NewRecorder()
	handler.ServeHTTP(conflict, signedIntegrationWebhook(t, different, "integration-delivery-1"))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different payload status = %d body=%s", conflict.Code, conflict.Body.String())
	}

	// The durable row contains hashes and identity only, never the secret,
	// signature header, raw delivery ID, or request body.
	var record, raw string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(d)::text FROM eregion_webhook_deliveries d`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	record = string(raw)
	for _, forbidden := range []string{integrationWebhookSecret, "integration-delivery-1", strings.Repeat("1", 40), "X-Hub-Signature"} {
		if strings.Contains(record, forbidden) {
			t.Fatalf("delivery row contains %q: %s", forbidden, record)
		}
	}
	var stored api.WebhookDelivery
	var found bool
	stored, found, err = store.Lookup(ctx, api.WebhookDeliveryHash("integration-delivery-1"))
	if err != nil || !found || !stored.Submitted || stored.OperationID != contract.OperationID(accepted.OperationID) {
		t.Fatalf("delivery lookup = %+v found=%v err=%v", stored, found, err)
	}
}
