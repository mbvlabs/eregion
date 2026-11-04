package retention

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

var planNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func daysAgo(days int) time.Time {
	return planNow.Add(-time.Duration(days) * 24 * time.Hour)
}

func hoursAgo(hours int) time.Time {
	return planNow.Add(-time.Duration(hours) * time.Hour)
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func numberedReleases(prefix, app, environment string, count, oldestDays int) []Release {
	releases := make([]Release, 0, count)
	for index := 1; index <= count; index++ {
		releases = append(releases, Release{
			ID:          fmt.Sprintf("%s_%04d", prefix, index),
			App:         app,
			Environment: environment,
			CreatedAt:   daysAgo(oldestDays - index + 1),
		})
	}
	return releases
}

func referencesByKind(items []Item, kind ItemKind) []string {
	references := []string{}
	for _, item := range items {
		if item.Kind == kind {
			references = append(references, item.Reference)
		}
	}
	return references
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// TestPlanKeepsNewestReleases is D30's 30-release acceptance case: every
// release is older than the age window, so exactly the newest keep_releases
// rows survive.
func TestPlanKeepsNewestReleases(t *testing.T) {
	releases := numberedReleases("rel", "api", "production", 30, 50)
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_0030", Policy: DefaultPolicy()}},
		Releases:     releases,
		Now:          planNow,
	})

	want := []string{}
	for index := 1; index <= 20; index++ {
		want = append(want, fmt.Sprintf("rel_%04d", index))
	}
	assertStrings(t, "release items", referencesByKind(items, ItemRelease), want)
	if bytes := TotalBytes(items); bytes != 0 {
		t.Fatalf("release-only bytes = %d, want 0 because release rows are never deleted", bytes)
	}
}

// TestPlanKeepsServingAndRollbackOutsidePolicy covers the D30 invariant that
// the serving release and the target a rollback would pick are kept even when
// both are older than the age window and outside the newest count.
func TestPlanKeepsServingAndRollbackOutsidePolicy(t *testing.T) {
	releases := numberedReleases("rel", "api", "production", 20, 62)
	deployments := []Deployment{
		{
			ID: "dep_serving", App: "api", Environment: "production",
			Release: "rel_0001", PreviousRelease: "rel_0002",
			State: DeploymentStateSucceeded, CreatedAt: daysAgo(61), CompletedAt: timePointer(daysAgo(61)),
		},
		{
			ID: "dep_latest", App: "api", Environment: "production",
			Release: "rel_0020", PreviousRelease: "rel_0019",
			State: DeploymentStateSucceeded, CreatedAt: daysAgo(42), CompletedAt: timePointer(daysAgo(42)),
		},
	}
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_0001", Policy: DefaultPolicy(),
		}},
		Releases:    releases,
		Deployments: deployments,
		Now:         planNow,
	})

	if target := RollbackTarget("api", "production", "rel_0001", deployments); target != "rel_0002" {
		t.Fatalf("rollback target = %q, want rel_0002", target)
	}
	want := []string{}
	for index := 3; index <= 10; index++ {
		want = append(want, fmt.Sprintf("rel_%04d", index))
	}
	assertStrings(t, "release items", referencesByKind(items, ItemRelease), want)
	for _, reference := range []string{"rel_0001", "rel_0002"} {
		if containsReference(items, ItemRelease, reference) {
			t.Fatalf("protected release %s was planned", reference)
		}
	}
}

func TestPlanRespectsEnvironmentOverride(t *testing.T) {
	stagingPolicy := DefaultPolicy()
	stagingPolicy.KeepReleases = 3
	stagingPolicy.KeepNewerThanDays = 1
	releases := append(
		numberedReleases("stg", "api", "staging", 6, 20),
		numberedReleases("prd", "api", "production", 6, 20)...,
	)
	items := Plan(Input{
		Environments: []Environment{
			{App: "api", Name: "production", ServingRelease: "prd_0006", Policy: DefaultPolicy()},
			{App: "api", Name: "staging", ServingRelease: "stg_0006", Policy: stagingPolicy},
		},
		Releases: releases,
		Now:      planNow,
	})

	assertStrings(t, "release items", referencesByKind(items, ItemRelease), []string{"stg_0001", "stg_0002", "stg_0003"})
}

