package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
)

type providerSetupFixture struct {
	mu        sync.Mutex
	tokens    map[string]bool
	calls     []string
	mutations int
}

func newProviderSetupFixture() *providerSetupFixture {
	return &providerSetupFixture{tokens: map[string]bool{
		"cf-token-one-0123456789":   true,
		"cf-token-two-0123456789":   true,
		"cf-token-three-0123456789": true,
	}}
}

func (f *providerSetupFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Method != http.MethodGet {
		f.mutations++
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !f.tokens[token] {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "result": []any{},
		"result_info": map[string]any{"page": 1, "total_pages": 1},
	})
}

func TestIntegrationProviderMigrationPreservesApplicationSecrets(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := context.Background()
	for _, migration := range migrations {
		if migration.version > 26 {
			break
		}
		if _, err := pool.Exec(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
	}
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := vault.NewNamespace("api", "production", "DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	actor := contract.Actor{Kind: "operator", Subject: "migration-test"}
	if _, err := secrets.Set(ctx, namespace, []byte("postgres://before-migration"), 0, actor); err != nil {
		t.Fatal(err)
	}
	opID := "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
		VALUES($1,'platform.app.create','platform',$2,$3,'provider-migration',$4,'{}','succeeded')`, opID, actor.Kind, actor.Subject, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, migration027); err != nil {
		t.Fatalf("migration 027: %v", err)
	}
	metadata, err := secrets.Get(ctx, namespace)
	if err != nil || metadata.LatestRevision != 1 {
		t.Fatalf("application secret after migration = %+v, %v", metadata, err)
	}
	_, value, err := secrets.Export(ctx, namespace)
	if err != nil || string(value) != "postgres://before-migration" {
		t.Fatalf("application secret value after migration = %q, %v", value, err)
	}
	var operationKind, operationTarget string
	if err := pool.QueryRow(ctx, `SELECT kind,target FROM eregion_operations WHERE id=$1`, opID).Scan(&operationKind, &operationTarget); err != nil {
		t.Fatalf("operation history after migration: %v", err)
	}
	if operationKind != "platform.app.create" || operationTarget != "platform" {
		t.Fatalf("operation history changed after migration: kind=%q target=%q", operationKind, operationTarget)
	}
	for _, table := range []string{"eregion_provider_credentials", "eregion_provider_credential_revisions"} {
		if _, err := pool.Exec(ctx, "SELECT 1 FROM "+table+" LIMIT 1"); err != nil {
			t.Fatalf("migration did not create %s: %v", table, err)
		}
	}
}

func TestIntegrationProviderCredentialValidationAtomicityAndRotation(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, server.URL+"/client/v4", server.Client())
	actor := contract.Actor{Kind: "operator", Subject: "first-operator"}
	const zoneID = "0123456789abcdef0123456789abcdef"
	preview, err := setup.PreviewCloudflareCredential(ctx, actor, zoneID, "example.com", []byte("cf-token-one-0123456789"))
	if err != nil || preview.CurrentRevision != 0 || preview.NextRevision != 1 || !preview.ReadValidated {
		t.Fatalf("initial preview = %+v, %v", preview, err)
	}
	first, err := setup.ConfigureCloudflareCredential(ctx, actor, zoneID, "example.com", preview.CurrentRevision, []byte("cf-token-one-0123456789"))
	if err != nil || first.LatestRevision != 1 || first.CreatedBy.Subject != actor.Subject {
		t.Fatalf("initial configure = %+v, %v", first, err)
	}
	var ciphertext []byte
	if err := pool.QueryRow(ctx, `SELECT ciphertext FROM eregion_provider_credential_revisions WHERE provider='cloudflare' AND zone_id=$1 AND revision=1`, zoneID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "cf-token-one") {
		t.Fatal("provider token was stored in plaintext")
	}
	scope, _ := vault.NewProviderScope(vault.ProviderCloudflare, zoneID)
	metadata, token, err := secrets.ResolveProviderCredential(ctx, scope)
	if err != nil || string(token) != "cf-token-one-0123456789" || metadata.LatestRevision != 1 {
		t.Fatalf("resolved provider credential = %+v, %q, %v", metadata, token, err)
	}
	clearProviderToken(token)

	// A stale replacement is a conflict and leaves revision 1 active.
	if _, err := setup.ConfigureCloudflareCredential(ctx, contract.Actor{Kind: "operator", Subject: "second-operator"}, zoneID, "example.com", 0, []byte("cf-token-two-0123456789")); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("stale replacement = %v", err)
	}
	if _, token, err := secrets.ResolveProviderCredential(ctx, scope); err != nil || string(token) != "cf-token-one-0123456789" {
		t.Fatalf("stale replacement changed the credential: %q, %v", token, err)
	} else {
		clearProviderToken(token)
	}

	// A failed token validation leaves the old credential active too.
	if _, err := setup.ConfigureCloudflareCredential(ctx, actor, zoneID, "example.com", 1, []byte("cf-token-bad-0123456789")); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("invalid token replacement = %v", err)
	}
	if _, token, err := secrets.ResolveProviderCredential(ctx, scope); err != nil || string(token) != "cf-token-one-0123456789" {
		t.Fatalf("failed validation changed the credential: %q, %v", token, err)
	} else {
		clearProviderToken(token)
	}

	// A failed audit insert inside the commit transaction must roll back the
	// encrypted revision too.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION eregion_test_fail_provider_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER eregion_test_fail_provider_audit BEFORE INSERT ON eregion_integration_audit
		FOR EACH ROW EXECUTE FUNCTION eregion_test_fail_provider_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.ConfigureCloudflareCredential(ctx, actor, zoneID, "example.com", 1, []byte("cf-token-two-0123456789")); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("audit failure replacement = %v", err)
	}
	if _, token, err := secrets.ResolveProviderCredential(ctx, scope); err != nil || string(token) != "cf-token-one-0123456789" {
		t.Fatalf("audit failure changed the credential: %q, %v", token, err)
	} else {
		clearProviderToken(token)
	}
	var afterAuditFailure int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credential_revisions WHERE provider='cloudflare' AND zone_id=$1`, zoneID).Scan(&afterAuditFailure); err != nil || afterAuditFailure != 1 {
		t.Fatalf("audit failure left revisions = %d, %v", afterAuditFailure, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER eregion_test_fail_provider_audit ON eregion_integration_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION eregion_test_fail_provider_audit()`); err != nil {
		t.Fatal(err)
	}

	// A second operator replaces the credential with the observed revision.
	second, err := setup.ConfigureCloudflareCredential(ctx, contract.Actor{Kind: "operator", Subject: "second-operator"}, zoneID, "example.com", 1, []byte("cf-token-two-0123456789"))
	if err != nil || second.LatestRevision != 2 || second.UpdatedBy.Subject != "second-operator" || second.CreatedBy.Subject != "first-operator" {
		t.Fatalf("replacement = %+v, %v", second, err)
	}
	// A freshly constructed service resolves the new revision without a
	// restart or a cached token.
	fresh, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	if _, token, err := fresh.ResolveProviderCredential(ctx, scope); err != nil || string(token) != "cf-token-two-0123456789" {
		t.Fatalf("fresh resolver = %q, %v", token, err)
	} else {
		clearProviderToken(token)
	}
	var revisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credential_revisions WHERE provider='cloudflare' AND zone_id=$1`, zoneID).Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("provider revisions = %d, %v", revisions, err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE entry->>'action'='cloudflare.configure' AND entry->>'outcome'='succeeded'`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("provider audit entries = %d, %v", audits, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.mutations != 0 {
		t.Fatalf("setup performed a provider mutation: %v", fixture.calls)
	}
}

func TestIntegrationProviderCredentialConcurrentCASActivatesExactlyOneRevision(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, server.URL+"/client/v4", server.Client())
	const zoneID = "0123456789abcdef0123456789abcdef"
	if _, err := setup.ConfigureCloudflareCredential(ctx, contract.Actor{Kind: "operator", Subject: "first-operator"}, zoneID, "example.com", 0, []byte("cf-token-one-0123456789")); err != nil {
		t.Fatal(err)
	}
	type result struct {
		metadata vault.ProviderMetadata
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for index, token := range []string{"cf-token-two-0123456789", "cf-token-three-0123456789"} {
		subject := "second-operator"
		if index == 1 {
			subject = "third-operator"
		}
		go func(subject, token string) {
			<-start
			metadata, err := setup.ConfigureCloudflareCredential(ctx, contract.Actor{Kind: "operator", Subject: subject}, zoneID, "example.com", 1, []byte(token))
			results <- result{metadata: metadata, err: err}
		}(subject, token)
	}
	close(start)
	successes, conflicts := 0, 0
	var winnerToken string
	for range 2 {
		out := <-results
		if out.err == nil {
			successes++
			if out.metadata.LatestRevision != 2 || out.metadata.Revision.Revision != 2 {
				t.Fatalf("winning metadata = %+v", out.metadata)
			}
			continue
		}
		if contract.Classify(out.err).Code != contract.Conflict {
			t.Fatalf("losing writer error = %v", out.err)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent CAS = %d success, %d conflict; want exactly one of each", successes, conflicts)
	}
	scope, _ := vault.NewProviderScope(vault.ProviderCloudflare, zoneID)
	metadata, token, err := secrets.ResolveProviderCredential(ctx, scope)
	if err != nil || metadata.LatestRevision != 2 {
		t.Fatalf("active credential after concurrent CAS = %+v, %v", metadata, err)
	}
	winnerToken = string(token)
	clearProviderToken(token)
	if winnerToken != "cf-token-two-0123456789" && winnerToken != "cf-token-three-0123456789" {
		t.Fatalf("active credential is not one of the concurrent winners: %q", winnerToken)
	}
	var revisions, audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credential_revisions WHERE provider='cloudflare' AND zone_id=$1`, zoneID).Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("revisions after concurrent CAS = %d, %v", revisions, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit WHERE entry->>'action'='cloudflare.configure' AND entry->>'outcome'='succeeded'`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit entries after concurrent CAS = %d, %v", audits, err)
	}
}

func TestIntegrationProviderScopeIsNotAnApplicationSecret(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, server.URL+"/client/v4", server.Client())
	const zoneID = "0123456789abcdef0123456789abcdef"
	if _, err := setup.ConfigureCloudflareCredential(ctx, contract.Actor{Kind: "operator", Subject: "ops"}, zoneID, "example.com", 0, []byte("cf-token-one-0123456789")); err != nil {
		t.Fatal(err)
	}
	listed, err := secrets.List(ctx, "cloudflare", "cloudflare", 100)
	if err != nil || len(listed) != 0 {
		t.Fatalf("generic secret list exposed provider state: %+v, %v", listed, err)
	}
	namespace, err := vault.NewNamespace("cloudflare", "cloudflare", "CLOUDFLARE_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Get(ctx, namespace); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("generic secret metadata addressed provider scope: %v", err)
	}
	if _, _, err := secrets.Export(ctx, namespace); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("generic secret export addressed provider scope: %v", err)
	}
}

func TestIntegrationProviderReadValidatedStateIsDescribedHonestly(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, server.URL+"/client/v4", server.Client())
	const zoneID = "0123456789abcdef0123456789abcdef"
	preview, err := setup.PreviewCloudflareCredential(ctx, contract.Actor{Kind: "operator", Subject: "ops"}, zoneID, "example.com", []byte("cf-token-one-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if !preview.ReadValidated || !strings.Contains(preview.Validation, "does not prove DNS write capability") {
		t.Fatalf("preview overclaims token capability: %+v", preview)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.calls) != 1 || fixture.calls[0] != "GET /zones/"+zoneID+"/dns_records" && fixture.calls[0] != "GET /client/v4/zones/"+zoneID+"/dns_records" {
		t.Fatalf("preview provider calls = %v", fixture.calls)
	}
}

func clearProviderToken(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
