package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
)

type fakeVault struct {
	mu      sync.Mutex
	items   map[string]fakeVaultItem
	scopes  []VaultScope
	actor   contract.Actor
	archive int
	set     int
}

type fakeVaultItem struct {
	metadata vault.Metadata
	value    string
}

func newFakeVault() *fakeVault {
	return &fakeVault{items: make(map[string]fakeVaultItem)}
}

func fakeKey(namespace vault.Namespace) string { return namespace.Key() }

func (f *fakeVault) Set(_ context.Context, namespace vault.Namespace, value []byte, expected uint64, actor contract.Actor) (vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set++
	now := time.Now().UTC()
	item, exists := f.items[fakeKey(namespace)]
	latest := uint64(0)
	if exists {
		latest = item.metadata.LatestRevision
	}
	if !exists && expected != 0 {
		return vault.Metadata{}, contract.NewFault(contract.Conflict, "secret revision does not match", "Read metadata.")
	}
	if exists && (expected == 0 || expected != latest) {
		return vault.Metadata{}, contract.NewFault(contract.Conflict, "secret revision does not match", "Read metadata.")
	}
	revision := latest + 1
	meta := vault.Metadata{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		LatestRevision: revision, State: vault.StateActive,
		Revision:  vault.RevisionRef{Revision: revision, Hash: strings.Repeat("a", 64), KeyID: "k_test", CreatedAt: now},
		CreatedBy: actor, UpdatedBy: actor, CreatedAt: now, UpdatedAt: now,
	}
	if exists {
		meta.CreatedBy = item.metadata.CreatedBy
		meta.CreatedAt = item.metadata.CreatedAt
	}
	f.items[fakeKey(namespace)] = fakeVaultItem{metadata: meta, value: string(value)}
	return meta, nil
}

func (f *fakeVault) Archive(_ context.Context, namespace vault.Namespace, expected uint64, actor contract.Actor) (vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archive++
	item, ok := f.items[fakeKey(namespace)]
	if !ok {
		return vault.Metadata{}, contract.NewFault(contract.NotFound, "secret was not found", "List metadata.")
	}
	if expected != item.metadata.LatestRevision {
		return vault.Metadata{}, contract.NewFault(contract.Conflict, "secret revision does not match", "Read metadata.")
	}
	now := time.Now().UTC()
	item.metadata.State = vault.StateArchived
	item.metadata.ArchivedAt = &now
	item.metadata.UpdatedAt = now
	item.metadata.UpdatedBy = actor
	f.items[fakeKey(namespace)] = item
	return item.metadata, nil
}

func (f *fakeVault) Get(_ context.Context, namespace vault.Namespace) (vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[fakeKey(namespace)]
	if !ok {
		return vault.Metadata{}, contract.NewFault(contract.NotFound, "secret was not found", "List metadata.")
	}
	return item.metadata, nil
}

func (f *fakeVault) List(_ context.Context, app, environment string, limit int) ([]vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vault.Metadata, 0)
	for _, item := range f.items {
		if item.metadata.App == app && item.metadata.Environment == environment {
			out = append(out, item.metadata)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeVault) Export(_ context.Context, namespace vault.Namespace) (vault.Metadata, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[fakeKey(namespace)]
	if !ok {
		return vault.Metadata{}, nil, contract.NewFault(contract.NotFound, "secret was not found", "List metadata.")
	}
	if item.metadata.State != vault.StateActive {
		return vault.Metadata{}, nil, contract.NewFault(contract.PreconditionFailed, "secret is archived", "Rotate the secret.")
	}
	return item.metadata, []byte(item.value), nil
}

func testAuthorizer(_ *http.Request, _ VaultScope) (VaultAccess, error) {
	return NewVaultAccess(contract.Actor{Kind: "operator", Subject: "alice"}, func(string, string) error { return nil })
}

func loopbackRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	request.RemoteAddr = "127.0.0.1:4242"
	return request
}

func TestVaultHandlerRejectsPlaintextNonLoopback(t *testing.T) {
	service := newFakeVault()
	handler := NewVaultHandler(service, testAuthorizer)
	request := httptest.NewRequest(http.MethodGet, "/v1/secrets?app=api&environment=production", nil)
	request.RemoteAddr = "192.0.2.10:4242"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", response.Code)
	}
	if strings.Contains(response.Body.String(), "value") {
		t.Fatalf("transport failure leaked a value: %s", response.Body.String())
	}
}

