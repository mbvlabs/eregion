package vault

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

// Querier is the read surface shared by pgxpool.Pool and pgx.Tx. Mutation methods
// explicitly require pgx.Tx so a caller can make a secret change atomic with its
// own domain write.
type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store owns encrypted secret persistence. It does not open transactions.
type Store struct {
	pool *pgxpool.Pool
	ring *KeyRing
}

// NewStore validates its dependencies. The pool is used only for reads and for
// the optional Service transaction wrapper; every mutation method takes a
// caller-owned pgx.Tx.
func NewStore(pool *pgxpool.Pool, ring *KeyRing) (*Store, error) {
	if pool == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "vault store requires a database pool", "Open PostgreSQL before constructing secret storage.")
	}
	if ring == nil || ring.current == "" {
		return nil, contract.NewFault(contract.ValidationFailed, "vault store requires a master-key ring", "Load the protected key file before constructing secret storage.")
	}
	return &Store{pool: pool, ring: ring}, nil
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }
func (s *Store) Ring() *KeyRing      { return s.ring }

// Set creates revision 1 when expectedRevision is 0 without an existing row, or
// appends latest+1 when expectedRevision equals the locked latest revision. Any
// other value is a CAS conflict. A previously archived namespace becomes active
// with the new revision.
func (s *Store) Set(ctx context.Context, tx pgx.Tx, namespace Namespace, value []byte, expectedRevision uint64, actor contract.Actor) (Metadata, error) {
	if tx == nil {
		return Metadata{}, contract.NewFault(contract.Internal, "secret mutation has no transaction", "Integrate secret storage through an explicit PostgreSQL transaction.")
	}
	if err := namespace.Validate(); err != nil {
		return Metadata{}, err
	}
	if err := ValidateValue(value); err != nil {
		return Metadata{}, err
	}
	if err := validateActor(actor); err != nil {
		return Metadata{}, err
	}

	var latest uint64
	err := tx.QueryRow(ctx, `
		SELECT latest_revision
		FROM eregion_secrets
		WHERE app = $1 AND environment = $2 AND name = $3
		FOR UPDATE`, namespace.App, namespace.Environment, namespace.Name).Scan(&latest)
	exists := true
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		exists = false
	case err != nil:
		return Metadata{}, databaseFault(err)
	}
	revision, create, err := nextRevision(exists, latest, expectedRevision)
	if err != nil {
		return Metadata{}, err
	}
	ciphertext, keyID, err := s.ring.Seal(namespace, revision, value)
	if err != nil {
		return Metadata{}, err
	}
	ciphertextHash := CiphertextSHA256(ciphertext)
	revisionHash := RevisionHash(namespace, revision, keyID, ciphertext)

	if create {
		_, err = tx.Exec(ctx, `
			INSERT INTO eregion_secrets
				(app, environment, name, latest_revision, state,
				 created_by_kind, created_by_subject, updated_by_kind, updated_by_subject)
			VALUES ($1,$2,$3,1,'active',$4,$5,$4,$5)`,
			namespace.App, namespace.Environment, namespace.Name, actor.Kind, actor.Subject)
		if err != nil {
			if isUniqueViolation(err) {
				return Metadata{}, revisionConflict(0, 0)
			}
			return Metadata{}, databaseFault(err)
		}
	} else {
		tag, err := tx.Exec(ctx, `
			UPDATE eregion_secrets
			SET latest_revision = $4, state = 'active', archived_at = NULL,
			    updated_by_kind = $5, updated_by_subject = $6, updated_at = clock_timestamp()
			WHERE app = $1 AND environment = $2 AND name = $3 AND latest_revision = $7`,
			namespace.App, namespace.Environment, namespace.Name, revision, actor.Kind, actor.Subject, expectedRevision)
		if err != nil {
			return Metadata{}, databaseFault(err)
		}
		if tag.RowsAffected() != 1 {
			return Metadata{}, revisionConflict(expectedRevision, latest)
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO eregion_secret_revisions
			(app, environment, name, revision, key_id, ciphertext, ciphertext_sha256,
			 revision_hash, created_by_kind, created_by_subject)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		namespace.App, namespace.Environment, namespace.Name, revision, keyID, ciphertext,
		ciphertextHash, revisionHash, actor.Kind, actor.Subject)
	if err != nil {
		if isUniqueViolation(err) {
			return Metadata{}, revisionConflict(expectedRevision, latest)
		}
		return Metadata{}, databaseFault(err)
	}
	return metadataTx(ctx, tx, namespace)
}

