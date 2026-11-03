package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/retention"
)

// RetentionFacts is the last durable retention run and the age of the worker
// that owns the daily job. Enabled is false on nodes that do not run the
// control plane; Known is false when the runs could not be read.
type RetentionFacts struct {
	Enabled bool
	Known   bool
	Error   string

	HasRun        bool
	LastRun       RetentionRunFacts
	LastSuccessAt time.Time

	// WorkerAge is how long eregion-worker.service has been continuously
	// active. It is only read when no run has been recorded yet; the startup
	// catch-up and daily job get a grace period before doctor warns.
	WorkerAge      time.Duration
	WorkerAgeKnown bool
}

// RetentionRunFacts is the operator-readable summary of one retention run.
type RetentionRunFacts struct {
	Status         retention.RunStatus
	StartedAt      time.Time
	FinishedAt     *time.Time
	PlannedItems   int
	RemovedItems   int
	PlannedBytes   int64
	ReclaimedBytes int64
	Errors         int
	// Error is the first recorded run error. It lets doctor point at a known
	// fix instead of a generic retry instruction.
	Error string
}

// BuildCacheFacts is the latest durable Buildpacks cache measurement and the
// server policy's absolute cap. Applicable is false on a role that runs no
// builds. Known is false when no fresh measurement exists; Bytes is never
// invented from a synchronous filesystem walk.
type BuildCacheFacts struct {
	Applicable bool
	Known      bool
	Error      string
	Bytes      int64
	CapBytes   int64
}

// GatherRetentionFacts observes the retention run history and the last durable
// Buildpacks cache measurement. It is read-only and never writes a run or a
// policy. The database part is only attempted on the main role because that is
// where the daily retention job records the measurement.
func GatherRetentionFacts(ctx context.Context, role, dsn string) (RetentionFacts, BuildCacheFacts) {
	retentionFacts := RetentionFacts{Enabled: role == "main"}
	cacheFacts := BuildCacheFacts{
		Applicable: buildCacheApplies(role),
		CapBytes:   retention.DefaultPolicy().BuildpackCacheMaxBytes,
	}
	if retentionFacts.Enabled {
		loadRetentionFacts(ctx, dsn, &retentionFacts, &cacheFacts)
	} else if cacheFacts.Applicable {
		cacheFacts.Error = "this role does not read the control database; the main node's retention run records the cache size"
	}
	return retentionFacts, cacheFacts
}

func loadRetentionFacts(ctx context.Context, dsn string, facts *RetentionFacts, cache *BuildCacheFacts) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		facts.Error = "the node has no control database URL"
		return
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		facts.Error = "the control database connection could not be created"
		return
	}
	defer pool.Close()

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	loadRetentionPolicyCap(queryCtx, pool, cache)

	var finished *time.Time
	var cacheBytes *int64
	var cacheMeasuredAt *time.Time
	var cacheError string
	err = pool.QueryRow(queryCtx, `SELECT status,started_at,finished_at,
        jsonb_array_length(planned_items),jsonb_array_length(removed_items),
        planned_bytes,reclaimed_bytes,jsonb_array_length(errors),
        coalesce(errors->0->>'message',''),
        cache_bytes,cache_measured_at,cache_measurement_error
        FROM eregion_retention_runs
        ORDER BY started_at DESC,id DESC LIMIT 1`).
		Scan(&facts.LastRun.Status, &facts.LastRun.StartedAt, &finished,
			&facts.LastRun.PlannedItems, &facts.LastRun.RemovedItems,
			&facts.LastRun.PlannedBytes, &facts.LastRun.ReclaimedBytes, &facts.LastRun.Errors,
			&facts.LastRun.Error,
			&cacheBytes, &cacheMeasuredAt, &cacheError)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		facts.Known = true
		facts.WorkerAge, facts.WorkerAgeKnown = workerActiveAge(ctx)
		cache.Error = "no retention run has recorded a Buildpacks cache measurement yet"
		return
	case err != nil:
		facts.Error = "the retention runs could not be read"
		cache.Error = facts.Error
		return
	}
	facts.HasRun = true
	facts.LastRun.FinishedAt = finished
	facts.Known = true
	applyBuildCacheMeasurement(cache, cacheBytes, cacheMeasuredAt, cacheError, time.Now().UTC())

	var success *time.Time
	if err := pool.QueryRow(queryCtx, `SELECT coalesce(finished_at,started_at) FROM eregion_retention_runs
        WHERE status='succeeded' ORDER BY finished_at DESC NULLS LAST,started_at DESC LIMIT 1`).Scan(&success); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			facts.Known = false
			facts.Error = "the last successful retention run could not be read"
			return
		}
	}
	if success != nil {
		facts.LastSuccessAt = success.UTC()
	}
}