func TestVaultHandlerSetAndListNeverEchoValue(t *testing.T) {
	service := newFakeVault()
	handler := NewVaultHandler(service, func(_ *http.Request, _ VaultScope) (VaultAccess, error) {
		return NewVaultAccess(contract.Actor{Kind: "operator", Subject: "alice"}, func(string, string) error { return nil })
	})
	secret := "postgres://user:password@db/app"
	body := `{"app":"api","environment":"production","name":"DATABASE_URL","value_base64":"` +
		base64.StdEncoding.EncodeToString([]byte(secret)) + `","expected_revision":0}`
	request := loopbackRequest(t, http.MethodPost, "/v1/secrets", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("set status = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("set response leaked plaintext: %s", response.Body.String())
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || !envelope.OK {
		t.Fatalf("invalid set envelope: %v %s", err, response.Body.String())
	}
	var metadata vault.Metadata
	if err := json.Unmarshal(envelope.Data, &metadata); err != nil || metadata.Validate() != nil || metadata.LatestRevision != 1 {
		t.Fatalf("invalid metadata: %v %+v", err, metadata)
	}

	list := loopbackRequest(t, http.MethodGet, "/v1/secrets?app=api&environment=production", nil)
	list.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, list)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("list response leaked plaintext: %s", response.Body.String())
	}
}

