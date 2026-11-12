package retention

import (
	"strings"
	"testing"
)

const (
	canonicalRegistry = "127.0.0.1:5000"
	canonicalRepo     = "eregion/api-production"
)

func canonicalImage(digest string, _ ...string) string {
	return canonicalRegistry + "/" + canonicalRepo + "@" + digest
}

func canonicalDigest(fill byte) string {
	return "sha256:" + strings.Repeat(string(fill), 64)
}

func TestPlanProtectsServingAndRollbackByCanonicalDigestWithoutLocalImages(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	servingDigest := canonicalDigest('1')
	rollbackDigest := canonicalDigest('2')
	orphanDigest := canonicalDigest('3')
	releases := []Release{
		{
			ID: "rel_serving", App: "api", Environment: "production",
			Image: canonicalImage(servingDigest, canonicalDigest('a')), ImageID: canonicalDigest('a'),
			Status: ReleaseStateReady, CreatedAt: daysAgo(40),
		},
		{
			ID: "rel_rollback", App: "api", Environment: "production",
			Image: canonicalImage(rollbackDigest, canonicalDigest('b')), ImageID: canonicalDigest('b'),
			Status: ReleaseStateReady, CreatedAt: daysAgo(39),
		},
		{
			ID: "rel_newest", App: "api", Environment: "production",
			Image: canonicalImage(canonicalDigest('4'), canonicalDigest('c')), ImageID: canonicalDigest('c'),
			Status: ReleaseStateReady, CreatedAt: daysAgo(2),
		},
	}
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_serving", Policy: policy}},
		Releases:     releases,
		Deployments: []Deployment{{
			ID: "dep_serving", App: "api", Environment: "production",
			Release: "rel_serving", PreviousRelease: "rel_rollback",
			State: DeploymentStateSucceeded, CreatedAt: daysAgo(40), CompletedAt: timePointer(daysAgo(40)),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: canonicalRepo, Digest: servingDigest, ConfigDigest: canonicalDigest('a'), CreatedAt: daysAgo(40), Bytes: 100},
			{Repository: canonicalRepo, Digest: rollbackDigest, ConfigDigest: canonicalDigest('b'), CreatedAt: daysAgo(39), Bytes: 100},
			{Repository: canonicalRepo, Digest: canonicalDigest('4'), ConfigDigest: canonicalDigest('c'), CreatedAt: daysAgo(2), Bytes: 100},
			{Repository: canonicalRepo, Digest: orphanDigest, ConfigDigest: canonicalDigest('d'), CreatedAt: daysAgo(40), Bytes: 100},
		}},
		Now: planNow,
	})
	artifacts := referencesByKind(items, ItemRegistryArtifact)
	if containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+servingDigest) {
		t.Fatalf("serving canonical manifest was planned for removal: %v", artifacts)
	}
	if containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+rollbackDigest) {
		t.Fatalf("rollback canonical manifest was planned for removal: %v", artifacts)
	}
	if !containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+orphanDigest) {
		t.Fatalf("eligible unreferenced manifest was not planned: %v", artifacts)
	}
}

func TestPlanDoesNotEvictTheLocalImageOfARetainedCanonicalRelease(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	config := canonicalDigest('b')
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_serving", Policy: policy}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			Image: canonicalImage(canonicalDigest('a'), config), ImageID: config, Status: ReleaseStateReady, CreatedAt: daysAgo(40),
		}},
		LocalImages: []LocalImageRef{{ID: config, RepoTags: []string{"eregion/api-production:old"}, RepoDigests: []string{canonicalImage(canonicalDigest('a'))}}},
		Now:         planNow,
	})
	for _, item := range items {
		if item.Kind == ItemImage || item.Kind == ItemRegistryArtifact {
			t.Fatalf("retained canonical release artifact was planned: %+v", item)
		}
	}
}

