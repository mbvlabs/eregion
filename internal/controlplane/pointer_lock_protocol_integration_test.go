package controlplane

import (
	"context"
	"testing"
	"time"
)

// Uses testPool through the existing fixture; never run without an explicitly
// authorized isolated EREGION_TEST_DATABASE_URL.
func TestIntegrationPointerKeyShareUpdateAndDeleteFence(t *testing.T) {
	f := newDeploymentRetryFixture(t)
	release := f.createRelease("key-share", "registry.example/live:base")
	env := f.environment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	appID, err := lockPointerEnvironmentTx(ctx, tx, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockReleasePointerTx(ctx, tx, appID, env.ID, release); err != nil {
		t.Fatal(err)
	}
	update := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(ctx, `UPDATE eregion_platform_releases SET error='test' WHERE id=$1`, release)
		update <- err
	}()
	select {
	case err := <-update:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("non-key release update blocked behind key-share")
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(ctx, `DELETE FROM eregion_platform_releases WHERE id=$1 /* pointer-delete-barrier */`, release)
		deleted <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%pointer-delete-barrier%' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-deleted:
			t.Fatalf("delete crossed key-share fence: %v", err)
		case <-ctx.Done():
			t.Fatal("delete did not reach lock barrier")
		case <-ticker.C:
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete after release: %v", err)
	}
}