func TestPlanKeepsSharedDigest(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	releases := numberedReleases("rel", "api", "production", 3, 30)
	releases[0].Artifacts = []string{"sha256:shared", "sha256:unique"}
	releases[2].Artifacts = []string{"sha256:shared"}
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_0003", Policy: policy}},
		Releases:     releases,
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:shared", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:unique", CreatedAt: daysAgo(30), Bytes: 200},
			{Repository: "api", Digest: "sha256:orphan", CreatedAt: daysAgo(30), Bytes: 300},
		}},
		Now: planNow,
	})

	assertStrings(t, "release items", referencesByKind(items, ItemRelease), []string{"rel_0001", "rel_0002"})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"api@sha256:orphan", "api@sha256:unique"})
	for _, item := range items {
		if item.Kind == ItemRegistryArtifact && item.Reference == "api@sha256:shared" {
			t.Fatal("shared digest referenced by a kept release was planned")
		}
		if item.Kind == ItemRegistryArtifact && item.Reference == "api@sha256:unique" {
			if item.App != "api" || item.Environment != "production" || item.Bytes != 200 {
				t.Fatalf("unique artifact item = %+v, want owner api/production and 200 bytes", item)
			}
		}
	}
}

func TestPlanProtectsInFlightResources(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	releases := numberedReleases("rel", "api", "production", 3, 30)
	releases[0].ImageID = "sha256:inflight"
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_0003", Policy: policy}},
		Releases:     releases,
		InFlight: InFlight{
			Releases:    []string{"rel_0001"},
			Deployments: []string{"dep_candidate"},
			Containers:  []string{"candidate-name", "container-id"},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:manifest-inflight", ConfigDigest: "sha256:inflight", CreatedAt: daysAgo(30), Bytes: 10},
		}},
		Containers: []Container{
			{Name: "inflight-release", App: "api", Environment: "production", Release: "rel_0001"},
			{Name: "stale", App: "api", Environment: "production", Release: "rel_0002"},
			{Name: "candidate-name", App: "api", Environment: "production", Release: "rel_0002"},
			{Name: "by-id", ID: "container-id", App: "api", Environment: "production", Release: "rel_0002"},
			{Name: "candidate", App: "api", Environment: "production", Release: "rel_0002", Deployment: "dep_candidate"},
			{Name: "serving", App: "api", Environment: "production", Release: "rel_0003"},
		},
		Now: planNow,
	})

	assertStrings(t, "release items", referencesByKind(items, ItemRelease), []string{"rel_0002"})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), nil)
	assertStrings(t, "container items", referencesByKind(items, ItemContainer), []string{"stale"})
}

func TestPlanProtectsPendingDeploymentRelease(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	releases := numberedReleases("rel", "api", "production", 3, 30)
	releases[1].ImageID = "sha256:pending"
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_0003", Policy: policy}},
		Releases:     releases,
		Deployments: []Deployment{
			{ID: "dep_pending", App: "api", Environment: "production", Release: "rel_0002", PreviousRelease: "rel_0003", State: "running"},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:manifest-pending", ConfigDigest: "sha256:pending", CreatedAt: daysAgo(30), Bytes: 10},
		}},
		Now: planNow,
	})

	assertStrings(t, "release items", referencesByKind(items, ItemRelease), []string{"rel_0001"})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), nil)
}

