package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// This test uses testPool's dedicated database and must only run on an
// explicitly authorized EREGION_TEST_DATABASE_URL.
func TestIntegrationPointerValidationAndDeletionFence(t *testing.T) {
	pool, _, _ := definitionIntegrationFixture(t)
	ctx := context.Background()
	var envID platform.EnvironmentID
	var revision platform.RevisionID
	if err := pool.QueryRow(ctx, `SELECT id,current_revision_id FROM eregion_platform_environments WHERE name='production'`).Scan(&envID, &revision); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	app, err := lockPointerEnvironmentTx(ctx, tx, envID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockRevisionPointerTx(ctx, tx, envID, revision); err != nil {
		t.Fatal(err)
	}
	if err := lockRevisionPointerTx(ctx, tx, "env_ffffffffffffffffffffffffffffffff", revision); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("cross-environment revision: %v", err)
	}
	if err := lockReleasePointerTx(ctx, tx, app, envID, "rel_ffffffffffffffffffffffffffffffff"); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("missing release: %v", err)
	}
	if err := lockServingPointersTx(ctx, tx, envID, "rel_ffffffffffffffffffffffffffffffff", "dep_ffffffffffffffffffffffffffffffff"); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("missing serving target: %v", err)
	}
	if err := setReleaseBuildPointerTx(ctx, tx, envID, "rel_ffffffffffffffffffffffffffffffff", "bld_ffffffffffffffffffffffffffffffff"); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("missing release/build target: %v", err)
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_, err = pool.Exec(deleteCtx, `DELETE FROM eregion_platform_environments WHERE id=$1`, envID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deletion did not wait for the pointer transaction: %v", err)
	}
}
