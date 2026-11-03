package platform

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/retention"
)

// Buildpacks cache volumes. An eregion cache embeds the app and environment
// names hex-encoded, joined by "_": app and environment names cannot contain
// "_", and hex is reversible and cannot collide (`a-b`/`c` vs `a`/`b-c`). The
// encoding also avoids pack's reserved-name rewriting (`aux`, `com`, `con`,
// `lpt`, `nul`, `prn`), which would otherwise silently change an explicit
// volume name. A pre-upgrade pack build left pack's own
// `pack-cache-<hash>.<build|launch>` volumes, reported as ownerless caches.
const (
	BuildpackCacheVolumePrefix       = "eregion-build-cache-"
	BuildpackLaunchCacheVolumePrefix = "eregion-launch-cache-"
	PackCacheVolumePrefix            = "pack-cache-"
)

// BuildpackCacheVolume is the deterministic build-cache volume of one
// environment. App and environment names are already validated by the caller.
func BuildpackCacheVolume(app, environment string) string {
	return BuildpackCacheVolumePrefix + hex.EncodeToString([]byte(app)) + "_" + hex.EncodeToString([]byte(environment))
}

// BuildpackLaunchCacheVolume is the deterministic launch-cache volume of one
// environment.
func BuildpackLaunchCacheVolume(app, environment string) string {
	return BuildpackLaunchCacheVolumePrefix + hex.EncodeToString([]byte(app)) + "_" + hex.EncodeToString([]byte(environment))
}

// ParseBuildpacksCacheVolume reports which app and environment own a cache
// volume. Kind is "build" or "launch" for an eregion cache and "pack" for a
// legacy ownerless pack cache.
func ParseBuildpacksCacheVolume(name string) (app, environment, kind string, ok bool) {
	for _, candidate := range []struct {
		prefix string
		kind   string
	}{
		{BuildpackCacheVolumePrefix, "build"},
		{BuildpackLaunchCacheVolumePrefix, "launch"},
	} {
		rest, found := strings.CutPrefix(name, candidate.prefix)
		if !found {
			continue
		}
		appHex, environmentHex, found := strings.Cut(rest, "_")
		if !found || len(appHex)%2 != 0 || len(environmentHex)%2 != 0 {
			return "", "", candidate.kind, false
		}
		appBytes, appErr := hex.DecodeString(appHex)
		environmentBytes, environmentErr := hex.DecodeString(environmentHex)
		if appErr != nil || environmentErr != nil {
			return "", "", candidate.kind, false
		}
		app, environment = string(appBytes), string(environmentBytes)
		if ValidateName(app) != nil || ValidateName(environment) != nil {
			return "", "", candidate.kind, false
		}
		return app, environment, candidate.kind, true
	}
	if strings.HasPrefix(name, PackCacheVolumePrefix) &&
		(strings.HasSuffix(name, ".build") || strings.HasSuffix(name, ".launch")) {
		return "", "", "pack", true
	}
	return "", "", "", false
}

// Production node paths read by the retention inventory. They match
// host.DefaultLayout; platform cannot import host (host imports platform).
const (
	DefaultDataDir     = "/var/lib/eregion"
	DefaultRegistryDir = DefaultDataDir + "/registry"
)

// RetentionInventoryPaths are the node directories the inventory reads.
type RetentionInventoryPaths struct {
	DataDir     string
	RegistryDir string
}

func (p RetentionInventoryPaths) resolved() RetentionInventoryPaths {
	if strings.TrimSpace(p.DataDir) == "" {
		p.DataDir = DefaultDataDir
	}
	if strings.TrimSpace(p.RegistryDir) == "" {
		p.RegistryDir = filepath.Join(p.DataDir, "registry")
	}
	return p
}

// NodeReadRunner runs one fixed, read-only node command and returns its
// standard output. A test replaces it; production runs the real program.
type NodeReadRunner func(ctx context.Context, argv ...string) ([]byte, error)

// RunNodeReadCommand runs one read-only node command with a bounded timeout and
// a fixed environment. It never uses a shell.
func RunNodeReadCommand(ctx context.Context, argv ...string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty node command")
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed argv, never request text.
	command.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root", "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if runCtx.Err() != nil {
			return nil, runCtx.Err()
		}
		detail := boundedCommandOutput(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, detail)
	}
	return stdout.Bytes(), nil
}

