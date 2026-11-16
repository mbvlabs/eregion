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
	// LocalImages maps Docker's local image ID (config digest) to the
	// repository digests Docker recorded for that image. It lets a release's
	// ImageID resolve to its immutable manifest after a tag moved. The node
	// inventory adapter fills it; older callers may omit it.
	LocalImages []LocalImageRef
	// Registry is the node registry's manifests and digests.
	Registry RegistryInventory
	// Cache is the Buildpacks cache and the filesystem state it lives on.
	Cache CacheInventory
	// Containers is every Docker container on the node, labeled or not. Every
	// entry protects the image it uses; only an eregion-owned container (the
	// io.eregion.app label) is ever a removal candidate.
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
	// RegistryUnsafeReason, when non-empty, prevents every registry deletion
	// and emits one registry_skipped diagnostic naming it. The inventory
	// adapter sets it when the node capped its observation, so a missing
	// artifact can never be mistaken for an unreferenced one.
	RegistryUnsafeReason string
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
	// ImageID is the release's authenticated image config digest. It is not a
	// manifest digest and is not treated as Docker's local runtime image ID,
	// which is a separate observation matched through RepoDigests.
	ImageID string
	// Status is the platform release status. A release that never reached
	// ready has no promise that it produced an image; a failed release that
	// did push one still records its ImageID. An empty status is unknown and
	// is treated like ready for registry protection.
	Status string
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
	// (repository, digest) pairs are planned once.
	Artifacts []RegistryArtifact
}

