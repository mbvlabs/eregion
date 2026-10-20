package resources

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func readyService(t *testing.T, spec Spec) (*Service, *fakeHost) {
	t.Helper()
	service, fake := newTestService(t, spec)
	fake.container, fake.running = true, true
	fake.databases[spec.Database] = true
	if _, err := service.ensureCredential(spec); err != nil {
		t.Fatal(err)
	}
	return service, fake
}

func TestMutatingActionRequiresStableEffectIdentity(t *testing.T) {
	service, fake := newTestService(t, testSpec())
	_, err := service.Execute(context.Background(), Request{Action: ActionCreate, Spec: testSpec()})
	if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("mutating action without effect identity was accepted: %v", err)
	}
	if len(fake.callArgs()) != 0 {
		t.Fatal("effect validation ran an external command")
	}
}

func TestRunnerNodeIdentityIsPinnedAtTheCodeBoundary(t *testing.T) {
	spec := testSpec()
	fake := newFakeHost(spec)
	fake.nodeIdentity = "other-node"
	_, err := New(Config{Host: spec.Host, StateDir: t.TempDir(), BackupDir: t.TempDir(), Runner: fake})
	if err == nil {
		t.Fatal("service accepted a host that the runner does not own")
	}
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unexpected identity failure: %v", err)
	}
}

