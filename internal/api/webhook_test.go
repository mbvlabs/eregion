package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

const webhookSecret = "github-webhook-test-secret-0123456789"

type webhookService struct {
	mu          sync.Mutex
	submissions []operations.Submission
	byKey       map[string]contract.Operation
	next        int
	err         error
}

func newWebhookService() *webhookService {
	return &webhookService{byKey: map[string]contract.Operation{}}
}

func (s *webhookService) Submit(_ context.Context, sub operations.Submission) (contract.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return contract.Operation{}, s.err
	}
	s.submissions = append(s.submissions, sub)
	if existing, ok := s.byKey[sub.IdempotencyKey]; ok {
		return existing, nil
	}
	s.next++
	id := contract.OperationID("op_" + strings.Repeat("a", 31) + string(rune('a'+s.next%26)))
	if s.next > 1 {
		id = contract.OperationID("op_" + strings.Repeat("b", 32))
	}
	op := contract.Operation{
		ID: id, Kind: sub.Kind, Target: sub.Target, Actor: sub.Actor, State: contract.Queued,
		LastSequence: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	s.byKey[sub.IdempotencyKey] = op
	return op, nil
}

func (*webhookService) Get(context.Context, contract.Actor, contract.OperationID) (contract.Operation, error) {
	return contract.Operation{}, errors.New("not used")
}
func (*webhookService) Events(context.Context, contract.Actor, contract.Cursor, int) (operations.EventPage, error) {
	return operations.EventPage{}, errors.New("not used")
}
func (*webhookService) RequestCancel(context.Context, contract.Actor, contract.OperationID) (contract.Operation, error) {
	return contract.Operation{}, errors.New("not used")
}

func webhookPayload() []byte {
	raw, _ := json.Marshal(map[string]any{
		"ref":     "refs/heads/main",
		"after":   strings.Repeat("1", 40),
		"deleted": false,
		"forced":  false,
		"repository": map[string]any{
			"full_name": "Acme/API", "name": "API", "clone_url": "https://github.com/Acme/API.git",
		},
		"sender": map[string]any{"login": "octocat"},
		"head_commit": map[string]any{
			"id": strings.Repeat("1", 40), "message": "change",
		},
	})
	return raw
}

