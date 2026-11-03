package retention

import (
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
