package retention

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// artifactIdentity is one manifest in one repository. A manifest digest may
// exist in several repositories; removal items and protection never collapse
// those into one item.
type artifactIdentity struct {
	repository string
	digest     string
}

type repositoryTag struct {
	repository string
	tag        string
}

type registryConfigReference struct {
	repository string
	digest     string
}

// localImageReference is one immutable repository digest for a local image ID.
type localImageReference struct {
	repository string
	digest     string
}

// localImageIndex maps a Docker image config digest to the repository digests
// Docker recorded for it.
type localImageIndex map[string][]localImageReference

func indexLocalImages(images []LocalImageRef) localImageIndex {
	index := localImageIndex{}
	for _, image := range images {
		id := normalizeDigest(image.ID)
		if id == "" {
			continue
		}
		seen := map[localImageReference]bool{}
		for _, existing := range index[id] {
			seen[existing] = true
		}
		for _, raw := range image.RepoDigests {
			reference, parsed := parseImageReference(raw)
			if !parsed || reference.digest == "" {
				continue
			}
			candidate := localImageReference{repository: reference.repository, digest: reference.digest}
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			index[id] = append(index[id], candidate)
		}
	}
	return index
}

func (i localImageIndex) references(id string) []localImageReference {
	return i[normalizeDigest(id)]
}

type registryArtifactEntry struct {
	artifact RegistryArtifact
	key      artifactIdentity
}

// registryInventoryIndex is the normalized, de-duplicated view of the registry
// inventory. It also answers whether a kept release's references resolve.
type registryInventoryIndex struct {
	entries   []registryArtifactEntry
	manifests map[artifactIdentity]bool
	digests   map[string]bool
	configs   []registryConfigReference
}

func indexRegistryArtifacts(registry RegistryInventory) registryInventoryIndex {
	index := registryInventoryIndex{
		manifests: map[artifactIdentity]bool{},
		digests:   map[string]bool{},
	}
	raw := append([]RegistryArtifact(nil), registry.Artifacts...)
	sort.SliceStable(raw, func(i, j int) bool {
		if left, right := strings.TrimSpace(raw[i].Repository), strings.TrimSpace(raw[j].Repository); left != right {
			return left < right
		}
		if left, right := normalizeDigest(raw[i].Digest), normalizeDigest(raw[j].Digest); left != right {
			return left < right
		}
		if left, right := normalizeDigest(raw[i].ConfigDigest), normalizeDigest(raw[j].ConfigDigest); left != right {
			return left < right
		}
		return strings.Join(raw[i].Tags, ",") < strings.Join(raw[j].Tags, ",")
	})

	position := map[artifactIdentity]int{}
	for _, candidate := range raw {
		artifact, key, ok := normalizeRegistryArtifact(candidate)
		if !ok {
			continue
		}
		if existing, exists := position[key]; exists {
			entry := &index.entries[existing]
			if entry.artifact.Repository == "" {
				entry.artifact.Repository = artifact.Repository
			}
			if entry.artifact.ConfigDigest == "" {
				entry.artifact.ConfigDigest = artifact.ConfigDigest
			}
			entry.artifact.Tags = mergeTags(entry.artifact.Tags, artifact.Tags)
			if entry.artifact.Bytes == 0 {
				entry.artifact.Bytes = artifact.Bytes
			}
			if !artifact.CreatedAt.IsZero() && (entry.artifact.CreatedAt.IsZero() || artifact.CreatedAt.Before(entry.artifact.CreatedAt)) {
				entry.artifact.CreatedAt = artifact.CreatedAt
			}
			continue
		}
		position[key] = len(index.entries)
		index.entries = append(index.entries, registryArtifactEntry{artifact: artifact, key: key})
	}
	sort.SliceStable(index.entries, func(i, j int) bool {
		if index.entries[i].key.repository != index.entries[j].key.repository {
			return index.entries[i].key.repository < index.entries[j].key.repository
		}
		if index.entries[i].key.digest != index.entries[j].key.digest {
			return index.entries[i].key.digest < index.entries[j].key.digest
		}
		return index.entries[i].artifact.Repository < index.entries[j].artifact.Repository
	})

	for _, entry := range index.entries {
		index.manifests[entry.key] = true
		index.digests[entry.key.digest] = true
		if entry.artifact.ConfigDigest != "" {
			index.configs = append(index.configs, registryConfigReference{
				repository: entry.key.repository,
				digest:     entry.artifact.ConfigDigest,
			})
		}
	}
	return index
}

