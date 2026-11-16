package entities

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/vault"
)

// SecretStore is the read-only secret persistence capability. It borrows the
// Store's pool and exposes only the vault.SecretReader port: no transaction,
// generated query or driver handle leaves this package.
type SecretStore struct {
	store *Store
	q     *query.Queries
}

// Secrets returns the secret read view of this Store. It also owns the
// standalone secret transaction entry point used by credential commands.
func (s *Store) Secrets() *SecretStore {
	return &SecretStore{store: s, q: query.New(s.pool)}
}

var _ vault.SecretReader = (*SecretStore)(nil)
var _ vault.SecretTransactions = (*SecretStore)(nil)

// WithinSecretTx begins one short transaction and runs the callback with its
// secret view. A callback error rolls the whole transaction back unchanged.
func (s *SecretStore) WithinSecretTx(ctx context.Context, fn func(vault.SecretTx) error) error {
	if fn == nil {
		return errors.New("entities: a secret transaction body is required")
	}
	return s.store.WithinTx(ctx, func(tx *StoreTx) error {
		return fn(tx.Secrets())
	})
}

func (s *SecretStore) SecretMetadata(ctx context.Context, namespace vault.Namespace) (vault.Metadata, bool, error) {
	return secretMetadataQuery(ctx, s.q, namespace)
}

func (s *SecretStore) ListSecretMetadata(ctx context.Context, app, environment string, limit int) ([]vault.Metadata, error) {
	return listSecretMetadataQuery(ctx, s.q, app, environment, limit)
}

func (s *SecretStore) SecretCiphertext(ctx context.Context, namespace vault.Namespace) (vault.StoredCiphertext, bool, error) {
	return secretCiphertextQuery(ctx, s.q, namespace)
}

