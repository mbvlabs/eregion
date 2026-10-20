package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationWebhookDeliveryIsDeduplicated exercises the mounted serve mux:
// a signed GitHub push crosses api.NewMux into api.WebhookHandler and the real
// control-plane Service.Submit with PostgreSQL and River. Two identical
// deliveries return the same operation and leave exactly one operation and job.
func TestIntegrationWebhookDeliveryIsDeduplicated(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	const secret = "0123456789abcdef0123456789abcdef"
	handler, err := api.NewWebhookHandler(api.WebhookConfig{
		Secret: []byte(secret),
		Allowed: []api.AllowedPush{{
			Repository: "owner/repo", Branch: "main", App: "shop", Environment: "production",
		}},
		Kind: platform.KindAppCreate,
		Target: func(api.WebhookEvent) (string, error) {
			return platform.PlatformTarget().String(), nil
		},
		Input: func(api.WebhookEvent) (json.RawMessage, error) {
			return json.RawMessage(`{"name":"shop"}`), nil
		},
	}, run.Service)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.NewMux(api.MuxConfig{Webhook: handler, Default: http.NotFoundHandler()}))
	defer server.Close()

	payload, _ := json.Marshal(map[string]any{
		"ref": "refs/heads/main", "after": "0123456789abcdef0123456789abcdef01234567",
		"repository": map[string]string{"full_name": "owner/repo"},
		"sender":     map[string]string{"login": "octocat"},
	})
	deliver := func() map[string]any {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/webhooks/github", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(payload)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-GitHub-Event", "push")
		request.Header.Set("X-GitHub-Delivery", "delivery-1")
		request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d body = %s", response.StatusCode, raw)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	first := deliver()
	second := deliver()
	if first["operation_id"] == "" || first["operation_id"] != second["operation_id"] {
		t.Fatalf("duplicate delivery created a different operation: %v / %v", first, second)
	}

	ctx := context.Background()
	var operations, jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1`, platform.KindAppCreate).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind='platform_action'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if operations != 1 || jobs != 1 {
		t.Fatalf("duplicate delivery persisted %d operations and %d jobs", operations, jobs)
	}
	var actorKind, actorSubject, key string
	if err := pool.QueryRow(ctx, `SELECT actor_kind, actor_subject, idempotency_key FROM eregion_operations WHERE kind=$1`, platform.KindAppCreate).
		Scan(&actorKind, &actorSubject, &key); err != nil {
		t.Fatal(err)
	}
	if actorKind != "webhook" || actorSubject != "github:owner/repo" || key != "ghdelivery-delivery-1" {
		t.Fatalf("webhook identity = %s/%s key=%s", actorKind, actorSubject, key)
	}
}