func TestPlanProtectsARetainedReleaseByInventoryConfigDigestAlone(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	config := canonicalDigest('c')
	manifest := canonicalDigest('d')
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_serving", Policy: policy}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			Image: "eregion/api-production:old", ImageID: config, Status: ReleaseStateReady, CreatedAt: daysAgo(40),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: canonicalRepo, Digest: manifest, ConfigDigest: config, CreatedAt: daysAgo(40), Bytes: 100},
		}},
		Now: planNow,
	})
	if containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+manifest) {
		t.Fatalf("a retained release's config digest did not protect its manifest without local image metadata: %v", items)
	}
}

func TestPlanProtectsRetainedLocalImageIDsThroughCanonicalRepoDigests(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	servingDigest := canonicalDigest('7')
	rollbackDigest := canonicalDigest('8')
	servingConfig := canonicalDigest('f')
	rollbackConfig := canonicalDigest('0')
	releases := []Release{
		{ID: "rel_serving", App: "api", Environment: "production", Image: "eregion/api-production:old", ImageID: servingConfig, Status: ReleaseStateReady, CreatedAt: daysAgo(40)},
		{ID: "rel_rollback", App: "api", Environment: "production", Image: "eregion/api-production:older", ImageID: rollbackConfig, Status: ReleaseStateReady, CreatedAt: daysAgo(39)},
		{ID: "rel_newest", App: "api", Environment: "production", Image: canonicalImage(canonicalDigest('9'), canonicalDigest('e')), ImageID: canonicalDigest('e'), Status: ReleaseStateReady, CreatedAt: daysAgo(2)},
	}
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_serving", Policy: policy}},
		Releases:     releases,
		Deployments: []Deployment{{
			ID: "dep_serving", App: "api", Environment: "production",
			Release: "rel_serving", PreviousRelease: "rel_rollback",
			State: DeploymentStateSucceeded, CreatedAt: daysAgo(40), CompletedAt: timePointer(daysAgo(40)),
		}},
		LocalImages: []LocalImageRef{
			{ID: servingConfig, RepoDigests: []string{canonicalImage(servingDigest, servingConfig)}},
			{ID: rollbackConfig, RepoDigests: []string{canonicalImage(rollbackDigest, rollbackConfig)}},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: canonicalRepo, Digest: servingDigest, ConfigDigest: servingConfig, CreatedAt: daysAgo(40), Bytes: 100},
			{Repository: canonicalRepo, Digest: rollbackDigest, ConfigDigest: rollbackConfig, CreatedAt: daysAgo(39), Bytes: 100},
			{Repository: canonicalRepo, Digest: canonicalDigest('9'), ConfigDigest: canonicalDigest('e'), CreatedAt: daysAgo(2), Bytes: 100},
			{Repository: canonicalRepo, Digest: canonicalDigest('a'), CreatedAt: daysAgo(40), Bytes: 100},
		}},
		Now: planNow,
	})
	for _, digest := range []string{servingDigest, rollbackDigest} {
		if containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+digest) {
			t.Fatalf("retained local image manifest %s was planned: %v", digest, items)
		}
	}
	if !containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+canonicalDigest('a')) {
		t.Fatalf("eligible unprotected manifest was not planned: %v", items)
	}
}

