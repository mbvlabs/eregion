package vault

import (
	"bytes"
	"testing"
)

func TestOpenRevisionAuthenticatesJoinedRow(t *testing.T) {
	ring := testRing(t)
	namespace := testNamespace(t)
	plaintext := []byte("0123456789abcdef")
	envelope, keyID, err := ring.Seal(namespace, 3, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	row := EncryptedRevision{
		Namespace: namespace, State: StateActive, Revision: 3, KeyID: keyID,
		Ciphertext: envelope, CiphertextSHA256: CiphertextSHA256(envelope),
		RevisionHash: RevisionHash(namespace, 3, keyID, envelope),
	}
	got, ref, err := OpenRevision(ring, row)
	if err != nil {
		t.Fatalf("OpenRevision: %v", err)
	}
	if !bytes.Equal(got, plaintext) || ref.Revision != 3 || ref.KeyID != keyID || ref.Hash != row.RevisionHash {
		t.Fatalf("round trip mismatch: %+v", ref)
	}

	row.Ciphertext = bytes.Clone(envelope)
	row.Ciphertext[0] ^= 0x80
	if _, _, err := OpenRevision(ring, row); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}

	row = EncryptedRevision{
		Namespace: namespace, State: StateActive, Revision: 3, KeyID: keyID,
		Ciphertext: envelope, CiphertextSHA256: CiphertextSHA256(envelope),
		RevisionHash: RevisionHash(namespace, 4, keyID, envelope),
	}
	if _, _, err := OpenRevision(ring, row); err == nil {
		t.Fatal("ciphertext moved to another revision was accepted")
	}

	row = EncryptedRevision{
		Namespace: namespace, State: StateArchived, Revision: 3, KeyID: keyID,
		Ciphertext: envelope, CiphertextSHA256: CiphertextSHA256(envelope),
		RevisionHash: RevisionHash(namespace, 3, keyID, envelope),
	}
	if _, _, err := OpenRevision(ring, row); err == nil {
		t.Fatal("archived secret was accepted")
	}
}
