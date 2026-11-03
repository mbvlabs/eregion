package retention

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// RunStatus is the lifecycle of one retention planner run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// ItemKind names one class of retention candidate. The planner and node
// effects record the same kinds in runs and command output.
type ItemKind string

const (
	ItemRelease          ItemKind = "release"
	ItemRegistryArtifact ItemKind = "registry_artifact"
	// ItemRegistrySkipped is a diagnostic, not a removal: it names a registry
	// repository the planner deliberately left untouched because a kept
	// release's reference could not be resolved to an inventory artifact.
	// Executors must not attempt to remove it.
	ItemRegistrySkipped ItemKind = "registry_skipped"
	// ItemRegistryGC records the registry garbage-collection effect in a run's
	// removed items. The planner never emits it.
	ItemRegistryGC ItemKind = "registry_gc"
	// ItemImage removes one local Docker image by its immutable image ID. It is
	// planned separately from ItemRelease so marking a release row can never
	// imply removing a tag or an image another release still needs.
	ItemImage ItemKind = "image"
	// ItemImageSkipped is a diagnostic, not a removal: it names a release whose
	// image cannot be removed because the release has no immutable identity, or
	// another retained release/container still uses that image.
	ItemImageSkipped ItemKind = "image_skipped"
	// ItemCacheSkipped is a diagnostic, not a removal: the Buildpacks cache
	// could not be measured, so no cache entry is removed.
	ItemCacheSkipped   ItemKind = "cache_skipped"
	ItemBuildpackCache ItemKind = "buildpack_cache"
	ItemContainer      ItemKind = "container"
	ItemDanglingImage  ItemKind = "dangling_image"
)

// Item is one planned or removed retention object, or a diagnostic skip. A
// registry_skipped item has no removal target; every other kind names one.
// Reference is the release ID, repository-qualified registry artifact (or
// repository for a skip), image ID, container name or cache path. Bytes is the
// reclaimed size when known, zero otherwise.
type Item struct {
	Kind        ItemKind `json:"kind"`
	App         string   `json:"app,omitempty"`
	Environment string   `json:"environment,omitempty"`
	// Release owns an ItemImage or ItemImageSkipped item. It lets the executor
	// re-check the right release after a retry without parsing the reason.
	Release   string `json:"release,omitempty"`
	Reference string `json:"reference,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// RunError is one per-item retention failure. A failure never hides the rest
// of the run: the remaining items stay recorded.
type RunError struct {
	Stage     string `json:"stage,omitempty"`
	Reference string `json:"reference,omitempty"`
	Message   string `json:"message"`
}

// Run is one durable retention planner run. It is the readable form of one
// eregion_retention_runs row.
type Run struct {
	ID             string     `json:"id"`
	OperationID    string     `json:"operation_id,omitempty"`
	Status         RunStatus  `json:"status"`
	Planned        []Item     `json:"planned"`
	Removed        []Item     `json:"removed"`
	PlannedBytes   int64      `json:"planned_bytes"`
	ReclaimedBytes int64      `json:"reclaimed_bytes"`
	Errors         []RunError `json:"errors"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	// CacheBytes is the latest complete Buildpacks cache measurement. Nil is
	// unknown, never a healthy zero. CacheMeasuredAt is when the node observed
	// it; doctor treats a measurement older than 48 hours as unknown.
	CacheBytes      *int64     `json:"cache_bytes,omitempty"`
	CacheMeasuredAt *time.Time `json:"cache_measured_at,omitempty"`
	CacheError      string     `json:"cache_measurement_error,omitempty"`
}

// NewRunID returns a retention run identity in the database's rtr_ format.
func NewRunID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "rtr_" + hex.EncodeToString(value[:]), nil
}