func TestPlanContainersAndDanglingImages(t *testing.T) {
	releases := []Release{{
		ID: "rel_serving", App: "api", Environment: "production",
		ImageID: "sha256:kept-image", Artifacts: []string{"sha256:kept-digest"},
		CreatedAt: daysAgo(1),
	}}
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving",
			ServingDeployment: "dep_serving", Policy: DefaultPolicy(),
		}},
		Releases: releases,
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:kept-digest", CreatedAt: daysAgo(1), Bytes: 100},
		}},
		Containers: []Container{
			{Name: "serving-release", App: "api", Environment: "production", Release: "rel_serving"},
			{Name: "serving-deployment", App: "api", Environment: "production", Deployment: "dep_serving"},
			{Name: "stale", App: "api", Environment: "production", Release: "rel_old"},
			{Name: "unknown-environment", App: "api", Environment: "ghost", Release: "rel_old"},
			{Name: "no-labels"},
			{ID: "container-without-name", App: "api", Environment: "production", Release: "rel_old"},
		},
		Images: []Image{
			{ID: "sha256:dangling", Dangling: true, Bytes: 100},
			{ID: "sha256:tagged", Dangling: false, Bytes: 100},
			{ID: "sha256:kept-image", Dangling: true, Bytes: 100},
			{ID: "sha256:other", Digest: "sha256:kept-digest", Dangling: true, Bytes: 100},
			{ID: "sha256:release-label", Release: "rel_serving", Dangling: true, Bytes: 100},
		},
		Now: planNow,
	})

	assertStrings(t, "container items", sortedStrings(referencesByKind(items, ItemContainer)),
		[]string{"container-without-name", "stale", "unknown-environment"})
	assertStrings(t, "dangling image items", referencesByKind(items, ItemDanglingImage), []string{"sha256:dangling"})
	for _, item := range items {
		if item.Kind == ItemContainer && item.Reference == "unknown-environment" && !strings.Contains(item.Reason, "known environment") {
			t.Fatalf("unknown-environment reason = %q", item.Reason)
		}
	}
}

func TestPlanBuildpackCache(t *testing.T) {
	t.Run("under both limits", func(t *testing.T) {
		items := Plan(Input{
			Cache: CacheInventory{
				Bytes:          5 << 30,
				DiskUsedBytes:  50 << 30,
				DiskTotalBytes: 100 << 30,
				Entries:        []CacheEntry{{Path: "/cache/a", Bytes: 5 << 30, LastUsedAt: daysAgo(10)}},
			},
			Now: planNow,
		})
		if len(items) != 0 {
			t.Fatalf("items = %+v, want none", items)
		}
	})

	t.Run("absolute cap removes oldest until under cap", func(t *testing.T) {
		items := Plan(Input{
			Cache: CacheInventory{
				Bytes:          12 << 30,
				DiskUsedBytes:  20 << 30,
				DiskTotalBytes: 100 << 30,
				Entries: []CacheEntry{
					{Path: "/cache/new", Bytes: 3 << 30, LastUsedAt: hoursAgo(1)},
					{Path: "/cache/oldest", Bytes: 5 << 30, LastUsedAt: hoursAgo(3)},
					{Path: "/cache/middle", Bytes: 4 << 30, LastUsedAt: hoursAgo(2)},
				},
			},
			Now: planNow,
		})
		assertStrings(t, "cache items", referencesByKind(items, ItemBuildpackCache), []string{"/cache/oldest"})
	})

	t.Run("filesystem pressure stops when utilization is back under the limit", func(t *testing.T) {
		items := Plan(Input{
			Cache: CacheInventory{
				Bytes:          10 << 30,
				DiskUsedBytes:  85 << 30,
				DiskTotalBytes: 100 << 30,
				Entries: []CacheEntry{
					{Path: "/cache/first", Bytes: 3 << 30, LastUsedAt: hoursAgo(3)},
					{Path: "/cache/second", Bytes: 2 << 30, LastUsedAt: hoursAgo(2)},
					{Path: "/cache/last", Bytes: 5 << 30, LastUsedAt: hoursAgo(1)},
				},
			},
			Now: planNow,
		})
		assertStrings(t, "cache items", referencesByKind(items, ItemBuildpackCache), []string{"/cache/first", "/cache/second"})
	})

	t.Run("filesystem pressure prunes all cache when that is not enough", func(t *testing.T) {
		items := Plan(Input{
			Cache: CacheInventory{
				Bytes:          5 << 30,
				DiskUsedBytes:  90 << 30,
				DiskTotalBytes: 100 << 30,
				Entries: []CacheEntry{
					{Path: "/cache/a", Bytes: 2 << 30, LastUsedAt: hoursAgo(2)},
					{Path: "/cache/b", Bytes: 2 << 30, LastUsedAt: hoursAgo(1)},
					{Path: "/cache/c", Bytes: 1 << 30, LastUsedAt: time.Time{}},
				},
			},
			Now: planNow,
		})
		assertStrings(t, "cache items", referencesByKind(items, ItemBuildpackCache), []string{"/cache/a", "/cache/b", "/cache/c"})
	})

	t.Run("cache bytes fall back to entry sum", func(t *testing.T) {
		items := Plan(Input{
			Cache: CacheInventory{
				DiskTotalBytes: 0,
				Entries:        []CacheEntry{{Path: "/cache/large", Bytes: 11 << 30, LastUsedAt: daysAgo(1)}},
			},
			Now: planNow,
		})
		assertStrings(t, "cache items", referencesByKind(items, ItemBuildpackCache), []string{"/cache/large"})
		if len(items) != 1 || !strings.Contains(items[0].Reason, "least recently used") {
			t.Fatalf("cache item reason = %+v", items)
		}
	})
}