// Archive makes the namespace unavailable for resolution while retaining every
// immutable revision. expectedRevision is a required CAS guard.
func (s *Store) Archive(ctx context.Context, tx pgx.Tx, namespace Namespace, expectedRevision uint64, actor contract.Actor) (Metadata, error) {
	if tx == nil {
		return Metadata{}, contract.NewFault(contract.Internal, "secret mutation has no transaction", "Integrate secret storage through an explicit PostgreSQL transaction.")
	}
	if err := namespace.Validate(); err != nil {
		return Metadata{}, err
	}
	if err := validateActor(actor); err != nil {
		return Metadata{}, err
	}
	if expectedRevision == 0 {
		return Metadata{}, contract.NewFault(contract.ValidationFailed, "archive requires the current revision", "Read the secret metadata and pass its latest revision as the CAS guard.")
	}
	var latest uint64
	var state State
	err := tx.QueryRow(ctx, `
		SELECT latest_revision, state
		FROM eregion_secrets
		WHERE app = $1 AND environment = $2 AND name = $3
		FOR UPDATE`, namespace.App, namespace.Environment, namespace.Name).Scan(&latest, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, notFound(namespace)
	}
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	if latest != expectedRevision {
		return Metadata{}, revisionConflict(expectedRevision, latest)
	}
	if state != StateArchived {
		_, err = tx.Exec(ctx, `
			UPDATE eregion_secrets
			SET state = 'archived', archived_at = clock_timestamp(),
			    updated_by_kind = $4, updated_by_subject = $5, updated_at = clock_timestamp()
			WHERE app = $1 AND environment = $2 AND name = $3 AND latest_revision = $6`,
			namespace.App, namespace.Environment, namespace.Name, actor.Kind, actor.Subject, expectedRevision)
		if err != nil {
			return Metadata{}, databaseFault(err)
		}
	}
	return metadataTx(ctx, tx, namespace)
}

// Get reads secret-free metadata through the store pool.
func (s *Store) Get(ctx context.Context, namespace Namespace) (Metadata, error) {
	if err := namespace.Validate(); err != nil {
		return Metadata{}, err
	}
	return metadataQuery(ctx, s.pool, namespace)
}

// List reads secret-free metadata for one environment. Archived namespaces are
// included so operators can see and rotate them.
func (s *Store) List(ctx context.Context, app, environment string, limit int) ([]Metadata, error) {
	if err := ValidateApp(app); err != nil {
		return nil, err
	}
	if err := ValidateEnvironment(environment); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid secret list limit", "Use a limit from 1 to 200.")
	}
	rows, err := s.pool.Query(ctx, metadataSelect+`
		WHERE s.app = $1 AND s.environment = $2
		ORDER BY s.name
		LIMIT $3`, app, environment, limit)
	if err != nil {
		return nil, databaseFault(err)
	}
	defer rows.Close()
	out := make([]Metadata, 0, limit)
	for rows.Next() {
		metadata, err := scanMetadata(rows)
		if err != nil {
			return nil, databaseFault(err)
		}
		out = append(out, metadata)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseFault(err)
	}
	return out, nil
}

// Export decrypts only the latest revision for an explicit protected local
// destination workflow. Callers must never write the returned value to stdout,
// operation rows, events, or logs.
func (s *Store) Export(ctx context.Context, namespace Namespace) (Metadata, []byte, error) {
	if err := namespace.Validate(); err != nil {
		return Metadata{}, nil, err
	}
	row, err := loadLatestEncrypted(ctx, s.pool, namespace)
	if err != nil {
		return Metadata{}, nil, err
	}
	if row.State != StateActive {
		return Metadata{}, nil, contract.NewFault(contract.PreconditionFailed, "secret is archived", "Rotate the secret to a new revision before exporting or resolving it.")
	}
	plaintext, err := decryptRow(s.ring, namespace, &row)
	if err != nil {
		return Metadata{}, nil, err
	}
	return row.Metadata, plaintext, nil
}

// Resolve pins exact revisions for transient environment injection. A nil map
// resolves every active latest revision; an empty non-nil map resolves nothing.
// Values are excluded from JSON by Resolution.
func (s *Store) Resolve(ctx context.Context, actor contract.Actor, app, environment string, revisions RevisionMap) (Resolution, error) {
	return Resolve(ctx, s.pool, s.ring, actor, app, environment, revisions)
}

