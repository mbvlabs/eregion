package vault

import (
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// State is the lifecycle of the namespace, not of one immutable revision.
type State string

const (
	StateActive   State = "active"
	StateArchived State = "archived"
)

func (s State) valid() bool { return s == StateActive || s == StateArchived }

// RevisionRef identifies one immutable ciphertext without containing plaintext.
// Hash binds app, environment, name, revision, key ID, and ciphertext bytes. It
// is safe to place in events, operation metadata, and deployment records.
type RevisionRef struct {
	Revision  uint64    `json:"revision"`
	Hash      string    `json:"hash"`
	KeyID     string    `json:"key_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Metadata is secret-free namespace state. It never includes ciphertext or
// plaintext, so it is safe in API responses, logs, and CLI inventory output.
type Metadata struct {
	App            string         `json:"app"`
	Environment    string         `json:"environment"`
	Name           string         `json:"name"`
	LatestRevision uint64         `json:"latest_revision"`
	State          State          `json:"state"`
	Revision       RevisionRef    `json:"revision"`
	CreatedBy      contract.Actor `json:"created_by"`
	UpdatedBy      contract.Actor `json:"updated_by"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	ArchivedAt     *time.Time     `json:"archived_at,omitempty"`
}

// Namespace returns the metadata identity.
func (m Metadata) Namespace() Namespace {
	return Namespace{App: m.App, Environment: m.Environment, Name: m.Name}
}

// Validate rejects a malformed API result before a caller trusts it.
func (m Metadata) Validate() error {
	if err := m.Namespace().Validate(); err != nil {
		return err
	}
	if m.LatestRevision == 0 || !m.State.valid() || m.Revision.Revision != m.LatestRevision ||
		!isHexDigest(m.Revision.Hash) || m.Revision.KeyID == "" ||
		m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || m.CreatedBy.Validate() != nil || m.UpdatedBy.Validate() != nil {
		return contract.NewFault(contract.Unavailable, "API returned invalid secret metadata", "Check binary compatibility before retrying a mutation.")
	}
	if (m.State == StateArchived) != (m.ArchivedAt != nil) {
		return contract.NewFault(contract.Unavailable, "API returned inconsistent secret metadata", "Check binary compatibility before retrying a mutation.")
	}
	return nil
}

// RevisionMap is the exact revision pin for one environment. A nil map resolves
// every active latest revision (bootstrap/discovery); an empty non-nil map
// resolves no secrets (explicitly secret-free).
type RevisionMap map[string]uint64

// Resolution is the transient plaintext result for node injection.
//
// Values is intentionally excluded from JSON. Never place a Resolution or its
// Values in an operation, event, River argument, node payload, journal, or log.
// Revisions and Digest are non-secret and may be recorded.
type Resolution struct {
	Values    map[string]string      `json:"-"`
	Revisions map[string]RevisionRef `json:"revisions"`
	Digest    string                 `json:"digest"`
}

// Env returns a copy of the plaintext environment map.
func (r Resolution) Env() map[string]string {
	out := make(map[string]string, len(r.Values))
	for key, value := range r.Values {
		out[key] = value
	}
	return out
}

// String is safe to log: it exposes only the digest.
func (r Resolution) String() string {
	if r.Digest == "" {
		return "secret resolution (empty)"
	}
	return "secret resolution " + r.Digest
}
