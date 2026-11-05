package vault

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

type fakeScanner func(dest ...any) error

func (f fakeScanner) Scan(dest ...any) error { return f(dest...) }

// TestExportMetadataKeepsTheRevisionKeyID is the regression for a fresh-node
// secret export. scanEncrypted reads the key ID into encryptedRow.KeyID, and
// decryptRow replaces Metadata.Revision with encryptedRow.Ref. If the scanned
// Ref has no key ID, Export returns metadata that Metadata.Validate rejects and
// the CLI reports "the node returned invalid secret export" even though the
// factor was verified and the value was decrypted.
func TestExportMetadataKeepsTheRevisionKeyID(t *testing.T) {
	ring := testRing(t)
	namespace := testNamespace(t)
	plaintext := []byte("fake-export-value")
	ciphertext, keyID, err := ring.Seal(namespace, 1, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	now := time.Now().UTC()
	row, err := scanEncrypted(fakeScanner(func(dest ...any) error {
		*dest[0].(*string) = namespace.Name
		*dest[1].(*State) = StateActive
		*dest[2].(*uint64) = 1
		*dest[3].(*string) = keyID
		*dest[4].(*[]byte) = ciphertext
		*dest[5].(*string) = CiphertextSHA256(ciphertext)
		*dest[6].(*string) = RevisionHash(namespace, 1, keyID, ciphertext)
		*dest[7].(*time.Time) = now
		return nil
	}))
	if err != nil {
		t.Fatalf("scanEncrypted: %v", err)
	}
	if row.Ref.KeyID != keyID {
		t.Fatalf("scanned revision key ID = %q, want %q", row.Ref.KeyID, keyID)
	}
	row.Metadata = Metadata{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		LatestRevision: 1, State: StateActive,
		Revision:  RevisionRef{Revision: 1, Hash: RevisionHash(namespace, 1, keyID, ciphertext), KeyID: keyID, CreatedAt: now},
		CreatedBy: contract.Actor{Kind: "operator", Subject: "admin"},
		UpdatedBy: contract.Actor{Kind: "operator", Subject: "admin"},
		CreatedAt: now, UpdatedAt: now,
	}
	got, err := decryptRow(ring, namespace, &row)
	if err != nil {
		t.Fatalf("decryptRow: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("decrypted value = %q, want %q", got, plaintext)
	}
	if row.Metadata.Revision.KeyID != keyID {
		t.Fatalf("exported revision key ID = %q, want %q", row.Metadata.Revision.KeyID, keyID)
	}
	if err := row.Metadata.Validate(); err != nil {
		t.Fatalf("exported metadata did not validate: %v", err)
	}
	// The node sends this metadata as JSON and the CLI validates the decoded
	// copy, so the boundary must preserve every field Validate depends on.
	raw, err := json.Marshal(row.Metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var decoded Metadata
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("JSON round trip did not validate: %v", err)
	}
}
