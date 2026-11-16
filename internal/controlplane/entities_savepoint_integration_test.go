package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/riverqueue/river"
)

// TestIntegrationEntitiesSavepointPoisonRejectsCommit proves the shared poison
// survives a containing savepoint's successful rollback and forces the root
// transaction to refuse commit without turning the failure into a CommitError.
func TestIntegrationEntitiesSavepointPoisonRejectsCommit(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	existing := vault.Namespace{App: "demo", Environment: "staging", Name: "EXISTING_TOKEN"}
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	existingCiphertext, existingKeyID, err := ring.Seal(existing, 1, []byte("existing-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Secrets().WithinSecretTx(ctx, func(tx vault.SecretTx) error {
		if err := tx.InsertSecretNamespace(ctx, existing, testActor); err != nil {
			return err
		}
		return tx.InsertSecretRevision(ctx, existing, vault.SecretRevisionInput{
			Namespace: existing, Revision: 1, KeyID: existingKeyID, Ciphertext: existingCiphertext,
			CiphertextSHA256: vault.CiphertextSHA256(existingCiphertext),
			RevisionHash:     vault.RevisionHash(existing, 1, existingKeyID, existingCiphertext),
			Actor:            testActor,
		})
	}); err != nil {
		t.Fatal(err)
	}

	marker := mustOperationID(t)
	checkSavepointPoisonRejectsCommit(t, ctx, store, existing, marker)
}

func checkSavepointPoisonRejectsCommit(t *testing.T, ctx context.Context, store *entities.Store, existing vault.Namespace, marker contract.OperationID) {
	t.Helper()
	markerKey := entities.OperationKey{
		Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
		IdempotencyKey: "savepoint-poison-marker",
	}
	outerSentinel := errors.New("outer savepoint sentinel")
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: marker, Key: markerKey, InputHash: "poison-marker",
			Input: json.RawMessage(`{"steps":1}`),
		})
		if err != nil || !inserted {
			t.Fatalf("marker insert = %v err %v", inserted, err)
		}
		outerErr := runPoisonedSavepoints(t, ctx, tx, existing, outerSentinel)
		if !errors.Is(outerErr, outerSentinel) {
			t.Fatalf("outer savepoint error = %v, want the identical sentinel", outerErr)
		}
		// ROLLBACK TO SAVEPOINT S1 restored PostgreSQL usability.
		if err := tx.Operations().SetJob(ctx, marker, 99); err != nil {
			t.Fatalf("statement after outer rollback = %v", err)
		}
		// Ignore the outer sentinel; the root must still reject the stored poison.
		return nil
	})
	var savepointErr *entities.SavepointError
	if !errors.As(err, &savepointErr) || savepointErr.Operation() != entities.SavepointRelease {
		t.Fatalf("root error = %v, want stored release SavepointError", err)
	}
	if entities.IsCommitFailure(err) {
		t.Fatalf("poison was reported as a commit failure")
	}
	if _, found, err := store.Operations().Get(ctx, marker); err != nil || found {
		t.Fatalf("poison marker found=%v err=%v", found, err)
	}
	if _, found, err := store.Secrets().SecretMetadata(ctx, existing); err != nil || !found {
		t.Fatalf("existing secret found=%v err=%v", found, err)
	}
}

func runPoisonedSavepoints(t *testing.T, ctx context.Context, tx *entities.StoreTx, existing vault.Namespace, outerSentinel error) error {
	t.Helper()
	return tx.WithinSavepoint(ctx, func(outer *entities.StoreTx) error {
		innerErr := outer.WithinSavepoint(ctx, func(inner *entities.StoreTx) error {
			duplicateErr := inner.Secrets().InsertSecretNamespace(ctx, existing, testActor)
			if contract.Classify(duplicateErr).Code != contract.Conflict {
				t.Fatalf("inner duplicate error = %v, want Conflict", duplicateErr)
			}
			// PostgreSQL has aborted the transaction. Deliberately ignore
			// the error and return nil so the release below must fail.
			return nil
		})
		var savepointErr *entities.SavepointError
		if !errors.As(innerErr, &savepointErr) || savepointErr.Operation() != entities.SavepointRelease {
			t.Fatalf("inner savepoint error = %v, want release SavepointError", innerErr)
		}
		return outerSentinel
	})
}

// TestIntegrationEntitiesSavepointCallbackIdentityAndPanicWrite proves a
// successful scope rollback returns the callback value unchanged and that a
// write before a panic is rolled back while the outer transaction commits.
func TestIntegrationEntitiesSavepointCallbackIdentityAndPanicWrite(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sentinel := errors.New("scope rollback sentinel")
	markerA := mustOperationID(t)
	markerB := mustOperationID(t)
	phantom := mustOperationID(t)
	markerC := mustOperationID(t)
	input := json.RawMessage(`{"steps":1}`)

	checkSavepointCallbackIdentity(t, ctx, store, markerA, markerB, markerC, phantom, sentinel, input)
}

