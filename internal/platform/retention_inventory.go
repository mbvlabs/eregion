package platform

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
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/retention"
)

// BuildpackCacheVolumePrefix names every Buildpacks build cache eregion
// creates. pack is given an explicit cache volume so the cache is attributable
// and can be pruned by name instead of by guessing pack's defaults.
const BuildpackCacheVolumePrefix = "eregion-build-cache-"

// BuildpackCacheVolume is the deterministic cache volume of one environment.
// App and environment names are already validated by the caller, so the result
// is a plain Docker volume name.
func BuildpackCacheVolume(app, environment string) string {
	return BuildpackCacheVolumePrefix + app + "-" + environment
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
		RegistryArtifacts: []retention.RegistryArtifact{},
		CacheVolumes:      []retention.CacheVolume{},
		Containers:        []retention.Container{},
		DanglingImages:    []retention.Image{},
		LocalImages:       []retention.LocalImage{},
	}
	artifacts, truncated, err := readRegistryArtifacts(paths.RegistryDir)
	if err != nil {
		return retention.Inventory{}, contract.NewFault(contract.Unavailable, "the node's registry storage is unreadable", err.Error())
	}
	inventory.RegistryArtifacts = artifacts
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

func readRetentionContainers(ctx context.Context, run NodeReadRunner) ([]retention.Container, bool, error) {
	raw, err := run(ctx, "docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "label=io.eregion.app")
	if err != nil {
		return nil, false, dockerObservationFault("list labeled containers", err)
	}
	ids := splitLines(raw)
	ids, truncated := capRetentionItems(ids)
	if len(ids) == 0 {
		return []retention.Container{}, false, nil
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
	containers := make([]retention.Container, 0, len(inspected))
	for _, item := range inspected {
		containers = append(containers, retention.Container{
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

func readDanglingImages(ctx context.Context, run NodeReadRunner) ([]retention.Image, bool, error) {
	raw, err := run(ctx, "docker", "image", "ls", "--quiet", "--no-trunc", "--filter", "dangling=true")
	if err != nil {
		return nil, false, dockerObservationFault("list dangling images", err)
	}
	ids := splitLines(raw)
	ids, truncated := capRetentionItems(ids)
	if len(ids) == 0 {
		return []retention.Image{}, false, nil
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
	images := make([]retention.Image, 0, len(inspected))
	for _, item := range inspected {
		images = append(images, retention.Image{ID: item.ID, SizeBytes: item.Size, CreatedAt: parseDockerTime(item.Created)})
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

// readLocalImages reports local images that name a registry repository@digest.
// The control plane uses those references to map a release's registry tag or
// config digest to the manifest digest the registry stores.
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
		if len(item.RepoDigests) == 0 {
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
	raw, err := run(ctx, "docker", "volume", "ls", "--quiet", "--filter", "name="+BuildpackCacheVolumePrefix)
	if err != nil {
		return nil, false, dockerObservationFault("list build cache volumes", err)
	}
	names := splitLines(raw)
	names, truncated := capRetentionItems(names)
	if len(names) == 0 {
		return []retention.CacheVolume{}, false, nil
	}
	volumes := make([]retention.CacheVolume, 0, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, BuildpackCacheVolumePrefix) {
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
		volumes = append(volumes, retention.CacheVolume{Name: name, SizeBytes: directorySize(ctx, mountpoint)})
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, truncated, nil
}

// directorySize sums regular files below one directory. A missing or
// unreadable directory is an unknown size, not a failed inventory.
func directorySize(ctx context.Context, root string) int64 {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return 0
	}
	var total int64
	entries := 0
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // An unreadable subtree is an unknown size, not a fault.
		}
		if entries++; entries%256 == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if entry.Type().IsRegular() {
			if info, statErr := entry.Info(); statErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// ---------------------------------------------------------------- registry

// readRegistryArtifacts reads the filesystem storage of the local registry.
// Releases are referenced by digest, so every manifest revision is reported,
// tagged or not. A repository or manifest blob that has already been removed is
// simply absent.
func readRegistryArtifacts(registryDir string) ([]retention.RegistryArtifact, bool, error) {
	registryDir = strings.TrimSpace(registryDir)
	if registryDir == "" {
		return []retention.RegistryArtifact{}, false, nil
	}
	base := filepath.Join(registryDir, "docker", "registry", "v2")
	repositoriesRoot := filepath.Join(base, "repositories")
	if _, err := os.Stat(repositoriesRoot); err != nil {
		if os.IsNotExist(err) {
			return []retention.RegistryArtifact{}, false, nil
		}
		return nil, false, err
	}
	repositories, err := registryRepositories(repositoriesRoot)
	if err != nil {
		return nil, false, err
	}
	artifacts := []retention.RegistryArtifact{}
	truncated := false
	for _, repository := range repositories {
		digests, err := registryManifestDigests(repositoriesRoot, repository)
		if err != nil {
			return nil, false, err
		}
		tags := registryTags(repositoriesRoot, repository)
		for _, digest := range digests {
			if len(artifacts) >= maxRetentionInventoryItems {
				truncated = true
				break
			}
			blobs := map[string]int64{}
			configDigest, created := collectRegistryManifest(base, digest, 0, blobs)
			var bytes int64
			for _, size := range blobs {
				bytes += size
			}
			artifacts = append(artifacts, retention.RegistryArtifact{
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
	return artifacts, truncated, nil
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
		digests = append(digests, "sha256:"+entry.Name())
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
// index are counted once. It returns the manifest's own config digest and the
// newest creation time it found. Missing blobs are unknown facts.
func collectRegistryManifest(base, digest string, depth int, blobs map[string]int64) (configDigest string, created *time.Time) {
	raw, err := readRegistryBlob(base, digest)
	if err != nil {
		return "", nil
	}
	blobs[digest] = registryBlobSize(base, blobs[digest], digest)
	var manifest registryManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", nil
	}
	if manifest.Config != nil {
		configDigest = manifest.Config.Digest
		blobs[configDigest] = registryBlobSize(base, manifest.Config.Size, configDigest)
		created = registryConfigCreated(base, configDigest)
	}
	for _, layer := range manifest.Layers {
		blobs[layer.Digest] = registryBlobSize(base, layer.Size, layer.Digest)
	}
	if depth < 3 {
		for _, child := range manifest.Manifests {
			blobs[child.Digest] = registryBlobSize(base, child.Size, child.Digest)
			_, childCreated := collectRegistryManifest(base, child.Digest, depth+1, blobs)
			if created == nil || (childCreated != nil && childCreated.After(*created)) {
				created = childCreated
			}
		}
	}
	return configDigest, created
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

// registryBlobSize prefers the size the manifest declares and falls back to the
// blob file's own size when the declared value is absent.
func registryBlobSize(base string, declared int64, digest string) int64 {
	if declared > 0 || digest == "" {
		return declared
	}
	path, ok := registryBlobPath(base, digest)
	if !ok {
		return declared
	}
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return declared
}

func readRegistryBlob(base, digest string) ([]byte, error) {
	path, ok := registryBlobPath(base, digest)
	if !ok {
		return nil, errors.New("invalid registry digest")
	}
	return os.ReadFile(path) //nolint:gosec // G304: fixed registry storage layout under the node data directory.
}

func registryBlobPath(base, digest string) (string, bool) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 64 || !lowerHex(hex) {
		return "", false
	}
	return filepath.Join(base, "blobs", "sha256", hex, "data"), true
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
