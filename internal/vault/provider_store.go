package vault

import (
	"context"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ProviderReader is the read-only provider credential persistence port. It is
// implemented outside this package so the vault never imports a database
// driver to read provider state.
type ProviderReader interface {
	// ProviderMetadata returns secret-free metadata. A missing scope is
	// (zero, false, nil).
	ProviderMetadata(context.Context, ProviderScope) (ProviderMetadata, bool, error)
	// ListProviderMetadata returns metadata ordered by zone name.
	ListProviderMetadata(context.Context, string, int) ([]ProviderMetadata, error)
	// ProviderEncryptedRevision returns the current encrypted revision. A
	// missing scope is (zero, false, nil).
	ProviderEncryptedRevision(context.Context, ProviderScope) (ProviderEncryptedRevision, bool, error)
}

// ProviderTx is the narrow provider credential transaction port. Every mutation
// runs in the caller's transaction; the port exposes no raw query or driver
// handle. Implementations classify missing rows and uniqueness conflicts
// internally.
type ProviderTx interface {
	// ProviderMetadata returns secret-free metadata for the current transaction.
	ProviderMetadata(context.Context, ProviderScope) (ProviderMetadata, bool, error)
	// LockProviderCredential locks the exact scope row and returns its current
	// revision and zone name.
	LockProviderCredential(context.Context, ProviderScope) (ProviderCredentialState, bool, error)
	// InsertProviderCredential creates revision 1 state and reports false when
	// the zone ID or zone name is already bound.
	InsertProviderCredential(context.Context, ProviderCredentialInput) (bool, error)
	// UpdateProviderCredentialRevision applies the locked CAS pointer update
	// and reports false when the expected revision no longer matches.
	UpdateProviderCredentialRevision(context.Context, ProviderScope, uint64, uint64, contract.Actor) (bool, error)
	// InsertProviderCredentialRevision appends one immutable revision and
	// reports false when that revision already exists.
	InsertProviderCredentialRevision(context.Context, ProviderRevisionInput) (bool, error)
}

// ProviderCredentialState is the locked current state of one provider scope.
type ProviderCredentialState struct {
	LatestRevision uint64
	ZoneName       string
}

// ProviderCredentialInput creates the first provider credential row.
type ProviderCredentialInput struct {
	Scope    ProviderScope
	ZoneName string
	Actor    contract.Actor
}

// ProviderRevisionInput is one immutable encrypted revision. Ciphertext never
// contains plaintext, and plaintext never enters the persistence port.
type ProviderRevisionInput struct {
	Scope            ProviderScope
	Revision         uint64
	KeyID            string
	Ciphertext       []byte
	CiphertextSHA256 string
	RevisionHash     string
	Actor            contract.Actor
}

// ProviderEncryptedRevision is the current encrypted revision read from
// persistence. It carries no plaintext.
type ProviderEncryptedRevision struct {
	Provider         string
	ZoneID           string
	ZoneName         string
	LatestRevision   uint64
	Revision         uint64
	KeyID            string
	Ciphertext       []byte
	CiphertextSHA256 string
	RevisionHash     string
	CreatedAt        time.Time
}
