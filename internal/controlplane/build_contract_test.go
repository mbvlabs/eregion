package controlplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestBuiltReleaseIdentityRequiresCanonicalDigestAndConfigID(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	imageID := "sha256:" + strings.Repeat("2", 64)
	raw, _ := json.Marshal(map[string]any{
		"reference": "127.0.0.1:5000/eregion/api@" + digest,
		"image_id":  imageID,
		"commit":    strings.Repeat("a", 40),
	})
	image, gotID, err := builtReleaseIdentity(raw)
	if err != nil || image != "127.0.0.1:5000/eregion/api@"+digest || gotID != imageID {
		t.Fatalf("identity = %q %q %v", image, gotID, err)
	}
	for name, fields := range map[string]map[string]any{
		"mutable tag":      {"reference": "127.0.0.1:5000/eregion/api:tag", "image_id": imageID},
		"no image ID":      {"reference": "127.0.0.1:5000/eregion/api@" + digest},
		"invalid image ID": {"reference": "127.0.0.1:5000/eregion/api@" + digest, "image_id": "sha256:short"},
	} {
		raw, _ := json.Marshal(fields)
		if _, _, err := builtReleaseIdentity(raw); contract.Classify(err).Code != contract.PreconditionFailed {
			t.Fatalf("%s identity = %v, want precondition failure", name, err)
		}
	}
}

func TestRequiresGitBuildOnlyForPendingOrMissingGitArtifacts(t *testing.T) {
	gitPending := platform.Release{Source: platform.Source{Type: "git"}, Status: platform.ReleasePending}
	if !requiresGitBuild(gitPending) {
		t.Fatal("a pending Git release did not require a build")
	}
	imagePending := platform.Release{Source: platform.Source{Type: "image"}, Status: platform.ReleasePending, ImageID: "sha256:" + strings.Repeat("a", 64)}
	if requiresGitBuild(imagePending) {
		t.Fatal("an image artifact was treated as a Git build")
	}
	gitReady := platform.Release{Source: platform.Source{Type: "git"}, Status: platform.ReleaseReady, ImageID: "sha256:" + strings.Repeat("b", 64)}
	if requiresGitBuild(gitReady) {
		t.Fatal("a ready Git release required another build")
	}
}

func TestReadyReleaseImagePayloadRequiresCanonicalReleaseIdentity(t *testing.T) {
	canonical := "127.0.0.1:5000/eregion/api@" + "sha256:" + strings.Repeat("3", 64)
	imageID := "sha256:" + strings.Repeat("4", 64)
	payload, err := readyReleaseImagePayload(platform.Release{
		Source: platform.Source{Type: "git"}, Image: canonical, ImageID: imageID,
	})
	if err != nil || payload.Image != canonical || payload.ImageID != imageID {
		t.Fatalf("canonical ready payload = %+v %v", payload, err)
	}
	if payload, err := readyReleaseImagePayload(platform.Release{
		Source: platform.Source{Type: "image"}, Image: canonical, ImageID: imageID,
	}); err != nil || payload.Image != canonical || payload.ImageID != imageID {
		t.Fatalf("artifact release payload = %+v %v", payload, err)
	}
	if _, err := readyReleaseImagePayload(platform.Release{
		Source: platform.Source{Type: "git"}, Image: "eregion/api-production:abc", ImageID: imageID,
	}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("historical local-only release = %v, want a clear refusal", err)
	}
	if _, err := readyReleaseImagePayload(platform.Release{
		Source: platform.Source{Type: "image"}, Image: "docker.io/library/nginx:latest", ImageID: imageID,
	}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("mutable external image source = %v, want a clear refusal", err)
	}
}
