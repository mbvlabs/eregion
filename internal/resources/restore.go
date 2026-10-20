package resources

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const restoreJournalSchema = "eregion/restore-journal/v1"

type restoreJournal struct {
	Schema        string    `json:"schema"`
	Resource      string    `json:"resource"`
	Host          string    `json:"host"`
	EffectID      string    `json:"effect_id"`
	OperationID   string    `json:"operation_id,omitempty"`
	Target        string    `json:"target"`
	Temporary     string    `json:"temporary"`
	Previous      string    `json:"previous"`
	Failed        string    `json:"failed"`
	Archive       string    `json:"archive"`
	SafetyArchive string    `json:"safety_archive"`
	Phase         string    `json:"phase"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *Service) readRestoreJournal(spec Spec) (restoreJournal, bool, error) {
	raw, err := readProtectedFile(s.restoreJournalPath(spec.Name), 32*1024)
	if err != nil {
		if os.IsNotExist(err) {
			return restoreJournal{}, false, nil
		}
		return restoreJournal{}, false, precondition("restore journal cannot be read safely", "Inspect the service state directory; do not delete an interrupted restore record.")
	}
	var journal restoreJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return restoreJournal{}, false, precondition("restore journal is corrupt", "Inspect the state directory; do not guess the database names in an interrupted restore.")
	}
	if journal.Schema != restoreJournalSchema || journal.Resource != spec.Name || journal.Host != spec.Host ||
		journal.Target != spec.Database || journal.Temporary == "" || journal.Previous == "" || journal.Failed == "" {
		return restoreJournal{}, false, precondition("restore journal does not belong to this resource", "Inspect the journal; this service will not rename databases it cannot prove belong to the interrupted restore.")
	}
	return journal, true, nil
}

func (s *Service) writeRestoreJournal(spec Spec, journal restoreJournal) error {
	journal.Schema = restoreJournalSchema
	journal.Resource = spec.Name
	journal.Host = spec.Host
	journal.UpdatedAt = s.now()
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(s.resourceDir(spec.Name)); err != nil {
		return unavailable("could not create the protected state directory", "Check permissions on the service state directory.")
	}
	if err := writeAtomic(s.restoreJournalPath(spec.Name), append(encoded, '\n'), 0o600); err != nil {
		return unavailable("could not write the restore journal", "Check permissions on the service state directory; a restore without a durable journal is refused.")
	}
	return nil
}

func (s *Service) removeRestoreJournal(spec Spec) error {
	if err := os.Remove(s.restoreJournalPath(spec.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(s.resourceDir(spec.Name))
}

func (s *Service) restore(ctx context.Context, spec Spec, request *RestoreRequest, effect effectRecord) (Result, error) {
	result := baseResult(ActionRestore, spec, StateUnknown)
	journal, journalExists, err := s.readRestoreJournal(spec)
	if err != nil {
		return result, err
	}
	if journalExists {
		if journal.Phase == "switched-healthy" {
			if effect.Phase == "cutover-complete" || journal.EffectID == effect.EffectID {
				// The cutover already completed durably. Never repeat it, even
				// when the previous acknowledgement was lost.
				return s.finalizeRestore(ctx, spec, journal, effect)
			}
			// A different restore completed cutover but did not clean up. Finish
			// its cleanup, then apply this request against the observed database.
			if _, err := s.finishRestore(ctx, spec, journal); err != nil {
				return result, err
			}
		} else {
			// Do not overwrite an incomplete journal. Recover the previous
			// database first, then this request can take a fresh safety backup.
			recovery, err := s.recoverInterrupted(ctx, spec)
			result.Recovery = &recovery
			if err != nil {
				return result, err
			}
		}
	} else if effect.Phase == "cutover-complete" {
		// A previous attempt durably completed cutover and removed its journal,
		// but the final effect acknowledgement was lost.
		return s.finalizeRestore(ctx, spec, restoreJournal{}, effect)
	}

	restoredFrom, err := s.verifyArchive(ctx, spec, request.Archive)
	if err != nil {
		return result, err
	}
	result.RestoredFrom = &restoredFrom

	safety, err := s.backupWithReason(ctx, spec, "safety", effect.EffectID+"\x00safety", effect.OperationID)
	if err != nil {
		return result, err
	}
	safety, err = s.verifyArchive(ctx, spec, safety.Archive)
	if err != nil {
		return result, err
	}
	result.SafetyBackup = &safety

	suffix, err := randomHex(5)
	if err != nil {
		return result, err
	}
	journal = restoreJournal{
		EffectID: effect.EffectID, OperationID: effect.OperationID,
		Target:    spec.Database,
		Temporary: spec.Database + "__restore_" + suffix,
		Previous:  spec.Database + "__previous_" + suffix,
		Failed:    spec.Database + "__failed_" + suffix,
		Archive:   request.Archive, SafetyArchive: safety.Archive, Phase: "prepared",
	}
	if err := s.writeRestoreJournal(spec, journal); err != nil {
		return result, err
	}
	result.Changed = true

	warnings, err := s.applyRestore(ctx, spec, &journal, restoredFrom.Archive)
	if err != nil {
		recovery, recoveryErr := s.recoverInterrupted(context.Background(), spec)
		result.Recovery = &recovery
		if recoveryErr != nil || !recovery.Succeeded {
			fault := contract.Classify(err)
			return result, &contract.Fault{
				Code:    contract.Internal,
				Message: "restore failed and the previous database could not be recovered",
				Hint:    "Inspect the restore journal and database names immediately; do not run another restore.",
				Details: map[string]any{"restore_error": fault.Code, "recovery": recovery.Detail, "safety_archive": safety.Archive},
				Cause:   err,
			}
		}
		fault := contract.Classify(err)
		hint := "The previous database was recovered from the safety point; inspect the failed restore before retrying."
		return result, &contract.Fault{
			Code: fault.Code, Message: "restore failed; previous database recovered", Hint: hint,
			Details: map[string]any{"restore_error": fault.Code, "safety_archive": safety.Archive},
			Cause:   err,
		}
	}

	endpoint, err := s.endpoint(ctx, spec)
	if err != nil {
		result.Warnings = append(result.Warnings, "restore succeeded but the published endpoint could not be observed")
	}
	result.State = StateReady
	result.Endpoint = endpoint
	result.Credential = s.credentialReference(spec)
	result.Health = &Health{Status: HealthHealthy, Endpoint: endpoint, Detail: "restored database accepted a read probe", CheckedAt: s.now()}
	result.Warnings = append(result.Warnings, warnings...)
	result.RestoredFrom = &restoredFrom
	result.SafetyBackup = &safety

	// Record the successful cutover before any cleanup or final acknowledgement.
	// A retry after this point finalizes without replaying the restore.
	if err := s.updateEffectPhase(effect.EffectID, "cutover-complete", &result); err != nil {
		return result, err
	}
	cleanupWarnings, err := s.finishRestore(ctx, spec, journal)
	if err != nil {
		return result, err
	}
	result.Warnings = append(result.Warnings, cleanupWarnings...)
	result.Warnings = append(result.Warnings, "applications must reconnect after the restore cutover")
	return result, nil
}

// finalizeRestore completes an already-cut-over restore without reapplying it.
// It is used when a retry observes durable cutover evidence but the original
// acknowledgement was lost.
func (s *Service) finalizeRestore(ctx context.Context, spec Spec, journal restoreJournal, effect effectRecord) (Result, error) {
	result := baseResult(ActionRestore, spec, StateUnknown)
	if effect.Result != nil {
		result = *effect.Result
	}
	if journal.Target != "" {
		if err := s.ensureContainerRunning(ctx, spec); err != nil {
			return result, err
		}
		targetExists, err := s.databaseExists(ctx, spec, journal.Target)
		if err != nil {
			return result, err
		}
		if !targetExists {
			return result, precondition(
				"durable restore evidence says cutover completed, but the target database is absent",
				"Do not replay the restore; inspect the restore journal and database immediately.",
			)
		}
		warnings, err := s.finishRestore(ctx, spec, journal)
		if err != nil {
			return result, err
		}
		result.Warnings = append(result.Warnings, warnings...)
	}
	if result.RestoredFrom == nil && journal.Archive != "" {
		if archive, err := s.resolveArchive(journal.Archive); err == nil {
			if manifest, err := s.readManifest(archive); err == nil {
				artifact := artifactFrom(archive, manifest)
				result.RestoredFrom = &artifact
			}
		}
	}
	if result.SafetyBackup == nil && journal.SafetyArchive != "" {
		if archive, err := s.resolveArchive(journal.SafetyArchive); err == nil {
			if manifest, err := s.readManifest(archive); err == nil {
				artifact := artifactFrom(archive, manifest)
				result.SafetyBackup = &artifact
			}
		}
	}
	if state, err := s.observeState(ctx, spec); err == nil {
		result.State = state
	}
	if endpoint, err := s.endpoint(ctx, spec); err == nil {
		result.Endpoint = endpoint
		if result.Health != nil {
			result.Health.Endpoint = endpoint
		}
	}
	result.Credential = s.credentialReference(spec)
	result.Replayed = true
	return result, nil
}

// finishRestore removes only post-cutover artifacts. It never renames or
// reapplies the target database.
func (s *Service) finishRestore(ctx context.Context, spec Spec, journal restoreJournal) ([]string, error) {
	var warnings []string
	for _, database := range []string{journal.Temporary, journal.Previous, journal.Failed} {
		if database == "" {
			continue
		}
		if err := s.dropDatabase(ctx, spec, database); err != nil {
			warnings = append(warnings, "restored database is healthy, but cleanup database "+database+" could not be dropped")
		}
	}
	if err := s.removeRestoreJournal(spec); err != nil {
		warnings = append(warnings, "restored database is healthy, but the restore journal could not be removed")
	}
	return warnings, nil
}

func (s *Service) applyRestore(ctx context.Context, spec Spec, journal *restoreJournal, archive string) ([]string, error) {
	var warnings []string
	if err := s.createDatabase(ctx, spec, journal.Temporary); err != nil {
		return warnings, err
	}
	journal.Phase = "temporary-created"
	if err := s.writeRestoreJournal(spec, *journal); err != nil {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, err
	}

	file, err := os.Open(archive) //nolint:gosec // G304: verified archive under the backup directory.
	if err != nil {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, unavailable("could not open the verified backup archive", "Check permissions on the archive.")
	}
	argv := []string{
		"exec", "--interactive", containerName(spec), "pg_restore",
		"--no-owner", "--no-privileges", "--exit-on-error",
		"--username=" + spec.Username, "--dbname=" + journal.Temporary,
	}
	restored, restoreErr := s.runCommand(ctx, "pg_restore", Command{Name: "docker", Args: argv, Stdin: file})
	_ = file.Close()
	if restoreErr != nil {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, restoreErr
	}
	if restored.ExitCode != 0 {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, stepFailure("pg_restore", restored)
	}
	if err := s.probeDatabase(ctx, spec, journal.Temporary, "restored temporary database"); err != nil {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, err
	}

	journal.Phase = "cutover"
	if err := s.writeRestoreJournal(spec, *journal); err != nil {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, err
	}
	if err := s.terminateConnections(ctx, spec, journal.Target, journal.Temporary); err != nil {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, err
	}
	if err := s.renameDatabase(ctx, spec, journal.Target, journal.Previous); err != nil {
		_ = s.dropDatabase(context.Background(), spec, journal.Temporary)
		return warnings, err
	}
	journal.Phase = "previous-renamed"
	_ = s.writeRestoreJournal(spec, *journal)
	if err := s.renameDatabase(ctx, spec, journal.Temporary, journal.Target); err != nil {
		return warnings, err
	}
	journal.Phase = "switched"
	if err := s.writeRestoreJournal(spec, *journal); err != nil {
		return warnings, err
	}
	if err := s.probeDatabase(ctx, spec, journal.Target, "restored database"); err != nil {
		return warnings, err
	}
	// Persist successful cutover before any cleanup. The caller also writes an
	// effect phase; either durable marker lets a retry finalize without a second
	// cutover.
	journal.Phase = "switched-healthy"
	if err := s.writeRestoreJournal(spec, *journal); err != nil {
		return warnings, err
	}
	return warnings, nil
}

// recoverInterrupted repairs the database names recorded by an interrupted
// restore. It uses a fresh bounded context so caller cancellation does not stop
// a safety-critical rollback.
func (s *Service) recover(ctx context.Context, spec Spec) (Result, error) {
	result := baseResult(ActionRecover, spec, StateUnknown)
	recovery, err := s.recoverInterrupted(ctx, spec)
	result.Recovery = &recovery
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
	result.Changed = recovery.Attempted && recovery.Succeeded
	if !recovery.Required {
		result.Warnings = append(result.Warnings, "no interrupted restore journal was present")
	}
	return result, nil
}

func (s *Service) recoverInterrupted(ctx context.Context, spec Spec) (Recovery, error) {
	journal, exists, err := s.readRestoreJournal(spec)
	if err != nil {
		return Recovery{Required: true, Attempted: false}, err
	}
	if !exists {
		return Recovery{Required: false, Attempted: false}, nil
	}
	recovery := Recovery{Required: true, Attempted: true}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), s.recoveryTimeout())
	defer cancel()
	if err := s.ensureContainerRunning(recoveryCtx, spec); err != nil {
		recovery.Detail = "database container is not ready; the restore journal was left in place"
		return recovery, err
	}
	targetExists, err := s.databaseExists(recoveryCtx, spec, journal.Target)
	if err != nil {
		return recovery, err
	}
	previousExists, err := s.databaseExists(recoveryCtx, spec, journal.Previous)
	if err != nil {
		return recovery, err
	}
	temporaryExists, err := s.databaseExists(recoveryCtx, spec, journal.Temporary)
	if err != nil {
		return recovery, err
	}
	failedExists, err := s.databaseExists(recoveryCtx, spec, journal.Failed)
	if err != nil {
		return recovery, err
	}

	switch {
	case targetExists && previousExists:
		// The target was renamed aside and either the restored database was
		// switched in or the second rename never happened. Return to the old
		// database; it is the last known good state.
		if err := s.terminateConnections(recoveryCtx, spec, journal.Target, journal.Temporary, journal.Previous); err != nil {
			return recovery, err
		}
		if failedExists {
			if err := s.dropDatabase(recoveryCtx, spec, journal.Failed); err != nil {
				return recovery, err
			}
		}
		if err := s.renameDatabase(recoveryCtx, spec, journal.Target, journal.Failed); err != nil {
			recovery.Detail = "target could not be renamed aside"
			return recovery, err
		}
		if err := s.renameDatabase(recoveryCtx, spec, journal.Previous, journal.Target); err != nil {
			recovery.Detail = "previous database could not be renamed back; target data remains in " + journal.Failed
			return recovery, err
		}
		if temporaryExists {
			if err := s.dropDatabase(recoveryCtx, spec, journal.Temporary); err != nil {
				return recovery, err
			}
		}
		if err := s.dropDatabase(recoveryCtx, spec, journal.Failed); err != nil {
			recovery.Detail = "previous database was restored but the failed restore database could not be dropped"
			return recovery, err
		}
		recovery.Detail = "target was renamed aside and the previous database was restored"
	case previousExists && !targetExists:
		// Crash after the first rename, before the second. Put the old database
		// back and discard the temporary restore.
		if err := s.renameDatabase(recoveryCtx, spec, journal.Previous, journal.Target); err != nil {
			recovery.Detail = "previous database could not be renamed back"
			return recovery, err
		}
		if temporaryExists {
			if err := s.dropDatabase(recoveryCtx, spec, journal.Temporary); err != nil {
				return recovery, err
			}
		}
		recovery.Detail = "previous database was renamed back before cutover completed"
	case targetExists && !previousExists:
		// No cutover rename happened, or a successful restore was already
		// switched in. Keep the target and remove only temporary artifacts.
		if temporaryExists {
			if err := s.dropDatabase(recoveryCtx, spec, journal.Temporary); err != nil {
				return recovery, err
			}
		}
		if failedExists {
			if err := s.dropDatabase(recoveryCtx, spec, journal.Failed); err != nil {
				return recovery, err
			}
		}
		recovery.Detail = "target database was already present; temporary restore artifacts removed"
	default:
		recovery.Detail = "neither the target nor the previous database exists; manual recovery is required"
		return recovery, precondition("interrupted restore cannot be recovered automatically", "Inspect the restore journal and PostgreSQL databases immediately; no automatic rename is safe.")
	}
	if err := s.removeRestoreJournal(spec); err != nil {
		recovery.Detail += "; journal could not be removed"
		return recovery, err
	}
	recovery.Succeeded = true
	return recovery, nil
}

func (s *Service) recoveryTimeout() time.Duration {
	timeout := 2 * s.config.CommandTimeout
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}
	return timeout
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
}

func (s *Service) psql(ctx context.Context, step string, spec Spec, database, sql string) (CommandResult, error) {
	argv := []string{
		"exec", containerName(spec), "psql", "--no-psqlrc", "--set", "ON_ERROR_STOP=1",
		"--username=" + spec.Username, "--dbname=" + database, "--command=" + sql,
	}
	result, err := s.runCommand(ctx, step, Command{Name: "docker", Args: argv})
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return result, stepFailure(step, result)
	}
	return result, nil
}

func (s *Service) databaseExists(ctx context.Context, spec Spec, database string) (bool, error) {
	sql := "SELECT 1 FROM pg_database WHERE datname=" + quoteLiteral(database)
	result, err := s.psql(ctx, "inspect database "+database, spec, "postgres", sql)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(result.Stdout), "1"), nil
}

func (s *Service) createDatabase(ctx context.Context, spec Spec, database string) error {
	_, err := s.psql(ctx, "create temporary database", spec, "postgres", "CREATE DATABASE "+quoteIdentifier(database))
	return err
}

func (s *Service) dropDatabase(ctx context.Context, spec Spec, database string) error {
	_, err := s.psql(ctx, "drop database "+database, spec, "postgres", "DROP DATABASE IF EXISTS "+quoteIdentifier(database)+" WITH (FORCE)")
	return err
}

func (s *Service) renameDatabase(ctx context.Context, spec Spec, from, to string) error {
	_, err := s.psql(ctx, "rename database", spec, "postgres", "ALTER DATABASE "+quoteIdentifier(from)+" RENAME TO "+quoteIdentifier(to))
	return err
}

func (s *Service) terminateConnections(ctx context.Context, spec Spec, databases ...string) error {
	quoted := make([]string, 0, len(databases))
	for _, database := range databases {
		quoted = append(quoted, quoteLiteral(database))
	}
	sql := "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN (" + strings.Join(quoted, ",") + ") AND pid <> pg_backend_pid()"
	_, err := s.psql(ctx, "terminate database connections", spec, "postgres", sql)
	return err
}

func (s *Service) probeDatabase(ctx context.Context, spec Spec, database, label string) error {
	result, err := s.psql(ctx, "probe "+label, spec, database, "SELECT 1")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result.Stdout)) == "" {
		return precondition(label+" did not answer a read probe", "Do not treat a restore as successful without a database read.")
	}
	return nil
}
