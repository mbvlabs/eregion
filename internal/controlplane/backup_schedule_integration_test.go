package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/resources"
)

// TestIntegrationDeclaredBackupScheduleRunsRetainsAndVerifies covers the
// platform.backup.schedule declaration that used to be written and never read:
// a tick submits exactly one backup for the window, a completed archive with
// VerifyAfter set submits one verification, and retention prunes the old archive.
func TestIntegrationDeclaredBackupScheduleRunsRetainsAndVerifies(t *testing.T) {
	pool, dsn := testPool(t, true)
	nodeDir := t.TempDir()
	cp := startControlPlane(t, dsn, writeKeyFile(t), nodeDir, 1, true)
	s := cp.run.Service
	ctx := context.Background()
	backupDir := filepath.Join(nodeDir, "backups")

	spec := resources.Spec{
		Name: "appdb", Engine: resources.EnginePostgreSQL, Version: "17",
		Host: "node-a", Database: "app", Username: "app",
	}
	oldArchive, oldManifest := writeBackupFixture(t, backupDir, spec, "op_old_backup", time.Now().UTC().Add(-10*24*time.Hour))
	policy := resources.Policy{
		Schedule:    resources.Schedule{EverySeconds: 3600},
		Retention:   resources.Retention{KeepLast: 1},
		VerifyAfter: true,
	}
	declaration := platformSubmit(t, s, resources.KindBackupSchedule, "node/node-a", "schedule-declaration",
		resources.BackupScheduleInput{Spec: spec, Policy: policy})
	platformAwait(t, s, declaration, contract.Succeeded)

	// One tick at a time after the declaration is due submits one backup.
	accepted, err := s.ScheduleBackups(ctx, time.Now().UTC().Add(2*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 1 {
		t.Fatalf("scheduler accepted %d backups, want exactly one", len(accepted))
	}
	backup := accepted[0]
	if backup.Kind != resources.KindBackupCreate || backup.Actor.Subject != scheduledBackupActor.Subject {
		t.Fatalf("scheduler submitted %s as %+v", backup.Kind, backup.Actor)
	}
	// The fake host cannot dump; the durable submission is what this test
	// observes. The backup belongs to the system actor, not the submitting test
	// actor. Mark it succeeded so the verify-after phase can see an archive.
	await(t, 60*time.Second, func() bool {
		current, getErr := s.Get(ctx, scheduledBackupActor, backup.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		backup = current
		return current.State.Terminal()
	})
	if backup.State != contract.Failed {
		t.Fatalf("backup state = %s, want failed on the fake host", backup.State)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded', error_code='' WHERE id=$1`, backup.ID); err != nil {
		t.Fatal(err)
	}
	newArchive, newManifest := writeBackupFixtureEffect(t, backupDir, spec, string(backup.ID), time.Now().UTC())

	// A second tick must not create another backup for the same window; it
	// submits the verification for the completed archive and prunes the old one.
	if _, err := s.ScheduleBackups(ctx, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var backups, verifies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1 AND actor_subject=$2`,
		resources.KindBackupCreate, scheduledBackupActor.Subject).Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if backups != 1 {
		t.Fatalf("duplicate backups for one window: %d", backups)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1 AND actor_subject=$2`,
		resources.KindBackupVerify, scheduledBackupActor.Subject).Scan(&verifies); err != nil {
		t.Fatal(err)
	}
	if verifies != 1 {
		t.Fatalf("verify-after submitted %d verifications, want one", verifies)
	}
	if _, err := os.Stat(oldArchive); !os.IsNotExist(err) {
		t.Fatal("retention did not prune the expired archive")
	}
	if _, err := os.Stat(oldManifest); !os.IsNotExist(err) {
		t.Fatal("retention did not prune the expired manifest")
	}
	if _, err := os.Stat(newArchive); err != nil {
		t.Fatal("retention pruned the retained archive")
	}
	if _, err := os.Stat(newManifest); err != nil {
		t.Fatal("retention pruned the retained manifest")
	}
}

func writeBackupFixture(t *testing.T, dir string, spec resources.Spec, effectID string, createdAt time.Time) (string, string) {
	t.Helper()
	return writeBackupFixtureEffect(t, dir, spec, effectID, createdAt)
}

func writeBackupFixtureEffect(t *testing.T, dir string, spec resources.Spec, effectID string, createdAt time.Time) (string, string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := spec.Name + "-" + effectID + ".dump"
	archive := filepath.Join(dir, base)
	if err := os.WriteFile(archive, []byte("fixture archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := archive + ".manifest.json"
	document := map[string]any{
		"schema": "eregion/backup-manifest/v1", "resource": spec.Name, "host": spec.Host,
		"engine": spec.Engine, "version": spec.Version, "database": spec.Database,
		"format": "pg_dump-custom", "archive": base, "effect_id": effectID,
		"size_bytes": 16, "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"reason": "scheduled", "created_at": createdAt.UTC(),
	}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(manifest, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive, manifest
}