func TestPlanUsesEnvironmentArtifactGrace(t *testing.T) {
	shortGrace := DefaultPolicy()
	shortGrace.KeepReleases = 1
	shortGrace.KeepNewerThanDays = 1
	shortGrace.ArtifactGraceDays = 1
	releases := []Release{
		{ID: "rel_0001", App: "api", Environment: "staging", Image: "registry.example.com:5000/team/api@sha256:short", CreatedAt: daysAgo(30)},
		{ID: "rel_0002", App: "api", Environment: "staging", CreatedAt: daysAgo(20)},
	}
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "staging", ServingRelease: "rel_0002", Policy: shortGrace}},
		Releases:     releases,
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "team/api", Digest: "sha256:short", CreatedAt: daysAgo(5), Bytes: 10},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"team/api@sha256:short"})
}

func TestPlanProtectsTaggedReleaseManifest(t *testing.T) {
	releases := []Release{{
		ID: "rel_serving", App: "api", Environment: "production",
		Image:     "registry/api:v1",
		ImageID:   "sha256:config",
		CreatedAt: daysAgo(1),
	}}
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: releases,
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:manifest", ConfigDigest: "sha256:config", Tags: []string{"v1"}, CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:old", Tags: []string{"v0"}, CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"api@sha256:old"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
}

func TestPlanFailsSafeForUnresolvedKeptTag(t *testing.T) {
	releases := []Release{{
		ID: "rel_serving", App: "api", Environment: "production",
		Image:     "registry.example.com:5000/api:v1",
		ImageID:   "sha256:config",
		CreatedAt: daysAgo(1),
	}}
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: releases,
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:manifest", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "worker", Digest: "sha256:old", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"worker@sha256:old"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), []string{"api"})
}

func TestPlanMissingEnvironmentReleaseFailsSafe(t *testing.T) {
	items := Plan(Input{
		Releases: []Release{{
			ID: "rel_ghost", App: "api", Environment: "ghost",
			Image:     "registry.example.com:5000/team/ghost:v1",
			CreatedAt: daysAgo(1),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "team/ghost", Digest: "sha256:manifest", CreatedAt: daysAgo(30), Bytes: 100},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), nil)
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), []string{"team/ghost"})
}

func TestPlanRegistryItemsAreRepositoryQualified(t *testing.T) {
	items := Plan(Input{
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:shared", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:shared", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "worker", Digest: "sha256:shared", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact),
		[]string{"api@sha256:shared", "worker@sha256:shared"})
}

func TestPlanProtectsConfigDigest(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			ImageID:   "sha256:config",
			CreatedAt: daysAgo(1),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:manifest-kept", ConfigDigest: "sha256:config", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:manifest-old", ConfigDigest: "sha256:other", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"api@sha256:manifest-old"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
}

func TestPlanArtifactGraceSurvivesPrunedRelease(t *testing.T) {
	shortGrace := DefaultPolicy()
	shortGrace.KeepReleases = 1
	shortGrace.KeepNewerThanDays = 1
	shortGrace.ArtifactGraceDays = 1
	prunedAt := planNow
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "staging", ServingRelease: "rel_0002", Policy: shortGrace}},
		Releases: []Release{
			{
				ID: "rel_0001", App: "api", Environment: "staging",
				Image: "registry.example.com:5000/team/api:v1", CreatedAt: daysAgo(30), PrunedAt: &prunedAt,
			},
			{ID: "rel_0002", App: "api", Environment: "staging", CreatedAt: daysAgo(20)},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "team/api", Digest: "sha256:manifest", Tags: []string{"v1"}, CreatedAt: daysAgo(5), Bytes: 10},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"team/api@sha256:manifest"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
}

