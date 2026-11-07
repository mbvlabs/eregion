package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const (
	testWebhookDelivery = "12345678-1234-1234-1234-123456789abc"
	testWebhookSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var testWebhookSecret = []byte("0123456789abcdef")

func testPushTarget(secret []byte) PushWebhookTarget {
	return PushWebhookTarget{
		App: "shop", Environment: "production",
		EnvironmentID:  "env_" + strings.Repeat("1", 32),
		RevisionID:     "erv_" + strings.Repeat("2", 32),
		Repository:     "mbvlabs/hello",
		Branch:         "main",
		Secret:         secret,
		SecretRevision: 7, SecretRevisionHash: strings.Repeat("3", 64),
	}
}

func validPushBody() []byte {
	return []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"deleted":false,"repository":{"full_name":"mbvlabs/hello"},"sender":{"login":"octocat"}}`, testWebhookSHA))
}

func signedPushRequest(t *testing.T, secret []byte, deliveryID string, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/shop/production", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	signWebhook(request, secret, body)
	return request
}

func signWebhook(request *http.Request, secret, body []byte) {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
}

type fakePushResolver struct {
	mu      sync.Mutex
	targets []PushWebhookTarget
	enabled bool
	err     error
	calls   int
}

func (f *fakePushResolver) ResolvePushWebhook(context.Context, string, string) (PushWebhookTarget, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return PushWebhookTarget{}, false, f.err
	}
	if len(f.targets) == 0 {
		return PushWebhookTarget{}, false, nil
	}
	index := f.calls - 1
	if index >= len(f.targets) {
		index = len(f.targets) - 1
	}
	return f.targets[index], f.enabled, nil
}

type fakeLegacyOperation struct {
	ID        contract.OperationID
	Canonical []byte
}

type fakeAdmissions struct {
	mu                 sync.Mutex
	records            map[string]WebhookAdmission
	legacy             map[string]fakeLegacyOperation
	admitCalls         int
	admitErr           error
	staleRemaining     int
	block              bool
	lastLegacyIncoming WebhookAdmission
}

func newFakeAdmissions() *fakeAdmissions {
	return &fakeAdmissions{records: map[string]WebhookAdmission{}, legacy: map[string]fakeLegacyOperation{}}
}

func (f *fakeAdmissions) seed(record WebhookAdmission) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[record.DeliveryIDHash] = record
}

func (f *fakeAdmissions) addLegacy(key string, operationID contract.OperationID, canonical []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.legacy[key] = fakeLegacyOperation{ID: operationID, Canonical: canonical}
}

func (f *fakeAdmissions) Admit(ctx context.Context, incoming WebhookAdmission) (WebhookAdmission, bool, error) {
	f.mu.Lock()
	f.admitCalls++
	block := f.block
	err := f.admitErr
	f.admitErr = nil
	stale := f.staleRemaining
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return WebhookAdmission{}, false, ctx.Err()
	}
	if err != nil {
		return WebhookAdmission{}, false, err
	}
	if stale > 0 {
		f.mu.Lock()
		f.staleRemaining--
		f.mu.Unlock()
		return WebhookAdmission{}, false, ErrWebhookBindingChanged
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.records[incoming.DeliveryIDHash]; ok {
		if existing.BodySHA256 != incoming.BodySHA256 ||
			existing.Actor.Kind != incoming.Actor.Kind ||
			existing.Actor.Subject != incoming.Actor.Subject ||
			existing.Kind != incoming.Kind ||
			existing.Target != incoming.Target {
			return existing, false, contract.NewFault(contract.Conflict, "webhook delivery already bound a different identity", "Never submit a second operation for one GitHub delivery ID.")
		}
		return existing, false, nil
	}
	incoming.State = WebhookDeliveryAdmitted
	incoming.FirstSeenAt = time.Now().UTC()
	f.records[incoming.DeliveryIDHash] = incoming
	return incoming, true, nil
}

func (f *fakeAdmissions) ReconcileLegacy(_ context.Context, incoming WebhookAdmission, legacyKey string, canonical []byte) (WebhookAdmission, bool, error) {
	f.mu.Lock()
	f.lastLegacyIncoming = incoming
	f.mu.Unlock()
	// The production store validates the freshly verified snapshot. Legacy
	// pending rows have empty snapshot columns, so passing the stored row here
	// must fail just as it would in production.
	if incoming.EnvironmentID == "" || incoming.RevisionID == "" || incoming.SecretRevision == 0 ||
		incoming.SecretRevisionHash == "" || incoming.Repository == "" || incoming.Branch == "" ||
		!platform.IsGitCommit(incoming.Commit) {
		return WebhookAdmission{}, false, contract.NewFault(contract.ValidationFailed, "legacy webhook reconciliation has no fresh snapshot", "Use the request's verified snapshot, not the stored legacy row.")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.records[incoming.DeliveryIDHash]
	if !ok {
		return WebhookAdmission{}, false, contract.NewFault(contract.PreconditionFailed, "delivery disappeared", "Do not resubmit.")
	}
	if existing.State == WebhookDeliverySubmitted {
		return existing, true, nil
	}
	operation, ok := f.legacy[legacyKey]
	if !ok {
		return existing, false, nil
	}
	if !bytes.Equal(operation.Canonical, canonical) {
		return existing, false, contract.NewFault(contract.Conflict, "legacy delivery input differs", "Do not resubmit.")
	}
	existing.State = WebhookDeliverySubmitted
	existing.OperationID = operation.ID
	f.records[incoming.DeliveryIDHash] = existing
	return existing, true, nil
}

func newWebhookTestHandler(t *testing.T, resolver PushWebhookResolver, admissions WebhookAdmissionStore, configure func(*PushWebhookConfig)) *PushWebhookHandler {
	t.Helper()
	config := PushWebhookConfig{Resolver: resolver, Admissions: admissions}
	if configure != nil {
		configure(&config)
	}
	handler, err := NewPushWebhookHandler(config)
	if err != nil {
		t.Fatalf("NewPushWebhookHandler: %v", err)
	}
	return handler
}

func serveWebhook(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeWebhookResponse(t *testing.T, recorder *httptest.ResponseRecorder) webhookResponse {
	t.Helper()
	var response webhookResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return response
}

func TestPushWebhookAcceptsOnlyAfterDurableAdmission(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	body := validPushBody()
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, body))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeWebhookResponse(t, recorder)
	if !response.Accepted || response.OperationID != "" || response.Ambiguous {
		t.Fatalf("unexpected response: %+v", response)
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 1 {
		t.Fatalf("admit calls = %d, want 1", admissions.admitCalls)
	}
	record := admissions.records[WebhookDeliveryHash(testWebhookDelivery)]
	if record.State != WebhookDeliveryAdmitted {
		t.Fatalf("state = %q, want admitted", record.State)
	}
	if record.Commit != testWebhookSHA || record.Repository != "mbvlabs/hello" || record.Branch != "main" {
		t.Fatalf("sanitized intent = %+v", record)
	}
	if record.BodySHA256 != WebhookBodyHash(body) {
		t.Fatalf("body hash = %q", record.BodySHA256)
	}
	if record.EnvironmentID == "" || record.RevisionID == "" || record.SecretRevision == 0 {
		t.Fatalf("missing non-secret snapshot identity: %+v", record)
	}
}

func TestPushWebhookRejectsBadSignatureBeforeDuplicateLookup(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.seed(WebhookAdmission{
		DeliveryIDHash: WebhookDeliveryHash(testWebhookDelivery), BodySHA256: WebhookBodyHash(validPushBody()),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"}, Kind: "platform.deploy",
		Target: "env/shop/production", State: WebhookDeliverySubmitted, OperationID: contract.OperationID("op_" + strings.Repeat("4", 32)),
	})
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	request := signedPushRequest(t, []byte("wrong-secret-value"), testWebhookDelivery, validPushBody())
	recorder := serveWebhook(handler, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", recorder.Code, recorder.Body.String())
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 0 {
		t.Fatalf("duplicate lookup ran before signature rejection: %d calls", admissions.admitCalls)
	}
}

func TestPushWebhookDuplicateSubmittedReturnsOriginalOperation(t *testing.T) {
	admissions := newFakeAdmissions()
	operationID := contract.OperationID("op_" + strings.Repeat("5", 32))
	admissions.seed(WebhookAdmission{
		DeliveryIDHash: WebhookDeliveryHash(testWebhookDelivery), BodySHA256: WebhookBodyHash(validPushBody()),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"}, Kind: "platform.deploy",
		Target: "env/shop/production", State: WebhookDeliverySubmitted, OperationID: operationID,
	})
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeWebhookResponse(t, recorder)
	if response.OperationID != string(operationID) || !response.Accepted {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestPushWebhookDuplicateAdmittedHasNoFabricatedOperation(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.seed(WebhookAdmission{
		DeliveryIDHash: WebhookDeliveryHash(testWebhookDelivery), BodySHA256: WebhookBodyHash(validPushBody()),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"}, Kind: "platform.deploy",
		Target: "env/shop/production", State: WebhookDeliveryAdmitted,
	})
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeWebhookResponse(t, recorder)
	if response.OperationID != "" || !response.Accepted {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestPushWebhookConflictOnDifferentIdentity(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.seed(WebhookAdmission{
		DeliveryIDHash: WebhookDeliveryHash(testWebhookDelivery), BodySHA256: WebhookBodyHash([]byte(`{"other":true}`)),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"}, Kind: "platform.deploy",
		Target: "env/shop/production", State: WebhookDeliverySubmitted, OperationID: contract.OperationID("op_" + strings.Repeat("6", 32)),
	})
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPushWebhookAdmissionFailureReturns503(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.admitErr = contract.NewFault(contract.Unavailable, "control-plane database is unavailable", "Retry the delivery.")
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeWebhookResponse(t, recorder)
	if response.Accepted {
		t.Fatalf("failed admission claimed acceptance: %+v", response)
	}
}

func TestPushWebhookStaleBindingReresolvesOnce(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.staleRemaining = 1
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	resolver.mu.Lock()
	calls := resolver.calls
	resolver.mu.Unlock()
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2", calls)
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 2 {
		t.Fatalf("admit calls = %d, want 2", admissions.admitCalls)
	}
}

func TestPushWebhookStaleBindingReverifiesCurrentSecret(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.staleRemaining = 1
	resolver := &fakePushResolver{
		targets: []PushWebhookTarget{testPushTarget(testWebhookSecret), testPushTarget([]byte("fedcba9876543210"))},
		enabled: true,
	}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 after rotation: %s", recorder.Code, recorder.Body.String())
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 1 {
		t.Fatalf("stale retry admitted before reverifying: %d calls", admissions.admitCalls)
	}
}

func TestPushWebhookStaleBindingTwiceReturns503(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.staleRemaining = 2
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPushWebhookMissingEnvironmentReturns404(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{err: contract.NewFault(contract.NotFound, "environment not found", "Check the URL.")}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 0 {
		t.Fatalf("missing environment was admitted")
	}
}

func TestPushWebhookAutoDeployDisabledIgnored(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: false}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 0 {
		t.Fatalf("disabled auto-deploy was admitted")
	}
}

func TestPushWebhookPingRequiresSignatureAndDoesNotAdmit(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	body := []byte(`{"zen":"hello"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/shop/production", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "ping")
	signWebhook(request, testWebhookSecret, body)
	recorder := serveWebhook(handler, request)
	if recorder.Code != http.StatusOK || decodeWebhookResponse(t, recorder).Message != "pong" {
		t.Fatalf("ping = %d %s", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/shop/production", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "ping")
	request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	recorder = serveWebhook(handler, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("bad ping signature = %d, want 401", recorder.Code)
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 0 {
		t.Fatalf("ping was admitted")
	}
}

func TestPushWebhookUnsupportedEventIgnored(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	body := []byte(`{"action":"created"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/shop/production", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "issues")
	signWebhook(request, testWebhookSecret, body)
	recorder := serveWebhook(handler, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 0 {
		t.Fatalf("unsupported event was admitted")
	}
}

func TestPushWebhookMalformedPayloadBadRequest(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, []byte(`{"ref":`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPushWebhookFiltersDoNotAdmit(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		event   string
		target  PushWebhookTarget
		message string
	}{
		{name: "repository", body: validPushBody(), event: "push", target: func() PushWebhookTarget {
			target := testPushTarget(testWebhookSecret)
			target.Repository = "other/repo"
			return target
		}(), message: "repository is not bound"},
		{name: "branch", body: validPushBody(), event: "push", target: func() PushWebhookTarget {
			target := testPushTarget(testWebhookSecret)
			target.Branch = "release"
			return target
		}(), message: "push is not to the bound branch"},
		{name: "deletion", body: []byte(`{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000","deleted":true,"repository":{"full_name":"mbvlabs/hello"}}`), event: "push", target: testPushTarget(testWebhookSecret), message: "branch deletion is not deployed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			admissions := newFakeAdmissions()
			resolver := &fakePushResolver{targets: []PushWebhookTarget{test.target}, enabled: true}
			handler := newWebhookTestHandler(t, resolver, admissions, nil)
			request := signedPushRequest(t, testWebhookSecret, testWebhookDelivery, test.body)
			request.Header.Set("X-GitHub-Event", test.event)
			recorder := serveWebhook(handler, request)
			if recorder.Code != http.StatusOK || !strings.Contains(decodeWebhookResponse(t, recorder).Message, test.message) {
				t.Fatalf("filter = %d %s", recorder.Code, recorder.Body.String())
			}
			admissions.mu.Lock()
			defer admissions.mu.Unlock()
			if admissions.admitCalls != 0 {
				t.Fatalf("filtered delivery was admitted")
			}
		})
	}
}

func TestPushWebhookLegacyPendingReconcilesOperation(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.seed(WebhookAdmission{
		DeliveryIDHash: WebhookDeliveryHash(testWebhookDelivery), BodySHA256: WebhookBodyHash(validPushBody()),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"}, Kind: "platform.deploy",
		Target: "env/shop/production", State: WebhookDeliveryPending,
	})
	operationID := contract.OperationID("op_" + strings.Repeat("7", 32))
	canonical, err := json.Marshal(map[string]string{"app": "shop", "environment": "production", "ref": testWebhookSHA})
	if err != nil {
		t.Fatal(err)
	}
	admissions.addLegacy("ghdelivery-"+testWebhookDelivery, operationID, canonical)
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeWebhookResponse(t, recorder)
	if response.OperationID != string(operationID) || response.Ambiguous {
		t.Fatalf("unexpected legacy response: %+v", response)
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.lastLegacyIncoming.EnvironmentID == "" ||
		admissions.lastLegacyIncoming.RevisionID == "" ||
		admissions.lastLegacyIncoming.SecretRevision == 0 ||
		admissions.lastLegacyIncoming.Commit != testWebhookSHA {
		t.Fatalf("legacy reconciliation received the stored row, not the fresh snapshot: %+v", admissions.lastLegacyIncoming)
	}
}

func TestPushWebhookLegacyPendingMissingReportsAmbiguous(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.seed(WebhookAdmission{
		DeliveryIDHash: WebhookDeliveryHash(testWebhookDelivery), BodySHA256: WebhookBodyHash(validPushBody()),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"}, Kind: "platform.deploy",
		Target: "env/shop/production", State: WebhookDeliveryPending,
	})
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeWebhookResponse(t, recorder)
	if !response.Ambiguous || response.OperationID != "" {
		t.Fatalf("missing legacy operation was not reported as ambiguous: %+v", response)
	}
}

func TestPushWebhookAdmissionTimeoutReturns503(t *testing.T) {
	admissions := newFakeAdmissions()
	admissions.block = true
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, func(config *PushWebhookConfig) {
		config.AdmissionTimeout = 100 * time.Millisecond
	})
	started := time.Now()
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("admission timeout took %s", elapsed)
	}
}

func TestPushWebhookLogsDurationWithoutPayload(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	var logs bytes.Buffer
	handler := newWebhookTestHandler(t, resolver, admissions, func(config *PushWebhookConfig) {
		config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	body := validPushBody()
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, body))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}
	output := logs.String()
	if !strings.Contains(output, "duration_ms") || !strings.Contains(output, "outcome=accepted") {
		t.Fatalf("admission timing was not logged: %s", output)
	}
	if strings.Contains(output, string(body)) || strings.Contains(output, string(testWebhookSecret)) || strings.Contains(output, testWebhookDelivery) {
		t.Fatalf("log contains payload, secret or raw delivery ID: %s", output)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	err       error
}

func (d *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	d.deadlines = append(d.deadlines, deadline)
	return d.err
}

type timeoutReadCloser struct {
	delay time.Duration
}

func (r timeoutReadCloser) Read([]byte) (int, error) {
	time.Sleep(r.delay)
	return 0, timeoutError{}
}
func (timeoutReadCloser) Close() error { return nil }

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestPushWebhookSetsReadDeadlineWithinAdmissionBudget(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if writer.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", writer.Code, writer.Body.String())
	}
	if len(writer.deadlines) < 2 {
		t.Fatalf("read deadline calls = %d, want set and clear", len(writer.deadlines))
	}
	first := writer.deadlines[0]
	if first.IsZero() || time.Until(first) <= 0 || time.Until(first) > DefaultWebhookAdmissionTimeout {
		t.Fatalf("read deadline %s is outside the admission budget", first)
	}
	if !writer.deadlines[len(writer.deadlines)-1].IsZero() {
		t.Fatal("read deadline was not cleared")
	}
	if got := writer.Header().Get("Connection"); got != "" {
		t.Fatalf("fully consumed request left Connection=%q; keep-alive would be poisoned", got)
	}
}

func TestPushWebhookHTTP2UnreadBodyKeepsStreamDeadlineWithoutConnectionClose(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: false}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/shop/production", nil)
	request.Proto, request.ProtoMajor, request.ProtoMinor = "HTTP/2.0", 2, 0
	writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, request)
	if writer.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", writer.Code, writer.Body.String())
	}
	if got := writer.Header().Get("Connection"); got != "" {
		t.Fatalf("HTTP/2 response set Connection=%q", got)
	}
	if len(writer.deadlines) == 0 || writer.deadlines[0].IsZero() {
		t.Fatal("HTTP/2 stream read deadline was not set")
	}
	if writer.deadlines[len(writer.deadlines)-1].IsZero() {
		t.Fatal("HTTP/2 unread-body stream deadline was cleared")
	}
}