func checkSavepointCallbackIdentity(
	t *testing.T,
	ctx context.Context,
	store *entities.Store,
	markerA, markerB, markerC, phantom contract.OperationID,
	sentinel error,
	input json.RawMessage,
) {
	t.Helper()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := insertSavepointOperation(t, ctx, tx, markerA, input); err != nil {
			return err
		}
		scopeErr := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
			if err := insertSavepointOperation(t, ctx, nested, markerB, input); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(scopeErr, sentinel) {
			t.Fatalf("scope rollback error = %v, want the identical sentinel", scopeErr)
		}
		expectSavepointPanic(t, ctx, tx, phantom, input)
		return insertSavepointOperation(t, ctx, tx, markerC, input)
	}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []contract.OperationID{markerA, markerC} {
		if _, found, err := store.Operations().Get(ctx, id); err != nil || !found {
			t.Fatalf("committed marker %s found=%v err=%v", id, found, err)
		}
	}
	for _, id := range []contract.OperationID{markerB, phantom} {
		if _, found, err := store.Operations().Get(ctx, id); err != nil || found {
			t.Fatalf("rolled-back marker %s found=%v err=%v", id, found, err)
		}
	}
}

func savepointOperationKey(id contract.OperationID) entities.OperationKey {
	return entities.OperationKey{
		Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
		IdempotencyKey: "savepoint-" + string(id),
	}
}

func insertSavepointOperation(t *testing.T, ctx context.Context, tx *entities.StoreTx, id contract.OperationID, input json.RawMessage) error {
	t.Helper()
	inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
		ID: id, Key: savepointOperationKey(id), InputHash: "identity-hash", Input: input,
	})
	if err != nil {
		return err
	}
	if !inserted {
		t.Fatalf("operation %s was not inserted", id)
	}
	return nil
}

func expectSavepointPanic(t *testing.T, ctx context.Context, tx *entities.StoreTx, phantom contract.OperationID, input json.RawMessage) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatalf("savepoint panic did not propagate")
		}
	}()
	_ = tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
		if err := insertSavepointOperation(t, ctx, nested, phantom, input); err != nil {
			return err
		}
		panic("savepoint write panic")
	})
}

// TestIntegrationEntitiesSavepointAtomicSecretOperationRiverRollback proves a
// speculative operation, event, sealed secret revision and real River job are
// discarded together at savepoint scope while the outer transaction continues.
func TestIntegrationEntitiesSavepointAtomicSecretOperationRiverRollback(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := store.NewJobRuntime(&river.Config{}); err != nil {
		t.Fatal(err)
	}
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}

	speculative := mustOperationID(t)
	accepted := mustOperationID(t)
	namespace := vault.Namespace{App: "demo", Environment: "staging", Name: "SPECULATIVE_TOKEN"}
	ciphertext, keyID, err := ring.Seal(namespace, 1, []byte("speculative-secret"))
	if err != nil {
		t.Fatal(err)
	}
	revision := vault.SecretRevisionInput{
		Namespace: namespace, Revision: 1, KeyID: keyID, Ciphertext: ciphertext,
		CiphertextSHA256: vault.CiphertextSHA256(ciphertext),
		RevisionHash:     vault.RevisionHash(namespace, 1, keyID, ciphertext),
		Actor:            testActor,
	}
	input := json.RawMessage(`{"steps":1}`)
	discard := errors.New("discard speculative savepoint")

	runAtomicSecretSavepoint(t, ctx, store, accepted, speculative, namespace, revision, input, discard)
	checkAtomicSecretRolledBack(t, ctx, pool, store, accepted, speculative, namespace)
}

