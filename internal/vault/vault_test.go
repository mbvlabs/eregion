package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func testRing(t *testing.T) *KeyRing {
	t.Helper()
	ring, err := GenerateKeyRing()
	if err != nil {
		t.Fatalf("GenerateKeyRing: %v", err)
	}
	return ring
}

func testNamespace(t *testing.T) Namespace {
	t.Helper()
	namespace, err := NewNamespace("api", "production", "DATABASE_URL")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}
	return namespace
}

func TestSealOpenAndTamper(t *testing.T) {
	ring := testRing(t)
	namespace := testNamespace(t)
	plaintext := []byte("postgres://user:password@db/app?sslmode=verify-full")

	envelope, keyID, err := ring.Seal(namespace, 1, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if keyID != ring.CurrentKeyID() {
		t.Fatalf("key ID = %q, want %q", keyID, ring.CurrentKeyID())
	}
	got, gotKey, err := ring.Open(namespace, 1, envelope)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) || gotKey != keyID {
		t.Fatalf("round trip mismatch")
	}
	if bytes.Contains(envelope, plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}

	for i := range envelope {
		tampered := bytes.Clone(envelope)
		tampered[i] ^= 0x80
		if _, _, err := ring.Open(namespace, 1, tampered); err == nil {
			t.Fatalf("tampered byte %d was accepted", i)
		}
	}

	other, _ := NewNamespace("api", "production", "OTHER_SECRET")
	if _, _, err := ring.Open(other, 1, envelope); !IsCiphertextError(err) {
		t.Fatalf("wrong namespace error = %v, want authentication failure", err)
	}
	if _, _, err := ring.Open(namespace, 2, envelope); !IsCiphertextError(err) {
		t.Fatalf("wrong revision error = %v, want authentication failure", err)
	}

	unknown := testRing(t)
	if _, _, err := unknown.Open(namespace, 1, envelope); err == nil || strings.Contains(err.Error(), "password") {
		t.Fatalf("unknown key error = %v, want a safe failure", err)
	}
}

func TestSealNonceUniqueness(t *testing.T) {
	ring := testRing(t)
	namespace := testNamespace(t)
	seen := make(map[string]struct{}, 512)
	for i := 0; i < 512; i++ {
		envelope, _, err := ring.Seal(namespace, 1, []byte("same-value"))
		if err != nil {
			t.Fatalf("Seal %d: %v", i, err)
		}
		if _, duplicate := seen[string(envelope)]; duplicate {
			t.Fatalf("identical envelope at iteration %d", i)
		}
		seen[string(envelope)] = struct{}{}
	}
}

func TestKeyRotationKeepsOldCiphertextReadable(t *testing.T) {
	ring := testRing(t)
	namespace := testNamespace(t)
	oldID := ring.CurrentKeyID()
	oldEnvelope, _, err := ring.Seal(namespace, 1, []byte("first"))
	if err != nil {
		t.Fatalf("Seal old: %v", err)
	}

	newKey := &masterKey{id: "k_rotated", createdAt: time.Now().UTC()}
	for i := range newKey.key {
		newKey.key[i] = byte(i + 1)
	}
	ring.keys[newKey.id] = newKey
	ring.current = newKey.id

	newEnvelope, newID, err := ring.Seal(namespace, 2, []byte("second"))
	if err != nil {
		t.Fatalf("Seal new: %v", err)
	}
	if newID != newKey.id || newID == oldID {
		t.Fatalf("new sealing used %q, want %q", newID, newKey.id)
	}
	if got, keyID, err := ring.Open(namespace, 1, oldEnvelope); err != nil || string(got) != "first" || keyID != oldID {
		t.Fatalf("old ciphertext after rotation: %q %q %v", got, keyID, err)
	}
	if got, _, err := ring.Open(namespace, 2, newEnvelope); err != nil || string(got) != "second" {
		t.Fatalf("new ciphertext: %q %v", got, err)
	}
}

func TestKeyRingFileProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master-key.json")
	ring := testRing(t)
	if err := WriteKeyRing(path, ring); err != nil {
		t.Fatalf("WriteKeyRing: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := LoadKeyRing(path)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if loaded.CurrentKeyID() != ring.CurrentKeyID() {
		t.Fatalf("loaded current = %q, want %q", loaded.CurrentKeyID(), ring.CurrentKeyID())
	}
	if err := WriteKeyRing(path, ring); err == nil {
		t.Fatal("WriteKeyRing overwrote an existing key file")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyRing(path); err == nil {
		t.Fatal("LoadKeyRing accepted group/world-readable permissions")
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyRing(path); err != nil {
		t.Fatalf("LoadKeyRing rejected a stricter 0400 file: %v", err)
	}

	link := filepath.Join(dir, "key-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyRing(link); err == nil {
		t.Fatal("LoadKeyRing accepted a symlink")
	}
	if _, err := LoadKeyRing("relative-key.json"); err == nil {
		t.Fatal("LoadKeyRing accepted a relative path")
	}
}

func TestRevisionHashBinding(t *testing.T) {
	namespace := testNamespace(t)
	envelope := []byte("ciphertext")
	first := RevisionHash(namespace, 1, "k_one", envelope)
	if first != RevisionHash(namespace, 1, "k_one", envelope) {
		t.Fatal("RevisionHash is not deterministic")
	}
	if first == RevisionHash(namespace, 2, "k_one", envelope) {
		t.Fatal("RevisionHash ignores revision")
	}
	if first == RevisionHash(namespace, 1, "k_two", envelope) {
		t.Fatal("RevisionHash ignores key ID")
	}
	if first == RevisionHash(namespace, 1, "k_one", []byte("other")) {
		t.Fatal("RevisionHash ignores ciphertext")
	}
	other, _ := NewNamespace("api", "staging", "DATABASE_URL")
	if first == RevisionHash(other, 1, "k_one", envelope) {
		t.Fatal("RevisionHash ignores namespace")
	}
	if !isHexDigest(first) {
		t.Fatalf("RevisionHash = %q, want a lowercase SHA-256 hex digest", first)
	}
}

func TestNextRevisionCAS(t *testing.T) {
	tests := []struct {
		name     string
		exists   bool
		latest   uint64
		expected uint64
		want     uint64
		create   bool
		wantErr  bool
	}{
		{name: "create", exists: false, expected: 0, want: 1, create: true},
		{name: "create with guard", exists: false, expected: 1, wantErr: true},
		{name: "append", exists: true, latest: 3, expected: 3, want: 4},
		{name: "stale", exists: true, latest: 3, expected: 2, wantErr: true},
		{name: "zero cannot rotate", exists: true, latest: 3, expected: 0, wantErr: true},
		{name: "exhausted", exists: true, latest: ^uint64(0), expected: ^uint64(0), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			revision, create, err := nextRevision(test.exists, test.latest, test.expected)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected a conflict, got revision %d", revision)
				}
				if contract.Classify(err).Code != contract.Conflict {
					t.Fatalf("error code = %s, want conflict", contract.Classify(err).Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("nextRevision: %v", err)
			}
			if revision != test.want || create != test.create {
				t.Fatalf("got revision=%d create=%t, want revision=%d create=%t", revision, create, test.want, test.create)
			}
		})
	}
}

func TestResolutionRedactionAndJSON(t *testing.T) {
	namespace := testNamespace(t)
	plaintext := "super-secret-database-password"
	resolution := Resolution{
		Values: map[string]string{namespace.Name: plaintext},
		Revisions: map[string]RevisionRef{
			namespace.Name: {Revision: 2, Hash: strings.Repeat("a", 64), KeyID: "k_one", CreatedAt: time.Now().UTC()},
		},
		Digest: strings.Repeat("b", 64),
	}
	raw, err := json.Marshal(resolution)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatalf("resolution JSON leaked plaintext: %s", raw)
	}
	if strings.Contains(string(raw), `"Values"`) || strings.Contains(string(raw), `"values"`) {
		t.Fatalf("resolution JSON exposed the values field: %s", raw)
	}
	if strings.Contains(resolution.String(), plaintext) {
		t.Fatalf("resolution String leaked plaintext: %s", resolution.String())
	}
	registry := &secrets.Registry{}
	resolution.Register(registry)
	message := "connection failed for " + plaintext
	if strings.Contains(registry.Redact(message), plaintext) {
		t.Fatal("registered resolution value was not redacted")
	}
	env := resolution.Env()
	env[namespace.Name] = "changed"
	if resolution.Values[namespace.Name] != plaintext {
		t.Fatal("Env returned the live map instead of a copy")
	}
	if !strings.Contains(resolution.Digest, strings.Repeat("b", 64)) {
		t.Fatal("resolution digest was not preserved")
	}
}

func TestResolutionDigestStableAndBound(t *testing.T) {
	refA := RevisionRef{Revision: 1, Hash: strings.Repeat("a", 64), KeyID: "k_one"}
	refB := RevisionRef{Revision: 2, Hash: strings.Repeat("b", 64), KeyID: "k_two"}
	first := resolutionDigest("api", "production", map[string]RevisionRef{"A": refA, "B": refB})
	second := resolutionDigest("api", "production", map[string]RevisionRef{"B": refB, "A": refA})
	if first != second {
		t.Fatal("resolution digest depends on map iteration order")
	}
	if first == resolutionDigest("api", "staging", map[string]RevisionRef{"A": refA, "B": refB}) {
		t.Fatal("resolution digest ignores environment")
	}
	if first == resolutionDigest("api", "production", map[string]RevisionRef{"A": refA, "B": refA}) {
		t.Fatal("resolution digest ignores revision references")
	}
}

