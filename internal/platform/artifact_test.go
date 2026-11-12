package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeImageRepositoryUsesPrivatePathsOnly(t *testing.T) {
	valid := map[string]string{
		"mbvlabs/andurel": "mbvlabs/andurel",
		"  acme/app  ":    "acme/app",
		"andurel":         "andurel",
		"team_one/app-2":  "team_one/app-2",
	}
	for input, want := range valid {
		got, err := NormalizeImageRepository(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeImageRepository(%q) = %q,%v want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", "ghcr.io/acme/api", "docker.io/library/nginx", "mbvlabs/andurel:v1",
		"mbvlabs/andurel@sha256:" + strings.Repeat("a", 64),
		"https://registry.example/mbvlabs/andurel", "MBVLABS/andurel", "mbvlabs//andurel",
		"mbvlabs/andurel/extra/too/many", "mbvlabs/andurel?x=1", "team:5000/app",
	} {
		if got, err := NormalizeImageRepository(input); err == nil {
			t.Fatalf("NormalizeImageRepository(%q) = %q, want a refusal", input, got)
		}
	}
}

func TestDefaultImageRepositoryIsTheAppSlug(t *testing.T) {
	if got := DefaultImageRepository("andurel"); got != "andurel" {
		t.Fatalf("default repository = %q", got)
	}
}

func TestDeploySelectorsAreStrictAndMutuallyExclusive(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	form := func(fields map[string]any) Normalized {
		t.Helper()
		raw := mustJSON(t, fields)
		normalized, err := NewRegistry().Normalize(KindDeploy, "env/api/production", raw)
		if err != nil {
			t.Fatalf("normalize %v: %v", fields, err)
		}
		return normalized
	}
	form(map[string]any{"image_tag": "v1"})
	form(map[string]any{"image_digest": digest})
	form(map[string]any{"image": "ghcr.io/acme/api:v1"})
	form(map[string]any{"image": "ghcr.io/acme/api@" + digest})
	for _, fields := range []map[string]any{
		{"ref": "main", "image_tag": "v1"},
		{"release_id": "rel_" + strings.Repeat("a", 32), "image_digest": digest},
		{"image_tag": "v1", "image_digest": digest},
		{"image": "ghcr.io/acme/api:v1", "image_tag": "v2"},
	} {
		raw := mustJSON(t, fields)
		if _, err := NewRegistry().Normalize(KindDeploy, "env/api/production", raw); err == nil {
			t.Fatalf("combined selectors %v were accepted", fields)
		}
	}
	for _, fields := range []map[string]any{
		{"image": "ghcr.io/acme/api"},
		{"image": "acme/api:v1"},
		{"image": "ghcr.io/acme/api@" + strings.Repeat("a", 64)},
	} {
		raw := mustJSON(t, fields)
		if _, err := NewRegistry().Normalize(KindDeploy, "env/api/production", raw); err == nil {
			t.Fatalf("mutable or non-qualified image %v was accepted", fields)
		}
	}
}

func TestLegacyReleaseCreateActionIsNotRegistered(t *testing.T) {
	raw := mustJSON(t, map[string]any{"source": map[string]any{"type": "image", "image": "ghcr.io/acme/api:v1"}})
	if _, err := NewRegistry().Normalize(KindReleaseCreate, "env/api/production", raw); err == nil {
		t.Fatal("the legacy platform.release.create action was accepted")
	}
}

func TestEnvironmentSourceRejectsLegacyKindAndImageFields(t *testing.T) {
	for _, raw := range []string{
		`{"name":"production","node":"main","source":{"kind":"git","url":"https://github.com/acme/api"}}`,
		`{"name":"production","node":"main","source":{"kind":"image","image":"ghcr.io/acme/api:v1"}}`,
	} {
		if _, err := NewRegistry().Normalize(KindEnvironmentCreate, "app/api", []byte(raw)); err == nil {
			t.Fatalf("legacy source shape was accepted: %s", raw)
		}
	}
	normalized, err := NewRegistry().Normalize(KindEnvironmentCreate, "app/api", []byte(`{"name":"production","node":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	create := normalized.Input.(*EnvironmentCreateInput)
	if create.Source.URL != "" {
		t.Fatalf("an omitted source became %+v", create.Source)
	}
}

func TestAppCreateDefaultsImageRepositoryAndRejectsInvalidMetadata(t *testing.T) {
	normalized, err := NewRegistry().Normalize(KindAppCreate, "platform", []byte(`{"name":"andurel"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := normalized.Input.(*AppCreateInput).ImageRepository; got != "andurel" {
		t.Fatalf("default repository = %q", got)
	}
	normalized, err = NewRegistry().Normalize(KindAppCreate, "platform", []byte(`{"name":"andurel","image_repository":"mbvlabs/andurel"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := normalized.Input.(*AppCreateInput).ImageRepository; got != "mbvlabs/andurel" {
		t.Fatalf("explicit repository = %q", got)
	}
	if _, err := NewRegistry().Normalize(KindAppCreate, "platform", []byte(`{"name":"andurel","image_repository":"ghcr.io/mbvlabs/andurel"}`)); err == nil {
		t.Fatal("a host-qualified repository was accepted")
	}
}

func TestAppCreateDeployWithoutGitFailsBeforeAnyCommit(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"name":         "andurel",
		"environments": []map[string]any{{"name": "production", "node": "main"}},
		"deploy":       true,
	})
	if _, err := NewRegistry().Normalize(KindAppCreate, "platform", raw); err == nil {
		t.Fatal("a deploy intent without a Git source or artifact was accepted")
	}
	if _, err := NewRegistry().Normalize(KindAppCreate, "platform", []byte(`{"name":"andurel","image_repository":"mbvlabs/andurel"}`)); err != nil {
		t.Fatalf("image-only app creation was refused: %v", err)
	}
}

func TestValidateAppImageRepositoryForRegistryUsesTheActualEndpoint(t *testing.T) {
	repository := "mbvlabs/andurel"
	if err := ValidateAppImageRepositoryForRegistry(repository, "127.0.0.1:5000"); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", MaxImageRepositoryLength)
	if err := ValidateAppImageRepositoryForRegistry(long, "127.0.0.1:5000"); err == nil {
		t.Fatal("an app repository whose canonical reference exceeds the release column was accepted")
	}
	if err := ValidateAppImageRepositoryForRegistry(repository, ""); err == nil {
		t.Fatal("an app repository was accepted without the configured registry endpoint")
	}
}

func TestCanonicalArtifactReferenceBoundary(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	prefix := "127.0.0.1:5000/"
	// The release image column is 255 characters; the repository may use
	// exactly the remaining space.
	repository := strings.Repeat("a", MaxCanonicalArtifactReferenceLength-len(prefix)-len(digest)-1)
	if _, err := CanonicalArtifactReference("127.0.0.1:5000", repository, digest); err != nil {
		t.Fatalf("boundary reference was refused: %v", err)
	}
	if _, err := CanonicalArtifactReference("127.0.0.1:5000", repository+"a", digest); err == nil {
		t.Fatal("over-boundary reference was accepted")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