func TestCreateAndDestroyRetriesReplayCompletedEvidence(t *testing.T) {
	spec := testSpec()
	service := readyServiceForCreate(t, spec)
	createRequest := Request{Action: ActionCreate, Spec: spec, OperationID: "op-create-once"}
	created, err := service.Execute(context.Background(), createRequest)
	if err != nil {
		t.Fatal(err)
	}
	calls := len(service.runner.(*fakeHost).callArgs())
	replayed, err := service.Execute(context.Background(), createRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.OperationID != "op-create-once" || replayed.EffectID != "op-create-once" {
		t.Fatalf("create retry did not propagate/replay identity: %+v", replayed)
	}
	if calls != len(service.runner.(*fakeHost).callArgs()) {
		t.Fatal("create retry reapplied Docker commands")
	}

	destroyRequest := Request{
		Action: ActionDestroy, Spec: spec, OperationID: "op-destroy-once",
		Destroy: &DestroyRequest{Confirmation: spec.Name, RemoveVolume: true},
	}
	destroyed, err := service.Execute(context.Background(), destroyRequest)
	if err != nil {
		t.Fatal(err)
	}
	if destroyed.State != StateAbsent {
		t.Fatalf("unexpected destroy result: %+v", destroyed)
	}
	calls = len(service.runner.(*fakeHost).callArgs())
	destroyReplay, err := service.Execute(context.Background(), destroyRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !destroyReplay.Replayed || destroyReplay.State != StateAbsent {
		t.Fatalf("destroy retry was not replayed: %+v", destroyReplay)
	}
	if calls != len(service.runner.(*fakeHost).callArgs()) {
		t.Fatal("destroy retry reapplied Docker commands")
	}
	if created.EffectID != "op-create-once" {
		t.Fatalf("create result omitted effect id: %+v", created)
	}
}

func readyServiceForCreate(t *testing.T, spec Spec) *Service {
	t.Helper()
	service, _ := newTestService(t, spec)
	return service
}

func TestBackupRetryReusesDeterministicArchive(t *testing.T) {
	spec := testSpec()
	service, fake := readyService(t, spec)
	request := Request{Action: ActionBackup, Spec: spec, OperationID: "op-backup-once", Backup: &BackupRequest{Reason: "manual"}}
	backup, err := service.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backup.Backup.Archive, shortEffectID("op-backup-once")) {
		t.Fatalf("backup path is not deterministic for the effect: %s", backup.Backup.Archive)
	}
	assertNoNewDump := func() {
		t.Helper()
		before := countPgDump(fake.callArgs())
		replayed, err := service.Execute(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if !replayed.Replayed || replayed.Backup == nil || replayed.Backup.Archive != backup.Backup.Archive {
			t.Fatalf("backup retry did not reuse evidence: %+v", replayed)
		}
		if after := countPgDump(fake.callArgs()); after != before {
			t.Fatalf("backup retry ran pg_dump again: %d -> %d", before, after)
		}
	}
	assertNoNewDump()

	// Simulate a lost/corrupt effect record after the archive was durably
	// published. The deterministic path and manifest still prevent a duplicate
	// or a second dump.
	if err := removeEffectForTest(service, "op-backup-once"); err != nil {
		t.Fatal(err)
	}
	before := countPgDump(fake.callArgs())
	recovered, err := service.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Replayed || recovered.Backup == nil || recovered.Backup.Archive != backup.Backup.Archive {
		t.Fatalf("deterministic archive was not reused: %+v", recovered)
	}
	if after := countPgDump(fake.callArgs()); after != before {
		t.Fatal("lost effect evidence caused a second pg_dump")
	}
}

func TestRestoreRetryNeverRepeatsCutover(t *testing.T) {
	spec := testSpec()
	service, fake := readyService(t, spec)
	backupRequest := Request{Action: ActionBackup, Spec: spec, OperationID: "op-backup-for-restore"}
	backup, err := service.Execute(context.Background(), backupRequest)
	if err != nil {
		t.Fatal(err)
	}
	restoreRequest := Request{
		Action: ActionRestore, Spec: spec, OperationID: "op-restore-once",
		Restore: &RestoreRequest{Archive: backup.Backup.Archive, Confirmation: spec.Name},
	}
	restored, err := service.Execute(context.Background(), restoreRequest)
	if err != nil {
		t.Fatal(err)
	}
	psqlBefore := countPsql(fake.callArgs())
	replayed, err := service.Execute(context.Background(), restoreRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.SafetyBackup == nil || restored.SafetyBackup == nil ||
		replayed.SafetyBackup.Archive != restored.SafetyBackup.Archive {
		t.Fatalf("restore retry did not return completed evidence: %+v", replayed)
	}
	if after := countPsql(fake.callArgs()); after != psqlBefore {
		t.Fatalf("restore retry ran cutover commands: %d -> %d", psqlBefore, after)
	}

	// Simulate a crash after durable cutover-complete evidence but before the
	// final completed acknowledgement. A retry must finalize, not reapply.
	record, exists, err := service.readEffect("op-restore-once")
	if err != nil || !exists {
		t.Fatalf("missing restore effect: exists=%v err=%v", exists, err)
	}
	record.Status = effectStarted
	record.Phase = "cutover-complete"
	if err := service.writeEffect(record); err != nil {
		t.Fatal(err)
	}
	psqlBefore = countPsql(fake.callArgs())
	cutoverReplay, err := service.Execute(context.Background(), restoreRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !cutoverReplay.Replayed {
		t.Fatalf("cutover evidence retry was not marked replayed: %+v", cutoverReplay)
	}
	if after := countPsql(fake.callArgs()); after != psqlBefore {
		t.Fatalf("cutover evidence retry repeated database cutover: %d -> %d", psqlBefore, after)
	}
}

func TestEffectIdReuseWithDifferentPayloadIsRefused(t *testing.T) {
	spec := testSpec()
	service := readyServiceForCreate(t, spec)
	request := Request{Action: ActionCreate, Spec: spec, OperationID: "op-fixed"}
	if _, err := service.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Spec.Database = "other"
	_, err := service.Execute(context.Background(), changed)
	if err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("effect id reuse with changed payload was accepted: %v", err)
	}
}

func countPgDump(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if contains(call, "pg_dump") {
			count++
		}
	}
	return count
}

func countPsql(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if contains(call, "psql") {
			count++
		}
	}
	return count
}

func removeEffectForTest(service *Service, effectID string) error {
	err := os.Remove(service.effectPath(effectID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