func normalizeRegistryArtifact(candidate RegistryArtifact) (RegistryArtifact, artifactIdentity, bool) {
	digest := normalizeDigest(candidate.Digest)
	if !validDigest(digest) {
		return RegistryArtifact{}, artifactIdentity{}, false
	}
	artifact := candidate
	artifact.Digest = digest
	artifact.Repository = strings.TrimSpace(candidate.Repository)
	configDigest := normalizeDigest(candidate.ConfigDigest)
	if !validDigest(configDigest) {
		configDigest = ""
	}
	artifact.ConfigDigest = configDigest
	artifact.Tags = mergeTags(nil, candidate.Tags)
	repository := normalizeRepository(artifact.Repository)
	if repository == "" {
		return RegistryArtifact{}, artifactIdentity{}, false
	}
	key := artifactIdentity{repository: repository, digest: digest}
	return artifact, key, true
}

func mergeTags(existing, incoming []string) []string {
	seen := map[string]bool{}
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, tag := range append(append([]string(nil), existing...), incoming...) {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		merged = append(merged, tag)
	}
	sort.Strings(merged)
	return merged
}

// hasManifestFor reports whether an inventory artifact with the manifest digest
// exists in the release's repository. An empty repository matches any
// repository, which is the only option when the release names a bare digest.
func (i registryInventoryIndex) hasManifestFor(repository, digest string) bool {
	digest = normalizeDigest(digest)
	if !validDigest(digest) {
		return false
	}
	if repository == "" {
		return i.digests[digest]
	}
	for key := range i.manifests {
		if key.digest == digest && repositoryMatches(key.repository, repository) {
			return true
		}
	}
	return false
}

// hasConfigDigest reports whether an inventory artifact exposes the image config
// digest, optionally limited to the release's repository.
func (i registryInventoryIndex) hasConfigDigest(repository, digest string) bool {
	digest = normalizeDigest(digest)
	if !validDigest(digest) {
		return false
	}
	for _, config := range i.configs {
		if config.digest == digest && (repository == "" || repositoryMatches(config.repository, repository)) {
			return true
		}
	}
	return false
}

// hasRepository reports whether the inventory contains any artifact in the
// release's repository. When it does not, there is nothing to protect or
// remove, so an unresolved release is not a reason to skip anything.
func (i registryInventoryIndex) hasRepository(repository string) bool {
	repository = normalizeRepository(repository)
	if repository == "" {
		return false
	}
	for key := range i.manifests {
		if repositoryMatches(key.repository, repository) {
			return true
		}
	}
	return false
}

// repositoryMatches compares a release repository with an inventory
// repository. Registry catalogs may omit the registry host, while a canonical
// release reference includes it. Only a component that is unambiguously a
// registry host (localhost, a DNS name, or a host:port) may be ignored; an
// ordinary namespace component is never stripped, so two apps with the same
// final component do not share protection accidentally.
func repositoryMatches(left, right string) bool {
	if left == right {
		return true
	}
	if left == "" || right == "" {
		return false
	}
	return stripRegistryHost(left) == right || stripRegistryHost(right) == left
}

func stripRegistryHost(repository string) string {
	index := strings.Index(repository, "/")
	if index < 0 {
		return ""
	}
	first := repository[:index]
	if !looksLikeRegistryHost(first) {
		return ""
	}
	return repository[index+1:]
}

// registryProtection is the set of references a retained release still needs.
// Protection is fail-safe: a manifest is kept when any digest, tag or config
// digest matches.
type registryProtection struct {
	manifests map[artifactIdentity]bool
	tags      map[repositoryTag]bool
	digests   map[string]bool
	configs   map[string]bool
}

func newRegistryProtection() registryProtection {
	return registryProtection{
		manifests: map[artifactIdentity]bool{},
		tags:      map[repositoryTag]bool{},
		digests:   map[string]bool{},
		configs:   map[string]bool{},
	}
}

