package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/resources"
	"github.com/riverqueue/river"
)

// backupScheduleInterval is how often a worker looks for due backups. A policy
// resolution finer than a minute does not exist (cron has minute resolution
// and intervals start at 60 seconds).
const backupScheduleInterval = time.Minute

// scheduledBackupActor owns every backup this scheduler submits. It is a system
// actor, so the audit trail distinguishes scheduled from operator backups.
var scheduledBackupActor = contract.Actor{Kind: "system", Subject: "backup-scheduler"}

type backupScheduleArgs struct{}

func (backupScheduleArgs) Kind() string { return "backup_schedule" }

type backupScheduleWorker struct {
	river.WorkerDefaults[backupScheduleArgs]
	service *Service
	logger  *slog.Logger
}

func (w *backupScheduleWorker) Work(ctx context.Context, _ *river.Job[backupScheduleArgs]) error {
	_, err := w.service.ScheduleBackups(ctx, time.Now().UTC(), w.logger)
	return err
}

// backupSchedule is one declared policy for one resource. A declaration from
// platform.backup.schedule wins over a policy embedded in a backup.create
// submission; both forms keep working.
type backupSchedule struct {
	target      string
	spec        resources.Spec
	policy      resources.Policy
	declaredAt  time.Time
	lastAttempt time.Time
}

// ScheduleBackups submits every backup whose policy says it is due at now and
// returns the operations it accepted (or recovered). A policy comes from the
// most recent successful platform.backup.schedule declaration for a resource, or
// from the Policy carried by the most recent platform.backup.create submission
// (the compatibility path). Scheduled submissions repeat the policy, so a
// declared schedule sustains itself. A resource that a later
// platform.resource.destroy removed stops being scheduled.
//
// After the due check it reconciles each policy: when VerifyAfter is set it
// submits a verification for each completed scheduled backup that has a readable
// archive, and it prunes archives outside the declared retention.
//
// Every worker on every control plane may run this concurrently and repeatedly:
// the backup idempotency key is derived from the resource and the schedule slot
// (never from the wall clock), so a restart, a retry, or a second worker resolves
// to the same durable operation instead of a second backup for the same window.
func (s *Service) ScheduleBackups(ctx context.Context, now time.Time, logger *slog.Logger) ([]contract.Operation, error) {
	if logger == nil {
		logger = slog.Default()
	}
	candidates, err := s.backupSchedules(ctx)
	if err != nil {
		return nil, err
	}
	var accepted []contract.Operation
	for _, c := range candidates {
		destroyed, err := s.resourceDestroyedAfter(ctx, c.target, c.spec.Name, c.declaredAt)
		if err != nil {
			return accepted, err
		}
		if destroyed {
			continue
		}
		slot, due, err := nextScheduledRun(c.policy, c.declaredAt, c.lastAttempt, now)
		if err != nil {
			logger.Warn("backup policy has no valid next run", "resource", c.spec.Name, "error", contract.Classify(err).Message)
			continue
		}
		if !due {
			continue
		}
		input := resources.BackupCreateInput{Spec: c.spec, Reason: "scheduled", Policy: &c.policy}
		raw, err := json.Marshal(input)
		if err != nil {
			return accepted, err
		}
		op, err := s.Submit(ctx, operations.Submission{
			Kind: resources.KindBackupCreate, Target: c.target, Actor: scheduledBackupActor,
			IdempotencyKey: scheduledBackupKey(c.spec.Name, slot), Input: raw,
		})
		if err != nil {
			// One broken policy (an unconfigured host, a changed policy that binds
			// different inputs to a slot already taken) must not starve the rest.
			logger.Warn("scheduled backup was not accepted", "resource", c.spec.Name, "error", contract.Classify(err).Message)
			continue
		}
		accepted = append(accepted, op)
	}
	if err := s.reconcileBackupPolicies(ctx, candidates, now, logger); err != nil {
		return accepted, err
	}
	return accepted, nil
}

// nextScheduledRun returns the first slot after the later of the declaration and
// the most recent attempt. Using an attempt timestamp (rather than the wall
// clock) makes a repeated tick converge; the idempotency key makes a concurrent
// tick converge too.
func nextScheduledRun(policy resources.Policy, declaredAt, lastAttempt, now time.Time) (time.Time, bool, error) {
	base := declaredAt.UTC()
	if lastAttempt.After(base) {
		base = lastAttempt.UTC()
	}
	slot, err := policy.Schedule.Next(base)
	if err != nil {
		return time.Time{}, false, err
	}
	return slot, !slot.After(now.UTC()), nil
}

