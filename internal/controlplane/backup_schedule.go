package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// ScheduleBackups submits every backup whose policy says it is due at now and
// returns the operations it accepted (or recovered). A policy is the Policy
// carried by the most recent platform.backup.create submission for a resource;
// scheduled submissions repeat it, so a declared schedule sustains itself. A
// resource that a later platform.resource.destroy removed stops being scheduled.
//
// Every worker on every control plane may run this concurrently and repeatedly:
// the idempotency key is derived from the resource and the schedule slot (never
// from the wall clock), so a restart, a retry, or a second worker resolves to the
// same durable operation instead of a second backup.
func (s *Service) ScheduleBackups(ctx context.Context, now time.Time, logger *slog.Logger) ([]contract.Operation, error) {
	if logger == nil {
		logger = slog.Default()
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (o.target, o.input->'spec'->>'name') o.target, o.input, o.created_at
        FROM eregion_operations o
        WHERE o.kind=$1 AND jsonb_typeof(o.input->'policy')='object'
        ORDER BY o.target, o.input->'spec'->>'name', o.created_at DESC, o.id DESC`, resources.KindBackupCreate)
	if err != nil {
		return nil, unavailable(err)
	}
	type candidate struct {
		target  string
		input   resources.BackupCreateInput
		basedOn time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var raw []byte
		if err := rows.Scan(&c.target, &raw, &c.basedOn); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		if err := json.Unmarshal(raw, &c.input); err != nil || c.input.Policy == nil {
			continue // Not a policy this version understands; never guess a schedule.
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, unavailable(err)
	}
	var accepted []contract.Operation
	for _, c := range candidates {
		name := c.input.Spec.Name
		destroyed, err := s.resourceDestroyedAfter(ctx, c.target, name, c.basedOn)
		if err != nil {
			return accepted, err
		}
		if destroyed {
			continue
		}
		slot, err := c.input.Policy.Schedule.Next(c.basedOn)
		if err != nil {
			logger.Warn("backup policy has no valid next run", "resource", name, "error", contract.Classify(err).Message)
			continue
		}
		if slot.After(now) {
			continue
		}
		input := c.input
		input.Reason = "scheduled"
		raw, err := json.Marshal(input)
		if err != nil {
			return accepted, err
		}
		op, err := s.Submit(ctx, operations.Submission{
			Kind: resources.KindBackupCreate, Target: c.target, Actor: scheduledBackupActor,
			IdempotencyKey: scheduledBackupKey(name, slot), Input: raw,
		})
		if err != nil {
			// One broken policy (an unconfigured host, a changed policy that binds
			// different inputs to a slot already taken) must not starve the rest.
			logger.Warn("scheduled backup was not accepted", "resource", name, "error", contract.Classify(err).Message)
			continue
		}
		accepted = append(accepted, op)
	}
	return accepted, nil
}

// scheduledBackupKey is deterministic in (resource, slot). The slot is the
// schedule instant, not the time a worker noticed it.
func scheduledBackupKey(resource string, slot time.Time) string {
	return fmt.Sprintf("scheduled-backup:%s:%d", resource, slot.UTC().Unix())
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
