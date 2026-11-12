package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestPackBuildArgsPreservePlatformForDefaultAndExplicitBuilder(t *testing.T) {
	payload := PackPayload{
		Image: "eregion/api-production:abc", Builder: "heroku/heroku:24", Registry: packTestRegistryEndpoint,
		Cache: BuildpackCacheVolume("api", "production"),
	}
	const transient = "172.18.0.2:5000"
	reference := transient + "/eregion/api-production:abc"
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		defaultArgs := strings.Join(packBuildArgs(payload, "/src", "eregion-build-test", "default-builder", "default-run-image", reference, platform, transient), " ")
		explicitArgs := strings.Join(packBuildArgs(payload, "/src", "eregion-build-test", payload.Builder, "", reference, platform, transient), " ")
		for name, joined := range map[string]string{"default": defaultArgs, "explicit": explicitArgs} {
			for _, want := range []string{
				"build " + reference + " --publish",
				"--platform " + platform,
				"--insecure-registry " + transient,
				"--network eregion-build-test",
				"--cache type=build;format=volume;name=" + testBuildCacheVolume,
			} {
				if !strings.Contains(joined, want) {
					t.Fatalf("%s args lack %q: %s", name, want, joined)
				}
			}
		}
		if !strings.Contains(defaultArgs, "--run-image default-run-image") {
			t.Fatalf("default builder lost its pinned run image: %s", defaultArgs)
		}
		if strings.Contains(explicitArgs, "--run-image") {
			t.Fatalf("an explicit builder got an injected run image: %s", explicitArgs)
		}
	}
}

