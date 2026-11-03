// Package retention maps one node inventory plus the platform rows into the
// pure planner input. The mapping is deliberately in this package so the
// periodic job and `eregion prune --dry-run` use exactly one interpretation of
// the wire inventory.
package retention

import "time"

// PlatformState is the platform-side half of a retention plan: every
// environment the node serves, the release and deployment rows that decide the
// keep set, and everything an in-flight operation still owns.
type PlatformState struct {
	Environments []Environment
	Releases     []Release
	Deployments  []Deployment
	InFlight     InFlight
	ServerPolicy Policy
}

// The ownership labels every eregion workload container carries. They are the
// same keys the deploy path writes, never inferred from a container name.
const (
	ContainerAppLabel         = "io.eregion.app"
	ContainerEnvironmentLabel = "io.eregion.environment"
	ContainerReleaseLabel     = "io.eregion.release"
	ContainerDeploymentLabel  = "io.eregion.deployment"
)

// InputFromInventory maps one node observation and the platform rows into the
// planner input. A registry artifact whose creation time the node did not
// report is treated as new: an unknown age is never old enough to delete. A
// truncated inventory plans no registry deletion at all, because the missing
// entries could be artifacts a kept release still needs; the plan carries one
// registry_skipped diagnostic naming that reason.
func InputFromInventory(inventory Inventory, state PlatformState, now time.Time) Input {
	if now.IsZero() {
		now = inventory.CollectedAt
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	input := Input{
		Environments: state.Environments,
		Releases:     state.Releases,
		Deployments:  state.Deployments,
		InFlight:     state.InFlight,
		ServerPolicy: state.ServerPolicy,
		Now:          now.UTC(),
	}
	if inventory.Truncated {
		input.RegistryUnsafeReason = "the node inventory was truncated; no registry artifact is removed"
	}

	input.Registry.Artifacts = make([]RegistryArtifact, 0, len(inventory.RegistryArtifacts))
	for _, artifact := range inventory.RegistryArtifacts {
		createdAt := now.UTC()
		if artifact.CreatedAt != nil {
			createdAt = artifact.CreatedAt.UTC()
		}
		input.Registry.Artifacts = append(input.Registry.Artifacts, RegistryArtifact{
			Digest:       artifact.Digest,
			Repository:   artifact.Repository,
			Tags:         append([]string(nil), artifact.Tags...),
			ConfigDigest: artifact.ConfigDigest,
			CreatedAt:    createdAt,
			Bytes:        artifact.Bytes,
		})
	}

	input.Cache.DiskTotalBytes = inventory.Disk.TotalBytes
	if inventory.Disk.TotalBytes > 0 {
		used := inventory.Disk.TotalBytes - inventory.Disk.FreeBytes
		if used < 0 {
			used = 0
		}
		input.Cache.DiskUsedBytes = used
	}
	input.Cache.Unknown = inventory.CacheSizeUnknown
	for _, volume := range inventory.CacheVolumes {
		// Ownerless legacy pack caches are included: they belong to no
		// environment but still occupy the node, and the node host lock makes
		// their removal safe when no build is running.
		entry := CacheEntry{Path: volume.Name, Bytes: volume.SizeBytes}
		if volume.SizeUnknown {
			input.Cache.Unknown = true
			entry.Bytes = 0
		}
		input.Cache.Entries = append(input.Cache.Entries, entry)
		input.Cache.Bytes += entry.Bytes
	}

	for _, container := range inventory.Containers {
		input.Containers = append(input.Containers, Container{
			ID:          container.ID,
			Name:        container.Name,
			App:         container.Labels[ContainerAppLabel],
			Environment: container.Labels[ContainerEnvironmentLabel],
			Release:     container.Labels[ContainerReleaseLabel],
			Deployment:  container.Labels[ContainerDeploymentLabel],
			Image:       container.Image,
			State:       container.State,
			Bytes:       container.SizeBytes,
		})
	}

	for _, image := range inventory.DanglingImages {
		input.Images = append(input.Images, Image{
			ID:       image.ID,
			Dangling: true,
			Bytes:    image.SizeBytes,
		})
	}

	for _, image := range inventory.LocalImages {
		if len(image.RepoTags) == 0 && len(image.RepoDigests) == 0 {
			continue
		}
		input.LocalImages = append(input.LocalImages, LocalImageRef{
			ID:          image.ID,
			RepoDigests: append([]string(nil), image.RepoDigests...),
		})
	}
	return input
}

// PlanResult is one planner evaluation with its totals and node diagnostics.
// Items includes registry_skipped diagnostics; PlannedBytes counts only real
// removals.
type PlanResult struct {
	Node         string   `json:"node,omitempty"`
	Items        []Item   `json:"items"`
	PlannedBytes int64    `json:"planned_bytes"`
	Warnings     []string `json:"warnings,omitempty"`
}

// PlanInventory maps one inventory and platform state through the shared
// adapter and evaluates the planner. It is the one entry point the periodic
// job, the prune operation and `prune --dry-run` share.
func PlanInventory(inventory Inventory, state PlatformState, now time.Time) PlanResult {
	input := InputFromInventory(inventory, state, now)
	items := Plan(input)
	return PlanResult{
		Items:        items,
		PlannedBytes: TotalBytes(items),
		Warnings:     append([]string(nil), inventory.Warnings...),
	}
}