func TestImageReferenceNormalization(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want imageRef
		ok   bool
	}{
		{
			name: "registry host and port with tag",
			raw:  "registry.example.com:5000/api:v1",
			want: imageRef{repository: "api", tag: "v1"},
			ok:   true,
		},
		{
			name: "https prefix",
			raw:  "https://registry.example.com:5000/team/api:v1",
			want: imageRef{repository: "team/api", tag: "v1"},
			ok:   true,
		},
		{
			name: "digest reference",
			raw:  "registry.example.com:5000/team/api@sha256:MANIFEST",
			want: imageRef{repository: "team/api", digest: "sha256:manifest"},
			ok:   true,
		},
		{
			name: "tag and digest",
			raw:  "registry.example.com/team/api:v1@sha256:manifest",
			want: imageRef{repository: "team/api", tag: "v1", digest: "sha256:manifest"},
			ok:   true,
		},
		{
			name: "no tag means latest",
			raw:  "127.0.0.1:5000/api",
			want: imageRef{repository: "api", tag: "latest"},
			ok:   true,
		},
		{
			name: "localhost registry",
			raw:  "localhost:5000/team/api:latest",
			want: imageRef{repository: "team/api", tag: "latest"},
			ok:   true,
		},
		{
			name: "repository and tag only",
			raw:  "api:v1",
			want: imageRef{repository: "api", tag: "v1"},
			ok:   true,
		},
		{
			name: "namespace repository",
			raw:  "team/api:v1",
			want: imageRef{repository: "team/api", tag: "v1"},
			ok:   true,
		},
		{
			name: "docker hub host is stripped",
			raw:  "docker.io/library/api:v1",
			want: imageRef{repository: "library/api", tag: "v1"},
			ok:   true,
		},
		{
			name: "uppercase normalizes",
			raw:  "DOCKER.IO/Team/API:V1",
			want: imageRef{repository: "team/api", tag: "V1"},
			ok:   true,
		},
		{
			name: "pure digest",
			raw:  "sha256:ABC",
			want: imageRef{digest: "sha256:abc"},
			ok:   true,
		},
		{
			name: "empty reference",
			raw:  " ",
			ok:   false,
		},
		{
			name: "missing digest",
			raw:  "registry.example.com/api@sha256:",
			ok:   false,
		},
		{
			name: "missing tag",
			raw:  "registry.example.com/api:",
			ok:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseImageReference(test.raw)
			if ok != test.ok {
				t.Fatalf("parseImageReference(%q) ok = %t, want %t", test.raw, ok, test.ok)
			}
			if ok && got != test.want {
				t.Fatalf("parseImageReference(%q) = %+v, want %+v", test.raw, got, test.want)
			}
		})
	}
}

func TestNormalizeRepository(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "api", want: "api"},
		{raw: "API", want: "api"},
		{raw: "team/api", want: "team/api"},
		{raw: "registry.example.com:5000/api", want: "api"},
		{raw: "localhost:5000/team/api", want: "team/api"},
		{raw: "docker.io/library/api", want: "library/api"},
		{raw: "api@sha256:abc", want: "api"},
		{raw: "registry.example.com:5000/team/api/", want: "team/api"},
	}
	for _, test := range tests {
		if got := normalizeRepository(test.raw); got != test.want {
			t.Fatalf("normalizeRepository(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
}

func TestPlanMovedTagWithoutImmutableIdentitySkipsRepository(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			Image:     "registry.example.com:5000/api:v1",
			ImageID:   "sha256:config-old",
			CreatedAt: daysAgo(1),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:old-manifest", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:new-manifest", Tags: []string{"v1"}, CreatedAt: daysAgo(1), Bytes: 100},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), nil)
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), []string{"api"})
	if containsReference(items, ItemRegistryArtifact, "api@sha256:old-manifest") {
		t.Fatal("old manifest was deleted after its tag moved")
	}
	for _, item := range items {
		if item.Kind == ItemRegistrySkipped && !strings.Contains(item.Reason, "immutable") {
			t.Fatalf("skip reason = %q, want an immutable-identity explanation", item.Reason)
		}
	}
}

