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

type registryArtifactEntry struct {
	artifact RegistryArtifact
	key      artifactIdentity
}

// registryInventoryIndex is the normalized, de-duplicated view of the registry
// inventory. It also answers whether a kept release's references resolve.
type registryInventoryIndex struct {
	entries   []registryArtifactEntry
	manifests map[artifactIdentity]bool
	tags      map[repositoryTag]bool
	digests   map[string]bool
}

func indexRegistryArtifacts(registry RegistryInventory) registryInventoryIndex {
	index := registryInventoryIndex{
		manifests: map[artifactIdentity]bool{},
		tags:      map[repositoryTag]bool{},
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
		for _, tag := range entry.artifact.Tags {
			index.tags[repositoryTag{repository: entry.key.repository, tag: tag}] = true
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

func (i registryInventoryIndex) hasManifest(repository, digest string) bool {
	return i.manifests[artifactIdentity{repository: repository, digest: normalizeDigest(digest)}]
}

func (i registryInventoryIndex) hasTag(repository, tag string) bool {
	tag = strings.TrimSpace(tag)
	for key := range i.tags {
		if key.tag == tag && repositoryMatches(key.repository, repository) {
			return true
		}
	}
	return false
}

func (i registryInventoryIndex) hasDigest(digest string) bool {
	return i.digests[normalizeDigest(digest)]
}

// repositoryMatches compares a release repository with an inventory
// repository. Registry catalogs often omit a registry host, and the first path
// component of a reference can be a host in one spelling and a namespace in
// another; ignoring one leading component on either side keeps those spellings
// matched without collapsing unrelated namespaces.
func repositoryMatches(left, right string) bool {
	if left == right {
		return true
	}
	if left == "" || right == "" {
		return false
	}
	return stripRepositoryComponent(left) == right || stripRepositoryComponent(right) == left
}

func stripRepositoryComponent(repository string) string {
	if index := strings.Index(repository, "/"); index >= 0 {
		return repository[index+1:]
	}
	return ""
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

func (p *registryProtection) addRelease(release Release) {
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

func (i *artifactMetadataIndex) recordRelease(release Release, policy Policy) {
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

// registrySkipReasons implements the fail-safe rule: when a retained release's
// image reference cannot be resolved to an inventory artifact in its
// repository, that repository plans no deletions. A reference with no usable
// repository skips the whole registry.
func registrySkipReasons(releases []Release, planned, protected map[string]bool, environments map[environmentKey]Environment, index registryInventoryIndex) (map[string]string, bool) {
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
		image := strings.TrimSpace(release.Image)
		if image == "" {
			for _, raw := range release.Artifacts {
				digest := normalizeDigest(raw)
				if !validDigest(digest) {
					continue
				}
				if !index.hasDigest(digest) {
					skipEverything(fmt.Sprintf("release %s references manifest %s, which is not in the registry inventory", release.ID, digest))
					break
				}
			}
			continue
		}
		reference, parsed := parseImageReference(image)
		if !parsed || reference.repository == "" {
			skipEverything(fmt.Sprintf("release %s image %q cannot be resolved to a registry repository", release.ID, image))
			continue
		}
		unresolved := func(detail string) {
			skipRepository(reference.repository, fmt.Sprintf("release %s image %q does not resolve to a registry inventory artifact (%s)",
				release.ID, image, detail))
		}
		if reference.digest != "" {
			if !index.hasManifest(reference.repository, reference.digest) {
				unresolved("missing manifest " + reference.digest)
			}
		} else if reference.tag != "" {
			if !index.hasTag(reference.repository, reference.tag) {
				unresolved("missing tag " + reference.tag)
			}
		}
		for _, raw := range release.Artifacts {
			digest := normalizeDigest(raw)
			if !validDigest(digest) {
				continue
			}
			if !index.hasManifest(reference.repository, digest) {
				unresolved("missing manifest " + digest)
			}
		}
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