// RegistryArtifact is one manifest or digest in the node registry.
type RegistryArtifact struct {
	// Digest identifies the manifest and is matched against Release artifacts.
	Digest string
	// Repository is the registry repository, for operator-readable output and
	// for matching a Release.Image tag. An inventory catalog usually omits the
	// registry host; matching normalizes both spellings.
	Repository string
	// Tags are the tags currently pointing at this manifest in Repository.
	// A tag is extra protection only: it protects whatever manifest it points
	// at, but never resolves a retained release after a tag move. Immutable
	// manifest or config digests resolve the release.
	Tags []string
	// ConfigDigest is the image config digest exposed by the registry, when
	// known. It matches Release.ImageID (Docker's image ID) so a kept release's
	// local image identity can still protect its manifest.
	ConfigDigest string
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
	// Unknown reports that at least one cache volume could not be measured or
	// the volume list was truncated. The planner never treats that as zero: it
	// plans no cache removal and emits a cache_skipped diagnostic.
	Unknown bool
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

// Container is one Docker container from docker ps. A container is an eregion
// workload only when it carries the io.eregion.app label (Owned, or the App
// value supplied by older callers). Every container's Image protects that image,
// running or stopped, labeled or not.
type Container struct {
	ID          string
	Name        string
	App         string
	Environment string
	// Owned is true when the container carries the io.eregion.app ownership
	// label. An unowned container is never removed by retention.
	Owned bool
	// Release and Deployment are the io.eregion.release and
	// io.eregion.deployment ownership labels.
	Release    string
	Deployment string
	// Kind is io.eregion.kind. A "resource" container is an environment-owned
	// dependency, not a deployment workload, and retention never removes it.
	Kind string
	// Image is Docker's immutable image ID for the container. It protects an
	// image that a stopped or running container still uses from retention
	// removal, no matter who created the container.
	Image string
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

// LocalImageRef is the planner's minimal view of one Docker image on the node.
// It is intentionally compatible with a richer inventory LocalImage: the
// config ID, repository tags and RepoDigests are enough to map Release.ImageID
// to a manifest and to show which tags an image removal will untag.
type LocalImageRef struct {
	// ID is Docker's local runtime image ID (sha256:…). It is matched to a
	// release through RepoDigests, never by assuming it equals the recorded
	// config digest.
	ID string
	// RepoTags are the repository:tag references Docker still points at the
	// image. They are operator-visible dry-run detail; removal re-reads them.
	RepoTags []string
	// RepoDigests are Docker's repository@sha256:… entries for the image. They
	// are immutable manifest identities even when the release's tag has moved.
	RepoDigests []string
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

// Release states repeated from the platform model so the pure planner does not
// depend on it. Only a release that never reached ready can be treated as
// never having produced an image, and then only when it records no other image
// identity.
const (
	ReleaseStatePending = "pending"
	ReleaseStateReady   = "ready"
	ReleaseStateFailed  = "failed"
)

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
	inFlightReleases, inFlightDeployments, inFlightContainers := planInFlight(in)
	environments, environmentByKey := planEnvironments(in.Environments)
	releases := planReleases(in.Releases)
	releasesByEnvironment := groupReleases(releases)
	localImages := indexLocalImages(in.LocalImages)

	items, plannedReleaseIDs, protectedReleaseIDs := planReleaseRows(
		now,
		environments,
		releasesByEnvironment,
		in.Deployments,
		inFlightReleases,
	)
	artifactMetadata := planArtifactMetadata(releases, serverPolicy, environmentByKey, localImages)
	protection, protectedImageDigests, retainedReleaseIDs := planRegistryProtection(
		releases,
		plannedReleaseIDs,
		protectedReleaseIDs,
		environmentByKey,
		localImages,
	)
	registryIndex := indexRegistryArtifacts(in.Registry)
	skipReasons, skipAll := registrySkipReasons(
		releases,
		plannedReleaseIDs,
		protectedReleaseIDs,
		environmentByKey,
		registryIndex,
		localImages,
	)
	if in.RegistryUnsafeReason != "" {
		skipReasons, skipAll = map[string]string{"*": in.RegistryUnsafeReason}, true
	}
	items = append(items, planRegistryArtifacts(
		registryIndex,
		protection,
		artifactMetadata,
		skipReasons,
		skipAll,
		now,
	)...)
	items = append(items, planBuildpackCache(serverPolicy, in.Cache)...)
	items = append(items, planContainers(
		environmentByKey,
		in.Containers,
		inFlightReleases,
		inFlightDeployments,
		inFlightContainers,
	)...)
	imageItems := planReleaseImages(
		releases,
		plannedReleaseIDs,
		protectedReleaseIDs,
		in.Containers,
		in.LocalImages,
		in.Images,
	)
	items = append(items, imageItems...)
	items = append(items, planDanglingImages(
		in.Images,
		protectedImageDigests,
		retainedReleaseIDs,
		plannedImageReferences(imageItems),
		in.Containers,
	)...)
	return items
}

// A pending or running deployment in the history is an in-flight candidate
// even when the caller did not separately repeat it in InFlight. Protecting
// its release here keeps both the release and its artifacts out of the plan.
func planInFlight(in Input) (map[string]bool, map[string]bool, map[string]bool) {
	releases := stringSet(in.InFlight.Releases)
	deployments := stringSet(in.InFlight.Deployments)
	containers := stringSet(in.InFlight.Containers)
	for _, deployment := range in.Deployments {
		if deployment.State == "pending" || deployment.State == "running" {
			deployments[deployment.ID] = true
		}
		if !deployments[deployment.ID] {
			continue
		}
		if deployment.Release != "" {
			releases[deployment.Release] = true
		}
		if deployment.PreviousRelease != "" {
			releases[deployment.PreviousRelease] = true
		}
	}
	return releases, deployments, containers
}

func planEnvironments(values []Environment) ([]Environment, map[environmentKey]Environment) {
	environments := append([]Environment(nil), values...)
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
	return environments, environmentByKey
}

func planReleases(values []Release) []Release {
	releases := append([]Release(nil), values...)
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
	return releases
}

func groupReleases(releases []Release) map[environmentKey][]Release {
	releasesByEnvironment := map[environmentKey][]Release{}
	for _, release := range releases {
		key := environmentKey{app: release.App, environment: release.Environment}
		releasesByEnvironment[key] = append(releasesByEnvironment[key], release)
	}
	return releasesByEnvironment
}

func planReleaseRows(
	now time.Time,
	environments []Environment,
	releasesByEnvironment map[environmentKey][]Release,
	deployments []Deployment,
	inFlightReleases map[string]bool,
) ([]Item, map[string]bool, map[string]bool) {
	protectedReleaseIDs := map[string]bool{}
	for releaseID := range inFlightReleases {
		protectedReleaseIDs[releaseID] = true
	}
	plannedReleaseIDs := map[string]bool{}
	items := []Item{}

	seenEnvironments := map[environmentKey]bool{}
	for _, environment := range environments {
		key := environmentKeyOf(environment)
		if seenEnvironments[key] {
			continue
		}
		seenEnvironments[key] = true
		items = append(items, planEnvironmentReleases(
			now,
			environment,
			releasesByEnvironment[key],
			deployments,
			inFlightReleases,
			plannedReleaseIDs,
			protectedReleaseIDs,
		)...)
	}
	return items, plannedReleaseIDs, protectedReleaseIDs
}

func planEnvironmentReleases(
	now time.Time,
	environment Environment,
	environmentReleases []Release,
	deployments []Deployment,
	inFlightReleases map[string]bool,
	plannedReleaseIDs, protectedReleaseIDs map[string]bool,
) []Item {
	policy := normalizedPolicy(environment.Policy)
	rollbackTarget := RollbackTarget(
		environment.App,
		environment.Name,
		environment.ServingRelease,
		deployments,
	)
	if environment.ServingRelease != "" {
		protectedReleaseIDs[environment.ServingRelease] = true
	}
	if rollbackTarget != "" {
		protectedReleaseIDs[rollbackTarget] = true
	}
	releaseCutoff := daysBefore(now, policy.KeepNewerThanDays)

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
		if releaseKept(release, index, policy, releaseCutoff, environment.ServingRelease, rollbackTarget, inFlightReleases) {
			kept[release.ID] = true
		}
	}
	items := []Item{}
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
			Reason: fmt.Sprintf(
				"outside the newest %d releases and older than the %d-day keep window",
				policy.KeepReleases,
				policy.KeepNewerThanDays,
			),
		})
	}
	return items
}