func TestVaultHandlerRejectsBodyIdentityAndUnknownFields(t *testing.T) {
	service := newFakeVault()
	handler := NewVaultHandler(service, testAuthorizer)
	body := `{"app":"api","environment":"production","name":"DATABASE_URL","value_base64":"dmFsdWU=","expected_revision":0,"actor":{"kind":"operator","subject":"mallory"}}`
	request := loopbackRequest(t, http.MethodPost, "/v1/secrets", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if service.set != 0 {
		t.Fatal("body identity was accepted as a mutation")
	}
}

func TestVaultHandlerArchiveAndExport(t *testing.T) {
	service := newFakeVault()
	actor := contract.Actor{Kind: "operator", Subject: "alice"}
	namespace, _ := vault.NewNamespace("api", "production", "DATABASE_URL")
	metadata, err := service.Set(context.Background(), namespace, []byte("secret-value"), 0, actor)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewVaultHandler(service, testAuthorizer)

	export := loopbackRequest(t, http.MethodGet, "/v1/secrets/api/production/DATABASE_URL/export", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, export)
	if response.Code != http.StatusOK {
		t.Fatalf("export status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("export cache control = %q", response.Header().Get("Cache-Control"))
	}
	var outer struct {
		Data struct {
			Secret      vault.Metadata `json:"secret"`
			ValueBase64 string         `json:"value_base64"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &outer); err != nil {
		t.Fatal(err)
	}
	value, err := base64.StdEncoding.DecodeString(outer.Data.ValueBase64)
	if err != nil || string(value) != "secret-value" || outer.Data.Secret.Revision.Hash == "" {
		t.Fatalf("exported value/metadata mismatch: %q %v", value, err)
	}

	archive := loopbackRequest(t, http.MethodPost, "/v1/secrets/archive",
		strings.NewReader(`{"app":"api","environment":"production","name":"DATABASE_URL","expected_revision":`+jsonNumber(metadata.LatestRevision)+`}`))
	archive.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, archive)
	if response.Code != http.StatusOK {
		t.Fatalf("archive status = %d: %s", response.Code, response.Body.String())
	}
	if service.archive != 1 {
		t.Fatal("archive service was not called")
	}
}

func jsonNumber(value uint64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestVaultHandlerAuthFailure(t *testing.T) {
	handler := NewVaultHandler(newFakeVault(), func(*http.Request, VaultScope) (VaultAccess, error) {
		return VaultAccess{}, contract.NewFault(contract.Unauthenticated, "API authentication required", "Supply a token.")
	})
	request := loopbackRequest(t, http.MethodGet, "/v1/secrets?app=api&environment=production", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestVaultClientHelpers(t *testing.T) {
	service := newFakeVault()
	handler := NewVaultHandler(service, testAuthorizer)
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := NewClient(server.URL, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	namespace, _ := vault.NewNamespace("api", "production", "DATABASE_URL")
	metadata, err := client.SetSecret(context.Background(), namespace, []byte("client-secret"), 0)
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if metadata.LatestRevision != 1 {
		t.Fatalf("revision = %d", metadata.LatestRevision)
	}
	if _, err := client.SetSecret(context.Background(), namespace, []byte("stale"), 0); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("stale CAS error = %v, want conflict", err)
	}
	if _, err := client.GetSecretMetadata(context.Background(), namespace); err != nil {
		t.Fatalf("GetSecretMetadata: %v", err)
	}
	list, err := client.ListSecrets(context.Background(), "api", "production", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSecrets: %v %d", err, len(list))
	}
	exportedMetadata, value, err := client.ExportSecret(context.Background(), namespace)
	if err != nil || string(value) != "client-secret" || exportedMetadata.LatestRevision != 1 {
		t.Fatalf("ExportSecret: %q %v", value, err)
	}
	if _, err := client.ArchiveSecret(context.Background(), namespace, metadata.LatestRevision); err != nil {
		t.Fatalf("ArchiveSecret: %v", err)
	}
	if _, _, err := client.ExportSecret(context.Background(), namespace); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("archived export error = %v, want precondition_failed", err)
	}
}

func TestVaultHandlerListRejectsUndocumentedQuery(t *testing.T) {
	handler := NewVaultHandler(newFakeVault(), testAuthorizer)
	request := loopbackRequest(t, http.MethodGet, "/v1/secrets?app=api&environment=production&value=1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func newGrantTestAuth(t *testing.T, kind string, grants []ActionGrant, scopes ...string) (*Auth, string) {
	t.Helper()
	token := strings.Repeat("g", 32)
	digest := sha256.Sum256([]byte(token))
	principal := Principal{
		Actor:        contract.Actor{Kind: kind, Subject: "grant-test"},
		TokenSHA256:  hex.EncodeToString(digest[:]),
		Scopes:       scopes,
		ActionGrants: grants,
	}
	auth, err := NewAuth([]Principal{principal})
	if err != nil {
		t.Fatal(err)
	}
	return auth, token
}

func grantTestRequest(method, path, body, token string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = "127.0.0.1:4242"
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func validSetBody(app, environment string) string {
	return `{"app":"` + app + `","environment":"` + environment + `","name":"DATABASE_URL","value_base64":"dmFsdWU=","expected_revision":0}`
}

func TestVaultActionGrantsNeverInheritFromOperationsScope(t *testing.T) {
	service := newFakeVault()
	auth, token := newGrantTestAuth(t, "ci", nil, "operations:read", "operations:submit")
	handler := NewVaultHandler(service, AuthVaultAuthorizer(auth))

	request := grantTestRequest(http.MethodPost, "/v1/secrets", validSetBody("api", "production"), token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("ungranted set status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if service.set != 0 {
		t.Fatal("ungranted mutation reached the service")
	}

	request = grantTestRequest(http.MethodGet, "/v1/secrets?app=api&environment=production", "", token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("ungranted list status = %d, want 403", response.Code)
	}
}

func TestVaultActionGrantsBindKindAndEnvironmentTarget(t *testing.T) {
	service := newFakeVault()
	auth, token := newGrantTestAuth(t, "ci",
		[]ActionGrant{{Kind: VaultKindSecretSet, Target: "env/api/*"}},
		"operations:read", "operations:submit")
	handler := NewVaultHandler(service, AuthVaultAuthorizer(auth))

	request := grantTestRequest(http.MethodPost, "/v1/secrets", validSetBody("api", "production"), token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("granted set status = %d: %s", response.Code, response.Body.String())
	}

	request = grantTestRequest(http.MethodPost, "/v1/secrets", validSetBody("other", "production"), token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-environment set status = %d, want 403", response.Code)
	}

	request = grantTestRequest(http.MethodGet, "/v1/secrets?app=api&environment=production", "", token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("set grant leaked into list: status = %d", response.Code)
	}
}

func TestVaultActionGrantsSeparateListArchiveExport(t *testing.T) {
	service := newFakeVault()
	actor := contract.Actor{Kind: "operator", Subject: "admin"}
	namespace, _ := vault.NewNamespace("api", "production", "DATABASE_URL")
	if _, err := service.Set(context.Background(), namespace, []byte("grant-value"), 0, actor); err != nil {
		t.Fatal(err)
	}
	service.set = 0

	auth, token := newGrantTestAuth(t, "ci", []ActionGrant{
		{Kind: VaultKindSecretList, Target: "env/api/production"},
		{Kind: VaultKindSecretArchive, Target: "env/api/production"},
		{Kind: VaultKindSecretExport, Target: "env/api/production"},
	}, "operations:read", "operations:submit")
	handler := NewVaultHandler(service, AuthVaultAuthorizer(auth))

	request := grantTestRequest(http.MethodGet, "/v1/secrets?app=api&environment=production", "", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("granted list status = %d: %s", response.Code, response.Body.String())
	}
	request = grantTestRequest(http.MethodGet, "/v1/secrets/api/production/DATABASE_URL/export", "", token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("granted export status = %d: %s", response.Code, response.Body.String())
	}
	request = grantTestRequest(http.MethodGet, "/v1/secrets?app=api&environment=staging", "", token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-environment list status = %d, want 403", response.Code)
	}

	request = grantTestRequest(http.MethodPost, "/v1/secrets", validSetBody("api", "production"), token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || service.set != 0 {
		t.Fatalf("missing set grant: status = %d", response.Code)
	}
}

func TestVaultAdministrativeOperatorStillNeedsScope(t *testing.T) {
	service := newFakeVault()
	auth, token := newGrantTestAuth(t, "operator", nil, "operations:submit")
	handler := NewVaultHandler(service, AuthVaultAuthorizer(auth))
	request := grantTestRequest(http.MethodPost, "/v1/secrets", validSetBody("api", "production"), token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("administrative operator set status = %d: %s", response.Code, response.Body.String())
	}
	request = grantTestRequest(http.MethodGet, "/v1/secrets?app=api&environment=production", "", token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("read without operations:read status = %d, want 403", response.Code)
	}
}
