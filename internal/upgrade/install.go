package upgrade

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// DefaultStateDir returns the per-user upgrade state directory. Production
// services should set EREGION_UPGRADE_STATE_DIR to a root-owned path.
func DefaultStateDir() string {
	if value := os.Getenv("EREGION_UPGRADE_STATE_DIR"); value != "" {
		return value
	}
	if value := os.Getenv("XDG_STATE_HOME"); value != "" {
		return filepath.Join(value, "eregion", "upgrade")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "eregion-upgrade")
	}
	return filepath.Join(home, ".local", "state", "eregion", "upgrade")
}

// Manager owns one target's replacement state. It never runs a downloaded
// binary and never touches a path outside BinaryPath, StateDir, and the
// artifact/destination directories it is given.
type Manager struct {
	StateDir   string
	Target     Target
	BinaryPath string
	Env        Environment
	Downloader *Downloader
	// ArtifactDir is an optional operator-supplied offline mirror. Files are
	// still checked against the signed size and SHA256; no path traversal.
	ArtifactDir string
	// HealthProbe runs the newly installed binary after the atomic rename. A
	// failure automatically restores the verified backup; nil skips the probe
	// (used only by callers that perform their own verification).
	HealthProbe HealthProbe
	// HealthTimeout bounds one health probe. Zero selects 20 seconds.
	HealthTimeout time.Duration
}

// Plan is the dry-run diff and the confirmation binding for an install.
type Plan struct {
	Direction      string        `json:"direction"`
	Target         Target        `json:"target"`
	StateDir       string        `json:"state_dir"`
	BinaryPath     string        `json:"binary_path"`
	CurrentVersion string        `json:"current_version"`
	CurrentSHA256  string        `json:"current_sha256"`
	NextVersion    string        `json:"next_version"`
	NextSHA256     string        `json:"next_sha256,omitempty"`
	ArtifactName   string        `json:"artifact_name"`
	ArtifactEntry  string        `json:"artifact_entry"`
	ArtifactSource string        `json:"artifact_source"`
	ArtifactSHA256 string        `json:"artifact_sha256"`
	ArtifactSize   int64         `json:"artifact_size"`
	BackupPath     string        `json:"backup_path"`
	Changes        []string      `json:"changes"`
	Compatibility  Compatibility `json:"compatibility"`
	Confirmation   string        `json:"confirmation"`
}

// RollbackPlan is the dry-run diff for restoring one recorded backup.
type RollbackPlan struct {
	Direction      string   `json:"direction"`
	Target         Target   `json:"target"`
	StateDir       string   `json:"state_dir"`
	BinaryPath     string   `json:"binary_path"`
	CurrentVersion string   `json:"current_version"`
	CurrentSHA256  string   `json:"current_sha256"`
	RestoreVersion string   `json:"restore_version"`
	RestorePath    string   `json:"restore_path"`
	RestoreSHA256  string   `json:"restore_sha256"`
	SafetyBackup   string   `json:"safety_backup"`
	Changes        []string `json:"changes"`
	Confirmation   string   `json:"confirmation"`
}

// Result is one completed replacement and its journal.
type Result struct {
	Plan        any       `json:"plan"`
	Journal     Journal   `json:"journal"`
	Recovered   *Recovery `json:"recovered,omitempty"`
	HealthCheck string    `json:"health_check,omitempty"`
}

// Recovery describes what an interrupted journal reconciliation did.
type Recovery struct {
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// Backup is one retained previous binary.
type Backup struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
}

// Status is a read-only snapshot of the installed binary and recovery state.
type Status struct {
	Target         Target   `json:"target"`
	BinaryPath     string   `json:"binary_path"`
	StateDir       string   `json:"state_dir"`
	Version        string   `json:"version,omitempty"`
	ProcessVersion string   `json:"process_version,omitempty"`
	SHA256         string   `json:"sha256,omitempty"`
	Journal        *Journal `json:"journal,omitempty"`
	Backups        []Backup `json:"backups"`
	Interrupted    bool     `json:"interrupted"`
	RecoveryDetail string   `json:"recovery_detail,omitempty"`
}

func (m *Manager) normalize() error {
	if m.StateDir == "" {
		return contract.NewFault(contract.InputRequired, "upgrade state directory is required", "Set EREGION_UPGRADE_STATE_DIR or pass --state-dir.")
	}
	if m.BinaryPath == "" {
		return contract.NewFault(contract.InputRequired, "target binary path is required", "Pass --binary with the installed operator or node binary.")
	}
	if m.Target == "" {
		m.Target = TargetOperator
	}
	if m.Target != TargetOperator && m.Target != TargetNode {
		return contract.NewFault(contract.ValidationFailed, "upgrade target must be operator or node", "Use --target operator or --target node.")
	}
	if m.Downloader == nil {
		m.Downloader = NewDownloader(nil)
	}
	if m.Env.Target == "" {
		m.Env.Target = m.Target
	}
	if m.Env.OS == "" || m.Env.Arch == "" {
		return contract.NewFault(contract.InputRequired, "running OS and architecture are required", "Set the compatibility environment before planning an upgrade.")
	}
	return nil
}