// Resolve is the package-level form used by workers that already hold a read
// querier. It validates the actor even though resolution is a read, so callers
// cannot accidentally build an unauthenticated injection path.
func Resolve(ctx context.Context, q Querier, ring *KeyRing, actor contract.Actor, app, environment string, revisions RevisionMap) (Resolution, error) {
	if q == nil || ring == nil {
		return Resolution{}, contract.NewFault(contract.Internal, "secret resolution is not configured", "Construct the vault store with a database pool and master key.")
	}
	if err := validateActor(actor); err != nil {
		return Resolution{}, err
	}
	if err := ValidateApp(app); err != nil {
		return Resolution{}, err
	}
	if err := ValidateEnvironment(environment); err != nil {
		return Resolution{}, err
	}
	if revisions != nil && len(revisions) > MaxSecretsPerEnvironment {
		return Resolution{}, contract.NewFault(contract.ValidationFailed, "too many pinned secrets", "Pin at most 128 secret revisions for one environment.")
	}
	rows, err := resolveRows(ctx, q, app, environment, revisions)
	if err != nil {
		return Resolution{}, err
	}
	defer rows.Close()

	values := make(map[string]string, MaxSecretsPerEnvironment)
	refs := make(map[string]RevisionRef, MaxSecretsPerEnvironment)
	total := 0
	for rows.Next() {
		row, err := scanEncrypted(rows)
		if err != nil {
			return Resolution{}, databaseFault(err)
		}
		namespace := Namespace{App: app, Environment: environment, Name: row.Name}
		if row.State != StateActive {
			return Resolution{}, contract.NewFault(contract.PreconditionFailed, "pinned secret revision is archived", "Rotate the secret to a new revision or remove it from the deployment's pinned set.")
		}
		plaintext, err := decryptRow(ring, namespace, &row)
		if err != nil {
			return Resolution{}, err
		}
		if len(plaintext) > MaxValueBytes {
			clearBytes(plaintext)
			return Resolution{}, contract.NewFault(contract.PreconditionFailed, "stored secret exceeds the supported bound", "Replace the secret with a value no larger than 32768 bytes.")
		}
		total += len(plaintext)
		if total > MaxResolveBytes {
			clearBytes(plaintext)
			return Resolution{}, contract.NewFault(contract.PreconditionFailed, "resolved secrets exceed the injection bound", "Reduce the number or size of secrets pinned for one environment.")
		}
		if len(values) >= MaxSecretsPerEnvironment {
			clearBytes(plaintext)
			return Resolution{}, contract.NewFault(contract.PreconditionFailed, "resolved secret count exceeds the injection bound", "Archive unused secrets or pin at most 128 names for one environment.")
		}
		values[row.Name] = string(plaintext)
		clearBytes(plaintext)
		refs[row.Name] = row.Ref
	}
	if err := rows.Err(); err != nil {
		return Resolution{}, databaseFault(err)
	}
	if revisions != nil && len(refs) != len(revisions) {
		missing := make([]string, 0)
		for name := range revisions {
			if _, ok := refs[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		fault := contract.NewFault(contract.NotFound, "one or more pinned secret revisions are unavailable", "Read secret metadata and pin an existing active revision.")
		fault.Details = map[string]any{"missing": missing}
		return Resolution{}, fault
	}
	return Resolution{Values: values, Revisions: refs, Digest: resolutionDigest(app, environment, refs)}, nil
}

func resolveRows(ctx context.Context, q Querier, app, environment string, revisions RevisionMap) (pgx.Rows, error) {
	if revisions == nil {
		rows, err := q.Query(ctx, encryptedSelect+`
			WHERE s.app = $1 AND s.environment = $2 AND s.state = 'active'
			ORDER BY s.name
			LIMIT $3`, app, environment, MaxSecretsPerEnvironment+1)
		if err != nil {
			return nil, databaseFault(err)
		}
		return rows, nil
	}
	names := make([]string, 0, len(revisions))
	for name, revision := range revisions {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
		if revision == 0 {
			return nil, contract.NewFault(contract.ValidationFailed, "pinned secret revision must be positive", "Pin the revision returned by secret metadata.")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	numbers := make([]int64, len(names))
	for i, name := range names {
		revision := revisions[name]
		if revision > math.MaxInt64 {
			return nil, contract.NewFault(contract.ValidationFailed, "pinned secret revision is out of range", "Use the revision returned by secret metadata.")
		}
		numbers[i] = int64(revision)
	}
	rows, err := q.Query(ctx, encryptedPinnedSelect+`
		JOIN unnest($3::text[], $4::bigint[]) AS want(name, revision)
		  ON want.name = s.name AND want.revision = r.revision
		WHERE s.app = $1 AND s.environment = $2
		ORDER BY s.name`, app, environment, names, numbers)
	if err != nil {
		return nil, databaseFault(err)
	}
	return rows, nil
}

const metadataSelect = `
	SELECT s.app, s.environment, s.name, s.latest_revision, s.state,
	       s.created_by_kind, s.created_by_subject,
	       s.updated_by_kind, s.updated_by_subject,
	       s.created_at, s.updated_at, s.archived_at,
	       r.revision, r.key_id, r.revision_hash, r.created_at
	FROM eregion_secrets s
	JOIN eregion_secret_revisions r
	  ON r.app = s.app AND r.environment = s.environment
	 AND r.name = s.name AND r.revision = s.latest_revision`

const encryptedSelect = `
	SELECT s.name, s.state,
	       r.revision, r.key_id, r.ciphertext, r.ciphertext_sha256, r.revision_hash, r.created_at
	FROM eregion_secrets s
	JOIN eregion_secret_revisions r
	  ON r.app = s.app AND r.environment = s.environment
	 AND r.name = s.name AND r.revision = s.latest_revision`

const encryptedPinnedSelect = `
	SELECT s.name, s.state,
	       r.revision, r.key_id, r.ciphertext, r.ciphertext_sha256, r.revision_hash, r.created_at
	FROM eregion_secrets s
	JOIN eregion_secret_revisions r
	  ON r.app = s.app AND r.environment = s.environment
	 AND r.name = s.name`

func metadataQuery(ctx context.Context, q Querier, namespace Namespace) (Metadata, error) {
	row := q.QueryRow(ctx, metadataSelect+`
		WHERE s.app = $1 AND s.environment = $2 AND s.name = $3`,
		namespace.App, namespace.Environment, namespace.Name)
	metadata, err := scanMetadata(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, notFound(namespace)
	}
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	return metadata, nil
}

func metadataTx(ctx context.Context, tx pgx.Tx, namespace Namespace) (Metadata, error) {
	return metadataQuery(ctx, tx, namespace)
}

type scanner interface {
	Scan(...any) error
}

func scanMetadata(row scanner) (Metadata, error) {
	var metadata Metadata
	var createdAt, updatedAt, revisionCreated time.Time
	var archivedAt pgtype.Timestamptz
	err := row.Scan(
		&metadata.App, &metadata.Environment, &metadata.Name, &metadata.LatestRevision, &metadata.State,
		&metadata.CreatedBy.Kind, &metadata.CreatedBy.Subject,
		&metadata.UpdatedBy.Kind, &metadata.UpdatedBy.Subject,
		&createdAt, &updatedAt, &archivedAt,
		&metadata.Revision.Revision, &metadata.Revision.KeyID, &metadata.Revision.Hash, &revisionCreated,
	)
	if err != nil {
		return Metadata{}, err
	}
	metadata.CreatedAt = createdAt.UTC()
	metadata.UpdatedAt = updatedAt.UTC()
	metadata.Revision.CreatedAt = revisionCreated.UTC()
	if archivedAt.Valid {
		value := archivedAt.Time.UTC()
		metadata.ArchivedAt = &value
	}
	return metadata, nil
}

type encryptedRow struct {
	Name           string
	State          State
	Metadata       Metadata
	Ref            RevisionRef
	KeyID          string
	Ciphertext     []byte
	CiphertextHash string
	RevisionHash   string
}

func loadLatestEncrypted(ctx context.Context, q Querier, namespace Namespace) (encryptedRow, error) {
	row := q.QueryRow(ctx, encryptedSelect+`
		WHERE s.app = $1 AND s.environment = $2 AND s.name = $3`,
		namespace.App, namespace.Environment, namespace.Name)
	encrypted, err := scanEncrypted(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return encryptedRow{}, notFound(namespace)
	}
	if err != nil {
		return encryptedRow{}, databaseFault(err)
	}
	metadata, err := metadataQuery(ctx, q, namespace)
	if err != nil {
		return encryptedRow{}, err
	}
	if metadata.LatestRevision != encrypted.Ref.Revision || metadata.State != encrypted.State ||
		metadata.Revision.Hash != encrypted.RevisionHash || metadata.Revision.KeyID != encrypted.KeyID {
		return encryptedRow{}, contract.NewFault(contract.Unavailable, "secret changed while it was read", "Retry the read; secret revisions are immutable but the latest pointer may advance concurrently.")
	}
	encrypted.Metadata = metadata
	return encrypted, nil
}

func scanEncrypted(row scanner) (encryptedRow, error) {
	var out encryptedRow
	var revisionCreated time.Time
	err := row.Scan(&out.Name, &out.State, &out.Ref.Revision, &out.KeyID,
		&out.Ciphertext, &out.CiphertextHash, &out.RevisionHash, &revisionCreated)
	if err != nil {
		return encryptedRow{}, err
	}
	out.Ref.CreatedAt = revisionCreated.UTC()
	return out, nil
}

func decryptRow(ring *KeyRing, namespace Namespace, row *encryptedRow) ([]byte, error) {
	if row.CiphertextHash != CiphertextSHA256(row.Ciphertext) {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret ciphertext integrity check failed", "Restore the revision from backup; do not trust or re-encrypt modified ciphertext.")
	}
	expectedHash := RevisionHash(namespace, row.Ref.Revision, row.KeyID, row.Ciphertext)
	if expectedHash != row.RevisionHash {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret revision binding check failed", "Restore the revision from backup; ciphertext cannot be moved between namespaces or revisions.")
	}
	plaintext, keyID, err := ring.Open(namespace, row.Ref.Revision, row.Ciphertext)
	if err != nil {
		if IsCiphertextError(err) {
			return nil, contract.NewFault(contract.PreconditionFailed, "secret ciphertext failed authentication", "Restore the revision from backup and verify the master-key file; never log or retry modified ciphertext.")
		}
		return nil, err
	}
	if keyID != row.KeyID {
		clearBytes(plaintext)
		return nil, contract.NewFault(contract.PreconditionFailed, "secret key identity check failed", "Restore the revision from backup and verify the master-key file.")
	}
	row.Ref.Hash = expectedHash
	row.Metadata.Revision = row.Ref
	return plaintext, nil
}

func resolutionDigest(app, environment string, refs map[string]RevisionRef) string {
	hash := sha256.New()
	hash.Write([]byte("eregion-secret-resolution/v1"))
	writeHashField(hash, app)
	writeHashField(hash, environment)
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ref := refs[name]
		writeHashField(hash, name)
		var number [8]byte
		binary.BigEndian.PutUint64(number[:], ref.Revision)
		hash.Write(number[:])
		hash.Write([]byte(ref.Hash))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func revisionConflict(expected, current uint64) *contract.Fault {
	fault := contract.NewFault(contract.Conflict, "secret revision does not match", "Read the secret metadata and retry the mutation with its latest revision.")
	fault.Details = map[string]any{"expected_revision": expected, "current_revision": current}
	return fault
}

// nextRevision is the CAS decision used under the row lock. expected 0 creates
// only when no row exists; otherwise the immutable latest revision is the guard.
func nextRevision(exists bool, latest, expected uint64) (uint64, bool, error) {
	if !exists {
		if expected != 0 {
			return 0, false, revisionConflict(expected, 0)
		}
		return 1, true, nil
	}
	if expected == 0 || expected != latest {
		return 0, false, revisionConflict(expected, latest)
	}
	if latest == math.MaxUint64 {
		return 0, false, contract.NewFault(contract.Conflict, "secret revision space is exhausted", "Create a new secret name; revision numbers cannot wrap.")
	}
	return latest + 1, false, nil
}

func notFound(namespace Namespace) *contract.Fault {
	fault := contract.NewFault(contract.NotFound, "secret was not found", "List metadata for the app and environment, then use the exact secret name.")
	fault.Details = map[string]any{"app": namespace.App, "environment": namespace.Environment, "name": namespace.Name}
	return fault
}

func databaseFault(err error) *contract.Fault {
	fault := contract.NewFault(contract.Unavailable, "secret storage is unavailable", "Check PostgreSQL connectivity and schema readiness; re-read metadata before retrying a mutation.")
	fault.Cause = err
	return fault
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