func runAtomicSecretSavepoint(
	t *testing.T,
	ctx context.Context,
	store *entities.Store,
	accepted, speculative contract.OperationID,
	namespace vault.Namespace,
	revision vault.SecretRevisionInput,
	input json.RawMessage,
	discard error,
) {
	t.Helper()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: accepted,
			Key: entities.OperationKey{
				Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
				IdempotencyKey: "savepoint-accepted",
			},
			InputHash: "accepted", Input: input,
		})
		if err != nil || !inserted {
			t.Fatalf("accepted insert = %v err %v", inserted, err)
		}
		scopeErr := runSpeculativeSecretSavepoint(t, ctx, tx, speculative, namespace, revision, input, discard)
		if !errors.Is(scopeErr, discard) {
			t.Fatalf("savepoint error = %v, want the identical sentinel", scopeErr)
		}
		// The savepoint rollback leaves the outer transaction usable and all
		// speculative writes invisible.
		if _, found, err := tx.Operations().Lock(ctx, speculative); err != nil || found {
			t.Fatalf("speculative operation visible found=%v err=%v", found, err)
		}
		if _, found, err := tx.Secrets().SecretMetadata(ctx, namespace); err != nil || found {
			t.Fatalf("speculative secret visible found=%v err=%v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func runSpeculativeSecretSavepoint(
	t *testing.T,
	ctx context.Context,
	tx *entities.StoreTx,
	speculative contract.OperationID,
	namespace vault.Namespace,
	revision vault.SecretRevisionInput,
	input json.RawMessage,
	discard error,
) error {
	t.Helper()
	return tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
		inserted, err := nested.Operations().Insert(ctx, entities.OperationInsert{
			ID: speculative,
			Key: entities.OperationKey{
				Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
				IdempotencyKey: "savepoint-speculative",
			},
			InputHash: "speculative", Input: input,
		})
		if err != nil || !inserted {
			t.Fatalf("speculative insert = %v err %v", inserted, err)
		}
		op, found, err := nested.Operations().Lock(ctx, speculative)
		if err != nil || !found {
			t.Fatalf("speculative lock found=%v err=%v", found, err)
		}
		if err := nested.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "step", Stage: "speculative", Message: "speculative event",
		}); err != nil {
			return err
		}
		if err := nested.Secrets().InsertSecretNamespace(ctx, namespace, testActor); err != nil {
			return err
		}
		if err := nested.Secrets().InsertSecretRevision(ctx, namespace, revision); err != nil {
			return err
		}
		if _, err := nested.Jobs().EnqueueDiagnostic(ctx, speculative); err != nil {
			return err
		}
		return discard
	})
}

func checkAtomicSecretRolledBack(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *entities.Store, accepted, speculative contract.OperationID, namespace vault.Namespace) {
	t.Helper()
	if _, found, err := store.Operations().Get(ctx, speculative); err != nil || found {
		t.Fatalf("speculative operation after commit found=%v err=%v", found, err)
	}
	if _, found, err := store.Secrets().SecretMetadata(ctx, namespace); err != nil || found {
		t.Fatalf("speculative secret after commit found=%v err=%v", found, err)
	}
	var events, jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1`, speculative).Scan(&events); err != nil || events != 0 {
		t.Fatalf("speculative events = %d err %v", events, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("speculative River jobs = %d err %v", jobs, err)
	}
	if _, found, err := store.Operations().Get(ctx, accepted); err != nil || !found {
		t.Fatalf("accepted operation after commit found=%v err=%v", found, err)
	}
}

// TestIntegrationEntitiesSavepointNilCallbackPoison proves a containing
// savepoint whose callback returns nil while a nested scope is poisoned rolls
// its own scope back instead of releasing it, and the root still refuses commit.
func TestIntegrationEntitiesSavepointNilCallbackPoison(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	existing := vault.Namespace{App: "demo", Environment: "staging", Name: "NIL_CALLBACK_TOKEN"}
	if err := store.Secrets().WithinSecretTx(ctx, func(tx vault.SecretTx) error {
		return tx.InsertSecretNamespace(ctx, existing, testActor)
	}); err != nil {
		t.Fatal(err)
	}

	marker := mustOperationID(t)
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: marker,
			Key: entities.OperationKey{
				Kind: "diagnostic.test", Target: "control-plane", Actor: testActor,
				IdempotencyKey: "savepoint-nil-callback",
			},
			InputHash: "nil-callback", Input: json.RawMessage(`{"steps":1}`),
		})
		if err != nil || !inserted {
			t.Fatalf("marker insert = %v err %v", inserted, err)
		}
		outerErr := tx.WithinSavepoint(ctx, func(outer *entities.StoreTx) error {
			innerErr := outer.WithinSavepoint(ctx, func(inner *entities.StoreTx) error {
				duplicateErr := inner.Secrets().InsertSecretNamespace(ctx, existing, testActor)
				if contract.Classify(duplicateErr).Code != contract.Conflict {
					t.Fatalf("inner duplicate error = %v, want Conflict", duplicateErr)
				}
				return nil
			})
			var savepointErr *entities.SavepointError
			if !errors.As(innerErr, &savepointErr) || savepointErr.Operation() != entities.SavepointRelease {
				t.Fatalf("inner savepoint error = %v, want release SavepointError", innerErr)
			}
			// The containing callback deliberately returns nil. The poison
			// must force a scope rollback, not a release.
			return nil
		})
		var savepointErr *entities.SavepointError
		if !errors.As(outerErr, &savepointErr) || savepointErr.Operation() != entities.SavepointRelease {
			t.Fatalf("outer savepoint error = %v, want stored release SavepointError", outerErr)
		}
		return nil
	})
	var savepointErr *entities.SavepointError
	if !errors.As(err, &savepointErr) || savepointErr.Operation() != entities.SavepointRelease {
		t.Fatalf("root error = %v, want stored release SavepointError", err)
	}
	if _, found, err := store.Operations().Get(ctx, marker); err != nil || found {
		t.Fatalf("nil-callback marker found=%v err=%v", found, err)
	}
}