// scheduledBackupKey is deterministic in (resource, slot). The slot is the
// schedule instant, not the time a worker noticed it.
func scheduledBackupKey(resource string, slot time.Time) string {
	return fmt.Sprintf("scheduled-backup:%s:%d", resource, slot.UTC().Unix())
}

// backupSchedules reads the latest declaration and the compatibility policy from
// create submissions, then attaches the latest scheduled attempt for each
// resource. Declarations take precedence over embedded policies.
func (s *Service) backupSchedules(ctx context.Context) ([]backupSchedule, error) {
	byKey := map[string]backupSchedule{}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (o.target, o.input->'spec'->>'name') o.target, o.input, o.created_at
        FROM eregion_operations o
        WHERE o.kind=$1 AND o.state='succeeded'
        ORDER BY o.target, o.input->'spec'->>'name', o.created_at DESC, o.id DESC`, resources.KindBackupSchedule)
	if err != nil {
		return nil, unavailable(err)
	}
	for rows.Next() {
		var target string
		var raw []byte
		var createdAt time.Time
		if err := rows.Scan(&target, &raw, &createdAt); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		var input resources.BackupScheduleInput
		if err := json.Unmarshal(raw, &input); err != nil || input.Spec.Name == "" {
			continue
		}
		policy := input.Policy
		byKey[scheduleKey(target, input.Spec.Name)] = backupSchedule{
			target: target, spec: input.Spec, policy: policy, declaredAt: createdAt,
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, unavailable(err)
	}

	rows, err = s.pool.Query(ctx, `SELECT DISTINCT ON (o.target, o.input->'spec'->>'name') o.target, o.input, o.created_at
        FROM eregion_operations o
        WHERE o.kind=$1 AND jsonb_typeof(o.input->'policy')='object'
        ORDER BY o.target, o.input->'spec'->>'name', o.created_at DESC, o.id DESC`, resources.KindBackupCreate)
	if err != nil {
		return nil, unavailable(err)
	}
	for rows.Next() {
		var target string
		var raw []byte
		var createdAt time.Time
		if err := rows.Scan(&target, &raw, &createdAt); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		var input resources.BackupCreateInput
		if err := json.Unmarshal(raw, &input); err != nil || input.Policy == nil {
			continue // Not a policy this version understands; never guess a schedule.
		}
		key := scheduleKey(target, input.Spec.Name)
		if _, declared := byKey[key]; declared {
			continue // An explicit declaration is authoritative.
		}
		byKey[key] = backupSchedule{
			target: target, spec: input.Spec, policy: *input.Policy, declaredAt: createdAt,
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, unavailable(err)
	}

	attempts, err := s.lastScheduledAttempts(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]backupSchedule, 0, len(byKey))
	for key, candidate := range byKey {
		candidate.lastAttempt = attempts[key]
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func scheduleKey(target, name string) string { return target + "\x00" + name }

func (s *Service) lastScheduledAttempts(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `SELECT target, input->'spec'->>'name', max(created_at)
        FROM eregion_operations
        WHERE kind=$1 AND actor_subject=$2
        GROUP BY target, input->'spec'->>'name'`, resources.KindBackupCreate, scheduledBackupActor.Subject)
	if err != nil {
		return nil, unavailable(err)
	}
	attempts := map[string]time.Time{}
	for rows.Next() {
		var target, name string
		var at time.Time
		if err := rows.Scan(&target, &name, &at); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		attempts[scheduleKey(target, name)] = at
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, unavailable(err)
	}
	return attempts, nil
}

// reconcileBackupPolicies enforces VerifyAfter and Retention after the due
// check. Both phases use only manifests the resource service can read, so a
// corrupt or foreign file is never deleted on a guess.
func (s *Service) reconcileBackupPolicies(ctx context.Context, candidates []backupSchedule, now time.Time, logger *slog.Logger) error {
	services := resources.DefaultServices()
	for _, c := range candidates {
		service, err := services.Service(ctx, c.spec.Host)
		if err != nil {
			continue // No resource host in this process: nothing to reconcile.
		}
		artifacts, err := service.ListBackups()
		if err != nil {
			logger.Warn("backup retention could not read the backup directory", "resource", c.spec.Name, "error", contract.Classify(err).Message)
			continue
		}
		owned := make([]resources.BackupArtifact, 0, len(artifacts))
		for _, artifact := range artifacts {
			if artifact.Resource == c.spec.Name && artifact.Host == c.spec.Host {
				owned = append(owned, artifact)
			}
		}
		if c.policy.VerifyAfter {
			if err := s.verifyScheduledBackups(ctx, c, owned, logger); err != nil {
				return err
			}
		}
		if _, prune, err := c.policy.Retention.SelectRetained(owned, now); err != nil {
			logger.Warn("backup retention policy is invalid", "resource", c.spec.Name, "error", contract.Classify(err).Message)
		} else {
			for _, artifact := range prune {
				if err := removeBackupArtifact(artifact); err != nil {
					logger.Warn("expired backup could not be removed", "resource", c.spec.Name, "archive", artifact.Archive, "error", contract.Classify(err).Message)
				}
			}
		}
	}
	return nil
}

func (s *Service) verifyScheduledBackups(ctx context.Context, c backupSchedule, artifacts []resources.BackupArtifact, logger *slog.Logger) error {
	rows, err := s.pool.Query(ctx, `SELECT id FROM eregion_operations
        WHERE kind=$1 AND actor_subject=$2 AND target=$3 AND input->'spec'->>'name'=$4 AND state='succeeded'
        ORDER BY created_at DESC, id DESC LIMIT 20`,
		resources.KindBackupCreate, scheduledBackupActor.Subject, c.target, c.spec.Name)
	if err != nil {
		return unavailable(err)
	}
	var ids []contract.OperationID
	for rows.Next() {
		var id contract.OperationID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return unavailable(err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return unavailable(err)
	}
	byEffect := map[string]resources.BackupArtifact{}
	for _, artifact := range artifacts {
		if artifact.EffectID != "" {
			byEffect[artifact.EffectID] = artifact
		}
	}
	for _, id := range ids {
		artifact, ok := byEffect[string(id)]
		if !ok {
			continue // The archive is missing or still being written.
		}
		input, err := json.Marshal(resources.BackupVerifyInput{Spec: c.spec, Archive: artifact.Archive})
		if err != nil {
			return err
		}
		_, err = s.Submit(ctx, operations.Submission{
			Kind: resources.KindBackupVerify, Target: c.target, Actor: scheduledBackupActor,
			IdempotencyKey: "scheduled-verify:" + string(id), Input: input,
		})
		if err != nil {
			logger.Warn("scheduled verification was not accepted", "resource", c.spec.Name, "backup", id, "error", contract.Classify(err).Message)
			continue
		}
	}
	return nil
}

// removeBackupArtifact deletes one archive and its manifest. Paths came from the
// resource service's manifest listing, and both must remain regular files inside
// one directory; anything else is refused.
func removeBackupArtifact(artifact resources.BackupArtifact) error {
	if artifact.Archive == "" || artifact.Manifest == "" {
		return errors.New("artifact has no archive or manifest path")
	}
	if !filepath.IsAbs(artifact.Archive) || !filepath.IsAbs(artifact.Manifest) || filepath.Dir(artifact.Archive) != filepath.Dir(artifact.Manifest) {
		return contract.NewFault(contract.PreconditionFailed, "refusing to prune an unexpected backup path", "Inspect the backup directory; retention only removes archives listed beside their own manifest.")
	}
	for _, path := range []string{artifact.Archive, artifact.Manifest} {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return contract.NewFault(contract.Unavailable, "expired backup cannot be inspected", "Check permissions on the backup directory.")
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return contract.NewFault(contract.PreconditionFailed, "refusing to prune a non-regular backup path", "Inspect the backup directory; retention never follows links.")
		}
	}
	if err := os.Remove(artifact.Archive); err != nil && !os.IsNotExist(err) {
		return contract.NewFault(contract.Unavailable, "expired backup archive could not be removed", "Check permissions on the backup directory.")
	}
	if err := os.Remove(artifact.Manifest); err != nil && !os.IsNotExist(err) {
		return contract.NewFault(contract.Unavailable, "expired backup manifest could not be removed", "Check permissions on the backup directory.")
	}
	return nil
}

func (s *Service) resourceDestroyedAfter(ctx context.Context, target, name string, after time.Time) (bool, error) {
	var destroyed bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM eregion_operations
        WHERE kind=$1 AND target=$2 AND input->'spec'->>'name'=$3 AND state='succeeded' AND updated_at > $4)`,
		resources.KindResourceDestroy, target, name, after).Scan(&destroyed); err != nil {
		return false, unavailable(err)
	}
	return destroyed, nil
}