func TestPlanFailedReleaseWithoutImageDoesNotSkipItsRepository(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "githello", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{
			{
				ID: "rel_serving", App: "githello", Environment: "production",
				Image:     "registry.example.com:5000/githello:serving",
				ImageID:   "sha256:serving-config",
				Status:    ReleaseStateReady,
				CreatedAt: daysAgo(1),
			},
			{
				ID: "rel_failed", App: "githello", Environment: "production",
				Image:     "registry.example.com:5000/githello:d0f76b56af71",
				Status:    ReleaseStateFailed,
				CreatedAt: daysAgo(2),
			},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "githello", Digest: "sha256:serving", ConfigDigest: "sha256:serving-config", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "githello", Digest: "sha256:old", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"githello@sha256:old"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
}

func TestPlanPendingReleaseWithoutImageDoesNotSkipItsRepository(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{
			{
				ID: "rel_serving", App: "api", Environment: "production",
				Image:     "registry.example.com:5000/api:serving",
				ImageID:   "sha256:serving-config",
				Status:    ReleaseStateReady,
				CreatedAt: daysAgo(1),
			},
			{
				ID: "rel_pending", App: "api", Environment: "production",
				Image:     "registry.example.com:5000/api:pending",
				Status:    ReleaseStatePending,
				CreatedAt: daysAgo(2),
			},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:serving", ConfigDigest: "sha256:serving-config", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:old", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"api@sha256:old"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
}

func TestPlanFailedReleaseWithImageIdentityStillRequiresResolution(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{
			{
				ID: "rel_serving", App: "api", Environment: "production",
				Image:     "registry.example.com:5000/api:serving",
				ImageID:   "sha256:serving-config",
				Status:    ReleaseStateReady,
				CreatedAt: daysAgo(1),
			},
			{
				ID: "rel_failed", App: "api", Environment: "production",
				Image:     "registry.example.com:5000/api:d0f76b56af71",
				ImageID:   "sha256:failed-config",
				Status:    ReleaseStateFailed,
				CreatedAt: daysAgo(2),
			},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:serving", ConfigDigest: "sha256:serving-config", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:old", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), nil)
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), []string{"api"})
}

func TestPlanIgnoresRepositoriesAbsentFromRegistryInventory(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			Image:     "registry.example.com:5000/team/api:v1",
			ImageID:   "sha256:missing-config",
			Status:    ReleaseStateReady,
			CreatedAt: daysAgo(1),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "worker", Digest: "sha256:old", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"worker@sha256:old"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
}

func TestPlanMovedTagWithConfigDigestProtectsOldManifest(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			Image:     "registry.example.com:5000/api:v1",
			ImageID:   "sha256:config-old",
			CreatedAt: daysAgo(1),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:old-manifest", ConfigDigest: "sha256:config-old", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:new-manifest", Tags: []string{"v1"}, CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:unrelated", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"api@sha256:unrelated"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
	if containsReference(items, ItemRegistryArtifact, "api@sha256:old-manifest") {
		t.Fatal("manifest matching the release config digest was planned")
	}
}

func TestPlanImageIDResolvesThroughRepoDigests(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			Image:     "registry.example.com:5000/api:v1",
			ImageID:   "sha256:config-old",
			CreatedAt: daysAgo(1),
		}},
		LocalImages: []LocalImageRef{{
			ID: "sha256:config-old",
			RepoDigests: []string{
				"registry.example.com:5000/api@sha256:old-manifest",
			},
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:old-manifest", CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: "api", Digest: "sha256:unrelated", CreatedAt: daysAgo(30), Bytes: 50},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"api@sha256:unrelated"})
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), nil)
	if containsReference(items, ItemRegistryArtifact, "api@sha256:old-manifest") {
		t.Fatal("manifest resolved from RepoDigests was planned")
	}
}