func (p *registryProtection) addRelease(release Release, localImages localImageIndex) {
	reference, parsed := parseImageReference(release.Image)
	if parsed {
		if reference.digest != "" {
			p.digests[reference.digest] = true
			if reference.repository != "" {
				p.manifests[artifactIdentity{repository: reference.repository, digest: reference.digest}] = true
			}
		}
		if reference.tag != "" && reference.repository != "" {
			p.tags[repositoryTag{repository: reference.repository, tag: reference.tag}] = true
		}
	}
	for _, raw := range release.Artifacts {
		digest := normalizeDigest(raw)
		if !validDigest(digest) {
			continue
		}
		p.digests[digest] = true
		if parsed && reference.repository != "" {
			p.manifests[artifactIdentity{repository: reference.repository, digest: digest}] = true
		}
	}
	for _, local := range localImages.references(release.ImageID) {
		p.digests[local.digest] = true
		if local.repository != "" {
			p.manifests[artifactIdentity{repository: local.repository, digest: local.digest}] = true
		}
	}
	if config := normalizeDigest(release.ImageID); validDigest(config) {
		p.configs[config] = true
	}
}

func (p registryProtection) protects(entry registryArtifactEntry) bool {
	if p.manifests[entry.key] || p.digests[entry.key.digest] {
		return true
	}
	if entry.artifact.ConfigDigest != "" && p.configs[entry.artifact.ConfigDigest] {
		return true
	}
	for _, tag := range entry.artifact.Tags {
		for key := range p.tags {
			if key.tag == tag && repositoryMatches(key.repository, entry.key.repository) {
				return true
			}
		}
	}
	return false
}

// artifactMetadata is the owning release and applicable grace for one artifact
// reference. It is recorded from every release row, including previously pruned
// rows, so an environment override survives later runs.
type artifactMetadata struct {
	app         string
	environment string
	release     string
	graceDays   int
}

type artifactMetadataIndex struct {
	manifests     map[artifactIdentity]artifactMetadata
	tags          map[repositoryTag]artifactMetadata
	digests       map[string]artifactMetadata
	configs       map[string]artifactMetadata
	fallbackGrace int
}

func newArtifactMetadataIndex(serverPolicy Policy) artifactMetadataIndex {
	return artifactMetadataIndex{
		manifests:     map[artifactIdentity]artifactMetadata{},
		tags:          map[repositoryTag]artifactMetadata{},
		digests:       map[string]artifactMetadata{},
		configs:       map[string]artifactMetadata{},
		fallbackGrace: serverPolicy.ArtifactGraceDays,
	}
}

func (i *artifactMetadataIndex) recordRelease(release Release, policy Policy, localImages localImageIndex) {
	metadata := artifactMetadata{
		app:         release.App,
		environment: release.Environment,
		release:     release.ID,
		graceDays:   policy.ArtifactGraceDays,
	}
	reference, parsed := parseImageReference(release.Image)
	if parsed {
		if reference.digest != "" {
			mergeArtifactMetadata(i.digests, reference.digest, metadata)
			if reference.repository != "" {
				mergeArtifactMetadata(i.manifests, artifactIdentity{repository: reference.repository, digest: reference.digest}, metadata)
			}
		}
		if reference.tag != "" && reference.repository != "" {
			mergeArtifactMetadata(i.tags, repositoryTag{repository: reference.repository, tag: reference.tag}, metadata)
		}
	}
	for _, raw := range release.Artifacts {
		digest := normalizeDigest(raw)
		if !validDigest(digest) {
			continue
		}
		mergeArtifactMetadata(i.digests, digest, metadata)
		if parsed && reference.repository != "" {
			mergeArtifactMetadata(i.manifests, artifactIdentity{repository: reference.repository, digest: digest}, metadata)
		}
	}
	for _, local := range localImages.references(release.ImageID) {
		mergeArtifactMetadata(i.digests, local.digest, metadata)
		if local.repository != "" {
			mergeArtifactMetadata(i.manifests, artifactIdentity{repository: local.repository, digest: local.digest}, metadata)
		}
	}
	if config := normalizeDigest(release.ImageID); validDigest(config) {
		mergeArtifactMetadata(i.configs, config, metadata)
	}
}