func newTestWebhook(t *testing.T, service operations.Service, config WebhookConfig) *WebhookHandler {
	t.Helper()
	if config.Secret == nil {
		config.Secret = []byte(webhookSecret)
	}
	if config.Allowed == nil {
		config.Allowed = []AllowedPush{{Repository: "Acme/API", Branch: "main", App: "api", Environment: "production"}}
	}
	if config.Deliveries == nil {
		config.Deliveries = NewMemoryWebhookDeliveries()
	}
	handler, err := NewWebhookHandler(config, service)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func signedWebhookRequest(t *testing.T, body []byte, event, delivery string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-GitHub-Delivery", delivery)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	// Deliberately no Authorization header: GitHub cannot send a bearer token.
	return request
}

func serveWebhook(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestWebhookVerifiesAndSubmitsThroughOperationService(t *testing.T) {
	service := newWebhookService()
	handler := newTestWebhook(t, service, WebhookConfig{})
	body := webhookPayload()
	response := serveWebhook(handler, signedWebhookRequest(t, body, "push", "delivery-1"))
	if response.Code != http.StatusAccepted {
		t.Fatalf("push status = %d body=%s", response.Code, response.Body.String())
	}
	if len(service.submissions) != 1 {
		t.Fatalf("submissions = %d", len(service.submissions))
	}
	submission := service.submissions[0]
	if submission.Kind != platform.KindReleaseCreate || submission.Target != "env/api/production" {
		t.Fatalf("unexpected submission: %+v", submission)
	}
	if submission.Actor != (contract.Actor{Kind: "webhook", Subject: "github:acme/api"}) {
		t.Fatalf("unexpected actor: %+v", submission.Actor)
	}
	if submission.IdempotencyKey != "ghdelivery-delivery-1" {
		t.Fatalf("unstable delivery key: %s", submission.IdempotencyKey)
	}
	var input struct {
		App         string `json:"app"`
		Environment string `json:"environment"`
		Source      struct {
			Type string `json:"type"`
			URL  string `json:"url"`
			Ref  string `json:"ref"`
		} `json:"source"`
	}
	if err := json.Unmarshal(submission.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.App != "api" || input.Environment != "production" || input.Source.Type != "git" ||
		input.Source.URL != "https://github.com/acme/api.git" || input.Source.Ref != strings.Repeat("1", 40) {
		t.Fatalf("unexpected default release input: %+v", input)
	}
	if strings.Contains(string(submission.Input), webhookSecret) {
		t.Fatal("webhook secret entered input")
	}
	// GitHub retries the same delivery; the stable key returns the same durable
	// operation rather than creating a second one.
	response = serveWebhook(handler, signedWebhookRequest(t, body, "push", "delivery-1"))
	if response.Code != http.StatusAccepted || len(service.byKey) != 1 {
		t.Fatalf("retry was not deduplicated: %d %+v", response.Code, service.byKey)
	}
}

func TestWebhookRejectsInvalidSignatureAndBounds(t *testing.T) {
	service := newWebhookService()
	handler := newTestWebhook(t, service, WebhookConfig{MaxBody: 1024})
	body := webhookPayload()
	bad := signedWebhookRequest(t, body, "push", "delivery-1")
	bad.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	if response := serveWebhook(handler, bad); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d", response.Code)
	}
	missing := signedWebhookRequest(t, body, "push", "delivery-1")
	missing.Header.Del("X-Hub-Signature-256")
	if response := serveWebhook(handler, missing); response.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature status = %d", response.Code)
	}
	unsigned := signedWebhookRequest(t, body, "push", "delivery-1")
	unsigned.Header.Del("X-GitHub-Delivery")
	if response := serveWebhook(handler, unsigned); response.Code != http.StatusBadRequest {
		t.Fatalf("missing delivery status = %d", response.Code)
	}
	oversized := signedWebhookRequest(t, append(body, []byte(strings.Repeat(" ", 2048))...), "push", "delivery-2")
	if response := serveWebhook(handler, oversized); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d", response.Code)
	}
	invalid := signedWebhookRequest(t, []byte(`{"ref":`), "push", "delivery-3")
	if response := serveWebhook(handler, invalid); response.Code != http.StatusBadRequest {
		t.Fatalf("malformed payload status = %d", response.Code)
	}
	get := httptest.NewRequest(http.MethodGet, "/webhooks/github", nil)
	if response := serveWebhook(handler, get); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", response.Code)
	}
	if len(service.submissions) != 0 {
		t.Fatal("rejected delivery reached the service")
	}
}