// The inventory caps every unbounded Docker list so one chatty host cannot
// produce a result larger than the node protocol frame.
const maxRetentionInventoryItems = 500

// ObserveRetentionInventory reads the node's reclaimable artifacts. It is
// read-only: no journal entry, operation or database row is created.
func ObserveRetentionInventory(ctx context.Context, paths RetentionInventoryPaths, run NodeReadRunner) (retention.Inventory, error) {
	if run == nil {
		run = RunNodeReadCommand
	}
	paths = paths.resolved()
	inventory := retention.Inventory{
		CollectedAt:       time.Now().UTC(),
		RegistryArtifacts: []retention.InventoryRegistryArtifact{},
		CacheVolumes:      []retention.CacheVolume{},
		Containers:        []retention.InventoryContainer{},
		DanglingImages:    []retention.InventoryImage{},
		LocalImages:       []retention.LocalImage{},
	}
	artifacts, warnings, truncated, err := readRegistryArtifacts(paths.RegistryDir)
	if err != nil {
		return retention.Inventory{}, contract.NewFault(contract.Unavailable, "the node's registry storage is unreadable", err.Error())
	}
	inventory.RegistryArtifacts = artifacts
	inventory.Warnings = append(inventory.Warnings, warnings...)
	inventory.Truncated = inventory.Truncated || truncated

	containers, truncated, err := readRetentionContainers(ctx, run)
	if err != nil {
		return retention.Inventory{}, err
	}
	inventory.Containers = containers
	inventory.Truncated = inventory.Truncated || truncated

	images, truncated, err := readDanglingImages(ctx, run)
	if err != nil {
		return retention.Inventory{}, err
	}
	inventory.DanglingImages = images
	inventory.Truncated = inventory.Truncated || truncated

	localImages, truncated, err := readLocalImages(ctx, run)
	if err != nil {
		return retention.Inventory{}, err
	}
	inventory.LocalImages = localImages
	inventory.Truncated = inventory.Truncated || truncated

	volumes, truncated, err := readCacheVolumes(ctx, run)
	if err != nil {
		return retention.Inventory{}, err
	}
	inventory.CacheVolumes = volumes
	inventory.Truncated = inventory.Truncated || truncated
	for _, volume := range volumes {
		if volume.SizeUnknown {
			inventory.CacheSizeUnknown = true
			inventory.CacheError = "one or more Buildpacks cache volumes could not be measured"
			break
		}
	}
	if truncated && !inventory.CacheSizeUnknown {
		inventory.CacheSizeUnknown = true
		inventory.CacheError = "the Buildpacks cache volume list was truncated"
	}

	inventory.Disk = retentionDisk(paths.DataDir)
	return inventory, nil
}

// --------------------------------------------------------------- containers

