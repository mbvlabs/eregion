package retention

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPlanReleaseImagesUsesImmutableIdentityAndSharedProtection(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	shared := "sha256:" + strings.Repeat("a", 64)
	containerUsed := "sha256:" + strings.Repeat("b", 64)
	unshared := "sha256:" + strings.Repeat("e", 64)
	missing := ""
	releases := []Release{
		{ID: "rel_old_shared", App: "api", Environment: "production", ImageID: shared, CreatedAt: daysAgo(30)},
		{ID: "rel_old_container", App: "api", Environment: "production", ImageID: containerUsed, CreatedAt: daysAgo(29)},
		{ID: "rel_old_missing", App: "api", Environment: "production", ImageID: missing, CreatedAt: daysAgo(28)},
		{ID: "rel_old_unshared", App: "api", Environment: "production", ImageID: unshared, CreatedAt: daysAgo(27)},
		{ID: "rel_kept", App: "api", Environment: "production", ImageID: shared, CreatedAt: daysAgo(1)},
	}
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_kept", Policy: policy}},
		Releases:     releases,
		Containers: []Container{{
			Name: "kept-container", App: "api", Environment: "production", Image: containerUsed,
		}},
		Now: planNow,
	})
	for _, release := range []string{"rel_old_shared", "rel_old_container", "rel_old_missing", "rel_old_unshared"} {
		if !containsReference(items, ItemRelease, release) {
			t.Fatalf("release %s was not planned for row marking: %+v", release, items)
		}
	}
	if containsReference(items, ItemImage, shared) {
		t.Fatalf("a shared image was planned for removal: %+v", items)
	}
	if containsReference(items, ItemImage, containerUsed) {
		t.Fatalf("a container-used image was planned for removal: %+v", items)
	}
	if containsReference(items, ItemImage, "") {
		t.Fatalf("an image removal was planned without an immutable identity: %+v", items)
	}
	if !containsReference(items, ItemImage, unshared) {
		t.Fatalf("an unshared immutable image was not planned for removal: %+v", items)
	}
	if !containsReference(items, ItemImageSkipped, "rel_old_shared") {
		t.Fatalf("shared image skip reason missing: %+v", items)
	}
	if !containsReference(items, ItemImageSkipped, "rel_old_container") {
		t.Fatalf("container image skip reason missing: %+v", items)
	}
	if !containsReference(items, ItemImageSkipped, "rel_old_missing") {
		t.Fatalf("missing image identity skip reason missing: %+v", items)
	}
}

func TestPlanUnknownCacheMeasurementDoesNotRemoveEntries(t *testing.T) {
	policy := DefaultPolicy()
	policy.BuildpackCacheMaxBytes = 1 << 30
	items := Plan(Input{
		Cache: CacheInventory{
			Bytes:   2 << 30,
			Unknown: true,
			Entries: []CacheEntry{{Path: "/cache/a", Bytes: 2 << 30, LastUsedAt: daysAgo(1)}},
		},
		ServerPolicy: policy,
		Now:          planNow,
	})
	for _, item := range items {
		if item.Kind == ItemBuildpackCache {
			t.Fatalf("an unknown cache measurement planned a removal: %+v", item)
		}
	}
	if !containsReference(items, ItemCacheSkipped, "*") {
		t.Fatalf("unknown cache measurement did not produce a diagnostic: %+v", items)
	}
}

func TestPlanPrunedReleaseStillRemovesPresentImage(t *testing.T) {
	policy := DefaultPolicy()
	prunedAt := planNow.Add(-time.Hour)
	imageID := "sha256:" + strings.Repeat("c", 64)
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_kept", Policy: policy}},
		Releases: []Release{
			{ID: "rel_pruned", App: "api", Environment: "production", ImageID: imageID, CreatedAt: daysAgo(30), PrunedAt: &prunedAt},
			{ID: "rel_kept", App: "api", Environment: "production", ImageID: "sha256:" + strings.Repeat("d", 64), CreatedAt: daysAgo(1)},
		},
		LocalImages: []LocalImageRef{{ID: imageID}},
		Now:         planNow,
	})
	if containsReference(items, ItemRelease, "rel_pruned") {
		t.Fatalf("an already pruned release was planned for marking again: %+v", items)
	}
	if !containsReference(items, ItemImage, imageID) {
		t.Fatalf("a pruned release's still-present image was not planned for removal: %+v", items)
	}
}

func TestPlanImageItemShowsCurrentRepoTags(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	imageID := "sha256:" + strings.Repeat("f", 64)
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_kept", Policy: policy}},
		Releases: []Release{
			{ID: "rel_old", App: "api", Environment: "production", ImageID: imageID, CreatedAt: daysAgo(30)},
			{ID: "rel_kept", App: "api", Environment: "production", ImageID: "sha256:" + strings.Repeat("d", 64), CreatedAt: daysAgo(1)},
		},
		LocalImages: []LocalImageRef{{
			ID:       imageID,
			RepoTags: []string{"127.0.0.1:5000/api:old", "127.0.0.1:5000/api:older"},
		}},
		Now: planNow,
	})
	for _, item := range items {
		if item.Kind != ItemImage || item.Reference != imageID {
			continue
		}
		want := []string{"127.0.0.1:5000/api:old", "127.0.0.1:5000/api:older"}
		if !reflect.DeepEqual(item.Tags, want) {
			t.Fatalf("image item tags = %v, want %v", item.Tags, want)
		}
		return
	}
	t.Fatalf("the release image was not planned: %+v", items)
}

func TestPlanProtectsImagesUsedByUnlabeledContainers(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	releaseImage := "sha256:" + strings.Repeat("a", 64)
	danglingImage := "sha256:" + strings.Repeat("b", 64)
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_kept", Policy: policy}},
		Releases: []Release{
			{ID: "rel_old", App: "api", Environment: "production", ImageID: releaseImage, CreatedAt: daysAgo(30)},
			{ID: "rel_kept", App: "api", Environment: "production", ImageID: "sha256:" + strings.Repeat("d", 64), CreatedAt: daysAgo(1)},
		},
		Containers: []Container{
			{Name: "pedantic_jackson", Image: releaseImage, Owned: false},
			{Name: "ea1e68560301", Image: danglingImage, Owned: false},
		},
		Images: []Image{{ID: danglingImage, Dangling: true, Bytes: 1 << 30}},
		LocalImages: []LocalImageRef{{
			ID: releaseImage, RepoTags: []string{"127.0.0.1:5000/api:old"},
		}},
		Now: planNow,
	})
	if containsReference(items, ItemImage, releaseImage) {
		t.Fatalf("an image used by an unlabeled container was planned for removal: %+v", items)
	}
	if containsReference(items, ItemDanglingImage, danglingImage) {
		t.Fatalf("a dangling image used by an unlabeled container was planned: %+v", items)
	}
	if containsReference(items, ItemContainer, "pedantic_jackson") {
		t.Fatalf("an unlabeled container was planned for removal: %+v", items)
	}
	reasons := []string{}
	for _, item := range items {
		if item.Kind == ItemImageSkipped {
			reasons = append(reasons, item.Reason)
		}
	}
	joined := strings.Join(reasons, "\n")
	for _, want := range []string{"used by container pedantic_jackson", "used by container ea1e68560301"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("skip diagnostics %q lack %q", joined, want)
		}
	}
	if bytes := TotalBytes(items); bytes != 0 {
		t.Fatalf("a protected image's bytes counted as reclaimable: %d", bytes)
	}
}
