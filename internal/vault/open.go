package vault

import (
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// EncryptedRevision is one secret revision row read by a caller-owned query.
// It carries ciphertext, never plaintext. OpenRevision authenticates the row
// with the same checks as Resolve before decrypting it, so a caller that joins
// environment and secret reads does not have to copy or weaken the crypto.
type EncryptedRevision struct {
	Namespace        Namespace
	State            State
	Revision         uint64
	KeyID            string
	Ciphertext       []byte
	CiphertextSHA256 string
	RevisionHash     string
	CreatedAt        time.Time
}

// OpenRevision verifies the ciphertext hash, the namespace/revision binding and
// the key identity, then decrypts the exact revision with the key ring. It is
// intentionally narrow: callers still own authorization and must never persist
// the returned plaintext.
func OpenRevision(ring *KeyRing, row EncryptedRevision) ([]byte, RevisionRef, error) {
	if ring == nil {
		return nil, RevisionRef{}, contract.NewFault(contract.Unavailable, "secret storage is not configured", "Set EREGION_VAULT_KEY_FILE on every control-plane worker.")
	}
	if err := row.Namespace.Validate(); err != nil {
		return nil, RevisionRef{}, err
	}
	if row.State != StateActive {
		return nil, RevisionRef{}, contract.NewFault(contract.PreconditionFailed, "an environment secret is archived", "Rotate or remove the archived secret name from the environment revision.")
	}
	if row.Revision == 0 || row.KeyID == "" || len(row.Ciphertext) == 0 {
		return nil, RevisionRef{}, contract.NewFault(contract.PreconditionFailed, "the environment webhook secret is unusable", "Rotate it with eregion env update.")
	}
	plaintext, ref, err := decryptStored(ring, StoredCiphertext{
		Namespace: row.Namespace, State: row.State, Revision: row.Revision,
		KeyID: row.KeyID, Ciphertext: row.Ciphertext, CiphertextSHA256: row.CiphertextSHA256,
		RevisionHash: row.RevisionHash, CreatedAt: row.CreatedAt,
	})
	if err != nil {
		return nil, RevisionRef{}, err
	}
	return plaintext, ref, nil
}
