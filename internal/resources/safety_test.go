package resources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

// TestLocalRunnerRejectsOtherHost proves the executor is bound to the node it
// can actually reach. Multi-node placement would need a remote runner; a Host
// string alone must never imply that this process owns another machine.
func TestLocalRunnerRejectsOtherHost(t *testing.T) {
	spec := testSpec()
	fake := newFakeHost(spec)
	fake.nodeIdentity = "another-node"
	_, err := New(Config{Host: spec.Host, StateDir: t.TempDir(), BackupDir: t.TempDir(), Runner: fake})
	if err == nil {
		t.Fatal("a runner for another node was accepted")
	}
	if contract.Classify(err).Code != contract.PreconditionFailed || !strings.Contains(contract.Classify(err).Message, "verified node identity") {
		t.Fatalf("unexpected host guard error: %v", err)
	}
}

// TestControlPlaneDatabaseExcludedFromOrdinaryActions proves the configured
// control-plane database cannot be destroyed or backed up through the
// application-resource path even when a caller omits the protection marker.
func TestControlPlaneDatabaseExcludedFromOrdinaryActions(t *testing.T) {
	spec := testSpec()
	spec.Database = "eregion_control_plane"
	fake := newFakeHost(spec)
	service, err := New(Config{
		Host: spec.Host, StateDir: t.TempDir(), BackupDir: t.TempDir(), Runner: fake,
		ControlPlaneDatabase: spec.Database,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{
		{Action: ActionDestroy, Spec: spec, OperationID: "op-destroy-control-plane",
			Destroy: &DestroyRequest{Confirmation: spec.Name, RemoveVolume: true}},
		{Action: ActionBackup, Spec: spec, OperationID: "op-backup-control-plane"},
		{Action: ActionSchedule, Spec: spec, OperationID: "op-schedule-control-plane",
			Policy: &Policy{Schedule: Schedule{EverySeconds: 3600}, Retention: Retention{KeepLast: 3}}},
		{Action: ActionCredentials, Spec: spec, OperationID: "op-credentials-control-plane",
			Export: &ExportRequest{Destination: filepath.Join(t.TempDir(), "control-plane.json")}},
	} {
		if _, err := service.Execute(context.Background(), request); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
			t.Fatalf("control-plane database accepted %s: %v", request.Action, err)
		}
	}
	if len(fake.callArgs()) != 0 {
		t.Fatalf("control-plane guard contacted Docker: %v", fake.callArgs())
	}

	protected := spec
	protected.Protection = ProtectionControlPlane
	if _, err := service.Plan(context.Background(), Request{Action: ActionDestroy, Spec: protected,
		Destroy: &DestroyRequest{Confirmation: protected.Name}}); err == nil {
		t.Fatal("control-plane protection marker was ignored by Plan")
	}
}

// TestFailedRestoreRestoresPreviousDatabase proves the tested recovery path:
// a post-cutover probe failure renames the previous database back and removes
// the failed restore database, so the pre-restore state keeps serving.
func TestFailedRestoreRestoresPreviousDatabase(t *testing.T) {
	spec := testSpec()
	service, fake := newTestService(t, spec)
	ctx := context.Background()
	if _, err := service.Execute(ctx, Request{Action: ActionCreate, Spec: spec, OperationID: "op-create"}); err != nil {
		t.Fatal(err)
	}
	backup, err := service.Execute(ctx, Request{Action: ActionBackup, Spec: spec, OperationID: "op-backup"})
	if err != nil || backup.Backup == nil {
		t.Fatalf("backup=%+v err=%v", backup, err)
	}
	fake.failProbe = spec.Database
	failed, restoreErr := service.Execute(ctx, Request{
		Action: ActionRestore, Spec: spec, OperationID: "op-failed-restore",
		Restore: &RestoreRequest{Archive: backup.Backup.Archive, Confirmation: spec.Name},
	})
	if restoreErr == nil {
		t.Fatal("injected post-cutover probe failure reported success")
	}
	if failed.Recovery == nil || !failed.Recovery.Succeeded || !failed.Recovery.Attempted {
		t.Fatalf("failed restore did not report a successful recovery: %+v", failed.Recovery)
	}
	if !fake.databases[spec.Database] {
		t.Fatal("target database is not serving after recovery")
	}
	for name := range fake.databases {
		if strings.Contains(name, "__previous_") || strings.Contains(name, "__temporary_") || strings.Contains(name, "__failed_") {
			t.Fatalf("failed restore left an intermediate database: %s", name)
		}
	}
	if _, err := os.Lstat(service.restoreJournalPath(spec.Name)); !os.IsNotExist(err) {
		t.Fatalf("restore journal was not removed after recovery: %v", err)
	}
}

// TestCredentialExportNeverReturnsPlaintext checks that the command surface
// can produce a usable protected file without placing the credential in the
// durable result.
func TestCredentialExportNeverReturnsPlaintext(t *testing.T) {
	spec := testSpec()
	service, _ := newTestService(t, spec)
	ctx := context.Background()
	created, err := service.Execute(ctx, Request{Action: ActionCreate, Spec: spec, OperationID: "op-create"})
	if err != nil {
		t.Fatal(err)
	}
	password, err := os.ReadFile(created.Credential.PasswordFile)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "appdb.json")
	exported, err := service.Execute(ctx, Request{
		Action: ActionCredentials, Spec: spec, OperationID: "op-credentials",
		Export: &ExportRequest{Destination: destination},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exported.Export == nil || exported.Export.Destination != destination || exported.Export.Mode != "0600" {
		t.Fatalf("unexpected export result: %+v", exported.Export)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential export is not a 0600 file: %v %v", info, err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	var value credentialExport
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value.Password != strings.TrimSpace(string(password)) || value.DSN == "" {
		t.Fatal("credential export did not contain the usable protected credential")
	}
	encoded, _ := json.Marshal(exported)
	if strings.Contains(string(encoded), value.Password) {
		t.Fatal("durable result leaked credential plaintext")
	}
	replay, err := service.Execute(ctx, Request{
		Action: ActionCredentials, Spec: spec, OperationID: "op-credentials",
		Export: &ExportRequest{Destination: destination},
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("credential export retry was not replayed: %+v err=%v", replay, err)
	}
}

func TestBackupSchedulePolicyIsRecordedWithoutStartingWork(t *testing.T) {
	spec := testSpec()
	service, fake := newTestService(t, spec)
	policy := Policy{Schedule: Schedule{EverySeconds: 86400}, Retention: Retention{KeepLast: 7}, VerifyAfter: true}
	result, err := service.Execute(context.Background(), Request{
		Action: ActionSchedule, Spec: spec, OperationID: "op-schedule", Policy: &policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Scheduled == nil || result.Scheduled.Kind != KindBackupCreate || result.Scheduled.Target != "node/"+spec.Host {
		t.Fatalf("unexpected schedule declaration: %+v", result.Scheduled)
	}
	raw, err := os.ReadFile(service.schedulePath(spec.Name))
	if err != nil {
		t.Fatal(err)
	}
	var state scheduleState
	if err := json.Unmarshal(raw, &state); err != nil || state.Policy.Retention.KeepLast != 7 {
		t.Fatalf("schedule state was not recorded: %v %s", err, raw)
	}
	if strings.Contains(string(raw), "password") {
		t.Fatal("schedule state contains credential material")
	}
	if len(fake.callArgs()) != 0 {
		t.Fatalf("recording a schedule executed Docker work: %v", fake.callArgs())
	}
}
