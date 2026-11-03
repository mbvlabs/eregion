package retention

import "time"

// Inventory is one read-only observation of the node's reclaimable disk
// artifacts. It is the wire form of the `retention.inventory` node method and
// the input the retention planner reasons over. Every field is plain data: no
// database row, journal entry or secret is part of it.
type Inventory struct {
	CollectedAt       time.Time                   `json:"collected_at"`
	RegistryArtifacts []InventoryRegistryArtifact `json:"registry_artifacts"`
	CacheVolumes      []CacheVolume               `json:"cache_volumes"`
	Containers        []InventoryContainer        `json:"containers"`
	DanglingImages    []InventoryImage            `json:"dangling_images"`
	LocalImages       []LocalImage                `json:"local_images"`
	Disk              Disk                        `json:"disk"`
	// CacheSizeUnknown reports that at least one Buildpacks cache volume could
	// not be measured. The planner must not treat the missing size as a healthy
	// zero, and the retention run persists an unknown measurement.
	CacheSizeUnknown bool   `json:"cache_size_unknown,omitempty"`
	CacheError       string `json:"cache_error,omitempty"`
	// Warnings record observations that could not be completed, such as a
	// registry revision whose manifest blob is missing. They are not a failed
	// inventory; the planner must treat the affected artifact as unknown.
	Warnings []string `json:"warnings,omitempty"`
	// Truncated reports that the node capped an unbounded list. The planner
	// must not treat a truncated inventory as a complete picture of the node.
	Truncated bool `json:"truncated,omitempty"`
}

// InventoryRegistryArtifact is one manifest stored in the local OCI registry,
// keyed by repository and manifest digest. Releases are referenced by Digest, not Tag,
// so an artifact with no tags is still live when a release names its digest.
// Identical digests in two repositories are two entries, because a registry
// manifest delete names one repository.
type InventoryRegistryArtifact struct {
	Repository string   `json:"repository"`
	Digest     string   `json:"digest"`
	Tags       []string `json:"tags,omitempty"`
	// ConfigDigest is the image config descriptor of a single-platform
	// manifest. An index has no config descriptor of its own, so it is empty.
	ConfigDigest string `json:"config_digest,omitempty"`
	// Bytes counts the manifest blob and every unique blob it references.
	Bytes     int64      `json:"bytes"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// CacheVolume is one Buildpacks cache volume created by an eregion pack build
// or by pack's own default naming. Kind is "build", "launch", or "pack" for a
// pre-upgrade pack-cache-* volume. App and Environment are set only for an
// eregion-named cache; an Ownerless pack cache belongs to no environment and is
// eligible for pruning when the node is not building.
type CacheVolume struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Ownerless   bool   `json:"ownerless,omitempty"`
	SizeBytes   int64  `json:"size_bytes"`
	// SizeUnknown reports a missing or unreadable volume mountpoint. SizeBytes
	// is then not a measurement and must not be added as zero.
	SizeUnknown bool `json:"size_unknown,omitempty"`
}

// InventoryContainer is one eregion-labeled Docker container.
type InventoryContainer struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	State     string            `json:"state"`
	Labels    map[string]string `json:"labels,omitempty"`
	SizeBytes int64             `json:"size_bytes,omitempty"`
	CreatedAt *time.Time        `json:"created_at,omitempty"`
}

// InventoryImage is one dangling Docker image: a layer set no tag and no container
// names. Its ID is the local immutable identifier a removal effect uses.
type InventoryImage struct {
	ID        string     `json:"id"`
	SizeBytes int64      `json:"size_bytes"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// LocalImage is one Docker image with its registry references. RepoDigests
// lets the control plane resolve a release's registry tag or config digest to
// the manifest digest the registry stores.
type LocalImage struct {
	ID          string   `json:"id"`
	RepoTags    []string `json:"repo_tags,omitempty"`
	RepoDigests []string `json:"repo_digests,omitempty"`
}

// Disk is one filesystem's capacity and free space, in bytes.
type Disk struct {
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
}
