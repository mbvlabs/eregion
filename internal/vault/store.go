package vault

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"sort"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Store owns encrypted secret persistence through narrow typed ports. It does
// not open transactions and never imports a database driver or pool.
type Store struct {
	secrets      SecretReader
	transactions SecretTransactions
	ring         *KeyRing
	providers    ProviderReader
}

// NewStore validates its dependencies. The transaction port is the standalone
// short-transaction form used by credential commands; domain code that already
// owns a StoreTx calls Set/Archive/GetTx directly.
func NewStore(secrets SecretReader, transactions SecretTransactions, ring *KeyRing, providers ProviderReader) (*Store, error) {
	if secrets == nil || transactions == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "vault store requires typed secret persistence", "Construct the entities secret views before constructing secret storage.")
	}
	if ring == nil || ring.current == "" {
		return nil, contract.NewFault(contract.ValidationFailed, "vault store requires a master-key ring", "Load the protected key file before constructing secret storage.")
	}
	if providers == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "vault store requires provider persistence", "Construct the typed entities provider reader before constructing secret storage.")
	}
	return &Store{secrets: secrets, transactions: transactions, ring: ring, providers: providers}, nil
}

func (s *Store) Ring() *KeyRing { return s.ring }

// Set creates revision 1 when expectedRevision is 0 without an existing row, or
// appends latest+1 when expectedRevision equals the locked latest revision. Any
// other value is a CAS conflict. A previously archived namespace becomes active
// with the new revision.
func (s *Store) Set(ctx context.Context, tx SecretTx, namespace Namespace, value []byte, expectedRevision uint64, actor contract.Actor) (Metadata, error) {
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

	latest, exists, err := tx.LockSecretLatestRevision(ctx, namespace)
	if err != nil {
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
		if err := tx.InsertSecretNamespace(ctx, namespace, actor); err != nil {
			if contract.Classify(err).Code == contract.Conflict {
				return Metadata{}, revisionConflict(0, 0)
			}
			return Metadata{}, err
		}
	} else {
		updated, err := tx.AdvanceSecretNamespace(ctx, namespace, expectedRevision, revision, actor)
		if err != nil {
			return Metadata{}, databaseFault(err)
		}
		if !updated {
			return Metadata{}, revisionConflict(expectedRevision, latest)
		}
	}
	if err := tx.InsertSecretRevision(ctx, namespace, SecretRevisionInput{
		Namespace: namespace, Revision: revision, KeyID: keyID, Ciphertext: ciphertext,
		CiphertextSHA256: ciphertextHash, RevisionHash: revisionHash, Actor: actor,
	}); err != nil {
		if contract.Classify(err).Code == contract.Conflict {
			return Metadata{}, revisionConflict(expectedRevision, latest)
		}
		return Metadata{}, err
	}
	metadata, found, err := tx.SecretMetadata(ctx, namespace)
	if err != nil {
		return Metadata{}, err
	}
	if !found {
		return Metadata{}, notFound(namespace)
	}
	return metadata, nil
}

// Archive makes the namespace unavailable for resolution while retaining every
// immutable revision. expectedRevision is a required CAS guard.
func (s *Store) Archive(ctx context.Context, tx SecretTx, namespace Namespace, expectedRevision uint64, actor contract.Actor) (Metadata, error) {
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
	state, found, err := tx.LockSecretNamespace(ctx, namespace)
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	if !found {
		return Metadata{}, notFound(namespace)
	}
	if state.LatestRevision != expectedRevision {
		return Metadata{}, revisionConflict(expectedRevision, state.LatestRevision)
	}
	if state.State != StateArchived {
		updated, err := tx.ArchiveSecretNamespace(ctx, namespace, expectedRevision, actor)
		if err != nil {
			return Metadata{}, databaseFault(err)
		}
		if !updated {
			return Metadata{}, revisionConflict(expectedRevision, state.LatestRevision)
		}
	}
	metadata, found, err := tx.SecretMetadata(ctx, namespace)
	if err != nil {
		return Metadata{}, err
	}
	if !found {
		return Metadata{}, notFound(namespace)
	}
	return metadata, nil
}

// Get reads secret-free metadata through the store's read port.
func (s *Store) Get(ctx context.Context, namespace Namespace) (Metadata, error) {
	if err := namespace.Validate(); err != nil {
		return Metadata{}, err
	}
	metadata, found, err := s.secrets.SecretMetadata(ctx, namespace)
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	if !found {
		return Metadata{}, notFound(namespace)
	}
	return metadata, nil
}

// GetTx reads secret-free metadata inside a caller-owned transaction without
// locking the namespace. Set independently locks and checks its CAS revision.
func (s *Store) GetTx(ctx context.Context, tx SecretTx, namespace Namespace) (Metadata, error) {
	if tx == nil {
		return Metadata{}, contract.NewFault(contract.Internal, "secret read has no transaction", "Integrate secret reads through an explicit PostgreSQL transaction.")
	}
	if err := namespace.Validate(); err != nil {
		return Metadata{}, err
	}
	metadata, found, err := tx.SecretMetadata(ctx, namespace)
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	if !found {
		return Metadata{}, notFound(namespace)
	}
	return metadata, nil
}