func TestPlanOnlyUnresolvedImageIDSkipsAll(t *testing.T) {
	items := Plan(Input{
		Environments: []Environment{{
			App: "api", Name: "production", ServingRelease: "rel_serving", Policy: DefaultPolicy(),
		}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			ImageID:   "sha256:config-missing",
			CreatedAt: daysAgo(1),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:old-manifest", CreatedAt: daysAgo(30), Bytes: 100},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), nil)
	assertStrings(t, "skip items", referencesByKind(items, ItemRegistrySkipped), []string{"*"})
}

func TestPlanIsIdempotentAfterPruning(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 2
	policy.KeepNewerThanDays = 1
	releases := numberedReleases("rel", "api", "production", 5, 30)
	input := Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_0005", Policy: policy}},
		Releases:     releases,
		Now:          planNow,
	}
	first := Plan(input)
	firstReleases := referencesByKind(first, ItemRelease)
	assertStrings(t, "first release items", firstReleases, []string{"rel_0001", "rel_0002", "rel_0003"})

	for index := range input.Releases {
		for _, reference := range firstReleases {
			if input.Releases[index].ID == reference {
				input.Releases[index].PrunedAt = timePointer(planNow)
			}
		}
	}
	second := Plan(input)
	if remaining := referencesByKind(second, ItemRelease); len(remaining) != 0 {
		t.Fatalf("second plan release items = %v, want none", remaining)
	}
}

func TestPlanOrderIsDeterministic(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 3
	policy.KeepNewerThanDays = 1
	input := Input{
		Environments: []Environment{
			{App: "api", Name: "staging", ServingRelease: "stg_0005", Policy: policy},
			{App: "api", Name: "production", ServingRelease: "prd_0005", Policy: policy},
		},
		Releases: append(
			numberedReleases("stg", "api", "staging", 5, 30),
			numberedReleases("prd", "api", "production", 5, 30)...,
		),
		Deployments: []Deployment{
			{ID: "dep_2", App: "api", Environment: "staging", Release: "stg_0001", PreviousRelease: "stg_0002", State: DeploymentStateSucceeded, CreatedAt: daysAgo(29), CompletedAt: timePointer(daysAgo(29))},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: "api", Digest: "sha256:z", CreatedAt: daysAgo(30), Bytes: 1},
			{Repository: "api", Digest: "sha256:a", CreatedAt: daysAgo(30), Bytes: 1},
		}},
		Cache: CacheInventory{
			Bytes:          12 << 30,
			DiskUsedBytes:  20 << 30,
			DiskTotalBytes: 100 << 30,
			Entries: []CacheEntry{
				{Path: "/cache/new", Bytes: 3 << 30, LastUsedAt: hoursAgo(1)},
				{Path: "/cache/oldest", Bytes: 5 << 30, LastUsedAt: hoursAgo(3)},
				{Path: "/cache/middle", Bytes: 4 << 30, LastUsedAt: hoursAgo(2)},
			},
		},
		Containers: []Container{
			{Name: "z", App: "api", Environment: "production", Release: "prd_0001"},
			{Name: "a", App: "api", Environment: "staging", Release: "stg_0001"},
		},
		Images: []Image{
			{ID: "sha256:z", Dangling: true, Bytes: 1},
			{ID: "sha256:a", Dangling: true, Bytes: 1},
		},
		Now: planNow,
	}
	first := Plan(input)
	reversed := input
	reversed.Environments = reversedSlice(input.Environments)
	reversed.Releases = reversedSlice(input.Releases)
	reversed.Deployments = reversedSlice(input.Deployments)
	reversed.Registry.Artifacts = reversedSlice(input.Registry.Artifacts)
	reversed.Cache.Entries = reversedSlice(input.Cache.Entries)
	reversed.Containers = reversedSlice(input.Containers)
	reversed.Images = reversedSlice(input.Images)
	second := Plan(reversed)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("plan changed with input order:\nfirst  = %+v\nsecond = %+v", first, second)
	}
	assertStrings(t, "cache items", referencesByKind(first, ItemBuildpackCache), []string{"/cache/oldest"})
	assertStrings(t, "artifact items", referencesByKind(first, ItemRegistryArtifact), []string{"api@sha256:a", "api@sha256:z"})
}