func releaseKept(
	release Release,
	index int,
	policy Policy,
	releaseCutoff time.Time,
	serving, rollbackTarget string,
	inFlightReleases map[string]bool,
) bool {
	return index < policy.KeepReleases ||
		release.CreatedAt.After(releaseCutoff) ||
		release.ID == serving ||
		release.ID == rollbackTarget ||
		inFlightReleases[release.ID]
}

// Artifact ownership and grace come from every release row, including rows
// pruned by an earlier run. A pruned release's environment override still
// governs artifacts that remain in the registry.
func planArtifactMetadata(
	releases []Release,
	serverPolicy Policy,
	environments map[environmentKey]Environment,
	localImages localImageIndex,
) artifactMetadataIndex {
	artifactMetadata := newArtifactMetadataIndex(serverPolicy)
	for _, release := range releases {
		policy := serverPolicy
		if environment, known := environments[environmentKey{app: release.App, environment: release.Environment}]; known {
			policy = normalizedPolicy(environment.Policy)
		}
		artifactMetadata.recordRelease(release, policy, localImages)
	}
	return artifactMetadata
}

// A retained release protects every artifact and image it still needs,
// including a digest or tag shared with a release that is being pruned.
func planRegistryProtection(
	releases []Release,
	planned, protected map[string]bool,
	environments map[environmentKey]Environment,
	localImages localImageIndex,
) (registryProtection, map[string]bool, map[string]bool) {
	protection := newRegistryProtection()
	protectedImageDigests := map[string]bool{}
	retainedReleaseIDs := map[string]bool{}
	for _, release := range releases {
		if !registryReleaseRetained(release, planned, protected, environments) {
			continue
		}
		retainedReleaseIDs[release.ID] = true
		protection.addRelease(release, localImages)
		for _, digest := range releaseImageDigests(release) {
			protectedImageDigests[digest] = true
		}
		// Docker's local runtime ID is a separate observation resolved through
		// RepoDigests, never the recorded config digest. Keep every plausible
		// mapping so an ambiguous retained release protects all of them.
		runtimeIDs, _ := releaseRuntimeImageIDs(release, localImages)
		for _, runtimeID := range runtimeIDs {
			protectedImageDigests[runtimeID] = true
		}
	}
	return protection, protectedImageDigests, retainedReleaseIDs
}

