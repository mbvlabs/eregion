package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const backupManifestSchema = "eregion/backup-manifest/v1"

// backupManifest is secret-free metadata stored beside an archive. Archive is a
// base name; the manifest never records a credential value.
type backupManifest struct {
	Schema      string    `json:"schema"`
	Resource    string    `json:"resource"`
	Host        string    `json:"host"`
	Engine      Engine    `json:"engine"`
	Version     string    `json:"version"`
	Database    string    `json:"database"`
	Format      string    `json:"format"`
	Archive     string    `json:"archive"`
	EffectID    string    `json:"effect_id"`
	OperationID string    `json:"operation_id,omitempty"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      string    `json:"sha256"`
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"created_at"`
	VerifiedAt  time.Time `json:"verified_at,omitempty"`
}

func validBackupReason(reason string) bool {
	switch reason {
	case "manual", "scheduled", "safety":
		return true
	}
	return false
}

func (s *Service) manifestPath(archive string) string { return archive + ".manifest.json" }

func (s *Service) backup(ctx context.Context, spec Spec, request *BackupRequest, effect effectRecord) (Result, error) {
	result := baseResult(ActionBackup, spec, StateUnknown)
	reason := "manual"
	if request != nil && request.Reason != "" {
		reason = request.Reason
	}
	if !validBackupReason(reason) {
		return result, invalidResource("invalid backup reason", "Use manual, scheduled, or safety.")
	}
	artifact, err := s.backupWithReason(ctx, spec, reason, effect.EffectID, effect.OperationID)
	if err != nil {
		return result, err
	}
	state, stateErr := s.observeState(ctx, spec)
	if stateErr == nil {
		result.State = state
	}
	endpoint, endpointErr := s.endpoint(ctx, spec)
	if endpointErr == nil {
		result.Endpoint = endpoint
	}
	result.Credential = s.credentialReference(spec)
	result.Backup = &artifact
	result.Changed = true
	return result, nil
}

// backupWithReason performs pg_dump and writes the archive plus manifest
// atomically. The archive name is deterministic for the effect id, so a retry
// after a lost acknowledgement reuses a complete verified-able artifact instead
// of creating a duplicate. It does not claim the backup is verified;
// verification runs pg_restore --list against the finished archive.
func (s *Service) backupWithReason(ctx context.Context, spec Spec, reason, effectID, operationID string) (BackupArtifact, error) {
	directory, err := s.requireBackupDir()
	if err != nil {
		return BackupArtifact{}, err
	}
	if strings.TrimSpace(effectID) == "" {
		return BackupArtifact{}, precondition("backup effect identity is missing", "Supply a stable operation/effect id; random backup names are not replay-safe.")
	}
	base := fmt.Sprintf("%s-%s.dump", spec.Name, shortEffectID(effectID))
	archive := filepath.Join(directory, base)
	if artifact, exists, err := s.existingBackup(archive, effectID); err != nil {
		return BackupArtifact{}, err
	} else if exists {
		return artifact, nil
	}
	if err := s.ensureReady(ctx, spec); err != nil {
		return BackupArtifact{}, err
	}
	now := s.now()
	temp := filepath.Join(directory, "."+base+".tmp")
	// An uncommitted temporary file from an interrupted attempt is not evidence.
	_ = os.Remove(temp)
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: service-owned backup directory.
	if err != nil {
		return BackupArtifact{}, unavailable("could not create the backup archive", "Check permissions on the dedicated backup directory.")
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(temp)
		}
	}()
	argv := []string{
		"exec", containerName(spec), "pg_dump",
		"--format=custom", "--no-owner", "--no-privileges",
		"--username=" + spec.Username, "--dbname=" + spec.Database,
	}
	commandResult, err := s.runCommand(ctx, "pg_dump", Command{Name: "docker", Args: argv, Stdout: file})
	if err != nil {
		_ = file.Close()
		return BackupArtifact{}, err
	}
	if commandResult.ExitCode != 0 {
		_ = file.Close()
		return BackupArtifact{}, stepFailure("pg_dump", commandResult)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return BackupArtifact{}, unavailable("could not flush the backup archive", "Check free space and permissions on the backup directory.")
	}
	if err := file.Close(); err != nil {
		return BackupArtifact{}, unavailable("could not close the backup archive", "Check free space and permissions on the backup directory.")
	}
	digest, size, err := fileSHA256(temp)
	if err != nil {
		return BackupArtifact{}, unavailable("could not checksum the backup archive", "Check permissions and free space on the backup directory.")
	}
	if size == 0 {
		return BackupArtifact{}, unavailable("pg_dump produced an empty archive", "Inspect the database and pg_dump diagnostics; an empty archive is not a backup.")
	}
	// Publish the manifest only after the archive is durable. A crash can leave
	// an archive without a manifest; existingBackup treats that as uncommitted
	// and this attempt replaces it deterministically.
	if err := os.Rename(temp, archive); err != nil {
		return BackupArtifact{}, unavailable("could not publish the backup archive", "Check permissions and free space on the backup directory.")
	}
	removeTemp = false
	if err := syncDir(directory); err != nil {
		return BackupArtifact{}, unavailable("could not sync the backup directory", "Check the filesystem and backup directory.")
	}
	manifest := backupManifest{
		Schema: backupManifestSchema, Resource: spec.Name, Host: spec.Host,
		Engine: spec.Engine, Version: spec.Version, Database: spec.Database,
		Format: "pg_dump-custom", Archive: base, EffectID: effectID, OperationID: operationID,
		SizeBytes: size, SHA256: digest, Reason: reason, CreatedAt: now,
	}
	if err := s.writeManifest(archive, manifest); err != nil {
		_ = os.Remove(archive)
		return BackupArtifact{}, err
	}
	return artifactFrom(archive, manifest), nil
}

