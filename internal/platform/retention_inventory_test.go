package platform

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	testManifestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testConfigDigest   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testLayerDigest    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	testUntaggedDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	testDeletedDigest  = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	testMissingDigest  = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	// The cache names embed hex("api") and hex("production").
	testBuildCacheVolume  = "eregion-build-cache-617069_70726f64756374696f6e"
	testLaunchCacheVolume = "eregion-launch-cache-617069_70726f64756374696f6e"
)

func TestBuildpackCacheVolumeNamesAreUnambiguous(t *testing.T) {
	first := BuildpackCacheVolume("a-b", "c")
	second := BuildpackCacheVolume("a", "b-c")
	if first == second {
		t.Fatalf("cache volumes collide: %q", first)
	}
	if first != "eregion-build-cache-612d62_63" || second != "eregion-build-cache-61_622d63" {
		t.Fatalf("cache volumes = %q, %q", first, second)
	}
	// pack rewrites reserved substrings in an explicit volume name; the hex
	// encoding never contains one.
	reserved := BuildpackCacheVolume("console", "nul")
	if strings.Contains(reserved, "con") || strings.Contains(reserved, "nul") {
		t.Fatalf("pack would rewrite the cache volume name %q", reserved)
	}
	for name, want := range map[string][3]string{
		first:                 {"a-b", "c", "build"},
		second:                {"a", "b-c", "build"},
		testLaunchCacheVolume: {"api", "production", "launch"},
		testBuildCacheVolume:  {"api", "production", "build"},
		reserved:              {"console", "nul", "build"},
	} {
		app, environment, kind, ok := ParseBuildpacksCacheVolume(name)
		if !ok || app != want[0] || environment != want[1] || kind != want[2] {
			t.Fatalf("parse %q = %q %q %q %v", name, app, environment, kind, ok)
		}
		if !cacheVolumePattern.MatchString(name) {
			t.Fatalf("the retention steps refuse the generated cache volume %q", name)
		}
	}

	legacy := "pack-cache-0123456789abcdef.build"
	app, environment, kind, ok := ParseBuildpacksCacheVolume(legacy)
	if !ok || kind != "pack" || app != "" || environment != "" {
		t.Fatalf("legacy pack cache = %q %q %q %v", app, environment, kind, ok)
	}
	if !cacheVolumePattern.MatchString(legacy) {
		t.Fatal("a legacy pack cache volume cannot be pruned")
	}
	for _, invalid := range []string{"eregion-build-cache-api", "eregion-build-cache-api_prod_extra", "pack-cache-random", "other-volume"} {
		if _, _, _, ok := ParseBuildpacksCacheVolume(invalid); ok {
			t.Fatalf("%q was parsed as an eregion cache", invalid)
		}
		if cacheVolumePattern.MatchString(invalid) {
			t.Fatalf("%q is removable as a cache", invalid)
		}
	}
}