func loadRetentionPolicyCap(ctx context.Context, pool *pgxpool.Pool, cache *BuildCacheFacts) {
	policy := retention.DefaultPolicy()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT retention FROM eregion_server_settings WHERE singleton`).Scan(&raw); err != nil {
		return
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &policy); err != nil {
			return
		}
	}
	if policy.BuildpackCacheMaxBytes > 0 {
		cache.CapBytes = policy.BuildpackCacheMaxBytes
	}
}

func applyBuildCacheMeasurement(cache *BuildCacheFacts, bytes *int64, measuredAt *time.Time, measurementError string, now time.Time) {
	switch {
	case bytes == nil:
		cache.Error = measurementError
		if cache.Error == "" {
			cache.Error = "the latest retention run could not measure the Buildpacks cache"
		}
	case measuredAt == nil:
		cache.Error = "the latest retention run has no cache measurement time"
	case now.Sub(measuredAt.UTC()) > 48*time.Hour:
		cache.Error = "the latest Buildpacks cache measurement is older than 48 hours; the next retention run refreshes it"
	default:
		cache.Known, cache.Bytes = true, *bytes
	}
}

func buildCacheApplies(role string) bool {
	switch role {
	case "main", "application", "builder":
		return true
	}
	return false
}

func retentionLastRun(facts Facts) []Result {
	if !facts.Retention.Enabled {
		return one(Pass, "disk retention runs on the main node", "")
	}
	if !facts.Retention.Known {
		detail := "cannot read the retention runs"
		if facts.Retention.Error != "" {
			detail += ": " + facts.Retention.Error
		}
		return one(Warn, detail, "fix the control database connection, then run eregion doctor")
	}
	if !facts.Retention.HasRun {
		// The daily job also runs at worker start. A fresh host still gets a
		// queue grace before a missing run is actionable; only warn once the
		// worker has been active for more than two days.
		if facts.Retention.WorkerAgeKnown && facts.Retention.WorkerAge > 48*time.Hour {
			return one(Warn,
				fmt.Sprintf("no retention run has been recorded since the worker started %s ago", humanDuration(facts.Retention.WorkerAge)),
				"check the maintenance queue, then run eregion prune --dry-run and eregion prune --confirm <server>")
		}
		return one(Pass, "no retention run yet; the startup catch-up is queued and runs when the maintenance queue is ready", "")
	}

	run := facts.Retention.LastRun
	finished := run.StartedAt
	if run.FinishedAt != nil {
		finished = *run.FinishedAt
	}
	switch run.Status {
	case retention.RunRunning:
		if facts.Now.Sub(run.StartedAt) > 48*time.Hour {
			return one(Warn,
				fmt.Sprintf("the retention run started %s ago has not finished", humanDuration(facts.Now.Sub(run.StartedAt))),
				"inspect eregion operations list --json and the worker logs")
		}
		return one(Pass, "the retention run started at "+run.StartedAt.UTC().Format(time.RFC3339)+" is still running", "")
	case retention.RunSucceeded:
	default:
		detail := fmt.Sprintf("the last retention run %s at %s", run.Status, finished.UTC().Format(time.RFC3339))
		if run.Errors > 0 {
			detail += fmt.Sprintf(" recorded %d error(s)", run.Errors)
		}
		if retentionRegistryStorageError(run.Error) {
			return one(Warn, detail+": the worker could not read the registry storage",
				"run eregion apply on the node to apply host-directories-v2, which grants the worker read access to the registry storage")
		}
		return one(Warn, detail,
			"inspect eregion operations list --json, fix the reported node or registry error, then run eregion prune --dry-run")
	}

	if facts.Retention.LastSuccessAt.IsZero() {
		return one(Warn, "no successful retention run has been recorded", "run eregion prune --dry-run, then eregion prune --confirm <server>")
	}
	since := facts.Now.Sub(facts.Retention.LastSuccessAt)
	if since > 48*time.Hour {
		return one(Warn,
			"the last successful retention run finished "+humanDuration(since)+" ago",
			"check the maintenance queue, then run eregion prune --dry-run and eregion prune --confirm <server>")
	}
	detail := fmt.Sprintf("last retention run %s succeeded: %d item(s) removed, %s reclaimed",
		finished.UTC().Format(time.RFC3339), run.RemovedItems, humanBytes(run.ReclaimedBytes))
	if run.PlannedBytes > 0 {
		detail += fmt.Sprintf(" (the plan reclaimable was %s)", humanBytes(run.PlannedBytes))
	}
	return one(Pass, detail, "")
}

// retentionRegistryStorageError recognizes the permission failure the worker
// records when host-directories-v1 left the registry storage root-only. The
// message text is the durable run error, not a fresh observation.
func retentionRegistryStorageError(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "registry storage") &&
		(strings.Contains(lower, "unreadable") || strings.Contains(lower, "not readable") ||
			strings.Contains(lower, "cannot read") || strings.Contains(lower, "permission denied"))
}

func buildpackCache(facts Facts) []Result {
	if !facts.BuildCache.Applicable {
		return one(Pass, "this role runs no Buildpacks builds", "")
	}
	if !facts.BuildCache.Known {
		detail := "Buildpacks cache size is unknown"
		if facts.BuildCache.Error != "" {
			detail += ": " + facts.BuildCache.Error
		}
		return one(Pass, detail, "the retention run records the cache size on the node; run eregion prune --dry-run to inspect the next plan")
	}
	limit := facts.BuildCache.CapBytes
	if limit <= 0 {
		limit = retention.DefaultPolicy().BuildpackCacheMaxBytes
	}
	detail := fmt.Sprintf("Buildpacks cache uses %s of the %s cap", humanBytes(facts.BuildCache.Bytes), humanBytes(limit))
	if facts.BuildCache.Bytes > limit {
		return one(Warn, detail,
			"run eregion prune --dry-run, then eregion prune --confirm <server>; the daily job also removes the least-recently-used entries")
	}
	return one(Pass, detail, "")
}

func workerActiveAge(ctx context.Context) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveEnterTimestampMonotonic", "--value", "eregion-worker.service") //nolint:gosec // G204: fixed unit name.
	output, err := command.Output()
	if err != nil {
		return 0, false
	}
	microseconds, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || microseconds <= 0 {
		return 0, false
	}
	uptime, ok := uptimeSeconds()
	if !ok {
		return 0, false
	}
	age := time.Duration(uptime)*time.Second - time.Duration(microseconds)*time.Microsecond
	if age < 0 {
		age = 0
	}
	return age, true
}

func uptimeSeconds() (int64, bool) {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return int64(seconds), true
}

func humanBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	const (
		kibibyte = int64(1) << 10
		mebibyte = int64(1) << 20
		gibibyte = int64(1) << 30
	)
	switch {
	case bytes >= gibibyte:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gibibyte))
	case bytes >= mebibyte:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mebibyte))
	case bytes >= kibibyte:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(kibibyte))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func humanDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	days := value.Hours() / 24
	if days >= 1 {
		if days < 2 {
			return "1 day"
		}
		return fmt.Sprintf("%.0f days", days)
	}
	hours := value.Hours()
	if hours < 1 {
		return value.Round(time.Minute).String()
	}
	return fmt.Sprintf("%.0f hours", hours)
}