// existingBackup returns a complete artifact only when the deterministic path
// has a manifest for this exact effect and its checksum matches. An archive
// without a manifest is an uncommitted attempt and is removed before a new
// dump; a manifest from another effect is never overwritten.
func (s *Service) existingBackup(archive, effectID string) (BackupArtifact, bool, error) {
	if _, err := os.Lstat(archive); err != nil {
		if os.IsNotExist(err) {
			return BackupArtifact{}, false, nil
		}
		return BackupArtifact{}, false, unavailable("backup destination cannot be inspected", "Check permissions on the dedicated backup directory.")
	}
	manifest, err := s.readManifest(archive)
	if err != nil {
		if fault := contract.Classify(err); fault.Code == contract.NotFound {
			// No manifest: this path is uncommitted evidence. Remove only the
			// deterministic archive this effect owns and dump again.
			if removeErr := os.Remove(archive); removeErr != nil {
				return BackupArtifact{}, false, unavailable("could not replace an uncommitted backup archive", "Check permissions on the backup directory.")
			}
			return BackupArtifact{}, false, nil
		}
		return BackupArtifact{}, false, err
	}
	if manifest.EffectID != effectID {
		return BackupArtifact{}, false, conflict("backup effect path belongs to another effect", "Use a fresh effect id or inspect the backup directory; this service will not overwrite another effect's artifact.")
	}
	digest, size, err := fileSHA256(archive)
	if err != nil {
		return BackupArtifact{}, false, unavailable("could not checksum the existing backup archive", "Check that the archive is readable.")
	}
	if size != manifest.SizeBytes || digest != manifest.SHA256 {
		return BackupArtifact{}, false, precondition("existing backup archive does not match its manifest", "Do not trust or overwrite the changed archive; inspect the backup directory and take a new effect.")
	}
	return artifactFrom(archive, manifest), true, nil
}

func artifactFrom(archive string, manifest backupManifest) BackupArtifact {
	artifact := BackupArtifact{
		Archive: archive, Manifest: archive + ".manifest.json",
		Resource: manifest.Resource, Host: manifest.Host, Database: manifest.Database,
		EffectID:  manifest.EffectID,
		SizeBytes: manifest.SizeBytes, SHA256: manifest.SHA256,
		CreatedAt: manifest.CreatedAt, Reason: manifest.Reason,
		Verified: !manifest.VerifiedAt.IsZero(),
	}
	if !manifest.VerifiedAt.IsZero() {
		verified := manifest.VerifiedAt
		artifact.VerifiedAt = &verified
	}
	return artifact
}

func (s *Service) writeManifest(archive string, manifest backupManifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.manifestPath(archive), append(encoded, '\n'), 0o600); err != nil {
		return unavailable("could not write the backup manifest", "Check permissions and free space on the backup directory.")
	}
	return nil
}

func (s *Service) readManifest(archive string) (backupManifest, error) {
	raw, err := readProtectedFile(s.manifestPath(archive), 64*1024)
	if err != nil {
		if os.IsNotExist(err) {
			return backupManifest{}, notFound("backup manifest is missing", "Restore the archive's manifest beside it; verification requires the recorded checksum and resource identity.")
		}
		return backupManifest{}, precondition("backup manifest cannot be read safely", "Check that it is a regular 0600 file in the backup directory.")
	}
	var manifest backupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return backupManifest{}, precondition("backup manifest is corrupt", "Inspect the manifest; do not trust the archive without its recorded checksum.")
	}
	if manifest.Schema != backupManifestSchema || manifest.Archive != filepath.Base(archive) ||
		manifest.EffectID == "" || manifest.SHA256 == "" || manifest.SizeBytes < 0 || !validBackupReason(manifest.Reason) {
		return backupManifest{}, precondition("backup manifest is invalid", "Inspect the manifest and archive; do not restore unverified data.")
	}
	return manifest, nil
}

func (s *Service) resolveArchive(archive string) (string, error) {
	directory, err := s.requireBackupDir()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(archive) == "" {
		return "", invalidResource("backup archive is required", "Name an archive in the configured backup directory.")
	}
	resolved, err := withinDir(directory, archive)
	if err != nil {
		return "", invalidResource("backup archive is outside the configured destination", "Use a regular archive file directly inside the dedicated backup directory.")
	}
	return resolved, nil
}