func TestPlanProtectsAllEnvironmentsSharingAnAppRepositoryAfterTagMovement(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	servingDigest := canonicalDigest('a')
	pendingDigest := canonicalDigest('b')
	rollbackDigest := canonicalDigest('c')
	movedTagDigest := canonicalDigest('d')
	items := Plan(Input{
		Environments: []Environment{
			{App: "api", Name: "production", ServingRelease: "rel_prod_serving", Policy: policy},
			{App: "api", Name: "staging", ServingRelease: "rel_stage_serving", Policy: policy},
		},
		Releases: []Release{
			{ID: "rel_prod_serving", App: "api", Environment: "production", Image: canonicalImage(servingDigest, canonicalDigest('e')), ImageID: canonicalDigest('e'), Status: ReleaseStateReady, CreatedAt: daysAgo(40)},
			{ID: "rel_stage_serving", App: "api", Environment: "staging", Image: canonicalImage(pendingDigest, canonicalDigest('f')), ImageID: canonicalDigest('f'), Status: ReleaseStateReady, CreatedAt: daysAgo(40)},
			{ID: "rel_rollback", App: "api", Environment: "production", Image: canonicalImage(rollbackDigest, canonicalDigest('0')), ImageID: canonicalDigest('0'), Status: ReleaseStateReady, CreatedAt: daysAgo(39)},
		},
		Deployments: []Deployment{
			{ID: "dep_prod", App: "api", Environment: "production", Release: "rel_prod_serving", PreviousRelease: "rel_rollback", State: DeploymentStateSucceeded, CreatedAt: daysAgo(40), CompletedAt: timePointer(daysAgo(40))},
			{ID: "dep_pending", App: "api", Environment: "staging", Release: "rel_stage_serving", State: "pending", CreatedAt: daysAgo(1)},
		},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: canonicalRepo, Digest: servingDigest, ConfigDigest: canonicalDigest('e'), CreatedAt: daysAgo(40), Bytes: 100},
			{Repository: canonicalRepo, Digest: pendingDigest, ConfigDigest: canonicalDigest('f'), CreatedAt: daysAgo(40), Bytes: 100},
			{Repository: canonicalRepo, Digest: rollbackDigest, ConfigDigest: canonicalDigest('0'), CreatedAt: daysAgo(39), Bytes: 100},
			{Repository: canonicalRepo, Digest: movedTagDigest, Tags: []string{"v1"}, CreatedAt: daysAgo(40), Bytes: 100},
		}},
		Now: planNow,
	})
	for _, digest := range []string{servingDigest, pendingDigest, rollbackDigest} {
		if containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+digest) {
			t.Fatalf("shared app repository artifact %s was planned: %v", digest, items)
		}
	}
	if !containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+movedTagDigest) {
		t.Fatalf("the moved tag's unreferenced manifest was not eligible: %v", items)
	}
}

func TestRepositoryMatchingDoesNotStripOrdinaryNamespaces(t *testing.T) {
	if repositoryMatches("mbvlabs/andurel", "other/andurel") {
		t.Fatal("an ordinary namespace component was stripped from repository matching")
	}
	if !repositoryMatches(canonicalRegistry+"/"+canonicalRepo, canonicalRepo) {
		t.Fatal("an exact registry host was not stripped for inventory matching")
	}
}

func TestPlanKeepsAProtectedCanonicalDigestWhenItsTagMoved(t *testing.T) {
	policy := DefaultPolicy()
	policy.KeepReleases = 1
	policy.KeepNewerThanDays = 1
	protectedDigest := canonicalDigest('5')
	movedTagDigest := canonicalDigest('6')
	items := Plan(Input{
		Environments: []Environment{{App: "api", Name: "production", ServingRelease: "rel_serving", Policy: policy}},
		Releases: []Release{{
			ID: "rel_serving", App: "api", Environment: "production",
			Image: canonicalImage(protectedDigest, canonicalDigest('e')), ImageID: canonicalDigest('e'),
			Status: ReleaseStateReady, CreatedAt: daysAgo(30),
		}},
		Registry: RegistryInventory{Artifacts: []RegistryArtifact{
			{Repository: canonicalRepo, Digest: protectedDigest, ConfigDigest: canonicalDigest('e'), CreatedAt: daysAgo(30), Bytes: 100},
			{Repository: canonicalRepo, Digest: movedTagDigest, Tags: []string{"v1"}, CreatedAt: daysAgo(30), Bytes: 100},
		}},
		Now: planNow,
	})
	if containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+protectedDigest) {
		t.Fatalf("the protected digest was removed because its tag moved: %v", items)
	}
	if !containsReference(items, ItemRegistryArtifact, canonicalRepo+"@"+movedTagDigest) {
		t.Fatalf("the moved tag's unreferenced manifest was not eligible: %v", items)
	}
}