func plannedImageReferences(items []Item) map[string]bool {
	planned := map[string]bool{}
	for _, item := range items {
		if item.Kind == ItemImage {
			planned[item.Reference] = true
		}
	}
	return planned
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
// created_at DESC, then id DESC. An empty result means the environment has no
// rollback target.
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

func releaseImageDigests(release Release) []string {
	digests := make([]string, 0, len(release.Artifacts)+2)
	for _, raw := range release.Artifacts {
		if digest := normalizeDigest(raw); validDigest(digest) {
			digests = append(digests, digest)
		}
	}
	if digest := normalizeDigest(release.ImageID); validDigest(digest) {
		digests = append(digests, digest)
	}
	if reference, ok := parseImageReference(release.Image); ok && reference.digest != "" {
		digests = append(digests, reference.digest)
	}
	return digests
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

func planBuildpackCache(policy Policy, cache CacheInventory) []Item {
	if cache.Unknown {
		return []Item{
			{
				Kind:      ItemCacheSkipped,
				Reference: "*",
				Reason:    "the Buildpacks cache could not be measured completely; no cache entry is removed",
			},
		}
	}
	entries, entryBytes := normalizeCacheEntries(cache.Entries)
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

	sort.SliceStable(entries, func(i, j int) bool { return cacheEntryLess(entries[i], entries[j]) })

	items := []Item{}
	for index, entry := range entries {
		items = append(items, Item{
			Kind:      ItemBuildpackCache,
			Reference: entry.Path,
			Bytes:     entry.Bytes,
			Reason: fmt.Sprintf(
				"Buildpacks cache exceeds the %s cap or %d%% disk use; least recently used entry",
				humanBytes(policy.BuildpackCacheMaxBytes),
				policy.BuildpackCacheMaxPercent,
			),
		})
		cacheBytes -= entry.Bytes
		if cacheBytes < 0 {
			cacheBytes = 0
		}
		usedBytes -= entry.Bytes
		if usedBytes < 0 {
			usedBytes = 0
		}
		if cacheRemovalComplete(policy, cache, cacheBytes, usedBytes, index, len(entries)) {
			break
		}
	}
	return items
}

func normalizeCacheEntries(values []CacheEntry) ([]CacheEntry, int64) {
	entries := make([]CacheEntry, 0, len(values))
	var entryBytes int64
	seen := map[string]bool{}
	for _, entry := range values {
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
	return entries, entryBytes
}

func cacheEntryLess(left, right CacheEntry) bool {
	switch {
	case left.LastUsedAt.IsZero() && right.LastUsedAt.IsZero():
	case left.LastUsedAt.IsZero():
		return false
	case right.LastUsedAt.IsZero():
		return true
	default:
		if !left.LastUsedAt.Equal(right.LastUsedAt) {
			return left.LastUsedAt.Before(right.LastUsedAt)
		}
	}
	return left.Path < right.Path
}

func cacheRemovalComplete(policy Policy, cache CacheInventory, cacheBytes, usedBytes int64, index, total int) bool {
	cacheWithinLimit := cacheBytes <= policy.BuildpackCacheMaxBytes
	diskWithinLimit := cache.DiskTotalBytes <= 0 ||
		float64(usedBytes) <= float64(cache.DiskTotalBytes)*float64(policy.BuildpackCacheMaxPercent)/100
	return cacheWithinLimit && (diskWithinLimit || cacheBytes == 0 || index == total-1)
}

func planContainers(
	environmentByKey map[environmentKey]Environment,
	containers []Container,
	inFlightReleases, inFlightDeployments, inFlightContainers map[string]bool,
) []Item {
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
		if item, ok := planContainer(container, environmentByKey, inFlightReleases, inFlightDeployments, inFlightContainers); ok {
			items = append(items, item)
		}
	}
	return items
}

func planContainer(
	container Container,
	environmentByKey map[environmentKey]Environment,
	inFlightReleases, inFlightDeployments, inFlightContainers map[string]bool,
) (Item, bool) {
	// Only an eregion-owned container is a removal candidate. Every
	// container still protects its image through planReleaseImages and
	// planDanglingImages.
	if !container.Owned && strings.TrimSpace(container.App) == "" {
		return Item{}, false
	}
	// Resources are environment-owned dependencies managed by the resource
	// lifecycle. Their containers are never deployment candidates.
	if container.Kind == "resource" {
		return Item{}, false
	}
	if inFlightReleases[container.Release] ||
		inFlightDeployments[container.Deployment] ||
		inFlightContainers[container.Name] ||
		inFlightContainers[container.ID] {
		return Item{}, false
	}
	key := environmentKey{app: container.App, environment: container.Environment}
	environment, known := environmentByKey[key]
	if known {
		if environment.ServingRelease != "" && container.Release == environment.ServingRelease {
			return Item{}, false
		}
		if environment.ServingDeployment != "" &&
			container.Deployment == environment.ServingDeployment {
			return Item{}, false
		}
	}
	reason := "container is neither a serving release nor an active candidate"
	if !known {
		reason = "container labels do not match a known environment"
	}
	return Item{
		Kind:        ItemContainer,
		App:         container.App,
		Environment: container.Environment,
		Reference:   containerReference(container),
		Bytes:       container.Bytes,
		Reason:      reason,
	}, true
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

// planReleaseImages plans the immutable local-image removal for releases that
// are being pruned, and for releases an earlier run already marked pruned whose
// image is still present. It never removes by a tag: a release without an
// ImageID keeps its row only. An image shared with a retained release or any
// existing container (running or stopped) is kept and reported as a diagnostic.
// releaseRuntimeImageIDs maps a release's canonical manifest reference to every
// local Docker runtime image ID whose RepoDigests contain that manifest. The
// recorded config digest is never treated as a Docker inventory ID, and a
// legacy/tag-only row never supplies a removal selector. Multiple IDs are
// plausible mappings, not a reason to forget them: callers protect all of them
// and refuse to delete any single ambiguous candidate.
func releaseRuntimeImageIDs(release Release, localImages localImageIndex) ([]string, string) {
	reference, parsed := parseImageReference(release.Image)
	if !parsed || reference.digest == "" {
		return nil, "release has no canonical manifest reference; the historical row is preserved without an image removal selector"
	}
	seen := map[string]bool{}
	ids := make([]string, 0, 1)
	for imageID, references := range localImages {
		for _, local := range references {
			if local.digest != reference.digest || seen[imageID] {
				continue
			}
			seen[imageID] = true
			ids = append(ids, imageID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, "no local image carries the canonical manifest " + reference.digest
	}
	return ids, ""
}

// planReleaseImages plans the immutable local-image removal for releases that
// are being pruned, and for releases an earlier run already marked pruned whose
// image is still present. It never removes by a tag or by the recorded config
// digest: the canonical manifest reference is resolved to exactly one proven
// local runtime image ID. An absent or ambiguous mapping is skipped
// conservatively, and a retained release protects every plausible mapping.
func planReleaseImages(
	releases []Release,
	planned, protected map[string]bool,
	containers []Container,
	localImages []LocalImageRef,
	dangling []Image,
) []Item {
	localIndex := indexLocalImages(localImages)
	retainedImages := retainedReleaseImages(releases, planned, protected, localIndex)
	containerImages := containerImageOwners(containers)
	imageTags := localImageRepoTags(localImages)
	presentImages := presentImageIDs(localImages, dangling)

	plannedImages := map[string]bool{}
	items := []Item{}
	for _, release := range releases {
		isPlanned := planned[release.ID]
		isPruned := release.PrunedAt != nil && !protected[release.ID]
		if !isPlanned && !isPruned {
			continue
		}
		item, ok := planReleaseImage(
			release,
			isPlanned,
			localIndex,
			retainedImages,
			containerImages,
			presentImages,
			plannedImages,
			imageTags,
		)
		if ok {
			items = append(items, item)
		}
	}
	return items
}

func retainedReleaseImages(releases []Release, planned, protected map[string]bool, localIndex localImageIndex) map[string]string {
	retainedImages := map[string]string{}
	for _, release := range releases {
		if planned[release.ID] {
			continue
		}
		if release.PrunedAt != nil && !protected[release.ID] {
			continue
		}
		imageIDs, _ := releaseRuntimeImageIDs(release, localIndex)
		for _, imageID := range imageIDs {
			if _, exists := retainedImages[imageID]; !exists {
				retainedImages[imageID] = release.ID
			}
		}
	}
	return retainedImages
}

func presentImageIDs(localImages []LocalImageRef, dangling []Image) map[string]bool {
	presentImages := map[string]bool{}
	for _, image := range localImages {
		if imageID := normalizeDigest(image.ID); imageID != "" {
			presentImages[imageID] = true
		}
	}
	for _, image := range dangling {
		if imageID := normalizeDigest(image.ID); imageID != "" {
			presentImages[imageID] = true
		}
	}
	return presentImages
}

func planReleaseImage(
	release Release,
	isPlanned bool,
	localIndex localImageIndex,
	retainedImages, containerImages map[string]string,
	presentImages, plannedImages map[string]bool,
	imageTags map[string][]string,
) (Item, bool) {
	imageIDs, mappingReason := releaseRuntimeImageIDs(release, localIndex)
	if len(imageIDs) == 0 {
		if !isPlanned {
			return Item{}, false
		}
		return releaseImageSkipped(release, mappingReason+"; no image is removed without a proven runtime mapping"), true
	}
	if len(imageIDs) > 1 {
		if !isPlanned {
			return Item{}, false
		}
		return releaseImageSkipped(release, "multiple local images carry the canonical manifest; removal is ambiguous and every plausible runtime image is protected"), true
	}
	imageID := imageIDs[0]
	if owner, shared := retainedImages[imageID]; shared {
		if !isPlanned {
			return Item{}, false
		}
		return releaseImageSkipped(release, "image "+imageID+" is still used by retained release "+owner), true
	}
	if container, shared := containerImages[imageID]; shared {
		if !isPlanned {
			return Item{}, false
		}
		return releaseImageSkipped(release, "used by container "+container), true
	}
	if !isPlanned && !presentImages[imageID] {
		// The earlier run's image is already gone. The release row stays
		// pruned; a later run has nothing to remove.
		return Item{}, false
	}
	if plannedImages[imageID] {
		return Item{}, false
	}
	plannedImages[imageID] = true
	return Item{
		Kind: ItemImage, App: release.App, Environment: release.Environment,
		Release: release.ID, Reference: imageID, Tags: imageTags[imageID],
		Reason: "release image is not used by a retained release or container",
	}, true
}

func releaseImageSkipped(release Release, reason string) Item {
	return Item{
		Kind:        ItemImageSkipped,
		App:         release.App,
		Environment: release.Environment,
		Release:     release.ID,
		Reference:   release.ID,
		Reason:      reason,
	}
}

// containerImageOwners maps every container's immutable image ID to one stable
// owner name. Both labeled and unlabeled containers protect their image; the
// lexicographically smallest name keeps the plan deterministic when several
// containers share an image.
func containerImageOwners(containers []Container) map[string]string {
	owners := map[string]string{}
	for _, container := range containers {
		imageID := normalizeDigest(container.Image)
		if imageID == "" {
			continue
		}
		owner := containerReference(container)
		if existing, found := owners[imageID]; !found || owner < existing {
			owners[imageID] = owner
		}
	}
	return owners
}

// localImageRepoTags maps an immutable image ID to its current repository tags.
// It is dry-run detail: the removal effect re-reads the tags before untagging.
func localImageRepoTags(localImages []LocalImageRef) map[string][]string {
	seen := map[string]map[string]bool{}
	for _, image := range localImages {
		imageID := normalizeDigest(image.ID)
		if imageID == "" {
			continue
		}
		for _, tag := range image.RepoTags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			if seen[imageID] == nil {
				seen[imageID] = map[string]bool{}
			}
			seen[imageID][tag] = true
		}
	}
	tags := make(map[string][]string, len(seen))
	for imageID, values := range seen {
		list := make([]string, 0, len(values))
		for tag := range values {
			list = append(list, tag)
		}
		sort.Strings(list)
		tags[imageID] = list
	}
	return tags
}

func planDanglingImages(
	images []Image,
	protectedArtifacts, retainedReleaseIDs, plannedImageIDs map[string]bool,
	containers []Container,
) []Item {
	sorted := append([]Image(nil), images...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return imageReference(sorted[i]) < imageReference(sorted[j])
	})
	containerImages := containerImageOwners(containers)

	items := []Item{}
	for _, image := range sorted {
		if !image.Dangling {
			continue
		}
		if image.Release != "" && retainedReleaseIDs[image.Release] {
			continue
		}
		reference := imageReference(image)
		if reference == "" || protectedArtifacts[reference] || plannedImageIDs[reference] {
			continue
		}
		if digest := strings.TrimSpace(image.Digest); digest != "" && protectedArtifacts[digest] {
			continue
		}
		if owner, used := containerImages[normalizeDigest(reference)]; used {
			items = append(items, Item{
				Kind: ItemImageSkipped, Reference: reference,
				Reason: "used by container " + owner,
			})
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