// writeRegistryFixture builds the filesystem layout of registry:2.8.3 with one
// tagged and one untagged manifest, the same manifest in a second repository, a
// deleted revision directory without its link, and a linked revision whose
// manifest blob is missing. It returns the registry directory and the manifest
// blob lengths.
func writeRegistryFixture(t *testing.T) (string, int, int) {
	t.Helper()
	registryDir := filepath.Join(t.TempDir(), "registry")
	base := filepath.Join(registryDir, "docker", "registry", "v2")
	repo := filepath.Join(base, "repositories", "demo", "app", "_manifests")
	putBlob := func(digest string, content []byte) {
		hex := strings.TrimPrefix(digest, "sha256:")
		// Distribution shards blobs by the first two hex characters.
		path := filepath.Join(base, "blobs", "sha256", hex[:2], hex, "data")
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

	// A deleted revision leaves its directory behind but Distribution removes
	// the link; it is not a live manifest.
	deletedDir := filepath.Join(base, "repositories", "deleted", "repo", "_manifests", "revisions", "sha256", strings.TrimPrefix(testDeletedDigest, "sha256:"))
	if err := os.MkdirAll(deletedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A revision link without a manifest blob is a warning, not a zero-byte
	// artifact.
	missingRepo := filepath.Join(base, "repositories", "missing", "repo", "_manifests")
	putLink(filepath.Join(missingRepo, "revisions", "sha256", strings.TrimPrefix(testMissingDigest, "sha256:"), "link"), testMissingDigest)
	return registryDir, len(manifest), len(untagged)
}

func TestObserveRetentionInventoryReadsRegistryDockerAndDisk(t *testing.T) {
	registryDir, taggedManifestBytes, untaggedManifestBytes := writeRegistryFixture(t)
	dockerSocket := dockerAPISocket(t, http.StatusOK, `{"Volumes":[
        {"Name":"`+testBuildCacheVolume+`","UsageData":{"Size":4096,"RefCount":0}},
        {"Name":"`+testLaunchCacheVolume+`","UsageData":{"Size":1024,"RefCount":0}},
        {"Name":"pack-cache-0123456789abcdef.build","UsageData":{"Size":512,"RefCount":0}}
    ]}`)

	answers := map[string]string{
		"docker ps --all --quiet --no-trunc":                                                           "cid1\ncid2\n",
		"docker container inspect --size cid1 cid2":                                                    `[{"Id":"cid1","Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Name":"/eregion-api-production-abc","Created":"2026-02-03T04:05:06.123456789Z","State":{"Status":"exited"},"Config":{"Image":"127.0.0.1:5000/api:old","Labels":{"io.eregion.app":"api","io.eregion.environment":"production"}},"SizeRw":1024},{"Id":"cid2","Image":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","Name":"/pedantic_jackson","Created":"2026-02-04T05:06:07Z","State":{"Status":"exited"},"Config":{"Image":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","Labels":{}},"SizeRw":2048}]`,
		"docker image ls --quiet --no-trunc --filter dangling=true":                                    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		"docker image inspect sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": `[{"Id":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Created":"2026-02-04T05:06:07Z","Size":2048}]`,
		"docker volume ls --quiet --filter name=eregion-build-cache-":                                  testBuildCacheVolume + "\n",
		"docker volume ls --quiet --filter name=eregion-launch-cache-":                                 testLaunchCacheVolume + "\n",
		"docker volume ls --quiet --filter name=pack-cache-":                                           "pack-cache-0123456789abcdef.build\n",
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
		RetentionInventoryPaths{DataDir: t.TempDir(), RegistryDir: registryDir, DockerSocket: dockerSocket}, run)
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
	if len(inventory.Warnings) == 0 || !strings.Contains(strings.Join(inventory.Warnings, "\n"), testMissingDigest) {
		t.Fatalf("a missing manifest blob must be an explicit warning: %+v", inventory.Warnings)
	}

	if len(inventory.Containers) != 2 {
		t.Fatalf("containers = %+v", inventory.Containers)
	}
	container := inventory.Containers[0]
	if container.Name != "eregion-api-production-abc" || !container.Owned || container.State != "exited" || container.SizeBytes != 1024 {
		t.Fatalf("container = %+v", container)
	}
	if container.Image != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("container image identity = %q", container.Image)
	}
	if container.Labels["io.eregion.app"] != "api" {
		t.Fatalf("container labels = %+v", container.Labels)
	}
	if container.CreatedAt == nil || container.CreatedAt.UTC().Format(time.RFC3339Nano) != "2026-02-03T04:05:06.123456789Z" {
		t.Fatalf("container created = %v", container.CreatedAt)
	}
	unlabeled := inventory.Containers[1]
	if unlabeled.Name != "pedantic_jackson" || unlabeled.Owned || unlabeled.Image == "" {
		t.Fatalf("an unlabeled container must be reported for image protection only: %+v", unlabeled)
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
	if len(inventory.CacheVolumes) != 3 {
		t.Fatalf("cache volumes = %+v", inventory.CacheVolumes)
	}
	buildCache := inventory.CacheVolumes[0]
	if buildCache.Name != testBuildCacheVolume || buildCache.Kind != "build" ||
		buildCache.App != "api" || buildCache.Environment != "production" || buildCache.SizeBytes != 4096 {
		t.Fatalf("build cache = %+v", buildCache)
	}
	launchCache := inventory.CacheVolumes[1]
	if launchCache.Name != testLaunchCacheVolume || launchCache.Kind != "launch" ||
		launchCache.App != "api" || launchCache.SizeBytes != 1024 {
		t.Fatalf("launch cache = %+v", launchCache)
	}
	legacyCache := inventory.CacheVolumes[2]
	if legacyCache.Name != "pack-cache-0123456789abcdef.build" || legacyCache.Kind != "pack" ||
		!legacyCache.Ownerless || legacyCache.SizeBytes != 512 {
		t.Fatalf("legacy pack cache = %+v", legacyCache)
	}
	if inventory.Disk.TotalBytes <= 0 || inventory.Disk.FreeBytes < 0 {
		t.Fatalf("disk = %+v", inventory.Disk)
	}
}

func TestObserveRetentionInventoryMarksUnknownCacheSizes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "docker reports -1",
			status: http.StatusOK,
			body:   `{"Volumes":[{"Name":"` + testBuildCacheVolume + `","UsageData":{"Size":-1,"RefCount":0}}]}`,
		},
		{
			name:   "volume missing from the response",
			status: http.StatusOK,
			body:   `{"Volumes":[]}`,
		},
		{
			name:   "docker API error",
			status: http.StatusInternalServerError,
			body:   `{}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dockerSocket := dockerAPISocket(t, tc.status, tc.body)
			answers := map[string]string{
				"docker ps --all --quiet --no-trunc":                           "",
				"docker image ls --quiet --no-trunc --filter dangling=true":    "",
				"docker image ls --all --quiet --no-trunc":                     "",
				"docker volume ls --quiet --filter name=eregion-build-cache-":  testBuildCacheVolume + "\n",
				"docker volume ls --quiet --filter name=eregion-launch-cache-": "",
				"docker volume ls --quiet --filter name=pack-cache-":           "",
			}
			run := func(_ context.Context, argv ...string) ([]byte, error) {
				return []byte(answers[strings.Join(argv, " ")]), nil
			}
			inventory, err := ObserveRetentionInventory(context.Background(),
				RetentionInventoryPaths{DataDir: t.TempDir(), RegistryDir: filepath.Join(t.TempDir(), "absent"), DockerSocket: dockerSocket}, run)
			if err != nil {
				t.Fatal(err)
			}
			if len(inventory.CacheVolumes) != 1 || !inventory.CacheVolumes[0].SizeUnknown {
				t.Fatalf("unknown cache volume = %+v, want an unknown size", inventory.CacheVolumes)
			}
			if !inventory.CacheSizeUnknown || inventory.CacheError == "" {
				t.Fatalf("inventory cache measurement = unknown=%t error=%q", inventory.CacheSizeUnknown, inventory.CacheError)
			}
			if inventory.CacheVolumes[0].SizeBytes != 0 {
				t.Fatalf("an unknown cache size carried a byte measurement: %+v", inventory.CacheVolumes[0])
			}
		})
	}
}

func TestReadDockerVolumeSizesParsesUsageData(t *testing.T) {
	dockerSocket := dockerAPISocket(t, http.StatusOK, `{"Volumes":[
        {"Name":"known","UsageData":{"Size":4096,"RefCount":1}},
        {"Name":"unknown","UsageData":{"Size":-1,"RefCount":0}},
        {"Name":"missing-usage"},
        {"Name":"zero","UsageData":{"Size":0,"RefCount":0}}
    ]}`)
	sizes, err := readDockerVolumeSizes(context.Background(), dockerSocket)
	if err != nil {
		t.Fatal(err)
	}
	if sizes["known"] != 4096 {
		t.Fatalf("known size = %d", sizes["known"])
	}
	if size, ok := sizes["zero"]; !ok || size != 0 {
		t.Fatalf("a reported zero is a known measurement: %d %t", size, ok)
	}
	for _, name := range []string{"unknown", "missing-usage"} {
		if _, ok := sizes[name]; ok {
			t.Fatalf("%s must be unknown, not a measurement", name)
		}
	}
}

func dockerAPISocket(t *testing.T, status int, body string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "eregion-docker-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/system/df" {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return socket
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

func TestRegistryStorageFaultNamesThePathAndTheFixOnPermissionDenied(t *testing.T) {
	registryDir := "/var/lib/eregion/registry"
	err := registryStorageFault(registryDir, &os.PathError{Op: "open", Path: filepath.Join(registryDir, "docker"), Err: fs.ErrPermission})
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("permission failure = %v, want a contract fault", err)
	}
	if fault.Code != contract.Unavailable {
		t.Fatalf("permission failure code = %s", fault.Code)
	}
	if !strings.Contains(fault.Message, registryDir) {
		t.Fatalf("permission failure does not name the path: %q", fault.Message)
	}
	if !strings.Contains(fault.Message, "eregion apply") || !strings.Contains(fault.Message, "host-directories-v2") {
		t.Fatalf("permission failure does not name the fix: %q", fault.Message)
	}
	if !strings.Contains(fault.Hint, "0750") {
		t.Fatalf("permission failure hint = %q", fault.Hint)
	}
}

func TestRegistryStorageFaultKeepsOtherReadFailuresGeneric(t *testing.T) {
	registryDir := "/var/lib/eregion/registry"
	err := registryStorageFault(registryDir, &os.PathError{Op: "read", Path: registryDir, Err: fs.ErrInvalid})
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("read failure = %v, want a contract fault", err)
	}
	if strings.Contains(fault.Message, "eregion apply") {
		t.Fatalf("an unrelated read failure points at the permission fix: %q", fault.Message)
	}
	if !strings.Contains(fault.Message, registryDir) || !strings.Contains(fault.Message, "unreadable") {
		t.Fatalf("read failure = %q", fault.Message)
	}
}

func TestObserveRetentionInventoryReportsAnUnreadableRegistryDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory mode this test needs")
	}
	registryDir := filepath.Join(t.TempDir(), "registry")
	if err := os.MkdirAll(filepath.Join(registryDir, "docker", "registry", "v2", "repositories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(registryDir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chmod(registryDir, 0o750)
	}()

	_, err := ObserveRetentionInventory(context.Background(),
		RetentionInventoryPaths{DataDir: t.TempDir(), RegistryDir: registryDir},
		func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil })
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("unreadable registry = %v, want a contract fault", err)
	}
	if fault.Code != contract.Unavailable || !strings.Contains(fault.Message, registryDir) || !strings.Contains(fault.Message, "eregion apply") {
		t.Fatalf("unreadable registry fault = %+v", fault)
	}
}