func TestPackBuildUsesTheRegistryContainerPortNotTheConfiguredHostPort(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	packLog := filepath.Join(dir, "pack-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
printf '%s\n' "$*" >> "`+packLog+`"
exit 0
`)
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	listener, err := net.Listen("tcp", "127.0.0.1:5500")
	if err != nil {
		t.Fatalf("reserve the non-default registry port: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Docker-Content-Digest", packTestManifestDigest)
		w.WriteHeader(http.StatusOK)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	endpoint := "127.0.0.1:5500"
	canonical := endpoint + "/eregion/api-production@" + packTestManifestDigest
	writePackRegistryFixture(t, dir, endpoint, canonical)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := packStepWithRegistry(t, operationID, stepID, t.TempDir(), endpoint).Apply(context.Background(), engine.Runtime{Root: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	packCalls, err := os.ReadFile(packLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packCalls), "build 172.18.0.2:5000/eregion/api-production:abc --publish") {
		t.Fatalf("pack did not target the container listener on 5000:\n%s", packCalls)
	}
	if strings.Contains(string(packCalls), "172.18.0.2:5500") {
		t.Fatalf("pack used the configured host port on the bridge:\n%s", packCalls)
	}
	dockerCalls, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerCalls), "pull --platform linux/"+runtime.GOARCH+" "+canonical) {
		t.Fatalf("the canonical pull did not keep the configured endpoint 5500:\n%s", dockerCalls)
	}
}

func TestPackBuildRefusesABuilderWhoseLifecycleCannotUseInsecureRegistry(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	packLog := filepath.Join(dir, "pack-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
printf '%s\n' "$*" >> "`+packLog+`"
exit 0
`)
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	if err := os.WriteFile(filepath.Join(dir, "builder-metadata.json"),
		[]byte(`{"lifecycle":{"apis":{"platform":{"deprecated":[],"supported":["0.10","0.11","0.12"]}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	_, err := step.Apply(context.Background(), engine.Runtime{Root: t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "0.13") {
		t.Fatalf("builder with an old Platform API = %v, want a useful 0.13 refusal", err)
	}
	calls, _ := os.ReadFile(dockerLog)
	if strings.Contains(string(calls), "network create") {
		t.Fatalf("an unverifiable builder reached network setup: %s", calls)
	}
	if _, statErr := os.Stat(packLog); !os.IsNotExist(statErr) {
		t.Fatalf("pack ran without insecure-registry support: %v", statErr)
	}
}

func TestPackBuildRefusesAForeignRegistryContainer(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	if err := os.WriteFile(filepath.Join(dir, "registry-inspect"),
		[]byte(packTestRegistryID+"|/eregion-registry|registry:2.8.3|true|{\"5000/tcp\":[{\"HostIp\":\"127.0.0.1\",\"HostPort\":\"5000\"}]}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	_, err := step.Apply(context.Background(), engine.Runtime{Root: t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "pinned image") {
		t.Fatalf("foreign registry identity = %v, want a pinned-image refusal", err)
	}
	if calls, _ := os.ReadFile(dockerLog); strings.Contains(string(calls), "network connect") {
		t.Fatalf("a foreign registry was attached to a build network: %s", calls)
	}
}

func TestPackBuildGracefullyHandlesAnAmbiguousRegistryAttachment(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
exit 0
`)
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	if err := os.WriteFile(filepath.Join(dir, "connect-fails"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", packTestManifestDigest)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	endpoint := server.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(endpoint)
	canonical := endpoint + "/eregion/api-production@" + packTestManifestDigest
	writePackRegistryFixture(t, dir, endpoint, canonical)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepWithRegistry(t, operationID, stepID, t.TempDir(), endpoint)
	if _, err := step.Apply(context.Background(), engine.Runtime{Root: t.TempDir()}, nil); err != nil {
		t.Fatalf("an attached registry reported as a connect error was not recovered: %v", err)
	}
	if calls, _ := os.ReadFile(dockerLog); !strings.Contains(string(calls), "network connect "+buildNetworkName(operationID, stepID, 1)+" "+packTestRegistryID) {
		t.Fatalf("the ambiguous connect was not attempted: %s", calls)
	}
	_ = port
}

func TestPackBuildCheckRejectsAPreExistingLocalTagWithoutDurableCompletion(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	check, err := step.Check(context.Background(), engine.Runtime{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete {
		t.Fatal("a local tag alone satisfied the registry-backed build contract")
	}
}

func TestPackBuildCheckRecoversTheExactCommitAndCanonicalDigest(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	step := packStepForTest(t, operationID, stepID, t.TempDir())
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	completion := packCompletion{
		Schema: packCompletionSchema, OperationID: string(operationID), Step: stepID,
		Attempt: 7, InputsHash: engine.InputsHash(stepID, inputs),
		Image: "eregion/api-production:abc", Registry: packTestRegistryEndpoint,
		Platform: "linux/" + runtime.GOARCH, Reference: packTestCanonical, Digest: packTestManifestDigest,
		ImageID: packTestImageID, Commit: strings.Repeat("a", 40),
	}
	if err := writePackCompletion(root, completion); err != nil {
		t.Fatal(err)
	}
	check, err := step.Check(context.Background(), engine.Runtime{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !check.Complete || check.AppliedAttempt != 7 {
		t.Fatalf("recovered check = %+v, want complete from attempt 7", check)
	}
	fields, ok := check.Result.(map[string]any)
	if !ok || fields["reference"] != packTestCanonical || fields["commit"] != completion.Commit || fields["image_id"] != packTestImageID {
		t.Fatalf("recovered result = %#v", check.Result)
	}
}

func TestPackBuildRemovesAStaleOwnedNetworkBeforeStarting(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	staleID := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	staleName := buildNetworkName(operationID, stepID, 2)
	stale := struct {
		Name       string            `json:"Name"`
		Labels     map[string]string `json:"Labels"`
		Containers map[string]any    `json:"Containers"`
	}{
		Name: staleName, Labels: buildNetworkLabels(operationID, stepID, 2),
		Containers: map[string]any{
			packTestRegistryID: map[string]string{"Name": "eregion-registry", "IPv4Address": "172.18.0.9/16"},
		},
	}
	raw, _ := json.Marshal(stale)
	for path, value := range map[string][]byte{
		filepath.Join(dir, "stale-networks"):       []byte(staleID + "\n"),
		filepath.Join(dir, "stale-network-id"):     []byte(staleID),
		filepath.Join(dir, "stale-network-name"):   []byte(staleName),
		filepath.Join(dir, "stale-network-detail"): raw,
	} {
		if err := os.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := removeStaleBuildNetworks(context.Background(), engine.Runtime{}, nil, packTestRegistryID, stepID, nil); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(dockerLog)
	for _, want := range []string{"network inspect", staleID, "network disconnect " + staleName + " " + packTestRegistryID, "network rm " + staleName} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("stale network cleanup lacks %q:\n%s", want, calls)
		}
	}
}

func TestResolvePublishedManifestDigestFallsBackToTheExactManifestBytes(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"config":{"digest":"sha256:` + strings.Repeat("2", 64) + `"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	digest, err := resolvePublishedManifestDigest(context.Background(), server.Listener.Addr().String(), "app/production", "tag")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if want := "sha256:" + hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("digest = %q, want %q", digest, want)
	}
}

func TestResolvePublishedManifestDigestRejectsAnUnexpectedIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", packTestManifestDigest)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if _, err := resolvePublishedManifestDigest(context.Background(), server.Listener.Addr().String(), "app/production", "tag"); err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("index publish = %v, want a bounded-contract refusal", err)
	}
}

func TestIsImageDigestReferenceRequiresAnImmutableDigest(t *testing.T) {
	if !IsImageDigestReference("127.0.0.1:5000/eregion/api@" + packTestManifestDigest) {
		t.Fatal("a canonical digest reference was refused")
	}
	for _, image := range []string{
		"eregion/api-production:abc",
		"127.0.0.1:5000/eregion/api@sha256:short",
		"127.0.0.1:5000/eregion/api@",
	} {
		if IsImageDigestReference(image) {
			t.Fatalf("mutable or malformed reference %q passed", image)
		}
	}
}

func TestPackPayloadRequiresTheActualConfiguredRegistry(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-pack-02-v1"}
	payload := PackPayload{
		Step: "platform-pack-02-v1", Image: "eregion/app-production:abc",
		Path: t.TempDir(), Builder: "paketobuildpacks/builder-jammy-base",
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("missing registry = %v, want an explicit refusal", err)
	}
	payload.Registry = "registry.example:5000"
	raw, _ = json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback registry = %v, want a loopback refusal", err)
	}
}