// Prepare computes the compatibility report, artifact selection, current
// binary hash, and confirmation token without downloading or replacing.
func (m *Manager) Prepare(manifest Manifest) (Plan, error) {
	if err := m.normalize(); err != nil {
		return Plan{}, err
	}
	return m.prepare(manifest)
}

func (m *Manager) prepare(manifest Manifest) (Plan, error) {
	plan := Plan{Direction: "install", Target: m.Target, StateDir: m.StateDir, BinaryPath: m.BinaryPath}
	if err := manifest.Validate(); err != nil {
		return plan, err
	}
	report := CheckCompatibility(manifest, m.Env)
	plan.Compatibility = report
	if !report.Compatible {
		return plan, report.Err()
	}
	artifact, err := SelectArtifact(manifest, m.Target, m.Env.OS, m.Env.Arch)
	if err != nil {
		return plan, err
	}
	currentSHA, _, _, err := ensureReplaceable(m.BinaryPath)
	if err != nil {
		return plan, err
	}
	plan.CurrentVersion = m.Env.Version
	if plan.CurrentVersion == "" {
		plan.CurrentVersion = "unknown"
	}
	plan.CurrentSHA256 = currentSHA
	plan.NextVersion = manifest.Version
	plan.ArtifactName = artifact.Name
	plan.ArtifactEntry = artifact.Entry
	plan.ArtifactSHA256 = artifact.SHA256
	plan.ArtifactSize = artifact.Size
	plan.BackupPath = m.backupPath(plan.CurrentVersion, currentSHA)
	if m.ArtifactDir != "" {
		plan.ArtifactSource = filepath.Join(m.ArtifactDir, artifact.Name)
	} else {
		plan.ArtifactSource = artifact.URL
	}
	plan.Changes = []string{
		"back up " + plan.BinaryPath + " to " + plan.BackupPath,
		fmt.Sprintf("download %s (%s, sha256 %s)", artifact.Name, formatBytes(artifact.Size), artifact.SHA256),
		fmt.Sprintf("verify SHA256 %s before use", artifact.SHA256),
		fmt.Sprintf("atomically replace %s: %s -> %s", plan.BinaryPath, plan.CurrentVersion, manifest.Version),
		"keep the previous binary for explicit rollback",
	}
	plan.Confirmation = installToken(plan)
	return plan, nil
}

func installToken(plan Plan) string {
	return token(
		"install", string(plan.Target), plan.StateDir, plan.BinaryPath,
		plan.CurrentSHA256, plan.NextVersion, plan.ArtifactName, plan.ArtifactSHA256,
		plan.ArtifactSource, plan.BackupPath,
	)
}

