package retention

import (
	"testing"
	"time"
)

func TestInputFromInventoryMapsNodeObservation(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	created := now.Add(-30 * 24 * time.Hour)
	inventory := Inventory{
		CollectedAt: now,
		RegistryArtifacts: []InventoryRegistryArtifact{
			{Repository: "api", Digest: "sha256:old", Bytes: 100, CreatedAt: &created},
			{Repository: "api", Digest: "sha256:unknown", Bytes: 200},
		},
		CacheVolumes: []CacheVolume{
			{Name: "eregion-build-cache-617069_70726f64", Kind: "build", App: "api", Environment: "prod", SizeBytes: 10},
			{Name: "pack-cache-abc.build", Kind: "pack", Ownerless: true, SizeBytes: 20},
		},
		Containers: []InventoryContainer{{
			ID: "cid1", Name: "api-prod-web-1-dep", State: "running", SizeBytes: 30,
			Labels: map[string]string{
				ContainerAppLabel: "api", ContainerEnvironmentLabel: "production",
				ContainerReleaseLabel: "rel_1", ContainerDeploymentLabel: "dep_1",
			},
		}, {
			ID: "cid2", Name: "pedantic_jackson", Image: "sha256:unlabeled", State: "exited",
		}},
		DanglingImages: []InventoryImage{{ID: "sha256:dangling", SizeBytes: 40}},
		LocalImages: []LocalImage{{
			ID: "sha256:config", RepoTags: []string{"api:v1"},
			RepoDigests: []string{"127.0.0.1:5000/api@sha256:manifest"},
		}},
		Disk: Disk{Path: "/var/lib/eregion", TotalBytes: 1000, FreeBytes: 250},
	}
	input := InputFromInventory(inventory, PlatformState{ServerPolicy: DefaultPolicy()}, now)

	if len(input.Registry.Artifacts) != 2 {
		t.Fatalf("registry artifacts = %+v", input.Registry.Artifacts)
	}
	if !input.Registry.Artifacts[0].CreatedAt.Equal(created) {
		t.Fatalf("known artifact time = %s, want %s", input.Registry.Artifacts[0].CreatedAt, created)
	}
	if !input.Registry.Artifacts[1].CreatedAt.Equal(now) {
		t.Fatalf("an artifact with an unknown age must be treated as new, got %s", input.Registry.Artifacts[1].CreatedAt)
	}
	if input.RegistryUnsafeReason != "" {
		t.Fatalf("complete inventory was marked unsafe: %q", input.RegistryUnsafeReason)
	}

	if input.Cache.Bytes != 30 || len(input.Cache.Entries) != 2 {
		t.Fatalf("cache = %+v", input.Cache)
	}
	if input.Cache.Entries[1].Path != "pack-cache-abc.build" {
		t.Fatalf("ownerless pack cache was dropped: %+v", input.Cache.Entries)
	}
	if input.Cache.DiskUsedBytes != 750 || input.Cache.DiskTotalBytes != 1000 {
		t.Fatalf("disk = used %d total %d", input.Cache.DiskUsedBytes, input.Cache.DiskTotalBytes)
	}

	if len(input.Containers) != 2 {
		t.Fatalf("containers = %+v", input.Containers)
	}
	container := input.Containers[0]
	if !container.Owned || container.App != "api" || container.Environment != "production" || container.Release != "rel_1" || container.Deployment != "dep_1" {
		t.Fatalf("container labels were not mapped: %+v", container)
	}
	if input.Containers[1].Owned || input.Containers[1].Image != "sha256:unlabeled" {
		t.Fatalf("an unlabeled container was mapped as owned or without its image: %+v", input.Containers[1])
	}
	if len(input.Images) != 1 || !input.Images[0].Dangling || input.Images[0].ID != "sha256:dangling" {
		t.Fatalf("dangling images = %+v", input.Images)
	}
	if len(input.LocalImages) != 1 || input.LocalImages[0].RepoDigests[0] != "127.0.0.1:5000/api@sha256:manifest" {
		t.Fatalf("local images = %+v", input.LocalImages)
	}
}

func TestPlanInventorySkipsRegistryWhenTruncated(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	inventory := Inventory{
		CollectedAt: now,
		Truncated:   true,
		RegistryArtifacts: []InventoryRegistryArtifact{{
			Repository: "api", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Bytes: 100, CreatedAt: &old,
		}},
	}
	result := PlanInventory(inventory, PlatformState{ServerPolicy: DefaultPolicy()}, now)
	if len(result.Items) != 1 {
		t.Fatalf("items = %+v, want one skip diagnostic", result.Items)
	}
	item := result.Items[0]
	if item.Kind != ItemRegistrySkipped || item.Reference != "*" || item.Reason == "" {
		t.Fatalf("truncated inventory item = %+v", item)
	}
	if result.PlannedBytes != 0 {
		t.Fatalf("planned bytes = %d, want 0", result.PlannedBytes)
	}
}

func TestPlanInventoryTreatsUnknownArtifactAgeAsNew(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	inventory := Inventory{
		CollectedAt: now,
		RegistryArtifacts: []InventoryRegistryArtifact{{
			Repository: "api", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Bytes: 100,
		}},
	}
	result := PlanInventory(inventory, PlatformState{ServerPolicy: DefaultPolicy()}, now)
	for _, item := range result.Items {
		if item.Kind == ItemRegistryArtifact {
			t.Fatalf("an artifact with an unknown age was planned for deletion: %+v", item)
		}
	}
}
