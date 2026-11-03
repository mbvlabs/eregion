package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	// active. It is only read when no run has been recorded yet, so a fresh
	// installation has two days to produce its first daily run before doctor
	// warns.
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
}

// BuildCacheFacts is the measured Buildpacks cache and the server policy's
// absolute cap. Applicable is false on a role that runs no builds.
type BuildCacheFacts struct {
	Applicable bool
	Known      bool
	Error      string
	Bytes      int64
	CapBytes   int64
}

// GatherRetentionFacts observes the retention run history and the Buildpacks
// cache sizes. It is read-only and never writes a run or a policy. The database
// part is only attempted on the main role because that is where the daily
// retention job runs; cache sizes are measured on roles that run builds.
func GatherRetentionFacts(ctx context.Context, role, dsn string) (RetentionFacts, BuildCacheFacts) {
	retentionFacts := RetentionFacts{Enabled: role == "main"}
	cacheFacts := BuildCacheFacts{
		Applicable: buildCacheApplies(role),
		CapBytes:   retention.DefaultPolicy().BuildpackCacheMaxBytes,
	}
	if retentionFacts.Enabled {
		loadRetentionFacts(ctx, dsn, &retentionFacts, &cacheFacts)
	}
	if cacheFacts.Applicable {
		bytes, err := measureBuildpackCaches(ctx)
		if err != nil {
			cacheFacts.Error = err.Error()
		} else {
			cacheFacts.Known, cacheFacts.Bytes = true, bytes
		}
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
	err = pool.QueryRow(queryCtx, `SELECT status,started_at,finished_at,
        jsonb_array_length(planned_items),jsonb_array_length(removed_items),
        planned_bytes,reclaimed_bytes,jsonb_array_length(errors)
        FROM eregion_retention_runs
        ORDER BY started_at DESC,id DESC LIMIT 1`).
		Scan(&facts.LastRun.Status, &facts.LastRun.StartedAt, &finished,
			&facts.LastRun.PlannedItems, &facts.LastRun.RemovedItems,
			&facts.LastRun.PlannedBytes, &facts.LastRun.ReclaimedBytes, &facts.LastRun.Errors)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		facts.Known = true
		facts.WorkerAge, facts.WorkerAgeKnown = workerActiveAge(ctx)
		return
	case err != nil:
		facts.Error = "the retention runs could not be read"
		return
	}
	facts.HasRun = true
	facts.LastRun.FinishedAt = finished
	facts.Known = true

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
		// The daily job first fires 24 hours after the worker starts. A fresh
		// host therefore has no run for its first day; only warn once the
		// worker has been active for more than two days.
		if facts.Retention.WorkerAgeKnown && facts.Retention.WorkerAge > 48*time.Hour {
			return one(Warn,
				fmt.Sprintf("no retention run has been recorded since the worker started %s ago", humanDuration(facts.Retention.WorkerAge)),
				"check the maintenance queue, then run eregion prune --dry-run and eregion prune --confirm <server>")
		}
		return one(Pass, "no retention run yet; the first daily run starts within 24 hours of the worker starting", "")
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
		return one(Warn, detail, "inspect eregion operations list --json, fix the reported node or registry error, then run eregion prune --dry-run")
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

func buildpackCache(facts Facts) []Result {
	if !facts.BuildCache.Applicable {
		return one(Pass, "this role runs no Buildpacks builds", "")
	}
	if !facts.BuildCache.Known {
		detail := "cannot measure the Buildpacks cache"
		if facts.BuildCache.Error != "" {
			detail += ": " + facts.BuildCache.Error
		}
		return one(Warn, detail, "check that Docker is running and the node can list its volumes")
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

func measureBuildpackCaches(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := exec.LookPath("docker"); err != nil {
		return 0, errors.New("docker is not available")
	}
	names, err := buildpackCacheVolumeNames(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, name := range names {
		mountpoint, err := dockerOutput(ctx, "volume", "inspect", "--format", "{{.Mountpoint}}", name)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such volume") {
				continue
			}
			return 0, err
		}
		size, err := directoryBytes(ctx, strings.TrimSpace(mountpoint))
		if err != nil {
			return 0, err
		}
		total += size
	}
	return total, nil
}

func buildpackCacheVolumeNames(ctx context.Context) ([]string, error) {
	prefixes := []string{"eregion-build-cache-", "eregion-launch-cache-", "pack-cache-"}
	seen := map[string]bool{}
	var names []string
	for _, prefix := range prefixes {
		output, err := dockerOutput(ctx, "volume", "ls", "--quiet", "--filter", "name="+prefix)
		if err != nil {
			return nil, err
		}
		for _, name := range strings.Split(output, "\n") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			if !isBuildpackCacheVolume(name) {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func isBuildpackCacheVolume(name string) bool {
	switch {
	case strings.HasPrefix(name, "eregion-build-cache-"), strings.HasPrefix(name, "eregion-launch-cache-"):
		return true
	case strings.HasPrefix(name, "pack-cache-"):
		return strings.HasSuffix(name, ".build") || strings.HasSuffix(name, ".launch")
	}
	return false
}

func dockerOutput(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: fixed read-only Docker inspection.
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", strings.Join(args, " "), detail)
	}
	return stdout.String(), nil
}

func directoryBytes(ctx context.Context, root string) (int64, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // An unreadable subtree is an unknown size, not a fault.
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.Type().IsRegular() {
			if info, statErr := entry.Info(); statErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
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
