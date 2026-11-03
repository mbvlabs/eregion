package retention

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Input is the complete state a retention plan reads. It is deliberately made
// of plain Go values: the control plane fills the platform rows, the node
// adapter fills the Docker, registry and filesystem inventory, and Plan makes
// the decision without a database, Docker or filesystem call of its own.
type Input struct {
	// Environments is each environment the node serves, its effective policy
	// and the releases that are currently serving.
	Environments []Environment
	// Releases is every platform release row, including rows already marked
	// pruned. Release rows are never deleted; only their artifacts disappear.
	Releases []Release
	// Deployments is the platform deployment history. Succeeded rows supply the
	// rollback target; a pending or running row is treated as in-flight and
	// protects its release. Callers may pass only succeeded rows.
	Deployments []Deployment
	// Registry is the node registry's manifests and digests.
	Registry RegistryInventory
	// Cache is the Buildpacks cache and the filesystem state it lives on.
	Cache CacheInventory
	// Containers is the eregion-labelled Docker containers on the node.
	Containers []Container
	// Images is the Docker images on the node.
	Images []Image
	// InFlight names everything a pending or running deploy, rollback or
	// restart still owns. The planner never plans an item for one of these.
	InFlight InFlight
	// ServerPolicy supplies the node-wide limits that are not overridable per
	// environment: the Buildpacks cache limits and the artifact grace used for
	// a registry artifact no environment references. A zero field inherits the
	// D30 default.
	ServerPolicy Policy
	// Now fixes every age and grace calculation. Callers pass one timestamp
	// for the whole plan so a run is reproducible.
	Now time.Time
}

// Environment is one platform environment as the planner needs it.
type Environment struct {
	// App and Name identify the environment and match release, deployment and
	// container labels.
	App  string
	Name string
	// ServingRelease is the release whose containers currently receive
	// traffic. It is always kept, even when older than every policy window.
	ServingRelease string
	// ServingDeployment is the deployment whose recorded instances are the
	// current upstreams. A container matching it is owned even if its release
	// label is missing or stale.
	ServingDeployment string
	// Policy is the effective policy for this environment: the server default
	// with the current revision's override fields merged in. A zero field
	// inherits the D30 default.
	Policy Policy
}

// Release is one eregion_platform_releases row. PrunedAt is set when an
// earlier plan removed the release's artifact; the row itself always remains.
type Release struct {
	ID          string
	App         string
	Environment string
	// Image is the registry reference recorded on the release, usually
	// repository:tag. A digest after "@" is treated as an artifact reference.
	Image string
	// ImageID is the local Docker image identity (sha256:…) or registry digest
	// the release needs. It is matched against RegistryInventory and Images.
	ImageID string
	// Artifacts lists any further registry manifest digests the release needs.
	// A digest shared with a kept release is never removed.
	Artifacts []string
	// CreatedAt is the release row's creation time and orders the keep set.
	CreatedAt time.Time
	// PrunedAt is nil for a release whose artifact is still present.
	PrunedAt *time.Time
}

