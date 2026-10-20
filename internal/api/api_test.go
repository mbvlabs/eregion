package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
)

const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const id contract.OperationID = "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func principal(scopes ...string) Principal {
	hash := sha256.Sum256([]byte(token))
	return Principal{Actor: contract.Actor{Kind: "ci", Subject: "test-ci"}, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: scopes}
}

type fakeService struct {
	called     int
	actor      contract.Actor
	panicOnGet bool
}

func fakeOperation(state contract.State) contract.Operation {
	return contract.Operation{ID: id, Kind: operations.TestKind, Target: operations.TestTarget, Actor: contract.Actor{Kind: "ci", Subject: "test-ci"}, State: state, Attempt: 1, LastSequence: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
}

func (s *fakeService) Submit(_ context.Context, sub operations.Submission) (contract.Operation, error) {
	s.called++
	s.actor = sub.Actor
	return fakeOperation(contract.Queued), nil
}
func (s *fakeService) Get(_ context.Context, actor contract.Actor, _ contract.OperationID) (contract.Operation, error) {
	if s.panicOnGet {
		panic("untrusted internal panic")
	}
	s.actor = actor
	return fakeOperation(contract.Running), nil
}
func (*fakeService) Events(_ context.Context, _ contract.Actor, cursor contract.Cursor, _ int) (operations.EventPage, error) {
	return operations.EventPage{Events: []contract.Event{}, Cursor: cursor}, nil
}
func (*fakeService) RequestCancel(context.Context, contract.Actor, contract.OperationID) (contract.Operation, error) {
	return fakeOperation(contract.Cancelling), nil
}

func handler(t *testing.T, service *fakeService, scopes ...string) http.Handler {
	t.Helper()
	auth, err := NewAuth([]Principal{principal(scopes...)})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(auth, service, func(context.Context) error { return nil })
}

func request(t *testing.T, h http.Handler, method, path, body, credential string) (*httptest.ResponseRecorder, contract.Envelope) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	r.Header.Set("Idempotency-Key", "unit:test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var envelope contract.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid API envelope: %s %v", w.Body.String(), err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatal(err)
	}
	return w, envelope
}

func TestAuthenticationAndScope(t *testing.T) {
	service := &fakeService{}
	h := handler(t, service, "operations:read")
	for _, test := range []struct {
		credential string
		status     int
	}{{"", 401}, {strings.Repeat("x", 64), 401}, {token, 403}} {
		w, envelope := request(t, h, http.MethodPost, "/v1/operations", `{"kind":"diagnostic.test","target":"control-plane","input":{}}`, test.credential)
		if w.Code != test.status || envelope.OK || service.called != 0 {
			t.Fatalf("authorization bypass: %d %+v", w.Code, envelope)
		}
	}
	w, envelope := request(t, h, http.MethodGet, "/v1/operations/"+string(id), "", token)
	if w.Code != 200 || !envelope.OK || service.actor.Subject != "test-ci" {
		t.Fatalf("identity not resolved by server: %+v", envelope)
	}
	_, envelope = request(t, h, http.MethodGet, "/healthz", "", "")
	if !envelope.OK {
		t.Fatal("liveness requires credentials")
	}
}