type dockerContainerInspect struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	State   struct {
		Status string `json:"Status"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	SizeRw int64 `json:"SizeRw"`
}

func readRetentionContainers(ctx context.Context, run NodeReadRunner) ([]retention.InventoryContainer, bool, error) {
	raw, err := run(ctx, "docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "label=io.eregion.app")
	if err != nil {
		return nil, false, dockerObservationFault("list labeled containers", err)
	}
	ids := splitLines(raw)
	ids, truncated := capRetentionItems(ids)
	if len(ids) == 0 {
		return []retention.InventoryContainer{}, false, nil
	}
	var inspected []dockerContainerInspect
	for _, chunk := range chunkStrings(ids, 100) {
		argv := append([]string{"docker", "container", "inspect", "--size"}, chunk...)
		raw, err := run(ctx, argv...)
		if err != nil {
			return nil, false, dockerObservationFault("inspect labeled containers", err)
		}
		var page []dockerContainerInspect
		if err := json.Unmarshal(bytes.TrimSpace(raw), &page); err != nil {
			return nil, false, contract.NewFault(contract.Unavailable, "the node returned an unreadable container observation", err.Error())
		}
		inspected = append(inspected, page...)
	}
	containers := make([]retention.InventoryContainer, 0, len(inspected))
	for _, item := range inspected {
		containers = append(containers, retention.InventoryContainer{
			ID: item.ID, Name: strings.TrimPrefix(item.Name, "/"), Image: item.Config.Image,
			State: item.State.Status, Labels: item.Config.Labels, SizeBytes: item.SizeRw,
			CreatedAt: parseDockerTime(item.Created),
		})
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	return containers, truncated, nil
}

// ------------------------------------------------------------ dangling images

type dockerImageInspect struct {
	ID      string `json:"Id"`
	Created string `json:"Created"`
	Size    int64  `json:"Size"`
}

func readDanglingImages(ctx context.Context, run NodeReadRunner) ([]retention.InventoryImage, bool, error) {
	raw, err := run(ctx, "docker", "image", "ls", "--quiet", "--no-trunc", "--filter", "dangling=true")
	if err != nil {
		return nil, false, dockerObservationFault("list dangling images", err)
	}
	ids := splitLines(raw)
	ids, truncated := capRetentionItems(ids)
	if len(ids) == 0 {
		return []retention.InventoryImage{}, false, nil
	}
	var inspected []dockerImageInspect
	for _, chunk := range chunkStrings(ids, 100) {
		argv := append([]string{"docker", "image", "inspect"}, chunk...)
		raw, err := run(ctx, argv...)
		if err != nil {
			return nil, false, dockerObservationFault("inspect dangling images", err)
		}
		var page []dockerImageInspect
		if err := json.Unmarshal(bytes.TrimSpace(raw), &page); err != nil {
			return nil, false, contract.NewFault(contract.Unavailable, "the node returned an unreadable image observation", err.Error())
		}
		inspected = append(inspected, page...)
	}
	images := make([]retention.InventoryImage, 0, len(inspected))
	for _, item := range inspected {
		images = append(images, retention.InventoryImage{ID: item.ID, SizeBytes: item.Size, CreatedAt: parseDockerTime(item.Created)})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].ID < images[j].ID })
	return images, truncated, nil
}

// ----------------------------------------------------------------- local images

type dockerLocalImageInspect struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
}

// readLocalImages reports local images that carry a tag or a registry
// repository@digest. RepoDigests lets the control plane map a release's
// registry tag or config digest to the manifest digest the registry stores;
// RepoTags lets retention find a pruned release's still-tagged local image.
func readLocalImages(ctx context.Context, run NodeReadRunner) ([]retention.LocalImage, bool, error) {
	raw, err := run(ctx, "docker", "image", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, false, dockerObservationFault("list local images", err)
	}
	ids := splitLines(raw)
	ids, truncated := capRetentionItems(ids)
	if len(ids) == 0 {
		return []retention.LocalImage{}, false, nil
	}
	var inspected []dockerLocalImageInspect
	for _, chunk := range chunkStrings(ids, 100) {
		argv := append([]string{"docker", "image", "inspect"}, chunk...)
		raw, err := run(ctx, argv...)
		if err != nil {
			return nil, false, dockerObservationFault("inspect local images", err)
		}
		var page []dockerLocalImageInspect
		if err := json.Unmarshal(bytes.TrimSpace(raw), &page); err != nil {
			return nil, false, contract.NewFault(contract.Unavailable, "the node returned an unreadable local image observation", err.Error())
		}
		inspected = append(inspected, page...)
	}
	images := make([]retention.LocalImage, 0, len(inspected))
	for _, item := range inspected {
		if len(item.RepoTags) == 0 && len(item.RepoDigests) == 0 {
			continue
		}
		images = append(images, retention.LocalImage{
			ID: item.ID, RepoTags: sortedCopy(item.RepoTags), RepoDigests: sortedCopy(item.RepoDigests),
		})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].ID < images[j].ID })
	return images, truncated, nil
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// --------------------------------------------------------------- cache volumes

func readCacheVolumes(ctx context.Context, run NodeReadRunner) ([]retention.CacheVolume, bool, error) {
	seen := map[string]bool{}
	var names []string
	for _, prefix := range []string{BuildpackCacheVolumePrefix, BuildpackLaunchCacheVolumePrefix, PackCacheVolumePrefix} {
		raw, err := run(ctx, "docker", "volume", "ls", "--quiet", "--filter", "name="+prefix)
		if err != nil {
			return nil, false, dockerObservationFault("list build cache volumes", err)
		}
		for _, name := range splitLines(raw) {
			if seen[name] {
				continue
			}
			if _, _, _, ok := ParseBuildpacksCacheVolume(name); !ok {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	names, truncated := capRetentionItems(names)
	if len(names) == 0 {
		return []retention.CacheVolume{}, false, nil
	}
	volumes := make([]retention.CacheVolume, 0, len(names))
	for _, name := range names {
		app, environment, kind, ok := ParseBuildpacksCacheVolume(name)
		if !ok {
			continue
		}
		raw, err := run(ctx, "docker", "volume", "inspect", "--format", "{{.Mountpoint}}", name)
		if err != nil {
			if alreadyGone(err) {
				continue
			}
			return nil, false, dockerObservationFault("inspect build cache volume "+name, err)
		}
		mountpoint := strings.TrimSpace(string(raw))
		size, sizeErr := directorySize(ctx, mountpoint)
		volume := retention.CacheVolume{
			Name: name, Kind: kind, App: app, Environment: environment,
			Ownerless: kind == "pack", SizeBytes: size,
		}
		if sizeErr != nil {
			volume.SizeBytes = 0
			volume.SizeUnknown = true
		}
		volumes = append(volumes, volume)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, truncated, nil
}

// directorySize sums regular files below one directory. A missing, unreadable
// or timed-out directory is an unknown size, never a healthy zero.
func directorySize(ctx context.Context, root string) (int64, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return 0, errors.New("the cache volume has no readable absolute mountpoint")
	}
	sizeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, errors.New("the cache volume mountpoint is not a directory")
	}
	var total int64
	entries := 0
	err = filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entries++; entries%256 == 0 {
			if err := sizeCtx.Err(); err != nil {
				return err
			}
		}
		if entry.Type().IsRegular() {
			if info, statErr := entry.Info(); statErr == nil {
				total += info.Size()
			} else {
				return statErr
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := sizeCtx.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

// ---------------------------------------------------------------- registry

// readRegistryArtifacts reads the filesystem storage of the local registry.
// Releases are referenced by digest, so every live manifest revision is
// reported, tagged or not. A repository or manifest blob that has already been
// removed is absent; a revision is live only when Distribution's link file
// still names its digest.
func readRegistryArtifacts(registryDir string) ([]retention.InventoryRegistryArtifact, []string, bool, error) {
	registryDir = strings.TrimSpace(registryDir)
	if registryDir == "" {
		return []retention.InventoryRegistryArtifact{}, nil, false, nil
	}
	base := filepath.Join(registryDir, "docker", "registry", "v2")
	repositoriesRoot := filepath.Join(base, "repositories")
	if _, err := os.Stat(repositoriesRoot); err != nil {
		if os.IsNotExist(err) {
			return []retention.InventoryRegistryArtifact{}, nil, false, nil
		}
		return nil, nil, false, err
	}
	repositories, err := registryRepositories(repositoriesRoot)
	if err != nil {
		return nil, nil, false, err
	}
	artifacts := []retention.InventoryRegistryArtifact{}
	var warnings []string
	truncated := false
	for _, repository := range repositories {
		digests, err := registryManifestDigests(repositoriesRoot, repository)
		if err != nil {
			return nil, nil, false, err
		}
		tags := registryTags(repositoriesRoot, repository)
		for _, digest := range digests {
			if len(artifacts) >= maxRetentionInventoryItems {
				truncated = true
				break
			}
			blobs := map[string]int64{}
			var artifactWarnings []string
			configDigest, created, ok := collectRegistryManifest(base, digest, 0, blobs, &artifactWarnings)
			warnings = append(warnings, artifactWarnings...)
			if !ok {
				// The revision link exists but its manifest blob is missing or
				// unreadable. Never report a zero-byte artifact as known.
				continue
			}
			var bytes int64
			for _, size := range blobs {
				bytes += size
			}
			artifacts = append(artifacts, retention.InventoryRegistryArtifact{
				Repository: repository, Digest: digest, Tags: tags[digest],
				ConfigDigest: configDigest, Bytes: bytes, CreatedAt: created,
			})
		}
	}
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].Repository != artifacts[j].Repository {
			return artifacts[i].Repository < artifacts[j].Repository
		}
		return artifacts[i].Digest < artifacts[j].Digest
	})
	return artifacts, warnings, truncated, nil
}

// registryRepositories finds every repository root by its _manifests
// directory, so a nested repository name is reported with its full path.
func registryRepositories(repositoriesRoot string) ([]string, error) {
	var repositories []string
	err := filepath.WalkDir(repositoriesRoot, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // An unreadable subtree contributes no repository.
		}
		if !entry.IsDir() || entry.Name() != "_manifests" {
			return nil
		}
		relative, relErr := filepath.Rel(repositoriesRoot, filepath.Dir(current))
		if relErr != nil || relative == "." {
			return nil
		}
		repositories = append(repositories, filepath.ToSlash(relative))
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(repositories)
	return repositories, nil
}

// registryManifestDigests lists the revision digests Distribution still links.
// The revision's `link` file is authoritative: a leftover directory without a
// valid link is a deleted manifest, not a live artifact.
func registryManifestDigests(repositoriesRoot, repository string) ([]string, error) {
	root := filepath.Join(repositoriesRoot, filepath.FromSlash(repository), "_manifests", "revisions", "sha256")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	digests := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 64 || !lowerHex(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, entry.Name(), "link")) //nolint:gosec // G304: fixed registry storage layout.
		if err != nil {
			continue
		}
		digest := strings.TrimSpace(string(raw))
		if digest != "sha256:"+entry.Name() {
			continue
		}
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests, nil
}

// registryTags maps a manifest digest to the tags that currently point at it.
func registryTags(repositoriesRoot, repository string) map[string][]string {
	tags := map[string][]string{}
	root := filepath.Join(repositoriesRoot, filepath.FromSlash(repository), "_manifests", "tags")
	entries, err := os.ReadDir(root)
	if err != nil {
		return tags
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, entry.Name(), "current", "link")) //nolint:gosec // G304: fixed registry storage layout.
		if err != nil {
			continue
		}
		digest := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(digest, "sha256:") {
			continue
		}
		tags[digest] = append(tags[digest], entry.Name())
	}
	for digest := range tags {
		sort.Strings(tags[digest])
	}
	return tags
}

type registryManifest struct {
	Config *struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"layers"`
	Manifests []struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"manifests"`
}

// collectRegistryManifest walks one manifest (or index) and records every
// distinct blob it references in blobs, keyed by digest, so shared layers of an
// index are counted once. It returns the manifest's own config digest, the
// newest creation time it found, and whether the manifest itself was readable.
// A missing referenced blob is recorded as a warning and its size is the
// manifest's declaration, never a silent zero.
func collectRegistryManifest(base, digest string, depth int, blobs map[string]int64, warnings *[]string) (configDigest string, created *time.Time, ok bool) {
	raw, err := readRegistryBlob(base, digest)
	if err != nil {
		*warnings = append(*warnings, "registry manifest "+digest+" is missing or unreadable")
		return "", nil, false
	}
	blobs[digest] = resolveRegistryBlobSize(base, digest, blobs[digest], digest, warnings)
	var manifest registryManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		*warnings = append(*warnings, "registry manifest "+digest+" is not valid manifest JSON")
		return "", nil, false
	}
	if manifest.Config != nil {
		configDigest = manifest.Config.Digest
		blobs[configDigest] = resolveRegistryBlobSize(base, configDigest, manifest.Config.Size, digest, warnings)
		created = registryConfigCreated(base, configDigest)
	}
	for _, layer := range manifest.Layers {
		blobs[layer.Digest] = resolveRegistryBlobSize(base, layer.Digest, layer.Size, digest, warnings)
	}
	if depth < 3 {
		for _, child := range manifest.Manifests {
			if _, known := blobs[child.Digest]; !known {
				// The index declares a size; the child's own manifest blob is
				// resolved (and a missing one is warned about) below.
				blobs[child.Digest] = child.Size
			}
			_, childCreated, childOK := collectRegistryManifest(base, child.Digest, depth+1, blobs, warnings)
			if !childOK {
				continue
			}
			if created == nil || (childCreated != nil && childCreated.After(*created)) {
				created = childCreated
			}
		}
	}
	return configDigest, created, true
}

// resolveRegistryBlobSize reports a missing or unreadable blob explicitly and
// returns the manifest's declared size when it has one. Only a known-present
// blob without a declaration falls back to its file size.
func resolveRegistryBlobSize(base, digest string, declared int64, manifest string, warnings *[]string) int64 {
	path, ok := registryBlobPath(base, digest)
	if !ok {
		*warnings = append(*warnings, "registry manifest "+manifest+" references an invalid blob digest")
		return declared
	}
	info, err := os.Stat(path)
	if err != nil {
		*warnings = append(*warnings, "registry blob "+digest+" referenced by "+manifest+" is missing or unreadable")
		return declared
	}
	if declared > 0 {
		return declared
	}
	return info.Size()
}

func registryConfigCreated(base, digest string) *time.Time {
	raw, err := readRegistryBlob(base, digest)
	if err != nil {
		return nil
	}
	var config struct {
		Created string `json:"created"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, config.Created)
	if err != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