// Apply verifies the plan-bound confirmation, recovers any interrupted prior
// replacement, verifies the signed artifact, and atomically replaces the
// binary. It never executes the new binary.
func (m *Manager) Apply(ctx context.Context, manifest Manifest, confirm string) (Result, error) {
	if err := m.normalize(); err != nil {
		return Result{}, err
	}
	// Refuse a missing or stale token before acquiring the lock or recovering a
	// prior journal. A mistyped confirmation must not mutate the installation.
	initial, err := m.prepare(manifest)
	if err != nil {
		return Result{}, err
	}
	if err := requireConfirmation(initial.Confirmation, confirm, "apply"); err != nil {
		return Result{}, err
	}
	release, err := m.lock(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	recovery, err := m.recoverLocked()
	if err != nil {
		return Result{}, err
	}
	plan, err := m.prepare(manifest)
	if err != nil {
		return Result{}, err
	}
	// Recovery may have changed the binary identity, so bind the token to the
	// plan computed after recovery as well.
	if err := requireConfirmation(plan.Confirmation, confirm, "apply"); err != nil {
		return Result{}, err
	}
	archive, err := m.obtain(ctx, plan)
	if err != nil {
		return Result{}, err
	}
	staged, err := extractBinary(archive, plan.ArtifactEntry, filepath.Dir(m.BinaryPath), m.Target)
	if err != nil {
		return Result{}, err
	}
	removeStaged := true
	defer func() {
		if removeStaged {
			_ = os.Remove(staged)
		}
	}()
	stagedSHA, _, _, err := fileSHA256(staged, MaxArtifactBytes)
	if err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "staged binary could not be re-read", "Check the filesystem; no binary was replaced.")
	}
	plan.NextSHA256 = stagedSHA
	currentSHA, _, _, err := ensureReplaceable(m.BinaryPath)
	if err != nil {
		return Result{}, err
	}
	if subtle.ConstantTimeCompare([]byte(currentSHA), []byte(plan.CurrentSHA256)) != 1 {
		return Result{}, contract.NewFault(contract.Conflict, "target binary changed while the upgrade was being planned", "Rerun the dry-run and apply the new plan; no replacement was made.")
	}
	if err := m.recordBackup(plan); err != nil {
		return Result{}, err
	}
	status := StatusInstalling
	if m.HealthProbe != nil {
		// A crash during a required health probe must never be mistaken for a
		// completed activation. Recovery of a "verifying" journal restores the
		// previous binary instead of finishing the install.
		status = StatusVerifying
	}
	journal := Journal{
		Schema: JournalSchema, Target: m.Target, Status: status, OperationID: operationID(),
		BinaryPath: plan.BinaryPath, FromVersion: plan.CurrentVersion, FromSHA256: plan.CurrentSHA256,
		ToVersion: plan.NextVersion, ToSHA256: stagedSHA,
		BackupPath: plan.BackupPath, BackupSHA256: plan.CurrentSHA256, StagedPath: staged,
	}
	path := journalPath(m.StateDir, m.Target)
	if err := saveJournal(path, journal); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "upgrade journal could not be written", "Check state-directory permissions; no binary was replaced.")
	}
	if err := os.Rename(staged, m.BinaryPath); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "binary replacement failed; the journal records the interrupted install", "Run upgrade apply or rollback again to reconcile, or restore the recorded backup.")
	}
	removeStaged = false
	if err := syncDir(filepath.Dir(m.BinaryPath)); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(m.BinaryPath, 0o755); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "installed binary permissions could not be set", "Restore the recorded backup and inspect the filesystem.")
	}
	health, healthErr := m.probeHealth(ctx, plan)
	if healthErr != nil {
		restoreErr := m.restoreAfterFailedHealth(path, plan, &journal, healthErr)
		if restoreErr != nil {
			return Result{Plan: plan, Journal: journal, Recovered: &recovery}, restoreErr
		}
		fault := contract.NewFault(contract.PreconditionFailed, "the new binary failed its health check and the previous binary was restored", "Do not activate this release. Inspect the health probe detail, fix the artifact, and rerun the dry run.")
		fault.Details = map[string]any{"health_check": healthErr.Error(), "restored": plan.CurrentVersion, "journal": journal.OperationID}
		return Result{Plan: plan, Journal: journal, Recovered: &recovery}, fault
	}
	journal.Status = StatusInstalled
	journal.StagedPath = ""
	journal.HealthCheck = health
	if err := saveJournal(path, journal); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "upgrade journal could not record completion", "Run upgrade apply or rollback again to reconcile from the journal.")
	}
	return Result{Plan: plan, Journal: journal, Recovered: &recovery, HealthCheck: health}, nil
}

// probeHealth runs one bounded health probe after the atomic rename. A caller
// that supplies no probe has explicitly opted out of local health checking.
func (m *Manager) probeHealth(ctx context.Context, plan Plan) (string, error) {
	if m.HealthProbe == nil {
		return "", nil
	}
	timeout := m.HealthTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	detail, err := m.HealthProbe(probeCtx, plan.BinaryPath)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(detail) == "" {
		detail = "probe passed"
	}
	return detail, nil
}

// restoreAfterFailedHealth puts the verified previous binary back while the
// journal still describes the install. It never guesses: a backup that does not
// match the recorded hash is a failed recovery.
func (m *Manager) restoreAfterFailedHealth(path string, plan Plan, journal *Journal, healthErr error) error {
	reason := "health check failed: " + healthErr.Error()
	sha, _, _, err := fileSHA256(plan.BackupPath, MaxArtifactBytes)
	if err != nil || sha != plan.CurrentSHA256 {
		journal.Status = StatusFailed
		journal.Error = reason + "; the recorded backup is missing or does not match its hash"
		_ = saveJournal(path, *journal)
		return contract.NewFault(contract.PreconditionFailed, "the new binary failed its health check and automatic restore could not verify the backup", "Restore the backup manually from the upgrade state directory and inspect the journal.")
	}
	staged, err := stageCopy(plan.BackupPath, filepath.Dir(m.BinaryPath), m.Target)
	if err == nil {
		err = os.Rename(staged, m.BinaryPath)
	}
	if err != nil {
		if staged != "" {
			_ = os.Remove(staged)
		}
		journal.Status = StatusFailed
		journal.Error = reason + "; restoring the backup failed"
		_ = saveJournal(path, *journal)
		return contract.NewFault(contract.PreconditionFailed, "the new binary failed its health check and automatic restore failed", "Restore the backup manually from the upgrade state directory and inspect the journal.")
	}
	_ = chmodExecutable(m.BinaryPath)
	_ = syncDir(filepath.Dir(m.BinaryPath))
	journal.Status = StatusRolledBack
	journal.StagedPath = ""
	journal.HealthCheck = reason
	journal.Error = reason + "; restored " + plan.CurrentVersion
	if err := saveJournal(path, *journal); err != nil {
		return contract.NewFault(contract.Unavailable, "the backup was restored but the journal could not record it", "Inspect the journal and confirm the current binary hash before retrying.")
	}
	return nil
}