func TestNamespaceAndValueValidation(t *testing.T) {
	valid := []Namespace{
		{App: "a", Environment: "b", Name: "A"},
		{App: "my-app", Environment: "production-2", Name: "DATABASE_URL"},
	}
	for _, namespace := range valid {
		if err := namespace.Validate(); err != nil {
			t.Fatalf("%v rejected: %v", namespace, err)
		}
	}
	invalid := []Namespace{
		{App: "API", Environment: "production", Name: "DATABASE_URL"},
		{App: "api", Environment: "../prod", Name: "DATABASE_URL"},
		{App: "api", Environment: "production", Name: "database-url"},
		{App: "api", Environment: "production", Name: "1SECRET"},
		{App: strings.Repeat("a", 41), Environment: "production", Name: "A"},
		{App: "api", Environment: "production", Name: strings.Repeat("A", 129)},
	}
	for _, namespace := range invalid {
		if err := namespace.Validate(); err == nil {
			t.Fatalf("%v was accepted", namespace)
		}
	}
	if err := ValidateValue(nil); err == nil {
		t.Fatal("empty value was accepted")
	}
	if err := ValidateValue([]byte("a\x00b")); err == nil {
		t.Fatal("NUL value was accepted")
	}
	if err := ValidateValue(bytes.Repeat([]byte("a"), MaxValueBytes+1)); err == nil {
		t.Fatal("oversized value was accepted")
	}
	if err := ValidateValue([]byte("ok")); err != nil {
		t.Fatalf("valid value rejected: %v", err)
	}
}

func TestSealRejectsInvalidInputs(t *testing.T) {
	ring := testRing(t)
	namespace := testNamespace(t)
	if _, _, err := ring.Seal(namespace, 0, []byte("value")); err == nil {
		t.Fatal("Seal accepted revision 0")
	}
	bad, _ := NewNamespace("api", "production", "OTHER")
	bad.Name = "bad-name"
	if _, _, err := ring.Seal(bad, 1, []byte("value")); err == nil {
		t.Fatal("Seal accepted an invalid name")
	}
	if _, _, err := (*KeyRing)(nil).Seal(namespace, 1, []byte("value")); err == nil {
		t.Fatal("nil key ring sealed")
	}
	if _, _, err := ring.Open(namespace, 1, []byte("short")); !IsCiphertextError(err) {
		t.Fatalf("short envelope error = %v, want authentication failure", err)
	}
}

func TestMetadataValidation(t *testing.T) {
	now := time.Now().UTC()
	actor := contract.Actor{Kind: "operator", Subject: "alice"}
	valid := Metadata{
		App: "api", Environment: "production", Name: "DATABASE_URL",
		LatestRevision: 1, State: StateActive,
		Revision:  RevisionRef{Revision: 1, Hash: strings.Repeat("a", 64), KeyID: "k_one", CreatedAt: now},
		CreatedBy: actor, UpdatedBy: actor, CreatedAt: now, UpdatedAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	valid.Name = "bad-name"
	if err := valid.Validate(); err == nil {
		t.Fatal("invalid metadata accepted")
	}
}

func TestSchemaSQLIsExported(t *testing.T) {
	for _, fragment := range []string{"eregion_secrets", "eregion_secret_revisions", "ciphertext bytea", "revision_hash"} {
		if !strings.Contains(SchemaSQL, fragment) {
			t.Fatalf("SchemaSQL is missing %q", fragment)
		}
	}
}

func TestCiphertextErrorIsSafe(t *testing.T) {
	err := error(ErrCiphertext)
	if strings.Contains(err.Error(), "password") || !errors.Is(err, ErrCiphertext) {
		t.Fatalf("unexpected ciphertext error: %v", err)
	}
}

func TestKeyRingRotationPolicy(t *testing.T) {
	ring := testRing(t)
	key := ring.keys[ring.current]
	key.createdAt = time.Now().Add(-100 * 24 * time.Hour).UTC()
	if err := ring.CheckRotation(time.Now(), 90*24*time.Hour); err == nil {
		t.Fatal("old current key did not trigger rotation policy")
	}
	key.createdAt = time.Now().UTC()
	if err := ring.CheckRotation(time.Now(), 90*24*time.Hour); err != nil {
		t.Fatalf("fresh key triggered rotation: %v", err)
	}
}
