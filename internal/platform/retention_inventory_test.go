package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testManifestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testConfigDigest   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testLayerDigest    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	testUntaggedDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

func TestBuildpackCacheVolumeIsDeterministicPerEnvironment(t *testing.T) {
	first := BuildpackCacheVolume("api", "production")
	if first != "eregion-build-cache-api-production" {
		t.Fatalf("cache volume = %q", first)
	}
	if first != BuildpackCacheVolume("api", "production") {
		t.Fatal("the cache volume name is not deterministic")
	}
	if first == BuildpackCacheVolume("api", "staging") {
		t.Fatal("two environments share one cache volume")
	}
	if !cacheVolumePattern.MatchString(first) {
		t.Fatalf("the generated cache volume name is not accepted by the retention steps: %q", first)
	}
}

// writeRegistryFixture builds the filesystem layout of registry:2.8.3 with one
// tagged and one untagged manifest, plus the same manifest in a second
// repository. It returns the registry directory and the manifest blob lengths.
func writeRegistryFixture(t *testing.T) (string, int, int) {
	t.Helper()
	registryDir := filepath.Join(t.TempDir(), "registry")
	base := filepath.Join(registryDir, "docker", "registry", "v2")
	repo := filepath.Join(base, "repositories", "demo", "app", "_manifests")
	putBlob := func(digest string, content []byte) {
		hex := strings.TrimPrefix(digest, "sha256:")
		path := filepath.Join(base, "blobs", "sha256", hex, "data")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	putLink := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `{"schemaVersion":2,"config":{"digest":"` + testConfigDigest + `","size":11},` +
		`"layers":[{"digest":"` + testLayerDigest + `","size":22}]}`
	putBlob(testManifestDigest, []byte(manifest))
	putBlob(testConfigDigest, []byte(`{"created":"2026-01-02T03:04:05Z"}`))
	putBlob(testLayerDigest, []byte(strings.Repeat("x", 7)))

	untagged := `{"schemaVersion":2,"config":{"digest":"` + testConfigDigest + `","size":11},"layers":[]}`
	putBlob(testUntaggedDigest, []byte(untagged))

	for _, digest := range []string{testManifestDigest, testUntaggedDigest} {
		hex := strings.TrimPrefix(digest, "sha256:")
		putLink(filepath.Join(repo, "revisions", "sha256", hex, "link"), digest)
	}
	putLink(filepath.Join(repo, "tags", "v1", "current", "link"), testManifestDigest)
	// The same digest in another repository is a separate artifact: a registry
	// manifest delete names one repository, so inventory must never collapse
	// them into one entry.
	otherRepo := filepath.Join(base, "repositories", "other", "repo", "_manifests")
	putLink(filepath.Join(otherRepo, "revisions", "sha256", strings.TrimPrefix(testManifestDigest, "sha256:"), "link"), testManifestDigest)
	return registryDir, len(manifest), len(untagged)
}

func TestObserveRetentionInventoryReadsRegistryDockerAndDisk(t *testing.T) {
	registryDir, taggedManifestBytes, untaggedManifestBytes := writeRegistryFixture(t)
	volumeMount := filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(volumeMount, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volumeMount, "cache.tar"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	answers := map[string]string{
		"docker ps --all --quiet --no-trunc --filter label=io.eregion.app":                             "cid1\n",
		"docker container inspect --size cid1":                                                         `[{"Id":"cid1","Name":"/eregion-api-production-abc","Created":"2026-02-03T04:05:06.123456789Z","State":{"Status":"exited"},"Config":{"Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Labels":{"io.eregion.app":"api","io.eregion.environment":"production"}},"SizeRw":1024}]`,
		"docker image ls --quiet --no-trunc --filter dangling=true":                                    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		"docker image inspect sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": `[{"Id":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Created":"2026-02-04T05:06:07Z","Size":2048}]`,
		"docker volume ls --quiet --filter name=eregion-build-cache-":                                  "eregion-build-cache-api-production\n",
		"docker volume inspect --format {{.Mountpoint}} eregion-build-cache-api-production":            volumeMount,
		"docker image ls --all --quiet --no-trunc":                                                     "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\n",
		"docker image inspect sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd": `[{"Id":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","RepoTags":["127.0.0.1:5000/demo/app:v1"],"RepoDigests":["127.0.0.1:5000/demo/app@` + testManifestDigest + `"]}]`,
	}
	run := func(_ context.Context, argv ...string) ([]byte, error) {
		key := strings.Join(argv, " ")
		answer, ok := answers[key]
		if !ok {
			t.Fatalf("unexpected node command %q", key)
		}
		return []byte(answer), nil
	}

	inventory, err := ObserveRetentionInventory(context.Background(),
		RetentionInventoryPaths{DataDir: t.TempDir(), RegistryDir: registryDir}, run)
	if err != nil {
		t.Fatal(err)
	}
	if !inventory.CollectedAt.After(time.Time{}) {
		t.Fatal("the inventory has no collection time")
	}
	if len(inventory.RegistryArtifacts) != 3 {
		t.Fatalf("registry artifacts = %+v", inventory.RegistryArtifacts)
	}
	tagged := inventory.RegistryArtifacts[0]
	if tagged.Repository != "demo/app" || tagged.Digest != testManifestDigest {
		t.Fatalf("first artifact = %+v", tagged)
	}
	if len(tagged.Tags) != 1 || tagged.Tags[0] != "v1" {
		t.Fatalf("tagged artifact tags = %+v", tagged.Tags)
	}
	if tagged.ConfigDigest != testConfigDigest {
		t.Fatalf("tagged artifact config digest = %q", tagged.ConfigDigest)
	}
	if tagged.Bytes != int64(taggedManifestBytes+11+22) {
		t.Fatalf("tagged artifact bytes = %d, want the manifest, config and layer sizes", tagged.Bytes)
	}
	if tagged.CreatedAt == nil || !tagged.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("tagged artifact created = %v", tagged.CreatedAt)
	}
	untagged := inventory.RegistryArtifacts[1]
	if untagged.Digest != testUntaggedDigest || len(untagged.Tags) != 0 {
		t.Fatalf("an untagged but digest-referenced manifest must still be reported: %+v", untagged)
	}
	if untagged.Bytes != int64(untaggedManifestBytes+11) || untagged.ConfigDigest != testConfigDigest {
		t.Fatalf("untagged artifact facts = %+v", untagged)
	}
	otherRepository := inventory.RegistryArtifacts[2]
	if otherRepository.Repository != "other/repo" || otherRepository.Digest != testManifestDigest {
		t.Fatalf("identical digests in two repositories must not collapse: %+v", otherRepository)
	}
	if len(otherRepository.Tags) != 0 || otherRepository.Bytes != tagged.Bytes || otherRepository.ConfigDigest != testConfigDigest {
		t.Fatalf("second-repository artifact facts = %+v", otherRepository)
	}

	if len(inventory.Containers) != 1 {
		t.Fatalf("containers = %+v", inventory.Containers)
	}
	container := inventory.Containers[0]
	if container.Name != "eregion-api-production-abc" || container.State != "exited" || container.SizeBytes != 1024 {
		t.Fatalf("container = %+v", container)
	}
	if container.Labels["io.eregion.app"] != "api" {
		t.Fatalf("container labels = %+v", container.Labels)
	}
	if container.CreatedAt == nil || container.CreatedAt.UTC().Format(time.RFC3339Nano) != "2026-02-03T04:05:06.123456789Z" {
		t.Fatalf("container created = %v", container.CreatedAt)
	}

	if len(inventory.DanglingImages) != 1 || inventory.DanglingImages[0].SizeBytes != 2048 {
		t.Fatalf("dangling images = %+v", inventory.DanglingImages)
	}
	if len(inventory.LocalImages) != 1 {
		t.Fatalf("local images = %+v", inventory.LocalImages)
	}
	if got := inventory.LocalImages[0].RepoDigests; len(got) != 1 || got[0] != "127.0.0.1:5000/demo/app@"+testManifestDigest {
		t.Fatalf("local image repository digests = %+v", got)
	}
	if len(inventory.CacheVolumes) != 1 {
		t.Fatalf("cache volumes = %+v", inventory.CacheVolumes)
	}
	if inventory.CacheVolumes[0].Name != "eregion-build-cache-api-production" || inventory.CacheVolumes[0].SizeBytes != 4096 {
		t.Fatalf("cache volume = %+v", inventory.CacheVolumes[0])
	}
	if inventory.Disk.TotalBytes <= 0 || inventory.Disk.FreeBytes < 0 {
		t.Fatalf("disk = %+v", inventory.Disk)
	}
}

func TestObserveRetentionInventoryWithoutARegistryIsEmpty(t *testing.T) {
	inventory, err := ObserveRetentionInventory(context.Background(),
		RetentionInventoryPaths{DataDir: t.TempDir(), RegistryDir: filepath.Join(t.TempDir(), "absent")},
		func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if inventory.RegistryArtifacts == nil || len(inventory.RegistryArtifacts) != 0 {
		t.Fatalf("registry artifacts = %+v", inventory.RegistryArtifacts)
	}
	if inventory.CacheVolumes == nil || inventory.Containers == nil || inventory.DanglingImages == nil || inventory.LocalImages == nil {
		t.Fatal("an empty inventory must encode empty arrays, not null")
	}
}