func TestSubmissionValidationAndPreview(t *testing.T) {
	service := &fakeService{}
	h := handler(t, service, "operations:submit")
	for _, body := range []string{
		`{"actor":{"kind":"system","subject":"root"},"kind":"diagnostic.test","target":"control-plane"}`,
		`{"kind":"deploy","target":"control-plane"}`,
		`{"kind":"diagnostic.test","target":"wrong"}`,
		`{"kind":"diagnostic.test","target":"control-plane","input":{"secret":"credential"}}`,
		`{"kind":"diagnostic.test","target":"control-plane","input":{"steps":999}}`,
		`{} {}`,
		strings.Repeat("x", 16385),
	} {
		w, envelope := request(t, h, http.MethodPost, "/v1/operations", body, token)
		if w.Code != 400 || envelope.OK || service.called != 0 {
			t.Fatalf("invalid request accepted: %s %+v", body, envelope)
		}
	}
	valid := `{"kind":"diagnostic.test","target":"control-plane","input":{}}`
	w, envelope := request(t, h, http.MethodPost, "/v1/operations", strings.TrimSuffix(valid, "}")+`,"dry_run":true}`, token)
	if w.Code != 200 || !envelope.OK || service.called != 0 {
		t.Fatalf("preview mutated state: %+v", envelope)
	}
	w, envelope = request(t, h, http.MethodPost, "/v1/operations", valid, token)
	if w.Code != 202 || !envelope.OK || service.called != 1 || service.actor.Subject != "test-ci" {
		t.Fatalf("submit failed: %+v", envelope)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/operations", strings.NewReader(valid))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, r)
	if response.Code != 400 || service.called != 1 {
		t.Fatal("submission without key accepted")
	}
}

func TestEventQueryAndPanicRecovery(t *testing.T) {
	h := handler(t, &fakeService{}, "operations:read")
	for _, query := range []string{"after=-1", "after=00", "after=x", "after=1&after=2", "limit=201", "limit=0", "limit=x", "unknown=1", "after=%xx"} {
		w, _ := request(t, h, http.MethodGet, "/v1/operations/"+string(id)+"/events?"+query, "", token)
		if w.Code != 400 {
			t.Fatalf("invalid query accepted: %s", query)
		}
	}
	h = handler(t, &fakeService{panicOnGet: true}, "operations:read")
	w, envelope := request(t, h, http.MethodGet, "/v1/operations/"+string(id), "", token)
	if w.Code != 500 || envelope.Error.Code != contract.Internal || strings.Contains(w.Body.String(), "untrusted") {
		t.Fatal("panic leaked or corrupted response")
	}
}

func TestAuthConfiguration(t *testing.T) {
	for _, principals := range [][]Principal{nil, {principal("invented")}, {principal("operations:read"), principal("operations:submit")}} {
		if _, err := NewAuth(principals); err == nil {
			t.Fatal("invalid auth configuration accepted")
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("server credential permissions require Unix file modes")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	raw, _ := json.Marshal([]Principal{principal("operations:read")})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuth(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuth(path); err == nil {
		t.Fatal("world-readable hashes accepted")
	}
}

func TestClientTransportAndProtocol(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "http://localhost:8080", "https://user:secret@example.com", "https://example.com/path", "https://example.com?token=x", "https://example.com?", "https://example.com/%2f"} {
		if _, err := NewClient(endpoint, token); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	h := handler(t, &fakeService{}, "operations:submit", "operations:read", "operations:cancel")
	server := httptest.NewServer(h)
	defer server.Close()
	client, err := NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Test(context.Background(), operations.TestInput{Steps: 1}, "key", false); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Events(context.Background(), contract.Cursor{OperationID: id}, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Get(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation identity: %v", err)
	}
	redirectCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectCalls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, _ = NewClient(redirect.URL, token)
	if _, err := client.Get(context.Background(), id); err == nil || redirectCalls != 0 {
		t.Fatal("followed a credential-bearing redirect")
	}
}

func TestSafeFailureAndReadiness(t *testing.T) {
	auth, _ := NewAuth([]Principal{principal("system:read")})
	h := NewHandler(auth, &fakeService{}, func(context.Context) error {
		return contract.NewFault(contract.Unavailable, "database unavailable", "")
	})
	w, envelope := request(t, h, http.MethodGet, "/readyz", "", token)
	if w.Code != 503 || envelope.OK {
		t.Fatal("unavailable database was ready")
	}
	if bytes.Contains(w.Body.Bytes(), []byte(token)) {
		t.Fatal("response leaked authorization token")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not a JSON response") }))
	defer bad.Close()
	client, _ := NewClient(bad.URL, token)
	if _, err := client.Get(context.Background(), id); contract.Classify(err).Code != contract.Unavailable {
		t.Fatal("untyped protocol failure")
	}
}