func (s *Service) verify(ctx context.Context, spec Spec, request *VerifyRequest) (Result, error) {
	result := baseResult(ActionVerify, spec, StateUnknown)
	artifact, err := s.verifyArchive(ctx, spec, request.Archive)
	if err != nil {
		return result, err
	}
	state, stateErr := s.observeState(ctx, spec)
	if stateErr == nil {
		result.State = state
	}
	result.Backup = &artifact
	return result, nil
}

// verifyArchive computes the archive digest, compares the manifest, and runs
// pg_restore --list inside the owned container. Both checks must pass.
func (s *Service) verifyArchive(ctx context.Context, spec Spec, archiveArg string) (BackupArtifact, error) {
	archive, err := s.resolveArchive(archiveArg)
	if err != nil {
		return BackupArtifact{}, err
	}
	manifest, err := s.readManifest(archive)
	if err != nil {
		return BackupArtifact{}, err
	}
	if manifest.Resource != spec.Name || manifest.Host != spec.Host || manifest.Database != spec.Database || manifest.Engine != spec.Engine {
		return BackupArtifact{}, precondition("backup belongs to a different resource", "Restore or verify it only against the resource named in its manifest.")
	}
	digest, size, err := fileSHA256(archive)
	if err != nil {
		return BackupArtifact{}, unavailable("could not checksum the backup archive", "Check that the archive is readable.")
	}
	if size != manifest.SizeBytes || digest != manifest.SHA256 {
		return BackupArtifact{}, precondition("backup archive checksum does not match its manifest", "Do not restore a changed or truncated archive; take a fresh backup.")
	}
	if err := s.ensureReady(ctx, spec); err != nil {
		return BackupArtifact{}, err
	}
	file, err := os.Open(archive) //nolint:gosec // G304: resolved under the configured backup directory.
	if err != nil {
		return BackupArtifact{}, unavailable("could not open the backup archive for verification", "Check permissions on the archive.")
	}
	defer func() { _ = file.Close() }()
	argv := []string{"exec", "--interactive", containerName(spec), "pg_restore", "--list"}
	listing, err := s.runCommand(ctx, "pg_restore --list", Command{Name: "docker", Args: argv, Stdin: file})
	if err != nil {
		return BackupArtifact{}, err
	}
	if listing.ExitCode != 0 {
		return BackupArtifact{}, stepFailure("pg_restore --list", listing)
	}
	if strings.TrimSpace(string(listing.Stdout)) == "" {
		return BackupArtifact{}, precondition("pg_restore returned an empty table of contents", "Do not restore an archive without a readable table of contents.")
	}
	manifest.VerifiedAt = s.now()
	if err := s.writeManifest(archive, manifest); err != nil {
		return BackupArtifact{}, err
	}
	return artifactFrom(archive, manifest), nil
}

func (s *Service) ensureReady(ctx context.Context, spec Spec) error {
	if err := s.ensureContainerRunning(ctx, spec); err != nil {
		return err
	}
	probe, err := s.containerExec(ctx, "pg_isready", spec, "pg_isready", "--host=127.0.0.1", "--username="+spec.Username, "--dbname="+spec.Database, "--timeout=3")
	if err != nil {
		return err
	}
	if probe.ExitCode != 0 {
		fault := precondition("database is not accepting connections", "Inspect the container health before backup, verify, or restore.")
		fault.Details = map[string]any{"detail": sanitizeDetail(probe.Stderr)}
		return fault
	}
	return nil
}

// ensureContainerRunning checks only the owned container and its process. It is
// used by recovery, where the target database name may temporarily not exist.
func (s *Service) ensureContainerRunning(ctx context.Context, spec Spec) error {
	labels, exists, err := s.containerLabels(ctx, spec)
	if err != nil {
		return err
	}
	if !exists {
		return precondition("database container does not exist", "Create the resource or recover it before backup, verify, or restore.")
	}
	if !s.labelsOwned(labels, spec) {
		return conflict("container name belongs to a different resource", "Inspect the Docker container and use its real owner; this service will not touch a foreign container.")
	}
	running, err := s.containerRunning(ctx, spec)
	if err != nil {
		return err
	}
	if !running {
		return precondition("database container is not running", "Start the resource or inspect it before using its database.")
	}
	return nil
}

// ListBackups reads valid manifests in the configured destination. A corrupt or
// unsafe manifest is an explicit error, not silently omitted.
func (s *Service) ListBackups() ([]BackupArtifact, error) {
	directory, err := s.requireBackupDir()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(directory, "*.dump.manifest.json"))
	if err != nil {
		return nil, err
	}
	artifacts := make([]BackupArtifact, 0, len(matches))
	for _, manifestPath := range matches {
		archive := strings.TrimSuffix(manifestPath, ".manifest.json")
		manifest, err := s.readManifest(archive)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(archive); err != nil {
			return nil, precondition("backup manifest has no archive", "Inspect the backup directory; do not treat a manifest as a usable backup.")
		}
		artifacts = append(artifacts, artifactFrom(archive, manifest))
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].CreatedAt.After(artifacts[j].CreatedAt) })
	return artifacts, nil
}

var _ = contract.Internal
