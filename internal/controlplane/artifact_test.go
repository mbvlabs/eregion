package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func testManifestBody(config string) []byte {
	return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + config + `"},"layers":[]}`)
}

func testIndexBody(entries ...struct{ Digest, Arch string }) []byte {
	manifests := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		manifests = append(manifests, map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    entry.Digest,
			"platform":  map[string]string{"os": "linux", "architecture": entry.Arch},
		})
	}
	raw, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     manifests,
	})
	return raw
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestArtifactResolverPinsTagToManifestAndConfigDigest(t *testing.T) {
	config := "sha256:" + strings.Repeat("a", 64)
	manifest := testManifestBody(config)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/mbvlabs/andurel/manifests/v1" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(manifest)
	}))
	defer server.Close()
	resolved, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "sha256:" + sha256HexBytes(manifest)
	if resolved.ManifestDigest != want || resolved.ConfigDigest != config {
		t.Fatalf("resolved manifest/config = %s/%s, want %s/%s", resolved.ManifestDigest, resolved.ConfigDigest, want, config)
	}
	if resolved.CanonicalImage != server.Listener.Addr().String()+"/mbvlabs/andurel@"+want {
		t.Fatalf("canonical image = %s", resolved.CanonicalImage)
	}
}

func TestArtifactResolverChoosesTargetArchitectureChildFromAnIndex(t *testing.T) {
	amdConfig := "sha256:" + strings.Repeat("b", 64)
	armConfig := "sha256:" + strings.Repeat("c", 64)
	amdManifest := testManifestBody(amdConfig)
	armManifest := testManifestBody(armConfig)
	index := testIndexBody(
		struct{ Digest, Arch string }{digestOf(amdManifest), "amd64"},
		struct{ Digest, Arch string }{digestOf(armManifest), "arm64"},
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/mbvlabs/andurel/manifests/v1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			_, _ = w.Write(index)
		case "/v2/mbvlabs/andurel/manifests/" + digestOf(armManifest):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(armManifest)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	resolved, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "arm64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ConfigDigest != armConfig || resolved.ManifestDigest != digestOf(armManifest) {
		t.Fatalf("resolved child = %+v", resolved)
	}
	if resolved.ManifestDigest == resolved.ConfigDigest {
		t.Fatal("manifest digest and config digest were treated as the same identity")
	}
}

func TestArtifactResolverRefusesAnIndexWithoutTheTargetArchitecture(t *testing.T) {
	manifest := testManifestBody("sha256:" + strings.Repeat("d", 64))
	index := testIndexBody(struct{ Digest, Arch string }{digestOf(manifest), "amd64"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		_, _ = w.Write(index)
	}))
	defer server.Close()
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "arm64",
	})
	if err == nil || !strings.Contains(err.Error(), "target node architecture") {
		t.Fatalf("missing architecture = %v", err)
	}
}

func TestArtifactResolverDoesNotReuseAMovedTag(t *testing.T) {
	firstConfig := "sha256:" + strings.Repeat("e", 64)
	secondConfig := "sha256:" + strings.Repeat("f", 64)
	first := testManifestBody(firstConfig)
	second := testManifestBody(secondConfig)
	current := first
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(current)
	}))
	defer server.Close()
	request := artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "amd64",
	}
	firstResolved, err := resolveRemoteArtifact(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	current = second
	secondResolved, err := resolveRemoteArtifact(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if firstResolved.ManifestDigest == secondResolved.ManifestDigest {
		t.Fatal("moving a tag did not produce a new immutable manifest digest")
	}
	if firstResolved.CanonicalImage == secondResolved.CanonicalImage {
		t.Fatal("the admitted artifact followed the moved tag")
	}
}

func TestArtifactResolverRefusesPrivateRepositoryMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image", Requested: server.Listener.Addr().String() + "/other/app:v1",
		Repository: "mbvlabs/andurel", Registry: server.Listener.Addr().String(),
		Image: server.Listener.Addr().String() + "/other/app:v1", Arch: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Fatalf("foreign private repository = %v", err)
	}
}

