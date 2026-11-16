package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/vault"
)

func isPostgresCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// TestIntegrationEntitiesSecretPortCiphertextMetadata drives the vault-owned
// secret ports through their entities implementation on isolated PostgreSQL.
// Ciphertext is produced by the real key ring; the persistence layer only sees
// sealed bytes. Metadata, pinned reads and the CAS/archive guards are all real
// rows.
func TestIntegrationEntitiesSecretPortCiphertextMetadata(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]string{
		"eregion_secrets":          "eregion_secrets_pkey",
		"eregion_secret_revisions": "eregion_secret_revisions_pkey",
	} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT conname FROM pg_constraint WHERE conrelid=$1::regclass AND contype='p'`, table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("primary key on %s = %s, want %s", table, got, want)
		}
	}
	namespace := vault.Namespace{App: "demo", Environment: "staging", Name: "API_TOKEN"}
	first, keyID, err := ring.Seal(namespace, 1, []byte("first-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	firstInput := vault.SecretRevisionInput{
		Namespace: namespace, Revision: 1, KeyID: keyID, Ciphertext: first,
		CiphertextSHA256: vault.CiphertextSHA256(first),
		RevisionHash:     vault.RevisionHash(namespace, 1, keyID, first),
		Actor:            testActor,
	}

	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		secrets := tx.Secrets()
		if err := secrets.InsertSecretNamespace(ctx, namespace, testActor); err != nil {
			t.Fatalf("insert namespace = %v", err)
		}
		if err := secrets.InsertSecretRevision(ctx, namespace, firstInput); err != nil {
			t.Fatalf("insert revision one = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	metadata, found, err := store.Secrets().SecretMetadata(ctx, namespace)
	if err != nil || !found {
		t.Fatalf("metadata: %v found=%v", err, found)
	}
	if metadata.LatestRevision != 1 || metadata.State != vault.StateActive ||
		metadata.Revision.Revision != 1 || metadata.Revision.KeyID != keyID ||
		metadata.Revision.Hash != firstInput.RevisionHash ||
		metadata.CreatedBy != testActor || metadata.UpdatedBy != testActor ||
		metadata.CreatedAt.IsZero() || metadata.UpdatedAt.IsZero() || metadata.ArchivedAt != nil {
		t.Fatalf("metadata mapping = %+v", metadata)
	}
	if err := metadata.Validate(); err != nil {
		t.Fatalf("mapped metadata is invalid: %v", err)
	}

	stored, found, err := store.Secrets().SecretCiphertext(ctx, namespace)
	if err != nil || !found {
		t.Fatalf("ciphertext: %v found=%v", err, found)
	}
	if stored.Revision != 1 || stored.KeyID != keyID || stored.State != vault.StateActive ||
		stored.CiphertextSHA256 != firstInput.CiphertextSHA256 ||
		stored.RevisionHash != firstInput.RevisionHash || stored.CreatedAt.IsZero() {
		t.Fatalf("stored ciphertext mapping = %+v", stored)
	}
	plaintext, openedKey, err := ring.Open(stored.Namespace, stored.Revision, stored.Ciphertext)
	if err != nil || openedKey != keyID || string(plaintext) != "first-secret-value" {
		t.Fatalf("open first ciphertext = %q key %q err %v", plaintext, openedKey, err)
	}

	// Rotation appends revision 2 under the locked CAS pointer.
	second, secondKeyID, err := ring.Seal(namespace, 2, []byte("rotated-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	secondInput := vault.SecretRevisionInput{
		Namespace: namespace, Revision: 2, KeyID: secondKeyID, Ciphertext: second,
		CiphertextSHA256: vault.CiphertextSHA256(second),
		RevisionHash:     vault.RevisionHash(namespace, 2, secondKeyID, second),
		Actor:            testActor,
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		secrets := tx.Secrets()
		latest, found, err := secrets.LockSecretLatestRevision(ctx, namespace)
		if err != nil || !found || latest != 1 {
			t.Fatalf("lock latest = %d found=%v err=%v", latest, found, err)
		}
		advanced, err := secrets.AdvanceSecretNamespace(ctx, namespace, 1, 2, testActor)
		if err != nil || !advanced {
			t.Fatalf("advance = %v err %v", advanced, err)
		}
		if err := secrets.InsertSecretRevision(ctx, namespace, secondInput); err != nil {
			t.Fatalf("insert revision two = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A stale CAS writes nothing and leaves the transaction usable; unlike a
	// duplicate insert, a zero-row CAS does not abort PostgreSQL.
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		advanced, err := tx.Secrets().AdvanceSecretNamespace(ctx, namespace, 1, 3, testActor)
		if err != nil {
			return err
		}
		if advanced {
			t.Fatalf("stale advance affected a row")
		}
		latest, found, err := tx.Secrets().LockSecretLatestRevision(ctx, namespace)
		if err != nil || !found || latest != 2 {
			t.Fatalf("statement after stale CAS = %d found=%v err=%v", latest, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	metadata, found, err = store.Secrets().SecretMetadata(ctx, namespace)
	if err != nil || !found || metadata.LatestRevision != 2 || metadata.Revision.Revision != 2 ||
		metadata.Revision.Hash != secondInput.RevisionHash {
		t.Fatalf("rotated metadata = %+v found=%v err=%v", metadata, found, err)
	}
	latest, found, err := store.Secrets().SecretCiphertext(ctx, namespace)
	if err != nil || !found || latest.Revision != 2 {
		t.Fatalf("latest ciphertext = %+v found=%v err=%v", latest, found, err)
	}
	plaintext, _, err = ring.Open(latest.Namespace, latest.Revision, latest.Ciphertext)
	if err != nil || string(plaintext) != "rotated-secret-value" {
		t.Fatalf("open rotated ciphertext = %q err %v", plaintext, err)
	}

	active, err := store.Secrets().ListActiveSecretCiphertexts(ctx, "demo", "staging", 10)
	if err != nil || len(active) != 1 || active[0].Revision != 2 || active[0].Namespace.Name != "API_TOKEN" {
		t.Fatalf("active ciphertexts = %+v err %v", active, err)
	}
	pinned, err := store.Secrets().ListPinnedSecretCiphertexts(ctx, "demo", "staging", vault.RevisionMap{"API_TOKEN": 1})
	if err != nil || len(pinned) != 1 || pinned[0].Revision != 1 {
		t.Fatalf("pinned ciphertexts = %+v err %v", pinned, err)
	}
	pinnedPlain, _, err := ring.Open(pinned[0].Namespace, pinned[0].Revision, pinned[0].Ciphertext)
	if err != nil || string(pinnedPlain) != "first-secret-value" {
		t.Fatalf("open pinned ciphertext = %q err %v", pinnedPlain, err)
	}
	// The AAD binds the namespace, so a ciphertext cannot be moved to another
	// secret even when the key is available.
	moved := pinned[0].Namespace
	moved.Name = "OTHER_TOKEN"
	if _, _, err := ring.Open(moved, 1, pinned[0].Ciphertext); err == nil {
		t.Fatalf("ciphertext opened under another namespace")
	}
	if unset, err := store.Secrets().ListPinnedSecretCiphertexts(ctx, "demo", "staging", vault.RevisionMap{}); err != nil || len(unset) != 0 {
		t.Fatalf("empty pinned read = %+v err %v", unset, err)
	}

	listed, err := store.Secrets().ListSecretMetadata(ctx, "demo", "staging", 10)
	if err != nil || len(listed) != 1 || listed[0].LatestRevision != 2 {
		t.Fatalf("list metadata = %+v err %v", listed, err)
	}

	// Duplicate expected-PK conflicts are typed Conflict errors. PostgreSQL has
	// already aborted the transaction at that point; the next statement must
	// fail with 25P02, and the rejected transaction must not commit its marker.
	duplicateNamespaceMarker := mustOperationID(t)
	duplicateNamespaceKey := entities.OperationKey{
		Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
		IdempotencyKey: "secret-duplicate-namespace",
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: duplicateNamespaceMarker, Key: duplicateNamespaceKey,
			InputHash: "duplicate-namespace", Input: json.RawMessage(`{"steps":1}`),
		})
		if err != nil || !inserted {
			t.Fatalf("duplicate marker insert = %v err %v", inserted, err)
		}
		duplicateErr := tx.Secrets().InsertSecretNamespace(ctx, namespace, testActor)
		if contract.Classify(duplicateErr).Code != contract.Conflict {
			t.Fatalf("duplicate namespace error = %v, want Conflict", duplicateErr)
		}
		if _, _, afterErr := tx.Secrets().LockSecretLatestRevision(ctx, namespace); !isPostgresCode(afterErr, "25P02") {
			t.Fatalf("statement after duplicate namespace = %v, want SQLSTATE 25P02", afterErr)
		}
		return duplicateErr
	}); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("duplicate namespace transaction error = %v", err)
	}
	if _, found, err := store.Operations().Get(ctx, duplicateNamespaceMarker); err != nil || found {
		t.Fatalf("duplicate namespace marker found=%v err=%v", found, err)
	}

	duplicateRevisionMarker := mustOperationID(t)
	duplicateRevisionKey := entities.OperationKey{
		Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
		IdempotencyKey: "secret-duplicate-revision",
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: duplicateRevisionMarker, Key: duplicateRevisionKey,
			InputHash: "duplicate-revision", Input: json.RawMessage(`{"steps":1}`),
		})
		if err != nil || !inserted {
			t.Fatalf("duplicate revision marker insert = %v err %v", inserted, err)
		}
		duplicateErr := tx.Secrets().InsertSecretRevision(ctx, namespace, firstInput)
		if contract.Classify(duplicateErr).Code != contract.Conflict {
			t.Fatalf("duplicate revision error = %v, want Conflict", duplicateErr)
		}
		if _, _, afterErr := tx.Secrets().LockSecretLatestRevision(ctx, namespace); !isPostgresCode(afterErr, "25P02") {
			t.Fatalf("statement after duplicate revision = %v, want SQLSTATE 25P02", afterErr)
		}
		return duplicateErr
	}); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("duplicate revision transaction error = %v", err)
	}
	if _, found, err := store.Operations().Get(ctx, duplicateRevisionMarker); err != nil || found {
		t.Fatalf("duplicate revision marker found=%v err=%v", found, err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		locked, found, err := tx.Secrets().LockSecretNamespace(ctx, namespace)
		if err != nil || !found || locked.LatestRevision != 2 || locked.State != vault.StateActive {
			t.Fatalf("locked namespace = %+v found=%v err=%v", locked, found, err)
		}
		lockedMetadata, found, err := tx.Secrets().LockSecretMetadata(ctx, namespace)
		if err != nil || !found || lockedMetadata.LatestRevision != 2 {
			t.Fatalf("locked metadata = %+v found=%v err=%v", lockedMetadata, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Archive is a required CAS guard and keeps the revisions. It runs through
	// the standalone SecretTransactions port, which owns the short transaction.
	if err := store.Secrets().WithinSecretTx(ctx, func(tx vault.SecretTx) error {
		archived, err := tx.ArchiveSecretNamespace(ctx, namespace, 2, testActor)
		if err != nil {
			return err
		}
		if !archived {
			t.Fatalf("archive affected no row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	archived, found, err := store.Secrets().SecretMetadata(ctx, namespace)
	if err != nil || !found || archived.State != vault.StateArchived || archived.ArchivedAt == nil {
		t.Fatalf("archived metadata = %+v found=%v err=%v", archived, found, err)
	}
	active, err = store.Secrets().ListActiveSecretCiphertexts(ctx, "demo", "staging", 10)
	if err != nil || len(active) != 0 {
		t.Fatalf("active after archive = %+v err %v", active, err)
	}
	pinned, err = store.Secrets().ListPinnedSecretCiphertexts(ctx, "demo", "staging", vault.RevisionMap{"API_TOKEN": 1})
	if err != nil || len(pinned) != 1 || pinned[0].State != vault.StateArchived {
		t.Fatalf("archived revision read = %+v err %v", pinned, err)
	}

	if _, found, err := store.Secrets().SecretMetadata(ctx, vault.Namespace{App: "demo", Environment: "staging", Name: "MISSING"}); err != nil || found {
		t.Fatalf("missing metadata = found %v err %v", found, err)
	}
	if _, found, err := store.Secrets().SecretCiphertext(ctx, vault.Namespace{App: "demo", Environment: "staging", Name: "MISSING"}); err != nil || found {
		t.Fatalf("missing ciphertext = found %v err %v", found, err)
	}

	// A caller-supplied revision above the signed bigint range is rejected
	// before any SQL runs.
	tooLarge := vault.RevisionMap{"API_TOKEN": 1<<63 + 1}
	if _, err := store.Secrets().ListPinnedSecretCiphertexts(ctx, "demo", "staging", tooLarge); err == nil ||
		!strings.Contains(err.Error(), "database range") {
		t.Fatalf("out-of-range pinned revision error = %v", err)
	}
}