func TestWebhookAllowlistAndIgnoredEvents(t *testing.T) {
	service := newWebhookService()
	handler := newTestWebhook(t, service, WebhookConfig{})
	cases := []struct {
		name     string
		payload  []byte
		event    string
		delivery string
	}{
		{"repo", []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("1", 40) + `","repository":{"full_name":"evil/repo"}}`), "push", "d1"},
		{"branch", []byte(`{"ref":"refs/heads/dev","after":"` + strings.Repeat("1", 40) + `","repository":{"full_name":"Acme/API"}}`), "push", "d2"},
		{"tag", []byte(`{"ref":"refs/tags/v1","after":"` + strings.Repeat("1", 40) + `","repository":{"full_name":"Acme/API"}}`), "push", "d3"},
		{"delete", []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("0", 40) + `","deleted":true,"repository":{"full_name":"Acme/API"}}`), "push", "d4"},
		{"other event", []byte(`{}`), "issues", "d5"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := serveWebhook(handler, signedWebhookRequest(t, test.payload, test.event, test.delivery))
			if response.Code != http.StatusOK {
				t.Fatalf("ignored status = %d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if len(service.submissions) != 0 {
		t.Fatalf("ignored deliveries were submitted: %+v", service.submissions)
	}
	ping := serveWebhook(handler, signedWebhookRequest(t, []byte(`{"zen":"hi"}`), "ping", "ping-1"))
	if ping.Code != http.StatusOK {
		t.Fatalf("ping status = %d", ping.Code)
	}
}

func TestWebhookCustomDeployMappingAndConflict(t *testing.T) {
	service := newWebhookService()
	handler := newTestWebhook(t, service, WebhookConfig{
		Kind:   platform.KindDeploy,
		Target: func(WebhookEvent) (string, error) { return "env/api/production", nil },
		Input: func(event WebhookEvent) (json.RawMessage, error) {
			return json.Marshal(map[string]string{"app": "api", "environment": "production", "release_id": "rel_" + strings.Repeat("c", 32)})
		},
	})
	body := webhookPayload()
	response := serveWebhook(handler, signedWebhookRequest(t, body, "push", "delivery-deploy"))
	if response.Code != http.StatusAccepted || service.submissions[0].Kind != platform.KindDeploy {
		t.Fatalf("custom mapping failed: %d %+v", response.Code, service.submissions)
	}
	// Same delivery with different canonical input is refused by the durable
	// delivery record before a second operation can be submitted.
	response = serveWebhook(handler, signedWebhookRequest(t, append(body, '\n'), "push", "delivery-deploy"))
	if response.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestWebhookReplaySurvivesOperationRetention(t *testing.T) {
	service := newWebhookService()
	deliveries := NewMemoryWebhookDeliveries()
	handler := newTestWebhook(t, service, WebhookConfig{Deliveries: deliveries})
	body := webhookPayload()
	first := serveWebhook(handler, signedWebhookRequest(t, body, "push", "delivery-retained"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	var accepted webhookResponse
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil || accepted.OperationID == "" {
		t.Fatalf("first response = %s %v", first.Body.String(), err)
	}
	// Simulate operation and idempotency-row purge: the durable delivery row is
	// the only replay guard left.
	service.mu.Lock()
	service.byKey = map[string]contract.Operation{}
	service.submissions = nil
	service.mu.Unlock()
	second := serveWebhook(handler, signedWebhookRequest(t, body, "push", "delivery-retained"))
	if second.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d body=%s", second.Code, second.Body.String())
	}
	var replay webhookResponse
	if err := json.Unmarshal(second.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.OperationID != accepted.OperationID {
		t.Fatalf("replay created a new operation: %s then %s", accepted.OperationID, replay.OperationID)
	}
	if len(service.submissions) != 0 {
		t.Fatalf("replay reached the operation service %d times", len(service.submissions))
	}
	records := deliveries.Records()
	if len(records) != 1 || !records[0].Submitted || records[0].BodySHA256 != WebhookBodyHash(body) {
		t.Fatalf("delivery record = %+v", records)
	}
	if strings.Contains(records[0].DeliveryIDHash, "delivery-retained") {
		t.Fatal("raw delivery ID was stored")
	}
	// The same delivery ID with different bytes is a conflict, not new work.
	other := webhookPayload()
	other = append(other, '\n')
	conflict := serveWebhook(handler, signedWebhookRequest(t, other, "push", "delivery-retained"))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different body status = %d body=%s", conflict.Code, conflict.Body.String())
	}
	if len(service.submissions) != 0 {
		t.Fatal("conflicting replay reached the operation service")
	}
}

func TestWebhookConfigFailsClosed(t *testing.T) {
	service := newWebhookService()
	for _, config := range []WebhookConfig{
		{},
		{Secret: []byte("short")},
		{Allowed: []AllowedPush{{Repository: "not-a-repo", Branch: "main", App: "api", Environment: "prod"}}},
		{Allowed: []AllowedPush{{Repository: "a/b", Branch: "", App: "api", Environment: "prod"}}},
		{Allowed: []AllowedPush{{Repository: "a/b", Branch: "main", App: "Invalid", Environment: "prod"}}},
		{Secret: []byte(webhookSecret), Allowed: []AllowedPush{{Repository: "a/b", Branch: "main", App: "api", Environment: "prod"}}, Kind: platform.KindDeploy},
		{Secret: []byte(webhookSecret), Allowed: []AllowedPush{{Repository: "a/b", Branch: "main", App: "api", Environment: "prod"}}, MaxBody: 1},
	} {
		if _, err := NewWebhookHandler(config, service); err == nil {
			t.Fatalf("unsafe webhook config accepted: %+v", config)
		}
	}
	if _, err := NewWebhookHandler(WebhookConfig{Secret: []byte(webhookSecret), Allowed: []AllowedPush{{Repository: "a/b", Branch: "main", App: "api", Environment: "prod"}}}, nil); err == nil {
		t.Fatal("webhook handler accepted a nil operation service")
	}
	if _, err := NewWebhookHandler(WebhookConfig{Secret: []byte(webhookSecret), Allowed: []AllowedPush{{Repository: "a/b", Branch: "main", App: "api", Environment: "prod"}}}, service); err == nil {
		t.Fatal("webhook handler accepted a nil delivery store")
	}
}