func TestArtifactResolverRefusesAMutableImageWithoutAnExplicitTagOrDigest(t *testing.T) {
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image", Requested: "registry.example.com/mbvlabs/andurel",
		Repository: "mbvlabs/andurel", Image: "registry.example.com/mbvlabs/andurel", Arch: "amd64",
	})
	if err == nil {
		t.Fatalf("mutable image = %v, want a refusal", err)
	}
}

func TestArtifactResolverRefusesAnInvalidConfigDigest(t *testing.T) {
	body := testManifestBody("not-a-digest")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "config digest") {
		t.Fatalf("invalid config digest = %v", err)
	}
}

func TestArtifactResolverUsesBoundedReads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(strings.Repeat("x", artifactManifestBodyLimit+1)))
	}))
	defer server.Close()
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "bounded read") {
		t.Fatalf("oversized manifest = %v", err)
	}
}

func TestArtifactRequestRequiresAnExplicitPrivateRegistryEndpoint(t *testing.T) {
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel", Tag: "v1",
	})
	if err == nil || !strings.Contains(err.Error(), "private registry") {
		t.Fatalf("missing endpoint = %v", err)
	}
}

func TestCanonicalArtifactReferenceIsBounded(t *testing.T) {
	repository := strings.Repeat("a", platform.MaxImageRepositoryLength)
	_, err := platform.CanonicalArtifactReference("127.0.0.1:5000", repository, "sha256:"+strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("an overlong canonical reference was accepted")
	}
}

func TestArtifactSelectorLabelIsSafe(t *testing.T) {
	label := artifactSelectorLabel(&resolvedArtifact{
		Kind: "image-tag", Requested: "v1", CanonicalImage: "127.0.0.1:5000/mbvlabs/andurel@" + strings.Repeat("a", 64),
		ConfigDigest: "sha256:" + strings.Repeat("b", 64),
	})
	if !strings.Contains(label, "v1") || strings.Contains(label, "password") || strings.Contains(label, "\x00") {
		t.Fatalf("selector label = %q", label)
	}
}

func TestParseBearerChallenge(t *testing.T) {
	realm, params, ok := parseBearerChallenge(`Bearer realm="https://registry.example.com/token",service="registry.example.com",scope="repository:acme/api:pull"`)
	if !ok || realm != "https://registry.example.com/token" || params["service"] != "registry.example.com" {
		t.Fatalf("challenge = %q %v %t", realm, params, ok)
	}
	if _, _, ok := parseBearerChallenge("Basic abc"); ok {
		t.Fatal("a Basic challenge was parsed as Bearer")
	}
}

func TestRegistryDestinationPolicyRejectsPrivateAndReservedAddresses(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.1.1",
		"100.64.0.1", "0.0.0.0", "192.0.2.1", "198.18.0.1", "203.0.113.1",
		"224.0.0.1", "::1", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1",
	} {
		if registryDestinationAllowed(net.ParseIP(value), false) {
			t.Fatalf("private or reserved destination %s was allowed", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !registryDestinationAllowed(net.ParseIP(value), false) {
			t.Fatalf("public destination %s was refused", value)
		}
	}
	if registryDestinationAllowed(net.ParseIP("127.0.0.1"), true) != true {
		t.Fatal("the explicitly isolated managed loopback exception was refused")
	}
	if registryDestinationAllowed(net.ParseIP("10.0.0.1"), true) {
		t.Fatal("the managed loopback exception admitted a non-loopback private address")
	}
}

func TestRegistryHTTPClientUsesNoProxyAndRefusesRedirects(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client := newRegistryHTTPClient(false)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("registry HTTP client inherited an environment proxy")
	}
	request, _ := http.NewRequest(http.MethodGet, "https://registry.example.com/v2/", nil)
	if err := client.CheckRedirect(request, nil); !errors.Is(err, errRegistryRedirectRefused) {
		t.Fatalf("redirect policy = %v", err)
	}
}

