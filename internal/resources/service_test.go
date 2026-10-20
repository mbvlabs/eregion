package resources

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestValidateSpecRejectsUnsafeResources(t *testing.T) {
	valid := testSpec()
	if err := ValidateSpec(valid); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := []struct {
		name string
		edit func(*Spec)
	}{
		{"engine", func(s *Spec) { s.Engine = "mysql" }},
		{"name", func(s *Spec) { s.Name = "../escape" }},
		{"host missing", func(s *Spec) { s.Host = "" }},
		{"version", func(s *Spec) { s.Version = "latest" }},
		{"unspecified address", func(s *Spec) { s.Address = "0.0.0.0" }},
		{"bad port", func(s *Spec) { s.Port = 70000 }},
		{"database", func(s *Spec) { s.Database = "Bad-Name" }},
		{"volume traversal", func(s *Spec) { s.Volume = "../data" }},
		{"protection", func(s *Spec) { s.Protection = "other" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			test.edit(&spec)
			if err := ValidateSpec(spec); err == nil {
				t.Fatal("unsafe spec accepted")
			}
		})
	}
}

func TestServiceRefusesAHostItDoesNotOwn(t *testing.T) {
	service, _ := newTestService(t, testSpec())
	spec := testSpec()
	spec.Host = "other-host"
	_, err := service.Plan(context.Background(), Request{Action: ActionCreate, Spec: spec})
	if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("expected validation failure, got %v", err)
	}
}

