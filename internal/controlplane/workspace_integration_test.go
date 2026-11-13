package controlplane

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestIntegrationWorkspaceAssignmentIsSingletonIdempotentAndImmutable(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()

	unconfigured, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured.Configured || unconfigured.ID != "" || unconfigured.Name != "" {
		t.Fatalf("a migrated installation started configured: %+v", unconfigured)
	}

	assigned, err := AssignWorkspace(ctx, pool, "thelab", testRegistryHostIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if !assigned.Configured || assigned.Name != "thelab" {
		t.Fatalf("assigned identity: %+v", assigned)
	}
	if err := ValidateWorkspaceID(assigned.ID); err != nil {
		t.Fatal(err)
	}
	replayed, err := AssignWorkspace(ctx, pool, "thelab", testRegistryHostIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != assigned.ID || replayed.Name != assigned.Name {
		t.Fatalf("same-name replay changed the identity: %+v -> %+v", assigned, replayed)
	}
	if _, err := AssignWorkspace(ctx, pool, "otherlab", testRegistryHostIdentity()); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting assignment = %v, want conflict", err)
	}
	read, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, assigned) {
		t.Fatalf("read identity = %+v, want %+v", read, assigned)
	}

	otherHost := testRegistryHostIdentity()
	otherHost.HostKeyFingerprint = "SHA256:" + strings.Repeat("B", 43)
	if _, err := AssignWorkspace(ctx, pool, "thelab", otherHost); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("different registry host assignment = %v, want conflict", err)
	}
	read, err = ReadWorkspace(ctx, pool)
	if err != nil || !reflect.DeepEqual(read, assigned) {
		t.Fatalf("identity after conflicting assignment = %+v, err %v; want %+v", read, err, assigned)
	}
}

func TestIntegrationWorkspaceRegistryHostBindingIsExactAndNeverBackfilled(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	identity := testRegistryHostIdentity()
	if _, err := AssignWorkspace(ctx, pool, "thelab", identity); err != nil {
		t.Fatal(err)
	}
	other := identity
	other.HostKeyFingerprint = "SHA256:" + strings.Repeat("B", 43)
	if _, err := AssignWorkspace(ctx, pool, "thelab", other); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("different fingerprint replay = %v, want conflict", err)
	}
	if _, err := VerifyWorkspaceRegistryHost(ctx, pool, other); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("different local identity = %v, want conflict", err)
	}
	if _, err := VerifyWorkspaceRegistryHost(ctx, pool, identity); err != nil {
		t.Fatalf("exact local identity was refused: %v", err)
	}

	unboundPool, _ := testPool(t, true)
	if _, err := unboundPool.Exec(ctx, `INSERT INTO eregion_workspace (singleton, workspace_id, name, created_at)
        VALUES (true,'ws_0123456789abcdef01234567','thelab',clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := AssignWorkspace(ctx, unboundPool, "thelab", identity); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unverified workspace assignment = %v, want precondition_failed", err)
	}
	if _, err := VerifyWorkspaceRegistryHost(ctx, unboundPool, identity); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unverified workspace verification = %v, want precondition_failed", err)
	}
}

func TestIntegrationConcurrentWorkspaceAssignmentAgrees(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	const workers = 8
	results := make([]WorkspaceIdentity, workers)
	errs := make([]error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = AssignWorkspace(ctx, pool, "thelab", testRegistryHostIdentity())
		}(i)
	}
	wait.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if results[i].ID != results[0].ID || results[i].Name != "thelab" {
			t.Fatalf("worker %d got %+v, want %+v", i, results[i], results[0])
		}
	}
}

func TestIntegrationConcurrentWorkspaceAssignmentWithDifferentNamesHasOneWinner(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	names := []string{"alpha", "beta"}
	results := make([]WorkspaceIdentity, len(names))
	errs := make([]error, len(names))
	var wait sync.WaitGroup
	for i := range names {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = AssignWorkspace(ctx, pool, names[index], testRegistryHostIdentity())
		}(i)
	}
	wait.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("two assignments won: %d and %d", winner, i)
			}
			winner = i
		case contract.Classify(err).Code != contract.Conflict:
			t.Fatalf("loser %d = %v, want conflict", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no assignment won")
	}
	stored, err := ReadWorkspace(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != names[winner] || !stored.Configured {
		t.Fatalf("stored workspace = %+v, want %q", stored, names[winner])
	}
}

func TestIntegrationWorkspaceReadBeforeMigrationDoesNotInitialize(t *testing.T) {
	pool, _ := testPool(t, false)
	identity, err := ReadWorkspace(context.Background(), pool)
	if !errors.Is(err, ErrWorkspaceSchemaMissing) {
		t.Fatalf("missing schema error = %v", err)
	}
	if identity.Configured {
		t.Fatalf("a pre-migration database was reported as configured: %+v", identity)
	}
	var table *string
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass('eregion_workspace')::text").Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != nil {
		t.Fatalf("a read created the workspace table: %v", *table)
	}
	if _, err := AssignWorkspace(context.Background(), pool, "thelab", testRegistryHostIdentity()); err == nil {
		t.Fatal("assignment succeeded without the application schema")
	}
}

func TestIntegrationAssignWorkspaceRejectsAnInvalidNameBeforeDatabaseAccess(t *testing.T) {
	pool, _ := testPool(t, false)
	_, err := AssignWorkspace(context.Background(), pool, "The Lab", testRegistryHostIdentity())
	if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("invalid name = %v, want validation_failed", err)
	}
}