func (m *Manager) obtain(ctx context.Context, plan Plan) (string, error) {
	dir := downloadDir(m.StateDir, m.Target)
	if m.ArtifactDir != "" {
		return CopyVerifiedFile(filepath.Join(m.ArtifactDir, plan.ArtifactName), plan.ArtifactSHA256, plan.ArtifactSize, dir)
	}
	return m.Downloader.DownloadVerified(ctx, plan.ArtifactSource, plan.ArtifactSHA256, plan.ArtifactSize, dir)
}

func (m *Manager) backupPath(version, sha string) string {
	return filepath.Join(backupDir(m.StateDir, m.Target), backupLabel(version, sha), filepath.Base(m.BinaryPath))
}

func backupLabel(version, sha string) string {
	safe := make([]rune, 0, len(version))
	for _, r := range version {
		if r == '.' || r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			safe = append(safe, r)
		} else {
			safe = append(safe, '_')
		}
	}
	label := string(safe)
	if label == "" {
		label = "unknown"
	}
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return label + "-" + short
}

func (m *Manager) recordBackup(plan Plan) error {
	dir := filepath.Dir(plan.BackupPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return contract.NewFault(contract.Unavailable, "backup directory could not be created", "Check state-directory permissions; no binary was replaced.")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return contract.NewFault(contract.Unavailable, "backup directory permissions could not be restricted", "Check state-directory ownership; no binary was replaced.")
	}
	if existing, err := verifyExisting(plan.BackupPath, plan.CurrentSHA256, 0); err == nil && existing {
		return m.writeBackupMeta(plan)
	}
	sha, _, err := copyFileAtomic(plan.BackupPath, plan.BinaryPath, 0o700, MaxArtifactBytes)
	if err != nil {
		return contract.NewFault(contract.Unavailable, "current binary could not be backed up", "Check free space and permissions; no binary was replaced.")
	}
	if subtle.ConstantTimeCompare([]byte(sha), []byte(plan.CurrentSHA256)) != 1 {
		_ = os.Remove(plan.BackupPath)
		return contract.NewFault(contract.PreconditionFailed, "backup copy does not match the current binary", "Refuse to replace; retry after checking the filesystem.")
	}
	return m.writeBackupMeta(plan)
}