func TestRegistryTokenRealmRejectsDowngradeCredentialsAndLocalHosts(t *testing.T) {
	for _, raw := range []string{
		"http://registry.example.com/token",
		"https://user:password@registry.example.com/token",
		"https://127.0.0.1/token",
		"https://[::1]/token",
		"https://localhost/token",
		"https://registry.example.com/token#fragment",
	} {
		if _, err := validateRegistryTokenRealm(raw); err == nil {
			t.Fatalf("unsafe token realm was accepted: %s", raw)
		}
	}
	if _, err := validateRegistryTokenRealm("https://auth.registry.example.com/token"); err != nil {
		t.Fatalf("public HTTPS token realm was refused: %v", err)
	}
}

func TestRegistryDialContextRejectsLoopbackForExternalReads(t *testing.T) {
	conn, err := registryDialContext(false)(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil {
		_ = conn.Close()
		t.Fatal("external registry dial reached a loopback address")
	}
	if !strings.Contains(err.Error(), "not an allowed public address") {
		t.Fatalf("loopback dial = %v", err)
	}
}

func TestArtifactResolverTreatsFullReferenceToManagedRegistryAsPrivate(t *testing.T) {
	config := "sha256:" + strings.Repeat("a", 64)
	manifest := testManifestBody(config)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/mbvlabs/andurel/manifests/v1" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(manifest)
	}))
	defer server.Close()
	endpoint := server.Listener.Addr().String()
	resolved, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image", Requested: endpoint + "/mbvlabs/andurel:v1",
		Repository: "mbvlabs/andurel", Registry: endpoint,
		Image: endpoint + "/mbvlabs/andurel:v1", Arch: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.CanonicalImage != endpoint+"/mbvlabs/andurel@"+digestOf(manifest) {
		t.Fatalf("canonical image = %s", resolved.CanonicalImage)
	}
}

func TestArtifactResolverRefusesARedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/private", http.StatusFound)
	}))
	defer server.Close()
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect = %v, want a refusal", err)
	}
}

func TestArtifactResolverRefusesARequestedTopLevelDigestMismatch(t *testing.T) {
	actual := testManifestBody("sha256:" + strings.Repeat("a", 64))
	requested := digestOf([]byte("a different manifest"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(actual)
	}))
	defer server.Close()
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-digest", Requested: requested, Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Digest: requested, Arch: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "different manifest bytes") {
		t.Fatalf("top-level digest mismatch = %v", err)
	}
}

func TestArtifactResolverRefusesAChildDigestMismatch(t *testing.T) {
	child := testManifestBody("sha256:" + strings.Repeat("b", 64))
	descriptorDigest := digestOf([]byte("not the child"))
	index := testIndexBody(struct{ Digest, Arch string }{descriptorDigest, "amd64"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/mbvlabs/andurel/manifests/v1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			_, _ = w.Write(index)
		default:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(child)
		}
	}))
	defer server.Close()
	_, err := resolveRemoteArtifact(context.Background(), artifactRequest{
		Kind: "image-tag", Requested: "v1", Repository: "mbvlabs/andurel",
		Registry: server.Listener.Addr().String(), Tag: "v1", Arch: "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "descriptor digest") {
		t.Fatalf("child digest mismatch = %v", err)
	}
}

func TestManifestIndexSelection(t *testing.T) {
	body := testIndexBody(struct{ Digest, Arch string }{"sha256:" + strings.Repeat("a", 64), "amd64"})
	var manifest distributionManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if got := selectManifestForArch(manifest, "amd64"); got != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("selected digest = %s", got)
	}
	if got := selectManifestForArch(manifest, "arm64"); got != "" {
		t.Fatalf("unexpected arm64 digest = %s", got)
	}
}
