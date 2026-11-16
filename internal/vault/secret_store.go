package vault

import (
	"context"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// SecretReader is the read-only secret persistence port. It is implemented
// outside this package so the vault never imports a database driver to read
// stored state. Missing rows are reported as (zero, false, nil).
type SecretReader interface {
	// SecretMetadata returns secret-free namespace metadata.
	SecretMetadata(context.Context, Namespace) (Metadata, bool, error)
	// ListSecretMetadata returns secret-free metadata for one environment,
	// including archived namespaces, ordered by name.
	ListSecretMetadata(context.Context, string, string, int) ([]Metadata, error)
	// SecretCiphertext returns the latest stored ciphertext for a namespace.
	SecretCiphertext(context.Context, Namespace) (StoredCiphertext, bool, error)
	// ListActiveSecretCiphertexts returns the latest ciphertext of every active
	// namespace in one environment, ordered by name.
	ListActiveSecretCiphertexts(context.Context, string, string, int) ([]StoredCiphertext, error)
	// ListPinnedSecretCiphertexts returns the exact pinned revisions of one
	// environment, ordered by name. An empty map returns no rows.
	ListPinnedSecretCiphertexts(context.Context, string, string, RevisionMap) ([]StoredCiphertext, error)
}

// SecretTx is the narrow secret mutation port. Every method runs in the
// caller's transaction and deals only in already-sealed ciphertext. Plaintext
// never crosses this boundary.
type SecretTx interface {
	// SecretMetadata returns secret-free metadata for the current transaction.
	SecretMetadata(context.Context, Namespace) (Metadata, bool, error)
	// LockSecretLatestRevision locks the namespace row and returns its latest
	// revision. It is the CAS guard used when appending a revision.
	LockSecretLatestRevision(context.Context, Namespace) (uint64, bool, error)
	// LockSecretNamespace locks the namespace row and returns its revision and
	// lifecycle state.
	LockSecretNamespace(context.Context, Namespace) (SecretNamespaceState, bool, error)
	// LockSecretMetadata locks the namespace row without reading ciphertext and
	// returns secret-free metadata.
	LockSecretMetadata(context.Context, Namespace) (Metadata, bool, error)
	// InsertSecretNamespace creates revision 1 state. A duplicate namespace
	// returns a Conflict error. That error means the current PostgreSQL
	// transaction or savepoint is aborted; the caller must roll it back before
	// issuing any further statement. The translated fault does not repair the
	// transaction.
	InsertSecretNamespace(context.Context, Namespace, contract.Actor) error
	// AdvanceSecretNamespace applies the locked CAS pointer update and reports
	// false when the expected revision no longer matches. A false result is a
	// legitimate CAS loss, not an aborted transaction.
	AdvanceSecretNamespace(context.Context, Namespace, uint64, uint64, contract.Actor) (bool, error)
	// ArchiveSecretNamespace marks a namespace archived under the locked CAS
	// guard and reports false when the expected revision no longer matches. A
	// false result is a legitimate CAS loss, not an aborted transaction.
	ArchiveSecretNamespace(context.Context, Namespace, uint64, contract.Actor) (bool, error)
	// InsertSecretRevision appends one immutable sealed revision. A duplicate
	// revision returns a Conflict error and aborts the current PostgreSQL
	// transaction or savepoint; roll it back before any further statement.
	InsertSecretRevision(context.Context, Namespace, SecretRevisionInput) error
}

// SecretTransactions begins one short transaction and runs the callback with
// its secret view. It is the standalone form used by credential commands that
// do not already own a transaction.
type SecretTransactions interface {
	WithinSecretTx(context.Context, func(SecretTx) error) error
}

// SecretNamespaceState is the locked lifecycle state of one namespace.
type SecretNamespaceState struct {
	LatestRevision uint64
	State          State
}

// SecretRevisionInput is one immutable sealed revision. Ciphertext never
// contains plaintext, and plaintext never enters the persistence port.
type SecretRevisionInput struct {
	Namespace        Namespace
	Revision         uint64
	KeyID            string
	Ciphertext       []byte
	CiphertextSHA256 string
	RevisionHash     string
	Actor            contract.Actor
}

// StoredCiphertext is one sealed revision read from persistence. It carries no
// plaintext. Ref binds the namespace, revision, key and ciphertext hash.
type StoredCiphertext struct {
	Namespace        Namespace
	State            State
	Revision         uint64
	KeyID            string
	Ciphertext       []byte
	CiphertextSHA256 string
	RevisionHash     string
	CreatedAt        time.Time
}

// Ref returns the immutable revision identity of the stored ciphertext.
func (c StoredCiphertext) Ref() RevisionRef {
	return RevisionRef{
		Revision:  c.Revision,
		Hash:      c.RevisionHash,
		KeyID:     c.KeyID,
		CreatedAt: c.CreatedAt,
	}
}