func (m *Manager) writeBackupMeta(plan Plan) error {
	meta := Backup{Path: plan.BackupPath, Version: plan.CurrentVersion, SHA256: plan.CurrentSHA256, CreatedAt: nowRFC3339()}
	if info, err := os.Stat(plan.BackupPath); err == nil {
		meta.Size = info.Size()
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(plan.BackupPath+".json", append(raw, '\n'), 0o600); err != nil {
		return contract.NewFault(contract.Unavailable, "backup metadata could not be written", "Check state-directory permissions; no binary was replaced.")
	}
	return nil
}

// Recover reconciles an interrupted install or rollback under the upgrade lock.
// It is also called automatically at the start of Apply and ApplyRollback.
func (m *Manager) Recover(ctx context.Context) (Recovery, error) {
	if err := m.normalize(); err != nil {
		return Recovery{}, err
	}
	release, err := m.lock(ctx)
	if err != nil {
		return Recovery{}, err
	}
	defer release()
	return m.recoverLocked()
}

func (m *Manager) recoverLocked() (Recovery, error) {
	path := journalPath(m.StateDir, m.Target)
	journal, err := loadJournal(path)
	if err != nil {
		return Recovery{}, contract.NewFault(contract.PreconditionFailed, "upgrade journal is corrupt", "Inspect the state directory; never guess which binary is installed.")
	}
	if journal == nil || terminalStatus(journal.Status) {
		return Recovery{Action: "none", Detail: "no interrupted replacement"}, nil
	}
	switch journal.Status {
	case StatusInstalling:
		return m.recoverInstall(path, journal)
	case StatusVerifying:
		return m.recoverVerifying(path, journal)
	case StatusRollingBack:
		return m.recoverRollback(path, journal)
	default:
		return Recovery{}, contract.NewFault(contract.PreconditionFailed, "upgrade journal has an unknown status", "Inspect the state directory; never guess which binary is installed.")
	}
}

func (m *Manager) recoverInstall(path string, journal *Journal) (Recovery, error) {
	current, _, _, currentErr := fileSHA256(m.BinaryPath, MaxArtifactBytes)
	if currentErr == nil && current == journal.ToSHA256 {
		if journal.StagedPath != "" {
			_ = os.Remove(journal.StagedPath)
		}
		journal.Status = StatusInstalled
		journal.StagedPath = ""
		return m.finishRecovery(path, journal, "completed", "the new binary is already in place; marked the interrupted install complete")
	}
	if journal.StagedPath != "" {
		if stagedSHA, _, _, err := fileSHA256(journal.StagedPath, MaxArtifactBytes); err == nil && stagedSHA == journal.ToSHA256 {
			if err := os.Rename(journal.StagedPath, m.BinaryPath); err == nil {
				_ = os.Chmod(m.BinaryPath, 0o755)
				_ = syncDir(filepath.Dir(m.BinaryPath))
				journal.Status = StatusInstalled
				journal.StagedPath = ""
				return m.finishRecovery(path, journal, "completed", "finished the interrupted atomic replacement from the verified staged binary")
			}
		}
	}
	if journal.BackupPath != "" {
		if backupSHA, _, _, err := fileSHA256(journal.BackupPath, MaxArtifactBytes); err == nil && backupSHA == journal.BackupSHA256 {
			if currentErr != nil || current == journal.FromSHA256 {
				if currentErr != nil {
					if _, _, err := copyFileAtomic(m.BinaryPath, journal.BackupPath, 0o755, MaxArtifactBytes); err != nil {
						return Recovery{}, m.failedRecovery(path, journal, "backup could not be restored")
					}
					_ = chmodExecutable(m.BinaryPath)
				}
				_ = os.Remove(journal.StagedPath)
				journal.Status = StatusRolledBack
				journal.StagedPath = ""
				return m.finishRecovery(path, journal, "rolled_back", "the interrupted install was rolled back to the recorded previous binary")
			}
		}
	}
	return Recovery{}, m.failedRecovery(path, journal, "neither the new binary, the staged binary, nor the recorded backup could be reconciled")
}

// recoverVerifying handles a crash after the journal was marked verifying but
// before the health probe confirmed the release. An unconfirmed binary is never
// promoted to installed: the verified previous binary is restored instead.
func (m *Manager) recoverVerifying(path string, journal *Journal) (Recovery, error) {
	current, _, _, currentErr := fileSHA256(m.BinaryPath, MaxArtifactBytes)
	if currentErr == nil && current == journal.FromSHA256 {
		if journal.StagedPath != "" {
			_ = os.Remove(journal.StagedPath)
		}
		journal.Status = StatusRolledBack
		journal.StagedPath = ""
		journal.Error = "health check did not complete; the previous binary was still in place"
		return m.finishRecovery(path, journal, "rolled_back", "the previous binary was still in place; health verification did not complete")
	}
	backupSHA, _, _, backupErr := fileSHA256(journal.BackupPath, MaxArtifactBytes)
	if backupErr == nil && backupSHA == journal.BackupSHA256 {
		staged, err := stageCopy(journal.BackupPath, filepath.Dir(m.BinaryPath), m.Target)
		if err == nil {
			err = os.Rename(staged, m.BinaryPath)
		}
		if err == nil {
			_ = chmodExecutable(m.BinaryPath)
			_ = syncDir(filepath.Dir(m.BinaryPath))
			if journal.StagedPath != "" {
				_ = os.Remove(journal.StagedPath)
			}
			journal.Status = StatusRolledBack
			journal.StagedPath = ""
			journal.Error = "health check did not complete before the process stopped; restored " + journal.FromVersion
			return m.finishRecovery(path, journal, "rolled_back", "restored the previous binary after an interrupted health check")
		}
		if staged != "" {
			_ = os.Remove(staged)
		}
	}
	return Recovery{}, m.failedRecovery(path, journal, "an interrupted health check left no verified backup to restore")
}

func (m *Manager) recoverRollback(path string, journal *Journal) (Recovery, error) {
	current, _, _, currentErr := fileSHA256(m.BinaryPath, MaxArtifactBytes)
	if currentErr == nil && current == journal.ToSHA256 {
		if journal.StagedPath != "" {
			_ = os.Remove(journal.StagedPath)
		}
		journal.Status = StatusRolledBack
		journal.StagedPath = ""
		return m.finishRecovery(path, journal, "completed", "the recorded backup is already in place; marked rollback complete")
	}
	if journal.SourcePath != "" {
		if sourceSHA, _, _, err := fileSHA256(journal.SourcePath, MaxArtifactBytes); err == nil && sourceSHA == journal.SourceSHA256 {
			staged, err := stageCopy(journal.SourcePath, filepath.Dir(m.BinaryPath), m.Target)
			if err == nil {
				if err := os.Rename(staged, m.BinaryPath); err == nil {
					_ = chmodExecutable(m.BinaryPath)
					_ = syncDir(filepath.Dir(m.BinaryPath))
					journal.Status = StatusRolledBack
					journal.StagedPath = ""
					return m.finishRecovery(path, journal, "completed", "finished the interrupted rollback from the verified backup")
				}
				_ = os.Remove(staged)
			}
		}
	}
	return Recovery{}, m.failedRecovery(path, journal, "the rollback backup or destination could not be reconciled")
}

func (m *Manager) finishRecovery(path string, journal *Journal, action, detail string) (Recovery, error) {
	if err := saveJournal(path, *journal); err != nil {
		return Recovery{}, contract.NewFault(contract.Unavailable, "recovered upgrade journal could not be saved", "Inspect the state directory before retrying.")
	}
	return Recovery{Action: action, Detail: detail}, nil
}

func (m *Manager) failedRecovery(path string, journal *Journal, detail string) error {
	journal.Status = StatusFailed
	journal.Error = detail
	_ = saveJournal(path, *journal)
	return contract.NewFault(contract.PreconditionFailed, "interrupted upgrade requires operator recovery", detail+"; inspect the journal and restore a verified backup explicitly.")
}

// PrepareRollback selects the journal's last backup or an explicit backup path
// inside this target's backup directory.
func (m *Manager) PrepareRollback(backupPath string) (RollbackPlan, error) {
	if err := m.normalize(); err != nil {
		return RollbackPlan{}, err
	}
	return m.prepareRollback(backupPath)
}

func (m *Manager) prepareRollback(backupPath string) (RollbackPlan, error) {
	plan := RollbackPlan{Direction: "rollback", Target: m.Target, StateDir: m.StateDir, BinaryPath: m.BinaryPath}
	currentSHA, _, _, err := ensureReplaceable(m.BinaryPath)
	if err != nil {
		return plan, err
	}
	plan.CurrentVersion = m.Env.Version
	journal, journalErr := loadJournal(journalPath(m.StateDir, m.Target))
	if journalErr != nil {
		return plan, contract.NewFault(contract.PreconditionFailed, "upgrade journal is corrupt", "Inspect the state directory; never guess which binary is installed.")
	}
	if journal != nil && journalVersionForSHA(journal, currentSHA) != "" {
		plan.CurrentVersion = journalVersionForSHA(journal, currentSHA)
	}
	if journal != nil && journal.Status == StatusInstalled && journal.BackupPath != "" && backupPath == "" {
		backupPath = journal.BackupPath
	}
	if backupPath == "" {
		backups, err := m.Backups()
		if err != nil {
			return plan, err
		}
		if len(backups) == 0 {
			return plan, contract.NewFault(contract.NotFound, "no recorded backup is available for rollback", "Upgrade once to create a backup, or pass --backup with a path inside the upgrade state directory.")
		}
		for _, backup := range backups {
			// After a completed rollback the newest copy is a safety copy of
			// the version that was rolled back. Do not offer to re-apply it as
			// the default; select the previous verified backup instead.
			if journal != nil && journal.Status != StatusInstalled && backup.SHA256 == journal.FromSHA256 {
				continue
			}
			backupPath = backup.Path
			break
		}
		if backupPath == "" {
			return plan, contract.NewFault(contract.Conflict, "the target binary already matches the recorded backups", "Choose a different backup or skip the rollback.")
		}
	}
	absBackup, err := safeBackupPath(m.StateDir, m.Target, backupPath)
	if err != nil {
		return plan, err
	}
	meta, err := loadBackupMeta(absBackup)
	if err != nil {
		return plan, err
	}
	sha, size, _, err := fileSHA256(absBackup, MaxArtifactBytes)
	if err != nil {
		return plan, contract.NewFault(contract.NotFound, "recorded rollback backup is missing", "Inspect the upgrade state directory; no binary was replaced.")
	}
	if sha != meta.SHA256 || size <= 0 {
		return plan, contract.NewFault(contract.PreconditionFailed, "rollback backup does not match its recorded SHA256", "Refuse the backup; restore a verified copy from outside the upgrade state.")
	}
	if subtle.ConstantTimeCompare([]byte(sha), []byte(currentSHA)) == 1 {
		return plan, contract.NewFault(contract.Conflict, "the target binary already matches the selected backup", "Choose a different backup or skip the rollback.")
	}
	plan.CurrentSHA256 = currentSHA
	plan.RestoreVersion = meta.Version
	plan.RestorePath = absBackup
	plan.RestoreSHA256 = sha
	plan.SafetyBackup = filepath.Join(backupDir(m.StateDir, m.Target), "rollback-"+backupLabel(plan.CurrentVersion, currentSHA), filepath.Base(m.BinaryPath))
	plan.Changes = []string{
		"back up current " + plan.BinaryPath + " to " + plan.SafetyBackup,
		fmt.Sprintf("verify rollback backup SHA256 %s", sha),
		fmt.Sprintf("atomically restore %s: %s -> %s", plan.BinaryPath, plan.CurrentVersion, meta.Version),
	}
	plan.Confirmation = rollbackToken(plan)
	return plan, nil
}

func rollbackToken(plan RollbackPlan) string {
	return token("rollback", string(plan.Target), plan.StateDir, plan.BinaryPath, plan.CurrentSHA256, plan.RestoreSHA256, plan.SafetyBackup)
}

// ApplyRollback restores one verified backup atomically after a human-visible
// dry-run plan. It records a safety copy of the current binary first.
func (m *Manager) ApplyRollback(ctx context.Context, backupPath, confirm string) (Result, error) {
	if err := m.normalize(); err != nil {
		return Result{}, err
	}
	release, err := m.lock(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	recovery, err := m.recoverLocked()
	if err != nil {
		return Result{}, err
	}
	plan, err := m.prepareRollback(backupPath)
	if err != nil {
		return Result{}, err
	}
	if err := requireConfirmation(plan.Confirmation, confirm, "rollback"); err != nil {
		return Result{}, err
	}
	currentSHA, _, _, err := ensureReplaceable(m.BinaryPath)
	if err != nil {
		return Result{}, err
	}
	if subtle.ConstantTimeCompare([]byte(currentSHA), []byte(plan.CurrentSHA256)) != 1 {
		return Result{}, contract.NewFault(contract.Conflict, "target binary changed while rollback was being planned", "Rerun the rollback dry-run; no replacement was made.")
	}
	if err := os.MkdirAll(filepath.Dir(plan.SafetyBackup), 0o700); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "rollback safety directory could not be created", "Check state-directory permissions; no binary was replaced.")
	}
	if err := os.Chmod(filepath.Dir(plan.SafetyBackup), 0o700); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "rollback safety directory permissions could not be restricted", "Check state-directory ownership; no binary was replaced.")
	}
	safetySHA, _, err := copyFileAtomic(plan.SafetyBackup, m.BinaryPath, 0o700, MaxArtifactBytes)
	if err != nil || subtle.ConstantTimeCompare([]byte(safetySHA), []byte(plan.CurrentSHA256)) != 1 {
		return Result{}, contract.NewFault(contract.Unavailable, "current binary could not be safety-copied", "Check free space; no binary was replaced.")
	}
	safetyMeta := Backup{Path: plan.SafetyBackup, Version: plan.CurrentVersion, SHA256: safetySHA, CreatedAt: nowRFC3339()}
	if info, err := os.Stat(plan.SafetyBackup); err == nil {
		safetyMeta.Size = info.Size()
	}
	raw, err := json.MarshalIndent(safetyMeta, "", "  ")
	if err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "rollback safety metadata could not be encoded", "No binary was replaced.")
	}
	if err := writeFileAtomic(plan.SafetyBackup+".json", append(raw, '\n'), 0o600); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "rollback safety metadata could not be written", "Check state-directory permissions; no binary was replaced.")
	}
	staged, err := stageCopy(plan.RestorePath, filepath.Dir(m.BinaryPath), m.Target)
	if err != nil {
		return Result{}, err
	}
	removeStaged := true
	defer func() {
		if removeStaged {
			_ = os.Remove(staged)
		}
	}()
	journal := Journal{
		Schema: JournalSchema, Target: m.Target, Status: StatusRollingBack, OperationID: operationID(),
		BinaryPath: plan.BinaryPath, FromVersion: plan.CurrentVersion, FromSHA256: plan.CurrentSHA256,
		ToVersion: plan.RestoreVersion, ToSHA256: plan.RestoreSHA256, BackupPath: plan.SafetyBackup,
		BackupSHA256: plan.CurrentSHA256, SourcePath: plan.RestorePath, SourceSHA256: plan.RestoreSHA256, StagedPath: staged,
	}
	path := journalPath(m.StateDir, m.Target)
	if err := saveJournal(path, journal); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "rollback journal could not be written", "Check state-directory permissions; no binary was replaced.")
	}
	if err := os.Rename(staged, m.BinaryPath); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "rollback replacement failed; the journal records the interruption", "Run upgrade rollback again to reconcile, or restore the safety backup explicitly.")
	}
	removeStaged = false
	if err := syncDir(filepath.Dir(m.BinaryPath)); err != nil {
		return Result{}, err
	}
	_ = chmodExecutable(m.BinaryPath)
	journal.Status = StatusRolledBack
	journal.StagedPath = ""
	if err := saveJournal(path, journal); err != nil {
		return Result{}, contract.NewFault(contract.Unavailable, "rollback journal could not record completion", "Run upgrade rollback again to reconcile from the journal.")
	}
	return Result{Plan: plan, Journal: journal, Recovered: &recovery}, nil
}