func (i artifactMetadataIndex) lookup(entry registryArtifactEntry) artifactMetadata {
	best := artifactMetadata{graceDays: i.fallbackGrace}
	if metadata, ok := i.manifests[entry.key]; ok {
		best = mergeArtifactMetadataValue(best, metadata)
	}
	if metadata, ok := i.digests[entry.key.digest]; ok {
		best = mergeArtifactMetadataValue(best, metadata)
	}
	for _, tag := range entry.artifact.Tags {
		for key, metadata := range i.tags {
			if key.tag == tag && repositoryMatches(key.repository, entry.key.repository) {
				best = mergeArtifactMetadataValue(best, metadata)
			}
		}
	}
	if entry.artifact.ConfigDigest != "" {
		if metadata, ok := i.configs[entry.artifact.ConfigDigest]; ok {
			best = mergeArtifactMetadataValue(best, metadata)
		}
	}
	return best
}

func mergeArtifactMetadata[K comparable](values map[K]artifactMetadata, key K, candidate artifactMetadata) {
	if existing, ok := values[key]; ok {
		values[key] = mergeArtifactMetadataValue(existing, candidate)
		return
	}
	values[key] = candidate
}

func mergeArtifactMetadataValue(existing, candidate artifactMetadata) artifactMetadata {
	if existing.release == "" {
		return candidate
	}
	if candidate.release == "" {
		return existing
	}
	if candidate.graceDays > existing.graceDays {
		existing.graceDays = candidate.graceDays
	}
	if candidate.release < existing.release {
		existing.app = candidate.app
		existing.environment = candidate.environment
		existing.release = candidate.release
	}
	return existing
}

// registryReleaseRetained reports whether a release still needs its registry
// references protected. A release with no known environment is always
// considered retained: the planner has no policy with which to judge it.
func registryReleaseRetained(release Release, planned, protected map[string]bool, environments map[environmentKey]Environment) bool {
	if planned[release.ID] {
		return false
	}
	if _, known := environments[environmentKey{app: release.App, environment: release.Environment}]; !known {
		return true
	}
	if release.PrunedAt != nil && !protected[release.ID] {
		return false
	}
	return true
}

// releaseRepository returns the normalized repository named by Release.Image,
// or an empty string when it cannot be determined.
func releaseRepository(release Release) string {
	reference, parsed := parseImageReference(release.Image)
	if !parsed {
		return ""
	}
	return reference.repository
}

func releaseReferencesRegistry(release Release) bool {
	if strings.TrimSpace(release.Image) != "" {
		return true
	}
	if validDigest(normalizeDigest(release.ImageID)) {
		return true
	}
	for _, raw := range release.Artifacts {
		if validDigest(normalizeDigest(raw)) {
			return true
		}
	}
	return false
}

// releaseNeverProducedImage reports whether a release row cannot own a registry
// artifact. A release that never reached ready and records no image identity
// has nothing to protect or remove. A failed release that did push an image
// keeps its ImageID, and any artifact digest or digest in Image means the
// release may own something: those still require immutable resolution. An
// unknown status is treated like ready so it stays fail-safe.
func releaseNeverProducedImage(release Release) bool {
	switch release.Status {
	case ReleaseStatePending, ReleaseStateFailed:
	default:
		return false
	}
	if strings.TrimSpace(release.ImageID) != "" {
		return false
	}
	for _, raw := range release.Artifacts {
		if strings.TrimSpace(raw) != "" {
			return false
		}
	}
	if reference, parsed := parseImageReference(release.Image); parsed && reference.digest != "" {
		return false
	}
	return true
}

