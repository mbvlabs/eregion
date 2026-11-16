package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
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

func TestIntegrationProviderCredentialValidationAtomicityAndRotation(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	store := newTestEntities(t, pool)
	secrets, err := vault.NewService(store.Secrets(), store.Secrets(), ring, store.Providers())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, store, server.URL+"/client/v4", server.Client())
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
	fresh, err := newTestVaultService(t, pool, ring)
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
	store := newTestEntities(t, pool)
	secrets, err := vault.NewService(store.Secrets(), store.Secrets(), ring, store.Providers())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, store, server.URL+"/client/v4", server.Client())
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
	store := newTestEntities(t, pool)
	secrets, err := vault.NewService(store.Secrets(), store.Secrets(), ring, store.Providers())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, store, server.URL+"/client/v4", server.Client())
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
	store := newTestEntities(t, pool)
	secrets, err := vault.NewService(store.Secrets(), store.Secrets(), ring, store.Providers())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newProviderSetupFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	setup := NewCloudflareSetup(secrets, store, server.URL+"/client/v4", server.Client())
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

// The provider persistence port preserves create-at-zero, locked CAS, immutable
// revisions, zone-name immutability and duplicate zone-name uniqueness through
// the real vault and entities transaction. Concurrent duplicate zone IDs are
// covered separately. No provider HTTP path is involved.
func TestIntegrationProviderCredentialEntityIdentityFaults(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	store := newTestEntities(t, pool)
	secrets, err := vault.NewService(store.Secrets(), store.Secrets(), ring, store.Providers())
	if err != nil {
		t.Fatal(err)
	}
	actor := contract.Actor{Kind: "operator", Subject: "entity-provider"}
	set := func(zoneID, zoneName string, expected uint64) (vault.ProviderMetadata, error) {
		t.Helper()
		var metadata vault.ProviderMetadata
		err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			var err error
			metadata, err = secrets.Store().SetProviderCredential(ctx, tx.Providers(),
				vault.ProviderScope{Provider: vault.ProviderCloudflare, ZoneID: zoneID},
				zoneName, []byte("provider-token-0123456789"), expected, actor)
			return err
		})
		return metadata, err
	}
	const (
		zoneA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		zoneB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		zoneC = "cccccccccccccccccccccccccccccccc"
		zoneD = "dddddddddddddddddddddddddddddddd"
	)
	first, err := set(zoneA, "example.com", 0)
	if err != nil || first.LatestRevision != 1 {
		t.Fatalf("create zone A = %+v, %v", first, err)
	}
	if _, err := set(zoneA, "changed.example.com", 1); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("zone name change = %v", err)
	}
	if _, err := set(zoneC, "example.com", 0); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("duplicate zone name = %v", err)
	}
	if _, err := set(zoneD, "d.example.com", 5); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("create with nonzero revision = %v", err)
	}
	second, err := set(zoneB, "other.example.com", 0)
	if err != nil || second.LatestRevision != 1 {
		t.Fatalf("create zone B = %+v, %v", second, err)
	}
	rotated, err := set(zoneA, "example.com", 1)
	if err != nil || rotated.LatestRevision != 2 || rotated.Revision.Revision != 2 {
		t.Fatalf("rotate zone A = %+v, %v", rotated, err)
	}
	if _, err := set(zoneA, "example.com", 1); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("stale rotate = %v", err)
	}
	listed, err := secrets.ListProviderCredentials(ctx, vault.ProviderCloudflare, 10)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list provider credentials = %+v, %v", listed, err)
	}
	if listed[0].ZoneName != "example.com" || listed[0].LatestRevision != 2 ||
		listed[1].ZoneName != "other.example.com" || listed[1].LatestRevision != 1 {
		t.Fatalf("listed provider credentials = %+v", listed)
	}
	var revisions, audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credential_revisions WHERE provider='cloudflare'`).Scan(&revisions); err != nil || revisions != 3 {
		t.Fatalf("provider revisions = %d, %v", revisions, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("provider-only port wrote %d audit rows, %v", audits, err)
	}
}

// setTestProviderCredential uses the real vault and entities transaction with a
// locally sealed opaque value. No provider HTTP endpoint is involved.
func setTestProviderCredential(t *testing.T, ctx context.Context, store *entities.Store, secrets *vault.Service, actor contract.Actor, zoneID, zoneName string, expected uint64) (vault.ProviderMetadata, error) {
	t.Helper()
	var metadata vault.ProviderMetadata
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		metadata, err = secrets.Store().SetProviderCredential(ctx, tx.Providers(),
			vault.ProviderScope{Provider: vault.ProviderCloudflare, ZoneID: zoneID},
			zoneName, []byte("opaque-provider-value-0123456789"), expected, actor)
		return err
	})
	return metadata, err
}

// The provider revision guards are exercised against real PostgreSQL rows:
// positive bigint exhaustion, explicit uint64-to-bigint rejection, and a
// persisted negative revision that must fail closed as corrupt state.
func TestIntegrationProviderRevisionBoundsAndCorruptState(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	store := newTestEntities(t, pool)
	secrets, err := vault.NewService(store.Secrets(), store.Secrets(), ring, store.Providers())
	if err != nil {
		t.Fatal(err)
	}
	actor := contract.Actor{Kind: "operator", Subject: "provider-bounds"}
	const zoneA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const zoneB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scopeA := vault.ProviderScope{Provider: vault.ProviderCloudflare, ZoneID: zoneA}

	if _, err := setTestProviderCredential(t, ctx, store, secrets, actor, zoneA, "bounds.example.com", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_provider_credentials
		SET latest_revision=$2 WHERE provider='cloudflare' AND zone_id=$1`, zoneA, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if _, err := setTestProviderCredential(t, ctx, store, secrets, actor, zoneA, "bounds.example.com", math.MaxInt64); contract.Classify(err).Code != contract.Conflict ||
		!strings.Contains(contract.Classify(err).Message, "revision space is exhausted") {
		t.Fatalf("MaxInt64 guard = %v", err)
	}
	var latest int64
	if err := pool.QueryRow(ctx, `SELECT latest_revision FROM eregion_provider_credentials
		WHERE provider='cloudflare' AND zone_id=$1`, zoneA).Scan(&latest); err != nil || latest != int64(math.MaxInt64) {
		t.Fatalf("MaxInt64 guard changed latest_revision = %d, %v", latest, err)
	}
	var revisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credential_revisions
		WHERE provider='cloudflare' AND zone_id=$1`, zoneA).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("MaxInt64 guard wrote revisions = %d, %v", revisions, err)
	}

	err = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Providers().UpdateProviderCredentialRevision(ctx, scopeA, math.MaxUint64, math.MaxUint64, actor)
		return err
	})
	if contract.Classify(err).Code != contract.Conflict || !strings.Contains(contract.Classify(err).Message, "database range") {
		t.Fatalf("uint64 bigint guard = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT latest_revision FROM eregion_provider_credentials
		WHERE provider='cloudflare' AND zone_id=$1`, zoneA).Scan(&latest); err != nil || latest != int64(math.MaxInt64) {
		t.Fatalf("uint64 bigint guard changed latest_revision = %d, %v", latest, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO eregion_provider_credentials
		(provider,zone_id,zone_name,latest_revision,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
		VALUES('cloudflare',$1,'negative.example.com',-1,'operator','test','operator','test',clock_timestamp(),clock_timestamp())`, zoneB); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_provider_credential_revisions
		(provider,zone_id,revision,key_id,ciphertext,ciphertext_sha256,revision_hash,created_by_kind,created_by_subject,created_at)
		VALUES('cloudflare',$1,-1,'test',''::bytea,'x','x','operator','test',clock_timestamp())`, zoneB); err != nil {
		t.Fatal(err)
	}
	scopeB := vault.ProviderScope{Provider: vault.ProviderCloudflare, ZoneID: zoneB}
	for name, call := range map[string]func() error{
		"get": func() error {
			_, err := secrets.GetProviderCredential(ctx, scopeB)
			return err
		},
		"list": func() error {
			_, err := secrets.ListProviderCredentials(ctx, vault.ProviderCloudflare, 10)
			return err
		},
		"resolve": func() error {
			_, _, err := secrets.ResolveProviderCredential(ctx, scopeB)
			return err
		},
		"set": func() error {
			_, err := setTestProviderCredential(t, ctx, store, secrets, actor, zoneB, "negative.example.com", 0)
			return err
		},
	} {
		fault := contract.Classify(call())
		if fault.Code != contract.Unavailable || !strings.Contains(fault.Message, "outside the supported range") {
			t.Fatalf("%s negative revision = %v", name, fault)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credential_revisions
		WHERE provider='cloudflare' AND zone_id=$1`, zoneB).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("negative revision wrote revisions = %d, %v", revisions, err)
	}
}

// Concurrent creates for one zone ID with distinct names must preserve conflict
// classification. Depending on timing the loser sees either the unique-violation
// conflict or the locked zone-name immutability conflict; no loser may become a
// revision conflict or an unclassified error.
func TestIntegrationProviderConcurrentDuplicateZoneIDConflict(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	store := newTestEntities(t, pool)
	secrets, err := vault.NewService(store.Secrets(), store.Secrets(), ring, store.Providers())
	if err != nil {
		t.Fatal(err)
	}
	actor := contract.Actor{Kind: "operator", Subject: "provider-concurrent"}
	const zoneID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const attempts = 8
	type result struct {
		metadata vault.ProviderMetadata
		name     string
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, attempts)
	for index := range attempts {
		name := fmt.Sprintf("duplicate-%d.example.com", index)
		go func(name string) {
			<-start
			metadata, err := setTestProviderCredential(t, ctx, store, secrets, actor, zoneID, name, 0)
			results <- result{metadata: metadata, name: name, err: err}
		}(name)
	}
	close(start)
	successes, conflicts := 0, 0
	var winner string
	for range attempts {
		out := <-results
		if out.err == nil {
			successes++
			if out.metadata.LatestRevision != 1 || out.metadata.ZoneName != out.name {
				t.Fatalf("winning metadata = %+v", out.metadata)
			}
			winner = out.name
			continue
		}
		fault := contract.Classify(out.err)
		if fault.Code != contract.Conflict {
			t.Fatalf("losing writer = %v", out.err)
		}
		switch fault.Message {
		case "provider zone is already configured", "provider zone name cannot change for an existing zone ID":
			conflicts++
		default:
			t.Fatalf("losing writer conflict was not a zone identity conflict: %v", out.err)
		}
	}
	if successes != 1 || conflicts != attempts-1 || winner == "" {
		t.Fatalf("concurrent duplicate zone ID = %d success, %d conflict", successes, conflicts)
	}
	var rows, revisions int
	var storedName string
	var latest int64
	if err := pool.QueryRow(ctx, `SELECT count(*),max(zone_name),max(latest_revision)
		FROM eregion_provider_credentials WHERE provider='cloudflare' AND zone_id=$1`, zoneID).
		Scan(&rows, &storedName, &latest); err != nil || rows != 1 || storedName != winner || latest != 1 {
		t.Fatalf("stored credential = rows:%d name:%q latest:%d err:%v", rows, storedName, latest, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_provider_credential_revisions
		WHERE provider='cloudflare' AND zone_id=$1`, zoneID).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("stored revisions = %d, %v", revisions, err)
	}
}