// Status reads the journal, binary identity, and retained backups. It does not
// mutate and does not acquire the upgrade lock.
func (m *Manager) Status(_ context.Context) (Status, error) {
	if err := m.normalize(); err != nil {
		return Status{}, err
	}
	status := Status{Target: m.Target, BinaryPath: m.BinaryPath, StateDir: m.StateDir, ProcessVersion: m.Env.Version}
	if sha, _, _, err := fileSHA256(m.BinaryPath, MaxArtifactBytes); err == nil {
		status.SHA256 = sha
	}
	journal, err := loadJournal(journalPath(m.StateDir, m.Target))
	if err != nil {
		return status, contract.NewFault(contract.PreconditionFailed, "upgrade journal is corrupt", "Inspect the state directory; never guess which binary is installed.")
	}
	status.Journal = journal
	if journal != nil {
		status.Version = journalVersionForSHA(journal, status.SHA256)
	}
	if journal != nil && !terminalStatus(journal.Status) {
		status.Interrupted = true
		status.RecoveryDetail = string(journal.Status) + "; the next apply or rollback reconciles it under the lock"
	}
	backups, err := m.Backups()
	if err != nil {
		return status, err
	}
	status.Backups = backups
	return status, nil
}

// Backups lists retained backups newest first. Metadata is required; a binary
// without its sidecar is never offered for rollback.
func (m *Manager) Backups() ([]Backup, error) {
	root := backupDir(m.StateDir, m.Target)
	var backups []Backup
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		meta, err := loadBackupMeta(strings.TrimSuffix(path, ".json"))
		if err != nil {
			return nil
		}
		info, err := os.Lstat(meta.Path)
		if err != nil || !info.Mode().IsRegular() || (meta.Size > 0 && info.Size() != meta.Size) {
			return nil
		}
		backups = append(backups, meta)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return []Backup{}, nil
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].CreatedAt > backups[j].CreatedAt })
	return backups, nil
}