// GetForUpdateTx locks the namespace row until the caller commits or rolls back,
// then reads secret-free metadata. It serializes admission decisions with Set
// and Archive without reading or decrypting secret values.
func (s *Store) GetForUpdateTx(ctx context.Context, tx SecretTx, namespace Namespace) (Metadata, error) {
	if tx == nil {
		return Metadata{}, contract.NewFault(contract.Internal, "secret read has no transaction", "Integrate secret reads through an explicit PostgreSQL transaction.")
	}
	if err := namespace.Validate(); err != nil {
		return Metadata{}, err
	}
	metadata, found, err := tx.LockSecretMetadata(ctx, namespace)
	if err != nil {
		return Metadata{}, databaseFault(err)
	}
	if !found {
		return Metadata{}, notFound(namespace)
	}
	return metadata, nil
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
	out, err := s.secrets.ListSecretMetadata(ctx, app, environment, limit)
	if err != nil {
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
	stored, found, err := s.secrets.SecretCiphertext(ctx, namespace)
	if err != nil {
		return Metadata{}, nil, databaseFault(err)
	}
	if !found {
		return Metadata{}, nil, notFound(namespace)
	}
	if stored.State != StateActive {
		return Metadata{}, nil, contract.NewFault(contract.PreconditionFailed, "secret is archived", "Rotate the secret to a new revision before exporting or resolving it.")
	}
	metadata, found, err := s.secrets.SecretMetadata(ctx, namespace)
	if err != nil {
		return Metadata{}, nil, databaseFault(err)
	}
	if !found {
		return Metadata{}, nil, notFound(namespace)
	}
	if metadata.LatestRevision != stored.Revision || metadata.State != stored.State ||
		metadata.Revision.Hash != stored.RevisionHash || metadata.Revision.KeyID != stored.KeyID {
		return Metadata{}, nil, contract.NewFault(contract.Unavailable, "secret changed while it was read", "Retry the read; secret revisions are immutable but the latest pointer may advance concurrently.")
	}
	plaintext, _, err := decryptStored(s.ring, stored)
	if err != nil {
		return Metadata{}, nil, err
	}
	return metadata, plaintext, nil
}

// Resolve pins exact revisions for transient environment injection. A nil map
// resolves every active latest revision; an empty non-nil map resolves nothing.
// Values are excluded from JSON by Resolution.
func (s *Store) Resolve(ctx context.Context, actor contract.Actor, app, environment string, revisions RevisionMap) (Resolution, error) {
	if s == nil || s.secrets == nil || s.ring == nil {
		return Resolution{}, contract.NewFault(contract.Internal, "secret resolution is not configured", "Construct the vault store with typed secret persistence and a master key.")
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
	var (
		rows []StoredCiphertext
		err  error
	)
	if revisions == nil {
		rows, err = s.secrets.ListActiveSecretCiphertexts(ctx, app, environment, MaxSecretsPerEnvironment+1)
	} else {
		rows, err = s.secrets.ListPinnedSecretCiphertexts(ctx, app, environment, revisions)
	}
	if err != nil {
		return Resolution{}, databaseFault(err)
	}

	values := make(map[string]string, MaxSecretsPerEnvironment)
	refs := make(map[string]RevisionRef, MaxSecretsPerEnvironment)
	total := 0
	for _, row := range rows {
		namespace := row.Namespace
		if row.State != StateActive {
			return Resolution{}, contract.NewFault(contract.PreconditionFailed, "pinned secret revision is archived", "Rotate the secret to a new revision or remove it from the deployment's pinned set.")
		}
		plaintext, ref, err := decryptStored(s.ring, row)
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
		values[namespace.Name] = string(plaintext)
		clearBytes(plaintext)
		refs[namespace.Name] = ref
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

func decryptStored(ring *KeyRing, stored StoredCiphertext) ([]byte, RevisionRef, error) {
	if stored.CiphertextSHA256 != CiphertextSHA256(stored.Ciphertext) {
		return nil, RevisionRef{}, contract.NewFault(contract.PreconditionFailed, "secret ciphertext integrity check failed", "Restore the revision from backup; do not trust or re-encrypt modified ciphertext.")
	}
	expectedHash := RevisionHash(stored.Namespace, stored.Revision, stored.KeyID, stored.Ciphertext)
	if expectedHash != stored.RevisionHash {
		return nil, RevisionRef{}, contract.NewFault(contract.PreconditionFailed, "secret revision binding check failed", "Restore the revision from backup; ciphertext cannot be moved between namespaces or revisions.")
	}
	plaintext, keyID, err := ring.Open(stored.Namespace, stored.Revision, stored.Ciphertext)
	if err != nil {
		if IsCiphertextError(err) {
			return nil, RevisionRef{}, contract.NewFault(contract.PreconditionFailed, "secret ciphertext failed authentication", "Restore the revision from backup and verify the master-key file; never log or retry modified ciphertext.")
		}
		return nil, RevisionRef{}, err
	}
	if keyID != stored.KeyID {
		clearBytes(plaintext)
		return nil, RevisionRef{}, contract.NewFault(contract.PreconditionFailed, "secret key identity check failed", "Restore the revision from backup and verify the master-key file.")
	}
	return plaintext, RevisionRef{
		Revision: stored.Revision, KeyID: stored.KeyID, Hash: expectedHash, CreatedAt: stored.CreatedAt,
	}, nil
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
