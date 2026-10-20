package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func archive(t *testing.T, entry string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: entry, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type fixture struct {
	root      string
	binary    string
	state     string
	artifacts string
	manager   *Manager
	manifest  Manifest
	env       Environment
	newBytes  []byte
	oldBytes  []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	binary := filepath.Join(root, "bin", "eregion")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBytes := []byte("old-binary")
	if err := os.WriteFile(binary, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	newBytes := []byte("new-binary-contents")
	archiveBytes := archive(t, "eregion", newBytes)
	name := "eregion_1.1.0_linux_amd64.tar.gz"
	if err := os.WriteFile(filepath.Join(artifacts, name), archiveBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archiveBytes)
	manifest := Manifest{
		Schema: ManifestSchema, Version: "1.1.0", Channel: "stable",
		PublishedAt: "2026-10-20T00:00:00Z", Contract: "eregion/v1",
		NodeProtocol: 1, SchemaVersion: 3, UpgradeFrom: []string{"1.0.0"},
		Artifacts: []Artifact{{
			Name: name, Target: TargetOperator, OS: "linux", Arch: "amd64", Entry: "eregion",
			URL: "https://releases.example.com/" + name, Size: int64(len(archiveBytes)),
			SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	_, private := testKey(t)
	if err := manifest.Sign(private); err != nil {
		t.Fatal(err)
	}
	env := Environment{Target: TargetOperator, OS: "linux", Arch: "amd64", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3}
	return &fixture{
		root: root, binary: binary, state: filepath.Join(root, "state"), artifacts: artifacts,
		manager:  &Manager{StateDir: filepath.Join(root, "state"), Target: TargetOperator, BinaryPath: binary, Env: env, ArtifactDir: artifacts},
		manifest: manifest, env: env, newBytes: newBytes, oldBytes: oldBytes,
	}
}

func (f *fixture) apply(t *testing.T) Plan {
	t.Helper()
	plan, err := f.manager.Prepare(f.manifest)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := f.manager.Apply(context.Background(), f.manifest, plan.Confirmation); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return plan
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPrepareDiffAndConfirmation(t *testing.T) {
	f := newFixture(t)
	plan, err := f.manager.Prepare(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CurrentSHA256 == "" || plan.ArtifactSHA256 == "" || plan.Confirmation == "" {
		t.Fatalf("incomplete plan: %+v", plan)
	}
	if len(plan.Changes) == 0 || plan.Direction != "install" {
		t.Fatalf("missing diff: %+v", plan)
	}
	if !plan.Compatibility.Compatible {
		t.Fatalf("unexpected incompatibility: %+v", plan.Compatibility)
	}
	other := f.manifest
	other.Version = "1.2.0"
	second, err := f.manager.Prepare(other)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Confirmation == second.Confirmation {
		t.Fatal("confirmation token did not bind the release version")
	}
}

func TestApplyRequiresMatchingConfirmation(t *testing.T) {
	f := newFixture(t)
	if _, err := f.manager.Apply(context.Background(), f.manifest, ""); err == nil {
		t.Fatal("apply without confirmation succeeded")
	} else if contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("unexpected code: %v", contract.Classify(err).Code)
	}
	if _, err := f.manager.Apply(context.Background(), f.manifest, "not-the-token"); err == nil {
		t.Fatal("apply with wrong confirmation succeeded")
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("binary changed after refused apply: %q", got)
	}
	if _, err := os.Stat(journalPath(f.state, TargetOperator)); !os.IsNotExist(err) {
		t.Fatalf("journal was written after refused apply: %v", err)
	}
}

func TestApplyReplacesAtomicallyAndRollsBack(t *testing.T) {
	f := newFixture(t)
	plan := f.apply(t)
	if got := readFile(t, f.binary); got != string(f.newBytes) {
		t.Fatalf("binary = %q, want new contents", got)
	}
	info, err := os.Stat(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary mode = %o, want 755", info.Mode().Perm())
	}
	if got := readFile(t, plan.BackupPath); got != string(f.oldBytes) {
		t.Fatalf("backup = %q, want old contents", got)
	}
	journal, err := loadJournal(journalPath(f.state, TargetOperator))
	if err != nil || journal == nil || journal.Status != StatusInstalled {
		t.Fatalf("journal = %+v, err = %v", journal, err)
	}
	if journal.FromSHA256 == "" || journal.ToSHA256 == "" || journal.ToSHA256 == journal.FromSHA256 {
		t.Fatalf("journal does not record distinct hashes: %+v", journal)
	}

	rollback, err := f.manager.PrepareRollback("")
	if err != nil {
		t.Fatalf("prepare rollback: %v", err)
	}
	if rollback.RestoreVersion != "1.0.0" || len(rollback.Changes) == 0 {
		t.Fatalf("unexpected rollback plan: %+v", rollback)
	}
	if _, err := f.manager.ApplyRollback(context.Background(), "", "wrong"); err == nil {
		t.Fatal("rollback accepted a wrong token")
	}
	if got := readFile(t, f.binary); got != string(f.newBytes) {
		t.Fatalf("refused rollback changed the binary: %q", got)
	}
	if _, err := f.manager.ApplyRollback(context.Background(), "", rollback.Confirmation); err != nil {
		t.Fatalf("apply rollback: %v", err)
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("rollback binary = %q, want old contents", got)
	}
	journal, err = loadJournal(journalPath(f.state, TargetOperator))
	if err != nil || journal == nil || journal.Status != StatusRolledBack {
		t.Fatalf("rollback journal = %+v, err = %v", journal, err)
	}
	backups, err := f.manager.Backups()
	if err != nil || len(backups) < 2 {
		t.Fatalf("expected recorded and safety backups: %v %+v", err, backups)
	}
}

func TestApplyRejectsTamperedArtifactWithoutReplacing(t *testing.T) {
	f := newFixture(t)
	artifact := filepath.Join(f.artifacts, f.manifest.Artifacts[0].Name)
	tampered := archive(t, "eregion", []byte("tampered-binary"))
	if err := os.WriteFile(artifact, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := f.manager.Prepare(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Apply(context.Background(), f.manifest, plan.Confirmation); err == nil {
		t.Fatal("tampered artifact was installed")
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("binary changed after tampered artifact: %q", got)
	}
	if _, err := os.Stat(journalPath(f.state, TargetOperator)); !os.IsNotExist(err) {
		t.Fatalf("journal written after tampered artifact: %v", err)
	}
}

func TestApplyRejectsArchiveWithoutDeclaredEntry(t *testing.T) {
	f := newFixture(t)
	content := archive(t, "not-the-binary", []byte("wrong"))
	name := f.manifest.Artifacts[0].Name
	if err := os.WriteFile(filepath.Join(f.artifacts, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	f.manifest.Artifacts[0].Size = int64(len(content))
	f.manifest.Artifacts[0].SHA256 = hex.EncodeToString(sum[:])
	plan, err := f.manager.Prepare(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Apply(context.Background(), f.manifest, plan.Confirmation); err == nil {
		t.Fatal("archive without the declared entry was installed")
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("binary changed after rejected archive: %q", got)
	}
}

func TestApplyRejectsIncompatibleRelease(t *testing.T) {
	f := newFixture(t)
	f.manager.Env.SchemaVersion = 4
	if _, err := f.manager.Prepare(f.manifest); err == nil {
		t.Fatal("schema downgrade was planned")
	} else if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unexpected code: %v", contract.Classify(err).Code)
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("binary changed after incompatible plan: %q", got)
	}
}

func TestApplyRejectsSymlinkAndWritableBinary(t *testing.T) {
	f := newFixture(t)
	real := filepath.Join(f.root, "real-eregion")
	if err := os.WriteFile(real, f.oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.root, "linked-eregion")
	if err := os.Symlink(real, link); err == nil {
		f.manager.BinaryPath = link
		if _, err := f.manager.Prepare(f.manifest); err == nil {
			t.Fatal("symlinked binary path was accepted")
		}
	}
	f.manager.BinaryPath = f.binary
	if runtime.GOOS != "windows" {
		if err := os.Chmod(f.binary, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := f.manager.Prepare(f.manifest); err == nil {
			t.Fatal("group/world-writable binary was accepted")
		}
	}
}

func TestStateAndBinaryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports synthetic permission bits; ACLs govern access")
	}
	f := newFixture(t)
	f.apply(t)
	for _, path := range []string{
		f.state,
		filepath.Join(f.state, string(TargetOperator)),
		downloadDir(f.state, TargetOperator),
		backupDir(f.state, TargetOperator),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", path, info.Mode().Perm())
		}
	}
	info, err := os.Stat(journalPath(f.state, TargetOperator))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o, want 600", info.Mode().Perm())
	}
	backups, err := f.manager.Backups()
	if err != nil || len(backups) == 0 {
		t.Fatalf("backups: %v %+v", err, backups)
	}
	meta, err := os.Stat(backups[0].Path + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Mode().Perm() != 0o600 {
		t.Fatalf("backup metadata mode = %o, want 600", meta.Mode().Perm())
	}
}

func TestInterruptedInstallRecovery(t *testing.T) {
	t.Run("completes from staged binary", func(t *testing.T) {
		f := newFixture(t)
		plan, err := f.manager.Prepare(f.manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.manager.recordBackup(plan); err != nil {
			t.Fatal(err)
		}
		staged, err := stageCopy(filepath.Join(f.artifacts, f.manifest.Artifacts[0].Name), filepath.Dir(f.binary), TargetOperator)
		if err != nil {
			// stageCopy copies the archive bytes as a binary stand-in; that is
			// enough to exercise the journal's hash comparison.
			t.Fatal(err)
		}
		stagedSHA, _, _, err := fileSHA256(staged, MaxArtifactBytes)
		if err != nil {
			t.Fatal(err)
		}
		journal := Journal{Schema: JournalSchema, Target: TargetOperator, Status: StatusInstalling,
			BinaryPath: f.binary, FromVersion: "1.0.0", FromSHA256: plan.CurrentSHA256,
			ToVersion: "1.1.0", ToSHA256: stagedSHA, BackupPath: plan.BackupPath,
			BackupSHA256: plan.CurrentSHA256, StagedPath: staged}
		if err := saveJournal(journalPath(f.state, TargetOperator), journal); err != nil {
			t.Fatal(err)
		}
		recovery, err := f.manager.Recover(context.Background())
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if recovery.Action != "completed" {
			t.Fatalf("recovery action = %q", recovery.Action)
		}
		if got := readFile(t, f.binary); got != string(readFileBytes(t, filepath.Join(f.artifacts, f.manifest.Artifacts[0].Name))) {
			t.Fatal("staged binary was not moved into place")
		}
		saved, _ := loadJournal(journalPath(f.state, TargetOperator))
		if saved == nil || saved.Status != StatusInstalled {
			t.Fatalf("journal = %+v", saved)
		}
	})

	t.Run("rolls back when the staged binary is gone", func(t *testing.T) {
		f := newFixture(t)
		plan, err := f.manager.Prepare(f.manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.manager.recordBackup(plan); err != nil {
			t.Fatal(err)
		}
		journal := Journal{Schema: JournalSchema, Target: TargetOperator, Status: StatusInstalling,
			BinaryPath: f.binary, FromVersion: "1.0.0", FromSHA256: plan.CurrentSHA256,
			ToVersion: "1.1.0", ToSHA256: strings.Repeat("a", 64), BackupPath: plan.BackupPath,
			BackupSHA256: plan.CurrentSHA256, StagedPath: filepath.Join(filepath.Dir(f.binary), ".missing")}
		if err := saveJournal(journalPath(f.state, TargetOperator), journal); err != nil {
			t.Fatal(err)
		}
		recovery, err := f.manager.Recover(context.Background())
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if recovery.Action != "rolled_back" {
			t.Fatalf("recovery action = %q", recovery.Action)
		}
		if got := readFile(t, f.binary); got != string(f.oldBytes) {
			t.Fatalf("binary = %q, want old contents", got)
		}
	})
}

func TestInterruptedRollbackRecovery(t *testing.T) {
	f := newFixture(t)
	f.apply(t)
	backups, err := f.manager.Backups()
	if err != nil || len(backups) == 0 {
		t.Fatal(err)
	}
	backup := backups[0]
	currentSHA, _, _, err := fileSHA256(f.binary, MaxArtifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	journal := Journal{Schema: JournalSchema, Target: TargetOperator, Status: StatusRollingBack,
		BinaryPath: f.binary, FromVersion: "1.1.0", FromSHA256: currentSHA,
		ToVersion: backup.Version, ToSHA256: backup.SHA256, SourcePath: backup.Path, SourceSHA256: backup.SHA256}
	if err := saveJournal(journalPath(f.state, TargetOperator), journal); err != nil {
		t.Fatal(err)
	}
	recovery, err := f.manager.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovery.Action != "completed" {
		t.Fatalf("recovery action = %q", recovery.Action)
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("binary = %q, want rolled-back contents", got)
	}
}

func TestStatusReportsInterruptedJournal(t *testing.T) {
	f := newFixture(t)
	journal := Journal{Schema: JournalSchema, Target: TargetOperator, Status: StatusInstalling,
		BinaryPath: f.binary, FromVersion: "1.0.0", FromSHA256: strings.Repeat("a", 64),
		ToVersion: "1.1.0", ToSHA256: strings.Repeat("b", 64)}
	if err := saveJournal(journalPath(f.state, TargetOperator), journal); err != nil {
		t.Fatal(err)
	}
	status, err := f.manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Interrupted || status.Journal == nil || status.Journal.Status != StatusInstalling {
		t.Fatalf("status = %+v", status)
	}
}

func TestRollbackRejectsOutsideBackupPath(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.root, "outside-backup")
	if err := os.WriteFile(outside, f.oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.PrepareRollback(outside); err == nil {
		t.Fatal("rollback accepted a path outside the upgrade state")
	}
}

func TestRollbackRejectsTamperedBackup(t *testing.T) {
	f := newFixture(t)
	plan := f.apply(t)
	if err := os.WriteFile(plan.BackupPath, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.PrepareRollback(plan.BackupPath); err == nil {
		t.Fatal("tampered backup was accepted")
	}
}

func TestLockContentionIsExplicit(t *testing.T) {
	f := newFixture(t)
	if err := ensureStateDir(f.state); err != nil {
		t.Fatal(err)
	}
	release, err := acquireLock(context.Background(), filepath.Join(f.state, "upgrade.lock"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := acquireLock(context.Background(), filepath.Join(f.state, "upgrade.lock"), 0); err == nil {
		t.Fatal("nested exclusive lock was granted")
	} else if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("unexpected code: %v", contract.Classify(err).Code)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
