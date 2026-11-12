package buildpacks

import "testing"

func TestTargetPlatformPreservesTheBuildNodeArchitecture(t *testing.T) {
	for _, want := range map[string]string{
		"amd64": "linux/amd64",
		"arm64": "linux/arm64",
	} {
		got, err := TargetPlatform(want[6:])
		if err != nil {
			t.Fatalf("TargetPlatform(%q): %v", want[6:], err)
		}
		if got != want {
			t.Fatalf("TargetPlatform(%q) = %q, want %q", want[6:], got, want)
		}
	}
	if _, err := TargetPlatform("386"); err == nil {
		t.Fatal("an unsupported build architecture was accepted")
	}
}

func TestProfileForArchitectureSelectsTheMatchingPinnedImages(t *testing.T) {
	for _, architecture := range []string{"amd64", "arm64"} {
		profile, err := ProfileForArchitecture(architecture)
		if err != nil {
			t.Fatalf("ProfileForArchitecture(%q): %v", architecture, err)
		}
		if profile.Builder == "" || profile.RunImage == "" {
			t.Fatalf("ProfileForArchitecture(%q) = %+v, want both pinned images", architecture, profile)
		}
	}
	if _, err := ProfileForArchitecture("386"); err == nil {
		t.Fatal("an unsupported architecture got a default builder profile")
	}
}