func (s *SecretStore) ListActiveSecretCiphertexts(ctx context.Context, app, environment string, limit int) ([]vault.StoredCiphertext, error) {
	rows, err := s.q.ListActiveSecretCiphertexts(ctx, query.ListActiveSecretCiphertextsParams{
		App: app, Environment: environment, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, secretUnavailable(err)
	}
	result := make([]vault.StoredCiphertext, 0, len(rows))
	for _, row := range rows {
		item, err := secretCiphertext(row.App, row.Environment, row.Name, row.State,
			row.Revision, row.KeyID, row.Ciphertext, row.CiphertextSha256,
			row.RevisionHash, row.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *SecretStore) ListPinnedSecretCiphertexts(ctx context.Context, app, environment string, revisions vault.RevisionMap) ([]vault.StoredCiphertext, error) {
	pins, err := encodeSecretPins(revisions)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListPinnedSecretCiphertexts(ctx, query.ListPinnedSecretCiphertextsParams{
		Pins: pins, App: app, Environment: environment,
	})
	if err != nil {
		return nil, secretUnavailable(err)
	}
	result := make([]vault.StoredCiphertext, 0, len(rows))
	for _, row := range rows {
		item, err := secretCiphertext(row.App, row.Environment, row.Name, row.State,
			row.Revision, row.KeyID, row.Ciphertext, row.CiphertextSha256,
			row.RevisionHash, row.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

// SecretTx is the secret view of one StoreTx. It implements the vault's narrow
// transaction port; every method uses the caller's transaction.
type SecretTx struct{ owner *StoreTx }

// Secrets returns the secret view of this transaction.
func (tx *StoreTx) Secrets() *SecretTx { return &SecretTx{owner: tx} }

var _ vault.SecretTx = (*SecretTx)(nil)

func (s *SecretTx) SecretMetadata(ctx context.Context, namespace vault.Namespace) (vault.Metadata, bool, error) {
	return secretMetadataQuery(ctx, s.owner.q, namespace)
}

// LockSecretLatestRevision locks the namespace row and returns its latest
// revision. A missing row is (0, false, nil).
func (s *SecretTx) LockSecretLatestRevision(ctx context.Context, namespace vault.Namespace) (uint64, bool, error) {
	latest, err := s.owner.q.LockSecretLatestRevision(ctx, query.LockSecretLatestRevisionParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, secretUnavailable(err)
	}
	revision, err := secretRevisionValue(latest)
	if err != nil {
		return 0, false, err
	}
	return revision, true, nil
}

func (s *SecretTx) LockSecretNamespace(ctx context.Context, namespace vault.Namespace) (vault.SecretNamespaceState, bool, error) {
	row, err := s.owner.q.LockSecretNamespace(ctx, query.LockSecretNamespaceParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.SecretNamespaceState{}, false, nil
	}
	if err != nil {
		return vault.SecretNamespaceState{}, false, secretUnavailable(err)
	}
	latest, err := secretRevisionValue(row.LatestRevision)
	if err != nil {
		return vault.SecretNamespaceState{}, false, err
	}
	return vault.SecretNamespaceState{LatestRevision: latest, State: vault.State(row.State)}, true, nil
}

func (s *SecretTx) LockSecretMetadata(ctx context.Context, namespace vault.Namespace) (vault.Metadata, bool, error) {
	if _, err := s.owner.q.LockSecretMetadataRow(ctx, query.LockSecretMetadataRowParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	}); errors.Is(err, pgx.ErrNoRows) {
		return vault.Metadata{}, false, nil
	} else if err != nil {
		return vault.Metadata{}, false, secretUnavailable(err)
	}
	return secretMetadataQuery(ctx, s.owner.q, namespace)
}

// InsertSecretNamespace creates revision 1 state. A duplicate namespace
// returns a Conflict error. That error means the current PostgreSQL transaction
// is aborted; the caller must roll the transaction or savepoint back before any
// further statement. The translated fault does not repair PG.
func (s *SecretTx) InsertSecretNamespace(ctx context.Context, namespace vault.Namespace, actor contract.Actor) error {
	err := s.owner.q.InsertSecretNamespace(ctx, query.InsertSecretNamespaceParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		ActorKind: actor.Kind, ActorSubject: actor.Subject,
	})
	if err == nil {
		return nil
	}
	if isSecretConstraintViolation(err, "eregion_secrets_pkey") {
		return secretConflict("secret namespace already exists")
	}
	return secretUnavailable(err)
}

// AdvanceSecretNamespace applies the locked CAS pointer update. It reports
// false when the expected revision no longer matches.
func (s *SecretTx) AdvanceSecretNamespace(ctx context.Context, namespace vault.Namespace, expectedRevision, revision uint64, actor contract.Actor) (bool, error) {
	expected, err := secretBigintValue(expectedRevision)
	if err != nil {
		return false, err
	}
	next, err := secretBigintValue(revision)
	if err != nil {
		return false, err
	}
	affected, err := s.owner.q.AdvanceSecretNamespace(ctx, query.AdvanceSecretNamespaceParams{
		Revision: next, ActorKind: actor.Kind, ActorSubject: actor.Subject,
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		ExpectedRevision: expected,
	})
	if err != nil {
		return false, secretUnavailable(err)
	}
	return affected == 1, nil
}

// ArchiveSecretNamespace marks a namespace archived under the locked CAS
// guard. The caller decides whether an already-archived namespace needs no
// write; this method reports false when the expected revision no longer
// matches.
func (s *SecretTx) ArchiveSecretNamespace(ctx context.Context, namespace vault.Namespace, expectedRevision uint64, actor contract.Actor) (bool, error) {
	expected, err := secretBigintValue(expectedRevision)
	if err != nil {
		return false, err
	}
	affected, err := s.owner.q.ArchiveSecretNamespace(ctx, query.ArchiveSecretNamespaceParams{
		ActorKind: actor.Kind, ActorSubject: actor.Subject,
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		ExpectedRevision: expected,
	})
	if err != nil {
		return false, secretUnavailable(err)
	}
	return affected == 1, nil
}

// InsertSecretRevision appends one immutable sealed revision. A duplicate
// revision returns a Conflict error and aborts the current PostgreSQL
// transaction; the caller must roll it back before continuing.
func (s *SecretTx) InsertSecretRevision(ctx context.Context, namespace vault.Namespace, input vault.SecretRevisionInput) error {
	revision, err := secretBigintValue(input.Revision)
	if err != nil {
		return err
	}
	err = s.owner.q.InsertSecretRevision(ctx, query.InsertSecretRevisionParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		Revision: revision, KeyID: input.KeyID, Ciphertext: input.Ciphertext,
		CiphertextSha256: input.CiphertextSHA256, RevisionHash: input.RevisionHash,
		ActorKind: input.Actor.Kind, ActorSubject: input.Actor.Subject,
	})
	if err == nil {
		return nil
	}
	if isSecretConstraintViolation(err, "eregion_secret_revisions_pkey") {
		return secretConflict("secret revision already exists")
	}
	return secretUnavailable(err)
}

func secretMetadataQuery(ctx context.Context, q *query.Queries, namespace vault.Namespace) (vault.Metadata, bool, error) {
	row, err := q.GetSecretMetadata(ctx, query.GetSecretMetadataParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.Metadata{}, false, nil
	}
	if err != nil {
		return vault.Metadata{}, false, secretUnavailable(err)
	}
	metadata, err := secretMetadata(row.App, row.Environment, row.Name, row.LatestRevision,
		row.State, row.CreatedByKind, row.CreatedBySubject, row.UpdatedByKind,
		row.UpdatedBySubject, row.CreatedAt, row.UpdatedAt, row.ArchivedAt,
		row.Revision, row.KeyID, row.RevisionHash, row.RevisionCreatedAt)
	if err != nil {
		return vault.Metadata{}, false, err
	}
	return metadata, true, nil
}

func listSecretMetadataQuery(ctx context.Context, q *query.Queries, app, environment string, limit int) ([]vault.Metadata, error) {
	rows, err := q.ListSecretMetadata(ctx, query.ListSecretMetadataParams{
		App: app, Environment: environment, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, secretUnavailable(err)
	}
	result := make([]vault.Metadata, 0, len(rows))
	for _, row := range rows {
		metadata, err := secretMetadata(row.App, row.Environment, row.Name, row.LatestRevision,
			row.State, row.CreatedByKind, row.CreatedBySubject, row.UpdatedByKind,
			row.UpdatedBySubject, row.CreatedAt, row.UpdatedAt, row.ArchivedAt,
			row.Revision, row.KeyID, row.RevisionHash, row.RevisionCreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, metadata)
	}
	return result, nil
}

func secretCiphertextQuery(ctx context.Context, q *query.Queries, namespace vault.Namespace) (vault.StoredCiphertext, bool, error) {
	row, err := q.GetLatestSecretCiphertext(ctx, query.GetLatestSecretCiphertextParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.StoredCiphertext{}, false, nil
	}
	if err != nil {
		return vault.StoredCiphertext{}, false, secretUnavailable(err)
	}
	item, err := secretCiphertext(row.App, row.Environment, row.Name, row.State,
		row.Revision, row.KeyID, row.Ciphertext, row.CiphertextSha256,
		row.RevisionHash, row.CreatedAt)
	if err != nil {
		return vault.StoredCiphertext{}, false, err
	}
	return item, true, nil
}

func secretMetadata(app, environment, name string, latestRevision int64, state string,
	createdByKind, createdBySubject, updatedByKind, updatedBySubject string,
	createdAt, updatedAt, archivedAt pgtype.Timestamptz, revision int64,
	keyID, revisionHash string, revisionCreatedAt pgtype.Timestamptz,
) (vault.Metadata, error) {
	latest, err := secretRevisionValue(latestRevision)
	if err != nil {
		return vault.Metadata{}, err
	}
	current, err := secretRevisionValue(revision)
	if err != nil {
		return vault.Metadata{}, err
	}
	created, err := secretTimestamp(createdAt)
	if err != nil {
		return vault.Metadata{}, err
	}
	updated, err := secretTimestamp(updatedAt)
	if err != nil {
		return vault.Metadata{}, err
	}
	revisionCreated, err := secretTimestamp(revisionCreatedAt)
	if err != nil {
		return vault.Metadata{}, err
	}
	metadata := vault.Metadata{
		App: app, Environment: environment, Name: name, LatestRevision: latest,
		State:     vault.State(state),
		Revision:  vault.RevisionRef{Revision: current, KeyID: keyID, Hash: revisionHash, CreatedAt: revisionCreated},
		CreatedBy: contract.Actor{Kind: createdByKind, Subject: createdBySubject},
		UpdatedBy: contract.Actor{Kind: updatedByKind, Subject: updatedBySubject},
		CreatedAt: created, UpdatedAt: updated,
	}
	if archivedAt.Valid {
		value := archivedAt.Time.UTC()
		metadata.ArchivedAt = &value
	}
	return metadata, nil
}

func secretCiphertext(app, environment, name, state string, revision int64, keyID string,
	ciphertext []byte, ciphertextSHA256, revisionHash string, createdAt pgtype.Timestamptz,
) (vault.StoredCiphertext, error) {
	current, err := secretRevisionValue(revision)
	if err != nil {
		return vault.StoredCiphertext{}, err
	}
	created, err := secretTimestamp(createdAt)
	if err != nil {
		return vault.StoredCiphertext{}, err
	}
	return vault.StoredCiphertext{
		Namespace:        vault.Namespace{App: app, Environment: environment, Name: name},
		State:            vault.State(state),
		Revision:         current,
		KeyID:            keyID,
		Ciphertext:       ciphertext,
		CiphertextSHA256: ciphertextSHA256,
		RevisionHash:     revisionHash,
		CreatedAt:        created,
	}, nil
}

type secretPin struct {
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

// encodeSecretPins converts the exact revision pins into the JSON set the
// pinned statement expands. A nil map resolves nothing here; the caller decides
// whether a nil map means "every active secret".
func encodeSecretPins(revisions vault.RevisionMap) (json.RawMessage, error) {
	if len(revisions) == 0 {
		return json.RawMessage("[]"), nil
	}
	names := make([]string, 0, len(revisions))
	for name := range revisions {
		names = append(names, name)
	}
	sort.Strings(names)
	pins := make([]secretPin, 0, len(names))
	for _, name := range names {
		revision, err := secretBigintValue(revisions[name])
		if err != nil {
			return nil, err
		}
		pins = append(pins, secretPin{Name: name, Revision: revision})
	}
	raw, err := json.Marshal(pins)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// secretRevisionValue converts a persisted bigint revision without wrapping.
// Revisions are positive; a stored zero or negative value is corrupt state.
func secretRevisionValue(value int64) (uint64, error) {
	if value <= 0 {
		return 0, secretCorrupt("secret revision is outside the supported range")
	}
	return uint64(value), nil
}

// secretBigintValue bounds a caller revision before it reaches a bigint
// parameter. The vault CAS check is the primary guard; this is defense in depth.
func secretBigintValue(value uint64) (int64, error) {
	if value > math.MaxInt64 {
		return 0, contract.NewFault(contract.Conflict, "secret revision is outside the database range", "Re-read the secret metadata; revision numbers cannot exceed the signed bigint range.")
	}
	return int64(value), nil
}

func secretTimestamp(value pgtype.Timestamptz) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, secretCorrupt("secret metadata is incomplete")
	}
	return value.Time.UTC(), nil
}

func secretCorrupt(message string) *contract.Fault {
	return contract.NewFault(contract.Unavailable, message, "Restore the secret from backup; do not infer state from a malformed row.")
}

func isSecretConstraintViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func secretConflict(message string) *contract.Fault {
	return contract.NewFault(contract.Conflict, message, "Read the current secret metadata; do not continue in the aborted transaction.")
}

func secretUnavailable(err error) *contract.Fault {
	fault := contract.NewFault(contract.Unavailable, "secret storage is unavailable", "Check PostgreSQL connectivity and schema readiness before retrying.")
	fault.Cause = err
	return fault
}

// SecretDefinitionSnapshot is one non-secret name/revision/state fact.
type SecretDefinitionSnapshot struct {
	Name     string
	Revision uint64
	State    string
}

// DefinitionSnapshot lists the persisted non-secret secret metadata for one
// environment in name order.
func (s *SecretTx) DefinitionSnapshot(ctx context.Context, app, environment string) ([]SecretDefinitionSnapshot, error) {
	rows, err := s.owner.q.ListDefinitionSecrets(ctx, query.ListDefinitionSecretsParams{App: app, Environment: environment})
	if err != nil {
		return nil, err
	}
	result := make([]SecretDefinitionSnapshot, 0, len(rows))
	for _, row := range rows {
		revision, err := secretRevisionValue(row.LatestRevision)
		if err != nil {
			return nil, err
		}
		result = append(result, SecretDefinitionSnapshot{Name: row.Name, Revision: revision, State: row.State})
	}
	return result, nil
}

// LockDefinitionSecretNames share-locks every secret name for the environment.
func (s *SecretTx) LockDefinitionSecretNames(ctx context.Context, app, environment string) ([]string, error) {
	return s.owner.q.LockDefinitionSecretNames(ctx, query.LockDefinitionSecretNamesParams{App: app, Environment: environment})
}

// LockBinding share-locks one named secret binding.
func (s *SecretTx) LockBinding(ctx context.Context, app, environment, name string) (bool, error) {
	_, err := s.owner.q.LockSecretBinding(ctx, query.LockSecretBindingParams{App: app, Environment: environment, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DeleteForEnvironment removes all secrets for one environment.
func (s *SecretTx) DeleteForEnvironment(ctx context.Context, app, environment string) (int64, error) {
	return s.owner.q.DeleteSecretsForEnvironment(ctx, query.DeleteSecretsForEnvironmentParams{App: app, Environment: environment})
}

// DeleteForApp removes all secrets for one application.
func (s *SecretTx) DeleteForApp(ctx context.Context, app string) (int64, error) {
	return s.owner.q.DeleteSecretsForApp(ctx, app)
}