// TestRollbackTargetSQLCases mirrors every branch of the control plane's
// previousRollbackRelease query. The production query lives in
// internal/controlplane/platform_state.go; this pure planner keeps the same
// selection rule so retention cannot prune a rollback target.
func TestRollbackTargetSQLCases(t *testing.T) {
	succeeded := func(id, env, release, previous string, createdDays, completedDays int) Deployment {
		return Deployment{
			ID: id, App: "api", Environment: env,
			Release: release, PreviousRelease: previous, State: DeploymentStateSucceeded,
			CreatedAt: daysAgo(createdDays), CompletedAt: timePointer(daysAgo(completedDays)),
		}
	}
	tests := []struct {
		name        string
		serving     string
		deployments []Deployment
		want        string
	}{
		{
			name:    "no deployment history",
			serving: "",
			want:    "",
		},
		{
			name:    "first deploy has no previous release",
			serving: "rel_1",
			deployments: []Deployment{
				succeeded("dep_1", "production", "rel_1", "", 10, 10),
			},
			want: "",
		},
		{
			name:    "serving deployment supplies its previous release",
			serving: "rel_2",
			deployments: []Deployment{
				succeeded("dep_2", "production", "rel_2", "rel_1", 10, 10),
			},
			want: "rel_1",
		},
		{
			name:    "newest serving deployment by completed time wins",
			serving: "rel_3",
			deployments: []Deployment{
				succeeded("dep_old", "production", "rel_3", "rel_1", 20, 20),
				succeeded("dep_new", "production", "rel_3", "rel_2", 10, 10),
			},
			want: "rel_2",
		},
		{
			name:    "null completed time sorts last",
			serving: "rel_3",
			deployments: []Deployment{
				{ID: "dep_null", App: "api", Environment: "production", Release: "rel_3", PreviousRelease: "rel_1", State: DeploymentStateSucceeded, CreatedAt: daysAgo(1)},
				succeeded("dep_done", "production", "rel_3", "rel_2", 5, 5),
			},
			want: "rel_2",
		},
		{
			name:    "created time breaks a completed time tie",
			serving: "rel_3",
			deployments: []Deployment{
				{ID: "dep_a", App: "api", Environment: "production", Release: "rel_3", PreviousRelease: "rel_1", State: DeploymentStateSucceeded, CreatedAt: daysAgo(2), CompletedAt: timePointer(daysAgo(5))},
				{ID: "dep_b", App: "api", Environment: "production", Release: "rel_3", PreviousRelease: "rel_2", State: DeploymentStateSucceeded, CreatedAt: daysAgo(3), CompletedAt: timePointer(daysAgo(5))},
			},
			want: "rel_1",
		},
		{
			name:    "id breaks an exact timestamp tie",
			serving: "rel_3",
			deployments: []Deployment{
				{ID: "dep_a", App: "api", Environment: "production", Release: "rel_3", PreviousRelease: "rel_1", State: DeploymentStateSucceeded, CreatedAt: daysAgo(5), CompletedAt: timePointer(daysAgo(5))},
				{ID: "dep_b", App: "api", Environment: "production", Release: "rel_3", PreviousRelease: "rel_2", State: DeploymentStateSucceeded, CreatedAt: daysAgo(5), CompletedAt: timePointer(daysAgo(5))},
			},
			want: "rel_2",
		},
		{
			name:    "fallback chooses the latest different release",
			serving: "rel_3",
			deployments: []Deployment{
				succeeded("dep_serving", "production", "rel_3", "", 20, 20),
				succeeded("dep_old", "production", "rel_1", "", 10, 10),
				succeeded("dep_new", "production", "rel_2", "", 5, 5),
			},
			want: "rel_2",
		},
		{
			name:    "no serving release falls back to latest succeeded",
			serving: "",
			deployments: []Deployment{
				succeeded("dep_old", "production", "rel_1", "", 10, 10),
				succeeded("dep_new", "production", "rel_2", "", 5, 5),
			},
			want: "rel_2",
		},
		{
			name:    "failed and other-environment deployments are ignored",
			serving: "rel_2",
			deployments: []Deployment{
				{ID: "dep_failed", App: "api", Environment: "production", Release: "rel_2", PreviousRelease: "rel_1", State: "failed", CreatedAt: daysAgo(1), CompletedAt: timePointer(daysAgo(1))},
				succeeded("dep_other", "staging", "rel_2", "rel_9", 1, 1),
			},
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RollbackTarget("api", "production", test.serving, test.deployments); got != test.want {
				t.Fatalf("RollbackTarget() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTotalBytes(t *testing.T) {
	items := []Item{{Bytes: 10}, {Bytes: -5}, {Bytes: 0}, {Bytes: 32}}
	if total := TotalBytes(items); total != 42 {
		t.Fatalf("TotalBytes() = %d, want 42", total)
	}
}

func containsReference(items []Item, kind ItemKind, reference string) bool {
	for _, item := range items {
		if item.Kind == kind && item.Reference == reference {
			return true
		}
	}
	return false
}

func reversedSlice[T any](values []T) []T {
	out := make([]T, len(values))
	for index, value := range values {
		out[len(values)-1-index] = value
	}
	return out
}
