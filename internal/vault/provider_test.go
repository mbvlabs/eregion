package vault

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/dbschema"
)

func testProviderScope(t *testing.T) ProviderScope {
	t.Helper()
	scope, err := NewProviderScope(ProviderCloudflare, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewProviderScope: %v", err)
	}
	return scope
}

func TestProviderScopeIsTypedAndValidated(t *testing.T) {
	scope := testProviderScope(t)
	if scope.Key() != "cloudflare/0123456789abcdef0123456789abcdef" {
		t.Fatalf("scope key = %q", scope.Key())
	}
	for _, test := range []struct{ provider, zone string }{
		{"", scope.ZoneID},
		{"cloudflare", ""},
		{"cloudflare", "not-a-zone"},
		{"other", scope.ZoneID},
	} {
		if _, err := NewProviderScope(test.provider, test.zone); contract.Classify(err).Code != contract.ValidationFailed {
			t.Fatalf("scope %q/%q = %v", test.provider, test.zone, err)
		}
	}
	// A provider scope cannot be represented as an application secret
	// namespace, so generic app secret APIs cannot address it.
	if _, err := NewNamespace(scope.Provider, scope.Provider, scope.ZoneID); err == nil {
		t.Fatal("application secret namespace accepted a provider scope")
	}
}

func TestValidateProviderZoneName(t *testing.T) {
	for _, name := range []string{"example.com", "sub.example.com"} {
		if err := ValidateProviderZoneName(name); err != nil {
			t.Fatalf("valid zone %q = %v", name, err)
		}
	}
	for _, name := range []string{"", "example", "Example.com", "example..com", " example.com", "example.com.", "example.com/evil"} {
		if err := ValidateProviderZoneName(name); contract.Classify(err).Code != contract.ValidationFailed {
			t.Fatalf("zone %q = %v", name, err)
		}
	}
}

func TestProviderCiphertextBindsScopeRevisionAndSeparateSecretDomain(t *testing.T) {
	ring := testRing(t)
	scope := testProviderScope(t)
	plaintext := []byte("cloudflare-token-value-0123456789")
	ciphertext, keyID, err := ring.SealProvider(scope, 1, plaintext)
	if err != nil {
		t.Fatalf("SealProvider: %v", err)
	}
	got, gotKey, err := ring.OpenProvider(scope, 1, ciphertext)
	if err != nil || gotKey != keyID || string(got) != string(plaintext) {
		t.Fatalf("OpenProvider = %q/%q, %v", got, gotKey, err)
	}
	otherScope := ProviderScope{Provider: ProviderCloudflare, ZoneID: "fedcba9876543210fedcba9876543210"}
	if _, _, err := ring.OpenProvider(otherScope, 1, ciphertext); !IsCiphertextError(err) {
		t.Fatalf("other scope opened provider ciphertext: %v", err)
	}
	if _, _, err := ring.OpenProvider(scope, 2, ciphertext); !IsCiphertextError(err) {
		t.Fatalf("other revision opened provider ciphertext: %v", err)
	}
	namespace := testNamespace(t)
	if _, _, err := ring.Open(namespace, 1, ciphertext); !IsCiphertextError(err) {
		t.Fatalf("application secret path opened provider ciphertext: %v", err)
	}
	appCiphertext, _, err := ring.Seal(namespace, 1, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ring.OpenProvider(scope, 1, appCiphertext); !IsCiphertextError(err) {
		t.Fatalf("provider path opened application ciphertext: %v", err)
	}
	if ProviderRevisionHash(scope, 1, keyID, ciphertext) == ProviderRevisionHash(otherScope, 1, keyID, ciphertext) {
		t.Fatal("provider revision hash ignores the zone scope")
	}
}

func TestProviderMetadataIsSecretFreeAndValidates(t *testing.T) {
	now := time.Now().UTC()
	scope := testProviderScope(t)
	metadata := ProviderMetadata{
		Provider: scope.Provider, ZoneID: scope.ZoneID, ZoneName: "example.com",
		LatestRevision: 2,
		Revision: RevisionRef{
			Revision: 2, Hash: strings.Repeat("a", 64), KeyID: "k_012345", CreatedAt: now,
		},
		CreatedBy: contract.Actor{Kind: "operator", Subject: "first"},
		UpdatedBy: contract.Actor{Kind: "operator", Subject: "second"},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := metadata.Validate(); err != nil {
		t.Fatalf("metadata.Validate: %v", err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ciphertext") || strings.Contains(string(raw), "token") {
		t.Fatalf("provider metadata looks secret-bearing: %s", raw)
	}
	metadata.LatestRevision = 0
	if err := metadata.Validate(); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("invalid metadata = %v", err)
	}
}

func TestProviderSchemaIsSeparateFromApplicationSecrets(t *testing.T) {
	data, err := dbschema.GooseFS.ReadFile("migrations/00001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	baseline := string(data)
	for _, table := range []string{"eregion_provider_credentials", "eregion_provider_credential_revisions"} {
		if !strings.Contains(baseline, "CREATE TABLE "+table+" (") {
			t.Fatalf("provider schema is missing %s", table)
		}
	}
	start := strings.Index(baseline, "CREATE TABLE eregion_provider_credentials (")
	end := strings.Index(baseline[start:], "CREATE TABLE eregion_access_grants (")
	if end < 0 {
		t.Fatal("provider schema boundary missing")
	}
	providerSchema := baseline[start : start+end]
	if strings.Contains(providerSchema, "eregion_secrets") || strings.Contains(providerSchema, "environment text") {
		t.Fatal("provider schema reuses the application secret namespace")
	}
	if !strings.Contains(providerSchema, "PRIMARY KEY (provider, zone_id)") {
		t.Fatal("provider metadata is not keyed by provider and zone ID")
	}
}

func TestProviderHashUsesRevisionAndKey(t *testing.T) {
	scope := testProviderScope(t)
	ciphertext := []byte("ciphertext")
	first := ProviderRevisionHash(scope, 1, "k_one", ciphertext)
	if first == ProviderRevisionHash(scope, 2, "k_one", ciphertext) {
		t.Fatal("revision hash ignores the revision")
	}
	if first == ProviderRevisionHash(scope, 1, "k_two", ciphertext) {
		t.Fatal("revision hash ignores the key ID")
	}
}

func TestProviderRevisionConflictCarriesObservedRevision(t *testing.T) {
	scope := testProviderScope(t)
	_, _, err := providerNextRevision(scope, true, 3, 2)
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("revision conflict = %v", err)
	}
	fault := contract.Classify(err)
	if fault.Details["expected_revision"] != uint64(2) || fault.Details["current_revision"] != uint64(3) {
		t.Fatalf("conflict details = %#v", fault.Details)
	}
}