// Deployment is one eregion_platform_deployments row. The rollback target uses
// only succeeded rows; pending or running rows belong in InFlight instead.
type Deployment struct {
	ID          string
	App         string
	Environment string
	// Release is the release the deployment switched to.
	Release string
	// PreviousRelease is the release that served before it. An empty value is
	// SQL NULL: there was no previous release.
	PreviousRelease string
	// State is the platform deployment state. Only "succeeded" rows form
	// rollback history. An empty State is accepted as succeeded when the caller
	// has already filtered the input to succeeded rows.
	State string
	// CreatedAt and CompletedAt order succeeded deployments exactly like the
	// control plane's query: completed_at DESC NULLS LAST, created_at DESC.
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// RegistryInventory is the registry content a plan may delete.
type RegistryInventory struct {
	// Artifacts are manifests or digests known to the registry. Duplicate
	// digests are planned once.
	Artifacts []RegistryArtifact
}

// RegistryArtifact is one manifest or digest in the node registry.
type RegistryArtifact struct {
	// Digest identifies the artifact and is matched against Release artifacts.
	Digest string
	// Repository is the registry repository, for operator-readable output.
	Repository string
	// CreatedAt is when the manifest was pushed; old unreferenced artifacts
	// are removed after the policy's grace period.
	CreatedAt time.Time
	// Bytes is what the registry reclaims when the artifact is removed.
	Bytes int64
}

// CacheInventory is the Buildpacks cache and the filesystem it occupies.
type CacheInventory struct {
	// Bytes is the measured cache total. Zero (or a value smaller than the sum
	// of Entries) falls back to the sum of Entries.
	Bytes int64
	// DiskUsedBytes and DiskTotalBytes describe the whole filesystem. Used
	// includes the cache; Total <= 0 means the filesystem size is unknown.
	DiskUsedBytes  int64
	DiskTotalBytes int64
	// Entries are individually removable cache directories, in any order.
	Entries []CacheEntry
}

// CacheEntry is one removable Buildpacks cache directory.
type CacheEntry struct {
	// Path is the cache directory and the removal reference.
	Path string
	// Bytes is the directory's size.
	Bytes int64
	// LastUsedAt is the least-recently-used ordering. A zero value means the
	// use time is unknown and such entries sort after known use times.
	LastUsedAt time.Time
}

// Container is one eregion-labelled Docker container from docker ps.
type Container struct {
	ID          string
	Name        string
	App         string
	Environment string
	// Release and Deployment are the io.eregion.release and
	// io.eregion.deployment ownership labels.
	Release    string
	Deployment string
	// State is Docker's container state, kept for operator-readable reasons.
	State string
	// Bytes is the container's writable-layer size when known.
	Bytes int64
}

// Image is one Docker image on the node.
type Image struct {
	// ID is the local image ID (sha256:…).
	ID string
	// Digest is the registry manifest digest, when known.
	Digest string
	// Repository and Tags are optional operator-readable detail.
	Repository string
	Tags       []string
	// Dangling marks an image Docker reports without tags.
	Dangling bool
	// Bytes is the image size when known.
	Bytes int64
	// Release is the optional io.eregion.release label.
	Release string
}

// InFlight is the set of resources a pending or running operation still owns.
// A release here is never planned for pruning, and a container here is never
// removed. Candidate releases of an in-progress blue/green deploy belong in
// Releases; a candidate deployment ID in Deployments protects its containers.
// A pending or running deployment in Input.Deployments is also treated as
// in-flight, so a caller may simply pass the whole deployment history.
type InFlight struct {
	Releases    []string
	Deployments []string
	Containers  []string
}

// DeploymentStateSucceeded is the platform deployment state that forms the
// rollback history. It is repeated here so the pure planner does not depend on
// the platform package.
const DeploymentStateSucceeded = "succeeded"

// Plan returns the deterministic list of retention items a node would remove
// for the supplied state. It is pure: it does not read or write anything, does
// not mutate its input, and never plans a kept release, a serving or rollback
// release, an in-flight release or an in-flight container.
//
// Items are ordered by phase and then by stable keys: releases by environment,
// then oldest release first; registry artifacts by digest; Buildpacks cache
// entries least recently used first; containers by app, environment and name;
// and dangling images by ID.
func Plan(in Input) []Item {
	now := in.Now
	serverPolicy := normalizedPolicy(in.ServerPolicy)
	inFlightReleases := stringSet(in.InFlight.Releases)
	inFlightDeployments := stringSet(in.InFlight.Deployments)
	inFlightContainers := stringSet(in.InFlight.Containers)
	// A pending or running deployment in the history is an in-flight candidate
	// even when the caller did not separately repeat it in InFlight. Protecting
	// its release here keeps both the release and its artifacts out of the plan.
	for _, deployment := range in.Deployments {
		if deployment.State == "pending" || deployment.State == "running" {
			inFlightDeployments[deployment.ID] = true
		}
		if !inFlightDeployments[deployment.ID] {
			continue
		}
		if deployment.Release != "" {
			inFlightReleases[deployment.Release] = true
		}
		if deployment.PreviousRelease != "" {
			inFlightReleases[deployment.PreviousRelease] = true
		}
	}

	environments := append([]Environment(nil), in.Environments...)
	sort.SliceStable(environments, func(i, j int) bool {
		if environments[i].App != environments[j].App {
			return environments[i].App < environments[j].App
		}
		return environments[i].Name < environments[j].Name
	})
	environmentByKey := map[environmentKey]Environment{}
	for _, environment := range environments {
		key := environmentKeyOf(environment)
		if _, exists := environmentByKey[key]; !exists {
			environmentByKey[key] = environment
		}
	}

	releases := append([]Release(nil), in.Releases...)
	sort.SliceStable(releases, func(i, j int) bool {
		if releases[i].App != releases[j].App {
			return releases[i].App < releases[j].App
		}
		if releases[i].Environment != releases[j].Environment {
			return releases[i].Environment < releases[j].Environment
		}
		if !releases[i].CreatedAt.Equal(releases[j].CreatedAt) {
			return releases[i].CreatedAt.Before(releases[j].CreatedAt)
		}
		return releases[i].ID < releases[j].ID
	})
	releasesByEnvironment := map[environmentKey][]Release{}
	for _, release := range releases {
		key := environmentKey{app: release.App, environment: release.Environment}
		releasesByEnvironment[key] = append(releasesByEnvironment[key], release)
	}

	protectedReleaseIDs := map[string]bool{}
	for releaseID := range inFlightReleases {
		protectedReleaseIDs[releaseID] = true
	}
	plannedReleaseIDs := map[string]bool{}
	plannedArtifacts := map[string]plannedArtifact{}
	items := []Item{}

	seenEnvironments := map[environmentKey]bool{}
	for _, environment := range environments {
		key := environmentKeyOf(environment)
		if seenEnvironments[key] {
			continue
		}
		seenEnvironments[key] = true

		policy := normalizedPolicy(environment.Policy)
		rollbackTarget := RollbackTarget(environment.App, environment.Name, environment.ServingRelease, in.Deployments)
		if environment.ServingRelease != "" {
			protectedReleaseIDs[environment.ServingRelease] = true
		}
		if rollbackTarget != "" {
			protectedReleaseIDs[rollbackTarget] = true
		}
		releaseCutoff := daysBefore(now, policy.KeepNewerThanDays)

		environmentReleases := releasesByEnvironment[key]
		// Find the keep set from newest to oldest; the output below walks the
		// same slice from oldest to newest for a stable removal order.
		sort.SliceStable(environmentReleases, func(i, j int) bool {
			if !environmentReleases[i].CreatedAt.Equal(environmentReleases[j].CreatedAt) {
				return environmentReleases[i].CreatedAt.After(environmentReleases[j].CreatedAt)
			}
			return environmentReleases[i].ID > environmentReleases[j].ID
		})
		kept := map[string]bool{}
		for index, release := range environmentReleases {
			if index < policy.KeepReleases ||
				release.CreatedAt.After(releaseCutoff) ||
				release.ID == environment.ServingRelease ||
				release.ID == rollbackTarget ||
				inFlightReleases[release.ID] {
				kept[release.ID] = true
			}
		}
		for index := len(environmentReleases) - 1; index >= 0; index-- {
			release := environmentReleases[index]
			if release.PrunedAt != nil || kept[release.ID] || inFlightReleases[release.ID] {
				continue
			}
			plannedReleaseIDs[release.ID] = true
			items = append(items, Item{
				Kind:        ItemRelease,
				App:         release.App,
				Environment: release.Environment,
				Reference:   release.ID,
				Reason: fmt.Sprintf("outside the newest %d releases and older than the %d-day keep window",
					policy.KeepReleases, policy.KeepNewerThanDays),
			})
			for _, reference := range releaseArtifactRefs(release) {
				existing, exists := plannedArtifacts[reference]
				if !exists {
					plannedArtifacts[reference] = plannedArtifact{
						app:         release.App,
						environment: release.Environment,
						release:     release.ID,
						graceDays:   policy.ArtifactGraceDays,
					}
					continue
				}
				if policy.ArtifactGraceDays > existing.graceDays {
					existing.graceDays = policy.ArtifactGraceDays
					plannedArtifacts[reference] = existing
				}
			}
		}
	}

	// A kept release protects every artifact it still needs, including a digest
	// shared with a release that is being pruned.
	protectedArtifacts := map[string]bool{}
	retainedReleaseIDs := map[string]bool{}
	for _, release := range releases {
		if plannedReleaseIDs[release.ID] {
			continue
		}
		if release.PrunedAt != nil && !protectedReleaseIDs[release.ID] {
			continue
		}
		retainedReleaseIDs[release.ID] = true
		for _, reference := range releaseArtifactRefs(release) {
			protectedArtifacts[reference] = true
		}
	}

	items = append(items, planRegistryArtifacts(in.Registry, protectedArtifacts, plannedArtifacts, serverPolicy, now)...)
	items = append(items, planBuildpackCache(serverPolicy, in.Cache)...)
	items = append(items, planContainers(environmentByKey, in.Containers, inFlightReleases, inFlightDeployments, inFlightContainers)...)
	items = append(items, planDanglingImages(in.Images, protectedArtifacts, retainedReleaseIDs)...)
	return items
}

// TotalBytes returns the bytes every item reclaims. Negative byte counts are
// ignored so a broken inventory cannot reduce the reported total.
func TotalBytes(items []Item) int64 {
	var total int64
	for _, item := range items {
		if item.Bytes > 0 {
			total += item.Bytes
		}
	}
	return total
}

// RollbackTarget returns the release a rollback would choose for one
// environment. It has the same semantics as the control plane's
// previousRollbackRelease: the latest succeeded deployment that switched to
// the serving release supplies its previous_release_id; when that history is
// absent, the latest succeeded deployment for a different release is the
// target. Deployments are ordered by completed_at DESC NULLS LAST, then
// created_at DESC. An empty result means the environment has no rollback
// target.
//
// The control plane keeps this query in platform_state.go because it reads a
// transaction; the planner uses this pure copy of the same selection rule so
// retention cannot disagree with the rollback path. TestRollbackTargetSQLCases
// covers each branch of that query.
func RollbackTarget(app, environment, serving string, deployments []Deployment) string {
	succeeded := make([]Deployment, 0, len(deployments))
	for _, deployment := range deployments {
		if deployment.App != app || deployment.Environment != environment {
			continue
		}
		if deployment.State != "" && deployment.State != DeploymentStateSucceeded {
			continue
		}
		succeeded = append(succeeded, deployment)
	}
	sort.SliceStable(succeeded, func(i, j int) bool {
		return deploymentNewer(succeeded[i], succeeded[j])
	})
	if serving != "" {
		for _, deployment := range succeeded {
			if deployment.Release == serving && deployment.PreviousRelease != "" {
				return deployment.PreviousRelease
			}
		}
	}
	for _, deployment := range succeeded {
		if deployment.Release != serving {
			return deployment.Release
		}
	}
	return ""
}

type environmentKey struct {
	app         string
	environment string
}

type plannedArtifact struct {
	app         string
	environment string
	release     string
	graceDays   int
}

func environmentKeyOf(environment Environment) environmentKey {
	return environmentKey{app: environment.App, environment: environment.Name}
}

func normalizedPolicy(policy Policy) Policy {
	defaults := DefaultPolicy()
	if policy.KeepReleases == 0 {
		policy.KeepReleases = defaults.KeepReleases
	}
	if policy.KeepNewerThanDays == 0 {
		policy.KeepNewerThanDays = defaults.KeepNewerThanDays
	}
	if policy.ArtifactGraceDays == 0 {
		policy.ArtifactGraceDays = defaults.ArtifactGraceDays
	}
	if policy.BuildpackCacheMaxBytes == 0 {
		policy.BuildpackCacheMaxBytes = defaults.BuildpackCacheMaxBytes
	}
	if policy.BuildpackCacheMaxPercent == 0 {
		policy.BuildpackCacheMaxPercent = defaults.BuildpackCacheMaxPercent
	}
	return policy
}

func daysBefore(now time.Time, days int) time.Time {
	if days <= 0 {
		return now
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = true
		}
	}
	return set
}

