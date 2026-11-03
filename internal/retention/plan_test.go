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
	releases[0].Artifacts = []string{"sha256:shared"}
	releases[0].ImageID = "sha256:unique"
	releases[2].Artifacts = []string{"sha256:shared"}
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_0003", Policy: policy}},
		Releases:     releases,
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Digest: "sha256:shared", CreatedAt: daysAgo(30), Bytes: 100},
			{Digest: "sha256:unique", CreatedAt: daysAgo(30), Bytes: 200},
			{Digest: "sha256:orphan", CreatedAt: daysAgo(30), Bytes: 300},
		}},
		Now: planNow,
	})

	assertStrings(t, "release items", referencesByKind(items, ItemRelease), []string{"rel_0001", "rel_0002"})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"sha256:orphan", "sha256:unique"})
	for _, item := range items {
		if item.Kind == ItemRegistryArtifact && item.Reference == "sha256:shared" {
			t.Fatal("shared digest referenced by a kept release was planned")
		}
		if item.Kind == ItemRegistryArtifact && item.Reference == "sha256:unique" {
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
			{Digest: "sha256:inflight", CreatedAt: daysAgo(30), Bytes: 10},
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
		[]string{"container-without-name", "no-labels", "stale", "unknown-environment"})
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
		{ID: "rel_0001", App: "api", Environment: "staging", ImageID: "sha256:short", CreatedAt: daysAgo(30)},
		{ID: "rel_0002", App: "api", Environment: "staging", CreatedAt: daysAgo(20)},
	}
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "staging", ServingRelease: "rel_0002", Policy: shortGrace}},
		Releases:     releases,
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Digest: "sha256:short", CreatedAt: daysAgo(5), Bytes: 10},
		}},
		Now: planNow,
	})
	assertStrings(t, "artifact items", referencesByKind(items, ItemRegistryArtifact), []string{"sha256:short"})
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
			{Digest: "sha256:z", CreatedAt: daysAgo(30), Bytes: 1},
			{Digest: "sha256:a", CreatedAt: daysAgo(30), Bytes: 1},
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
	assertStrings(t, "artifact items", referencesByKind(first, ItemRegistryArtifact), []string{"sha256:a", "sha256:z"})
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