func loadBackupMeta(binaryPath string) (Backup, error) {
	raw, err := os.ReadFile(binaryPath + ".json") //nolint:gosec // G304: state-directory backup.
	if err != nil {
		return Backup{}, contract.NewFault(contract.PreconditionFailed, "rollback backup has no verified metadata", "Refuse an unrecorded file; only backups written by this upgrade journal are restorable.")
	}
	var meta Backup
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Path != binaryPath || len(meta.SHA256) != sha256.Size*2 {
		return Backup{}, contract.NewFault(contract.PreconditionFailed, "rollback backup metadata is invalid", "Refuse the backup; restore a verified copy from outside the upgrade state.")
	}
	return meta, nil
}

func safeBackupPath(stateDir string, target Target, candidate string) (string, error) {
	root, err := filepath.Abs(backupDir(stateDir, target))
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", contract.NewFault(contract.NotFound, "rollback backup directory does not exist", "Upgrade once to create a verified backup.")
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", contract.NewFault(contract.NotFound, "rollback backup does not exist", "Inspect the upgrade state directory; no binary was replaced.")
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == "." {
		return "", contract.NewFault(contract.ValidationFailed, "rollback backup must be inside this target's upgrade state directory", "Use a backup recorded by eregion; arbitrary files are never restored.")
	}
	return absolute, nil
}