func releaseArtifactRefs(release Release) []string {
	references := make([]string, 0, len(release.Artifacts)+2)
	for _, reference := range release.Artifacts {
		if reference = strings.TrimSpace(reference); reference != "" {
			references = append(references, reference)
		}
	}
	if imageID := strings.TrimSpace(release.ImageID); imageID != "" {
		references = append(references, imageID)
	}
	image := strings.TrimSpace(release.Image)
	if image == "" {
		return references
	}
	if index := strings.Index(image, "@"); index >= 0 {
		if digest := strings.TrimSpace(image[index+1:]); digest != "" {
			references = append(references, digest)
		}
		return references
	}
	if strings.HasPrefix(image, "sha256:") || strings.HasPrefix(image, "sha512:") {
		references = append(references, image)
	}
	return references
}

func deploymentNewer(a, b Deployment) bool {
	switch {
	case a.CompletedAt == nil && b.CompletedAt == nil:
	case a.CompletedAt == nil:
		return false
	case b.CompletedAt == nil:
		return true
	default:
		if !a.CompletedAt.Equal(*b.CompletedAt) {
			return a.CompletedAt.After(*b.CompletedAt)
		}
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

func planRegistryArtifacts(registry RegistryInventory, protected map[string]bool, planned map[string]plannedArtifact, serverPolicy Policy, now time.Time) []Item {
	artifacts := append([]RegistryArtifact(nil), registry.Artifacts...)
	sort.SliceStable(artifacts, func(i, j int) bool {
		left := strings.TrimSpace(artifacts[i].Digest)
		right := strings.TrimSpace(artifacts[j].Digest)
		if left != right {
			return left < right
		}
		return artifacts[i].Repository < artifacts[j].Repository
	})

	items := []Item{}
	seen := map[string]bool{}
	for _, artifact := range artifacts {
		reference := strings.TrimSpace(artifact.Digest)
		if reference == "" || seen[reference] {
			continue
		}
		seen[reference] = true
		if protected[reference] {
			continue
		}
		owner, referencedByPruned := planned[reference]
		graceDays := serverPolicy.ArtifactGraceDays
		if referencedByPruned {
			graceDays = owner.graceDays
		}
		if !artifact.CreatedAt.Before(daysBefore(now, graceDays)) {
			continue
		}
		item := Item{
			Kind:      ItemRegistryArtifact,
			Reference: reference,
			Bytes:     artifact.Bytes,
		}
		if referencedByPruned {
			item.App = owner.app
			item.Environment = owner.environment
			item.Reason = fmt.Sprintf("registry artifact referenced by pruned release %s and older than %d days",
				owner.release, graceDays)
		} else {
			item.Reason = fmt.Sprintf("registry artifact is unreferenced by any kept release and older than %d days", graceDays)
		}
		items = append(items, item)
	}
	return items
}

func planBuildpackCache(policy Policy, cache CacheInventory) []Item {
	entries := make([]CacheEntry, 0, len(cache.Entries))
	var entryBytes int64
	seen := map[string]bool{}
	for _, entry := range cache.Entries {
		path := strings.TrimSpace(entry.Path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if entry.Bytes < 0 {
			entry.Bytes = 0
		}
		entries = append(entries, entry)
		entryBytes += entry.Bytes
	}
	cacheBytes := cache.Bytes
	if cacheBytes < entryBytes {
		cacheBytes = entryBytes
	}
	usedBytes := cache.DiskUsedBytes
	if usedBytes < 0 {
		usedBytes = 0
	}
	if !policy.BuildpackCacheExceeded(cacheBytes, usedBytes, cache.DiskTotalBytes) {
		return nil
	}

	sort.SliceStable(entries, func(i, j int) bool {
		switch {
		case entries[i].LastUsedAt.IsZero() && entries[j].LastUsedAt.IsZero():
		case entries[i].LastUsedAt.IsZero():
			return false
		case entries[j].LastUsedAt.IsZero():
			return true
		default:
			if !entries[i].LastUsedAt.Equal(entries[j].LastUsedAt) {
				return entries[i].LastUsedAt.Before(entries[j].LastUsedAt)
			}
		}
		return entries[i].Path < entries[j].Path
	})

	items := []Item{}
	for index, entry := range entries {
		items = append(items, Item{
			Kind:      ItemBuildpackCache,
			Reference: entry.Path,
			Bytes:     entry.Bytes,
			Reason: fmt.Sprintf("Buildpacks cache exceeds the %s cap or %d%% disk use; least recently used entry",
				humanBytes(policy.BuildpackCacheMaxBytes), policy.BuildpackCacheMaxPercent),
		})
		cacheBytes -= entry.Bytes
		if cacheBytes < 0 {
			cacheBytes = 0
		}
		usedBytes -= entry.Bytes
		if usedBytes < 0 {
			usedBytes = 0
		}
		cacheWithinLimit := cacheBytes <= policy.BuildpackCacheMaxBytes
		diskWithinLimit := cache.DiskTotalBytes <= 0 ||
			float64(usedBytes) <= float64(cache.DiskTotalBytes)*float64(policy.BuildpackCacheMaxPercent)/100
		if cacheWithinLimit && (diskWithinLimit || cacheBytes == 0 || index == len(entries)-1) {
			break
		}
	}
	return items
}

func planContainers(environmentByKey map[environmentKey]Environment, containers []Container, inFlightReleases, inFlightDeployments, inFlightContainers map[string]bool) []Item {
	sorted := append([]Container(nil), containers...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].App != sorted[j].App {
			return sorted[i].App < sorted[j].App
		}
		if sorted[i].Environment != sorted[j].Environment {
			return sorted[i].Environment < sorted[j].Environment
		}
		return containerReference(sorted[i]) < containerReference(sorted[j])
	})

	items := []Item{}
	for _, container := range sorted {
		if inFlightReleases[container.Release] ||
			inFlightDeployments[container.Deployment] ||
			inFlightContainers[container.Name] ||
			inFlightContainers[container.ID] {
			continue
		}
		key := environmentKey{app: container.App, environment: container.Environment}
		environment, known := environmentByKey[key]
		if known {
			if environment.ServingRelease != "" && container.Release == environment.ServingRelease {
				continue
			}
			if environment.ServingDeployment != "" && container.Deployment == environment.ServingDeployment {
				continue
			}
		}
		reason := "container is neither a serving release nor an active candidate"
		if !known {
			reason = "container labels do not match a known environment"
		}
		items = append(items, Item{
			Kind:        ItemContainer,
			App:         container.App,
			Environment: container.Environment,
			Reference:   containerReference(container),
			Bytes:       container.Bytes,
			Reason:      reason,
		})
	}
	return items
}

func containerReference(container Container) string {
	if name := strings.TrimSpace(container.Name); name != "" {
		return name
	}
	return strings.TrimSpace(container.ID)
}

func humanBytes(bytes int64) string {
	const gibibyte = int64(1) << 30
	if bytes >= gibibyte && bytes%gibibyte == 0 {
		return fmt.Sprintf("%d GiB", bytes/gibibyte)
	}
	return fmt.Sprintf("%d bytes", bytes)
}

func planDanglingImages(images []Image, protectedArtifacts, retainedReleaseIDs map[string]bool) []Item {
	sorted := append([]Image(nil), images...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return imageReference(sorted[i]) < imageReference(sorted[j])
	})

	items := []Item{}
	for _, image := range sorted {
		if !image.Dangling {
			continue
		}
		if image.Release != "" && retainedReleaseIDs[image.Release] {
			continue
		}
		reference := imageReference(image)
		if reference == "" || protectedArtifacts[reference] {
			continue
		}
		if digest := strings.TrimSpace(image.Digest); digest != "" && protectedArtifacts[digest] {
			continue
		}
		items = append(items, Item{
			Kind:      ItemDanglingImage,
			Reference: reference,
			Bytes:     image.Bytes,
			Reason:    "dangling image has no tag or container reference",
		})
	}
	return items
}

func imageReference(image Image) string {
	if id := strings.TrimSpace(image.ID); id != "" {
		return id
	}
	if digest := strings.TrimSpace(image.Digest); digest != "" {
		return digest
	}
	return strings.TrimSpace(image.Repository)
}