func TestPlanIsNonMutatingAndNamesConfirmation(t *testing.T) {
	service, fake := newTestService(t, testSpec())
	plan, err := service.Plan(context.Background(), Request{Action: ActionDestroy, Spec: testSpec(), Destroy: &DestroyRequest{Confirmation: testSpec().Name}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.CurrentState != StateAbsent || !plan.Destructive || !plan.ConfirmationRequired || plan.ConfirmationPhrase != "appdb" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if len(plan.Steps) < 2 {
		t.Fatalf("missing plan steps: %+v", plan)
	}
	for _, call := range fake.callArgs() {
		if len(call) > 2 && call[1] == "rm" {
			t.Fatal("plan executed a mutating command")
		}
	}
}

func TestCreateProvisionsWithProtectedCredentials(t *testing.T) {
	service, fake := newTestService(t, testSpec())
	result, err := service.Execute(context.Background(), Request{Action: ActionCreate, Spec: testSpec(), OperationID: "op-test-create"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateReady || !result.Changed || result.Endpoint == nil || result.Endpoint.Port != 55432 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Credential == nil || result.Credential.PasswordFile == "" {
		t.Fatal("no credential reference")
	}
	password, err := os.ReadFile(result.Credential.PasswordFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(password) < 32 {
		t.Fatalf("short password: %d", len(password))
	}
	info, err := os.Stat(result.Credential.PasswordFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode %o", info.Mode().Perm())
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(password)) {
		t.Fatal("result leaked the database password")
	}
	for _, call := range fake.callArgs() {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, string(password)) {
			t.Fatalf("command argument leaked the database password: %s", joined)
		}
	}
	if !fake.container || !fake.volume {
		t.Fatal("container or named volume was not created")
	}
	if !fake.databases["app"] {
		t.Fatal("database was not requested from the image")
	}
}

func TestControlPlaneDestroyIsRefusedAtTheCodeBoundary(t *testing.T) {
	spec := testSpec()
	spec.Protection = ProtectionControlPlane
	service, fake := newTestService(t, spec)
	_, err := service.Execute(context.Background(), Request{
		Action: ActionDestroy, Spec: spec,
		Destroy: &DestroyRequest{Confirmation: spec.Name, RemoveVolume: true},
	})
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("expected precondition failure, got %v", err)
	}
	if fake.container || len(fake.callArgs()) != 0 {
		t.Fatal("control-plane guard ran a Docker command")
	}

	configured := testSpec()
	service, fake = newTestService(t, configured)
	service.config.ControlPlaneDatabase = configured.Database
	_, err = service.Execute(context.Background(), Request{
		Action: ActionDestroy, Spec: configured,
		Destroy: &DestroyRequest{Confirmation: configured.Name},
	})
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("configured control-plane database was not protected: %v", err)
	}
	if len(fake.callArgs()) != 0 {
		t.Fatal("configured control-plane guard ran a Docker command")
	}
}

func TestDestroyRequiresConfirmationAndHonoursVolumeChoice(t *testing.T) {
	service, fake := newTestService(t, testSpec())
	if _, err := service.Execute(context.Background(), Request{Action: ActionCreate, Spec: testSpec(), OperationID: "op-test-create"}); err != nil {
		t.Fatal(err)
	}
	_, err := service.Execute(context.Background(), Request{
		Action: ActionDestroy, Spec: testSpec(), OperationID: "op-test-destroy",
		Destroy: &DestroyRequest{Confirmation: "wrong"},
	})
	if err == nil || contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("missing confirmation was accepted: %v", err)
	}
	result, err := service.Execute(context.Background(), Request{
		Action: ActionDestroy, Spec: testSpec(), OperationID: "op-test-destroy",
		Destroy: &DestroyRequest{Confirmation: "appdb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.container || !fake.volume {
		t.Fatal("container or retained volume state is wrong")
	}
	if _, err := os.Stat(service.passwordPath(testSpec().Name)); err != nil {
		t.Fatal("credential files were removed while the volume was retained")
	}
	if result.State != StateAbsent {
		t.Fatalf("unexpected state: %s", result.State)
	}
	if _, err := service.Execute(context.Background(), Request{
		Action: ActionDestroy, Spec: testSpec(), OperationID: "op-test-destroy-volume",
		Destroy: &DestroyRequest{Confirmation: "appdb", RemoveVolume: true},
	}); err != nil {
		t.Fatal(err)
	}
	if fake.volume {
		t.Fatal("volume was not removed")
	}
	if _, err := os.Stat(service.passwordPath(testSpec().Name)); !os.IsNotExist(err) {
		t.Fatal("credential files were not removed with the volume")
	}
}

func TestBackupVerifyAndRetention(t *testing.T) {
	service, fake := newTestService(t, testSpec())
	fake.container, fake.running = true, true
	fake.databases["app"] = true
	if _, err := service.ensureCredential(testSpec()); err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(context.Background(), Request{Action: ActionBackup, Spec: testSpec(), OperationID: "op-test-backup", Backup: &BackupRequest{Reason: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backup == nil || result.Backup.Verified {
		t.Fatalf("backup result is dishonest: %+v", result.Backup)
	}
	content, err := os.ReadFile(result.Backup.Archive)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(fake.dump) {
		t.Fatal("archive does not contain the pg_dump stream")
	}
	if mode := fileMode(t, result.Backup.Archive); mode != 0o600 {
		t.Fatalf("archive mode %o", mode)
	}
	verified, err := service.Execute(context.Background(), Request{
		Action: ActionVerify, Spec: testSpec(), Verify: &VerifyRequest{Archive: result.Backup.Archive},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Backup == nil || !verified.Backup.Verified || verified.Backup.VerifiedAt == nil {
		t.Fatalf("backup was not verified: %+v", verified.Backup)
	}
	listed, err := service.ListBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Resource != "appdb" || listed[0].Host != "app-1" {
		t.Fatalf("unexpected inventory: %+v", listed)
	}

	now := time.Now().UTC()
	artifacts := []BackupArtifact{
		{CreatedAt: now}, {CreatedAt: now.Add(-48 * time.Hour)}, {CreatedAt: now.Add(-96 * time.Hour)},
	}
	keep, prune, err := (Retention{KeepLast: 2}).SelectRetained(artifacts, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(keep) != 2 || len(prune) != 1 {
		t.Fatalf("retention kept %d and pruned %d", len(keep), len(prune))
	}
}

func TestRestoreTakesSafetyBackupAndCutsOver(t *testing.T) {
	service, fake := newTestService(t, testSpec())
	fake.container, fake.running = true, true
	fake.databases["app"] = true
	if _, err := service.ensureCredential(testSpec()); err != nil {
		t.Fatal(err)
	}
	backup, err := service.Execute(context.Background(), Request{Action: ActionBackup, Spec: testSpec(), OperationID: "op-test-backup"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(context.Background(), Request{
		Action: ActionRestore, Spec: testSpec(), OperationID: "op-test-restore",
		Restore: &RestoreRequest{Archive: backup.Backup.Archive, Confirmation: "appdb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RestoredFrom == nil || result.SafetyBackup == nil || result.SafetyBackup.Reason != "safety" {
		t.Fatalf("missing restore evidence: %+v", result)
	}
	if result.SafetyBackup.Archive == backup.Backup.Archive {
		t.Fatal("restore reused the selected archive instead of taking a safety backup")
	}
	if !fake.databases["app"] {
		t.Fatal("target database is missing after restore")
	}
	for database := range fake.databases {
		if strings.Contains(database, "__restore_") || strings.Contains(database, "__previous_") {
			t.Fatalf("restore left a temporary database %s", database)
		}
	}
	if _, err := os.Stat(service.restoreJournalPath("appdb")); !os.IsNotExist(err) {
		t.Fatal("restore journal was not removed")
	}

	_, err = service.Execute(context.Background(), Request{
		Action: ActionRestore, Spec: testSpec(), OperationID: "op-test-restore",
		Restore: &RestoreRequest{Archive: backup.Backup.Archive, Confirmation: "wrong"},
	})
	if err == nil || contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("restore without confirmation was accepted: %v", err)
	}
}

func TestFailedRestoreRecoversThePreviousDatabase(t *testing.T) {
	service, fake := newTestService(t, testSpec())
	fake.container, fake.running = true, true
	fake.databases["app"] = true
	if _, err := service.ensureCredential(testSpec()); err != nil {
		t.Fatal(err)
	}
	backup, err := service.Execute(context.Background(), Request{Action: ActionBackup, Spec: testSpec(), OperationID: "op-test-backup"})
	if err != nil {
		t.Fatal(err)
	}
	fake.failProbe = "app"
	result, err := service.Execute(context.Background(), Request{
		Action: ActionRestore, Spec: testSpec(), OperationID: "op-test-restore",
		Restore: &RestoreRequest{Archive: backup.Backup.Archive, Confirmation: "appdb"},
	})
	if err == nil {
		t.Fatal("failed restore reported success")
	}
	if result.Recovery == nil || !result.Recovery.Required || !result.Recovery.Attempted || !result.Recovery.Succeeded {
		t.Fatalf("recovery was not attempted or did not succeed: %+v", result.Recovery)
	}
	if !fake.databases["app"] {
		t.Fatal("previous database was not recovered at the target name")
	}
	for database := range fake.databases {
		if strings.Contains(database, "__restore_") || strings.Contains(database, "__previous_") || strings.Contains(database, "__failed_") {
			t.Fatalf("failed restore left %s", database)
		}
	}
	if _, err := os.Stat(service.restoreJournalPath("appdb")); !os.IsNotExist(err) {
		t.Fatal("recovery left the restore journal in place")
	}
}

func TestCommandTimeoutIsUnavailableNotSuccess(t *testing.T) {
	spec := testSpec()
	fake := newFakeHost(spec)
	fake.waitFirst = true
	service, err := New(Config{
		Host: spec.Host, StateDir: t.TempDir(), BackupDir: t.TempDir(), Runner: fake,
		CommandTimeout: 30 * time.Millisecond, StartTimeout: time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(context.Background(), Request{Action: ActionHealth, Spec: spec})
	if err == nil || contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("timeout was not an honest unavailable state: %v", err)
	}
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("unexpected error type: %T", err)
	}
}

func TestScheduleNextAndValidation(t *testing.T) {
	schedule := Schedule{Cron: "*/15 * * * *"}
	after := time.Date(2026, 10, 20, 12, 7, 0, 0, time.UTC)
	next, err := schedule.Next(after)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(time.Date(2026, 10, 20, 12, 15, 0, 0, time.UTC)) {
		t.Fatalf("unexpected next run %s", next)
	}
	if err := (Schedule{Cron: "*/15 * * * *", EverySeconds: 60}).Validate(); err == nil {
		t.Fatal("both schedule forms were accepted")
	}
	if err := (Schedule{Cron: "not cron"}).Validate(); err == nil {
		t.Fatal("invalid cron was accepted")
	}
	if err := (Policy{Schedule: Schedule{EverySeconds: 3600}, Retention: Retention{KeepLast: 3}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