func readRegistryBlob(base, digest string) ([]byte, error) {
	path, ok := registryBlobPath(base, digest)
	if !ok {
		return nil, errors.New("invalid registry digest")
	}
	return os.ReadFile(path) //nolint:gosec // G304: fixed registry storage layout under the node data directory.
}

// registryBlobPath is Distribution's filesystem layout: the digest is sharded
// by its first two hex characters.
func registryBlobPath(base, digest string) (string, bool) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 64 || !lowerHex(hex) {
		return "", false
	}
	return filepath.Join(base, "blobs", "sha256", hex[:2], hex, "data"), true
}

func lowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return value != ""
}

// ------------------------------------------------------------------- helpers

func splitLines(raw []byte) []string {
	var values []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			values = append(values, line)
		}
	}
	return values
}

func capRetentionItems(values []string) ([]string, bool) {
	if len(values) > maxRetentionInventoryItems {
		return values[:maxRetentionInventoryItems], true
	}
	return values, false
}

func chunkStrings(values []string, size int) [][]string {
	var chunks [][]string
	for len(values) > size {
		chunks = append(chunks, values[:size])
		values = values[size:]
	}
	if len(values) > 0 {
		chunks = append(chunks, values)
	}
	return chunks
}

func parseDockerTime(value string) *time.Time {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

func alreadyGone(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such volume") || strings.Contains(message, "no such object") ||
		strings.Contains(message, "no such image") || strings.Contains(message, "no such container")
}

func dockerObservationFault(action string, err error) error {
	message := strings.ToLower(err.Error())
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(message, "cannot connect to the docker daemon") ||
		strings.Contains(message, "executable file not found") {
		return contract.NewFault(contract.PreconditionFailed, "Docker is not available on this node", "Install Docker and start it before reading or pruning disk artifacts.")
	}
	return contract.NewFault(contract.Unavailable, "the node could not "+action, boundedCommandOutput(err.Error()))
}

// retentionDisk reports the filesystem holding the node data directory.
func retentionDisk(dataDir string) retention.Disk {
	path := nearestExistingPath(dataDir)
	disk := retention.Disk{Path: path}
	if path == "" {
		return disk
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return disk
	}
	blockSize := uint64(stat.Bsize) //nolint:gosec // G115: a filesystem block size is positive.
	total := stat.Blocks * blockSize
	free := stat.Bavail * blockSize
	disk.TotalBytes = int64(total) //nolint:gosec // G115: filesystem sizes fit in int64.
	disk.FreeBytes = int64(free)   //nolint:gosec // G115: filesystem sizes fit in int64.
	return disk
}

func nearestExistingPath(value string) string {
	value = strings.TrimSpace(value)
	for {
		if value == "" {
			return ""
		}
		if _, err := os.Stat(value); err == nil {
			return value
		}
		parent := filepath.Dir(value)
		if parent == value {
			return ""
		}
		value = parent
	}
}