func TestPushWebhookHTTP1KeepAliveSurvivesFullyConsumedRequest(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	ping := func() *http.Request {
		body := []byte(`{"zen":"hello"}`)
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/webhooks/github/shop/production", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-GitHub-Event", "ping")
		signWebhook(request, testWebhookSecret, body)
		return request
	}
	first, err := client.Do(ping())
	if err != nil {
		t.Fatal(err)
	}
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	// net/http returns an HTTP/1 connection to the keep-alive pool only after
	// the response body reaches EOF. Closing an unread body lets the transport
	// drain it asynchronously, so the next request can race the pool insertion
	// and open a new connection even though the server reused the connection.
	// Consume the response before attributing a missing reuse to the server.
	if _, err := io.Copy(io.Discard, first.Body); err != nil {
		t.Fatalf("read first response body: %v", err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatalf("close first response body: %v", err)
	}
	if first.Close {
		t.Fatal("fully consumed webhook request received a connection close")
	}
	reused := make(chan bool, 1)
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused <- info.Reused }}
	secondRequest := ping().WithContext(httptrace.WithClientTrace(context.Background(), trace))
	second, err := client.Do(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", second.StatusCode)
	}
	select {
	case got := <-reused:
		if !got {
			t.Fatal("fully consumed webhook request poisoned keep-alive; second request opened a new connection")
		}
	case <-time.After(time.Second):
		t.Fatal("client trace did not report a connection")
	}
}