// releaseHasImmutableIdentity reports whether a retained release can be tied to
// an inventory artifact through something a moved tag cannot change: a manifest
// digest or the image config digest. A tag is protection, never resolution.
func releaseHasImmutableIdentity(release Release, repository string, index registryInventoryIndex, localImages localImageIndex) bool {
	reference, parsed := parseImageReference(release.Image)
	if parsed && reference.digest != "" && index.hasManifestFor(repository, reference.digest) {
		return true
	}
	for _, raw := range release.Artifacts {
		digest := normalizeDigest(raw)
		if validDigest(digest) && index.hasManifestFor(repository, digest) {
			return true
		}
	}
	for _, local := range localImages.references(release.ImageID) {
		repositoryForLocal := repository
		if repositoryForLocal == "" {
			repositoryForLocal = local.repository
		}
		if index.hasManifestFor(repositoryForLocal, local.digest) {
			return true
		}
	}
	config := normalizeDigest(release.ImageID)
	return validDigest(config) && index.hasConfigDigest(repository, config)
}

// registrySkipReasons implements the fail-safe rule: when a retained release
// has no immutable identity that resolves to an inventory artifact, that
// repository plans no deletions. A release with no usable repository skips the
// whole registry. A tag is never resolution. A release that never produced an
// image needs no resolution, and a repository the inventory does not contain
// gets no diagnostic: there is nothing there to protect or remove.
func registrySkipReasons(releases []Release, planned, protected map[string]bool, environments map[environmentKey]Environment, index registryInventoryIndex, localImages localImageIndex) (map[string]string, bool) {
	reasons := map[string]string{}
	skipAll := false
	allReason := ""
	skipEverything := func(reason string) {
		if skipAll {
			return
		}
		skipAll = true
		allReason = reason
	}
	skipRepository := func(repository, reason string) {
		if _, exists := reasons[repository]; !exists {
			reasons[repository] = reason
		}
	}

	for _, release := range releases {
		if !registryReleaseRetained(release, planned, protected, environments) {
			continue
		}
		if !releaseReferencesRegistry(release) {
			continue
		}
		if releaseNeverProducedImage(release) {
			continue
		}
		repository := releaseRepository(release)
		if repository == "" {
			if releaseHasImmutableIdentity(release, repository, index, localImages) {
				continue
			}
			reason := fmt.Sprintf("release %s has no immutable image digest or config digest that resolves to a registry inventory artifact", release.ID)
			if image := strings.TrimSpace(release.Image); image != "" {
				reason = fmt.Sprintf("%s (image %q)", reason, image)
			}
			skipEverything(reason)
			continue
		}
		if !index.hasRepository(repository) {
			continue
		}
		if releaseHasImmutableIdentity(release, repository, index, localImages) {
			continue
		}
		reason := fmt.Sprintf("release %s has no immutable image digest or config digest that resolves to a registry inventory artifact", release.ID)
		if image := strings.TrimSpace(release.Image); image != "" {
			reason = fmt.Sprintf("%s (image %q)", reason, image)
		}
		skipRepository(repository, reason)
	}
	if skipAll {
		return map[string]string{"*": allReason}, true
	}
	return reasons, false
}

func planRegistryArtifacts(index registryInventoryIndex, protection registryProtection, metadata artifactMetadataIndex, skips map[string]string, skipAll bool, now time.Time) []Item {
	if skipAll {
		return []Item{{
			Kind:      ItemRegistrySkipped,
			Reference: "*",
			Reason:    skips["*"],
		}}
	}

	items := []Item{}
	for _, entry := range index.entries {
		if _, skipped := skips[entry.key.repository]; skipped {
			continue
		}
		if protection.protects(entry) {
			continue
		}
		owner := metadata.lookup(entry)
		if !entry.artifact.CreatedAt.Before(daysBefore(now, owner.graceDays)) {
			continue
		}
		item := Item{
			Kind:      ItemRegistryArtifact,
			Reference: artifactReference(entry.artifact),
			Bytes:     entry.artifact.Bytes,
		}
		if owner.release != "" {
			item.App = owner.app
			item.Environment = owner.environment
			item.Reason = fmt.Sprintf("registry artifact referenced by release %s and older than %d days", owner.release, owner.graceDays)
		} else {
			item.Reason = fmt.Sprintf("registry artifact is unreferenced by any kept release and older than %d days", owner.graceDays)
		}
		items = append(items, item)
	}

	repositories := make([]string, 0, len(skips))
	for repository := range skips {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	for _, repository := range repositories {
		items = append(items, Item{
			Kind:      ItemRegistrySkipped,
			Reference: repository,
			Reason:    skips[repository],
		})
	}
	return items
}