func stageCopy(source, destinationDir string, target Target) (string, error) {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return "", contract.NewFault(contract.PreconditionFailed, "rollback backup is not a regular file", "Refuse the backup; no binary was replaced.")
	}
	file, err := os.Open(source) //nolint:gosec // G304: verified backup inside the state directory.
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "rollback backup could not be opened", "Check permissions; no binary was replaced.")
	}
	defer func() { _ = file.Close() }()
	return stageReader(io.LimitReader(file, MaxArtifactBytes+1), destinationDir, target)
}

func (m *Manager) lock(ctx context.Context) (func(), error) {
	if err := ensureStateDir(m.StateDir); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "upgrade state directory is not usable", "Check the state-directory path and permissions.")
	}
	if err := ensureStateDir(filepath.Join(m.StateDir, string(m.Target))); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "upgrade target state directory is not usable", "Check the state-directory path and permissions.")
	}
	return acquireLock(ctx, filepath.Join(m.StateDir, "upgrade.lock"), 5*time.Second)
}

func requireConfirmation(want, got, action string) error {
	if got == "" {
		return contract.NewFault(contract.ConfirmationRequired, action+" requires a plan-bound confirmation token", "Run the same command with --dry-run, review the diff, then pass --confirm with its token.")
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return contract.NewFault(contract.ConfirmationRequired, action+" confirmation token does not match this plan", "Rerun with --dry-run; the binary, artifact, or state changed.")
	}
	return nil
}

func token(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

func operationID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("upg_%d", time.Now().UnixNano())
	}
	return "upg_" + hex.EncodeToString(raw)
}

func journalVersionForSHA(journal *Journal, sha string) string {
	if journal == nil || sha == "" || journal.ToSHA256 == "" {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(sha), []byte(journal.ToSHA256)) == 1 {
		return journal.ToVersion
	}
	return ""
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func chmodExecutable(path string) error {
	return os.Chmod(path, 0o755)
}