func TestPushWebhookHTTP1StalledBodyEarlyResponseClosesConnection(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: false}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(connection, "POST /v1/webhooks/github/shop/production HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000000\r\n\r\n", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("early response waited for the stalled body: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !response.Close {
		t.Fatal("early response did not close the HTTP/1 connection")
	}
}

func TestPushWebhookHTTP1UnauthorizedResponseIsBounded(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	body := validPushBody()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/webhooks/github/shop/production", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", testWebhookDelivery)
	request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("unauthorized response failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

func TestPushWebhookHTTP1StalledBodyTimeoutReturns503(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, func(config *PushWebhookConfig) {
		config.AdmissionTimeout = 200 * time.Millisecond
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = fmt.Fprintf(connection, "POST /v1/webhooks/github/shop/production HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000000\r\n\r\n", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("stalled-body timeout response failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout response took %s", elapsed)
	}
	if !response.Close {
		t.Fatal("timeout response did not close the HTTP/1 connection")
	}
}

func TestPushWebhookToleratesUnsupportedReadDeadline(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	recorder := serveWebhook(handler, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPushWebhookFailsClosedWhenReadDeadlineIsUnsupportedByTransport(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, nil)
	writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), err: errors.New("deadline unavailable")}
	handler.ServeHTTP(writer, signedPushRequest(t, testWebhookSecret, testWebhookDelivery, validPushBody()))
	if writer.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", writer.Code, writer.Body.String())
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if admissions.admitCalls != 0 {
		t.Fatalf("admission ran without a read deadline")
	}
}

func TestPushWebhookBodyReadTimeoutReturns503(t *testing.T) {
	admissions := newFakeAdmissions()
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	handler := newWebhookTestHandler(t, resolver, admissions, func(config *PushWebhookConfig) {
		config.AdmissionTimeout = 100 * time.Millisecond
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/shop/production", nil)
	request.Body = timeoutReadCloser{delay: 150 * time.Millisecond}
	request.ContentLength = 1
	request.Header.Set("Content-Type", "application/json")
	recorder := serveWebhook(handler, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
}

func TestNewPushWebhookHandlerValidatesConfig(t *testing.T) {
	resolver := &fakePushResolver{targets: []PushWebhookTarget{testPushTarget(testWebhookSecret)}, enabled: true}
	admissions := newFakeAdmissions()
	if _, err := NewPushWebhookHandler(PushWebhookConfig{Admissions: admissions}); err == nil {
		t.Fatal("missing resolver accepted")
	}
	if _, err := NewPushWebhookHandler(PushWebhookConfig{Resolver: resolver}); err == nil {
		t.Fatal("missing admission store accepted")
	}
	if _, err := NewPushWebhookHandler(PushWebhookConfig{Resolver: resolver, Admissions: admissions, AdmissionTimeout: 10 * time.Second}); err == nil {
		t.Fatal("overlong admission timeout accepted")
	}
}
