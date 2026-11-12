package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func packCancelTestIDs() (contract.OperationID, string) {
	return contract.OperationID("op_" + strings.Repeat("a", 32)), "platform-pack-02-v1"
}

const (
	packTestRegistryEndpoint = "127.0.0.1:5000"
	packTestRegistryID       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	packTestImageID          = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	packTestManifestDigest   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	packTestCanonical        = packTestRegistryEndpoint + "/eregion/api-production@" + packTestManifestDigest
)

func writePackTestExecutable(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// writeBuildDockerFixture writes the response files used by the fake Docker
// script. The network detail always carries the validated registry container
// and one pack lifecycle container so cleanup has concrete resources.
func writeBuildDockerFixture(t *testing.T, dir string, labels map[string]string, containers string) (string, string) {
	t.Helper()
	labelsPath := filepath.Join(dir, "network-labels.json")
	if labels != nil {
		detail := struct {
			Name       string            `json:"Name"`
			Labels     map[string]string `json:"Labels"`
			Containers map[string]any    `json:"Containers"`
		}{
			Name:   "eregion-build-test",
			Labels: labels,
			Containers: map[string]any{
				packTestRegistryID: map[string]string{"Name": "eregion-registry", "IPv4Address": "172.18.0.2/16"},
			},
		}
		encoded, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(labelsPath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	containersPath := filepath.Join(dir, "network-containers")
	if err := os.WriteFile(containersPath, []byte(containers), 0o600); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(dir, "registry-inspect")
	registry := packTestRegistryID + "|/eregion-registry|" + RegistryImage + "|true|{\"5000/tcp\":[{\"HostIp\":\"127.0.0.1\",\"HostPort\":\"5000\"}]}"
	if err := os.WriteFile(registryPath, []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	builderPath := filepath.Join(dir, "builder-metadata.json")
	builder := `{"lifecycle":{"apis":{"platform":{"deprecated":["0.12"],"supported":["0.13","0.14","0.15"]}}}}`
	if err := os.WriteFile(builderPath, []byte(builder), 0o600); err != nil {
		t.Fatal(err)
	}
	imagesPath := filepath.Join(dir, "image-inspect")
	images := packTestImageID + `|["` + packTestCanonical + `"]`
	if err := os.WriteFile(imagesPath, []byte(images), 0o600); err != nil {
		t.Fatal(err)
	}
	pulledPath := filepath.Join(dir, "pulled-image-inspect")
	pulled := packTestImageID + `|linux|` + runtime.GOARCH + `|["` + packTestCanonical + `"]`
	if err := os.WriteFile(pulledPath, []byte(pulled), 0o600); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(dir, "stale-networks")
	if err := os.WriteFile(stalePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return labelsPath, containersPath
}

// writeFakeBuildDocker writes a docker script that logs calls and answers the
// build, registry and completion lifecycle. createCase is the shell fragment
// after `"network create")` and must include its own exit.
func writeFakeBuildDocker(t *testing.T, dir, log, labelsPath, containersPath, createCase string) {
	t.Helper()
	registryPath := filepath.Join(dir, "registry-inspect")
	builderPath := filepath.Join(dir, "builder-metadata.json")
	imagesPath := filepath.Join(dir, "image-inspect")
	pulledPath := filepath.Join(dir, "pulled-image-inspect")
	stalePath := filepath.Join(dir, "stale-networks")
	staleIDPath := filepath.Join(dir, "stale-network-id")
	staleDetailPath := filepath.Join(dir, "stale-network-detail")
	connectFailsPath := filepath.Join(dir, "connect-fails")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + log + `"
case "$1 $2" in
  "network create") ` + createCase + `
  "network inspect")
    if [ -f "` + staleIDPath + `" ] && [ -f "` + staleDetailPath + `" ] && { [ "$5" = "$(cat "` + staleIDPath + `")" ] || [ "$5" = "$(cat "` + filepath.Join(dir, "stale-network-name") + `")" ]; }; then cat "` + staleDetailPath + `"; exit 0; fi
    if [ -f "` + labelsPath + `" ]; then cat "` + labelsPath + `"; exit 0; fi
    echo "Error response from daemon: network not found" >&2; exit 1;;
  "network ls") cat "` + stalePath + `"; exit 0;;
  "network connect")
    if [ -f "` + connectFailsPath + `" ]; then echo "endpoint already exists" >&2; exit 1; fi
    exit 0;;
  "network disconnect"|"network rm") exit 0;;
  "ps -aq") cat "` + containersPath + `"; exit 0;;
  "rm -f"|"tag"|"pull") exit 0;;
  "image inspect")
    case "$*" in
      *io.buildpacks.builder.metadata*) cat "` + builderPath + `"; exit 0;;
      *@sha256:*) cat "` + pulledPath + `"; exit 0;;
    esac
    cat "` + imagesPath + `"; exit 0;;
  inspect*) cat "` + registryPath + `"; exit 0;;
esac
exit 0
`
	writePackTestExecutable(t, dir, "docker", script)
}

// fakeBuildDocker answers the Docker calls the build network lifecycle makes.
// A nil labels map makes the inspect call report a missing network. The
// containers string is returned verbatim by `docker ps -aq`.
func fakeBuildDocker(t *testing.T, dir, log string, labels map[string]string, containers string) {
	t.Helper()
	labelsPath, containersPath := writeBuildDockerFixture(t, dir, labels, containers)
	writeFakeBuildDocker(t, dir, log, labelsPath, containersPath, `echo "network-id"; exit 0;;`)
}

// fakeBuildDockerWithCreateError simulates a docker network create whose
// command fails after the daemon may already have created the network. The
// inspect call still answers with labels.
func fakeBuildDockerWithCreateError(t *testing.T, dir, log string, labels map[string]string, containers, createError string) {
	t.Helper()
	labelsPath, containersPath := writeBuildDockerFixture(t, dir, labels, containers)
	errorPath := filepath.Join(dir, "network-create-error")
	if err := os.WriteFile(errorPath, []byte(createError), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFakeBuildDocker(t, dir, log, labelsPath, containersPath, `cat "`+errorPath+`" >&2; exit 1;;`)
}

// fakeBuildDockerWithHangingCreate simulates a docker network create that does
// not return until the build context is cancelled and the process is killed.
func fakeBuildDockerWithHangingCreate(t *testing.T, dir, log string, labels map[string]string, containers string) {
	t.Helper()
	labelsPath, containersPath := writeBuildDockerFixture(t, dir, labels, containers)
	writeFakeBuildDocker(t, dir, log, labelsPath, containersPath, `exec sleep 10;;`)
}

func dockerCallLog(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func waitForFileContains(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", want, path)
}

func packStepForTest(t *testing.T, operationID contract.OperationID, stepID, source string) engine.Step {
	t.Helper()
	return packStepWithRegistry(t, operationID, stepID, source, packTestRegistryEndpoint)
}

func packStepWithRegistry(t *testing.T, operationID contract.OperationID, stepID, source, registry string) engine.Step {
	t.Helper()
	payload := PackPayload{
		Step: stepID, Image: "eregion/api-production:abc", Builder: "paketobuildpacks/builder-jammy-base",
		Registry:    registry,
		Path:        source,
		Cache:       BuildpackCacheVolume("api", "production"),
		LaunchCache: BuildpackLaunchCacheVolume("api", "production"),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	step, err := buildPackStep(engine.Execution{OperationID: operationID, Attempt: 1, Step: stepID}, raw)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

// writePackRegistryFixture rewrites the fake registry and image responses for
// one test's actual loopback server port and canonical digest.
func writePackRegistryFixture(t *testing.T, dir, endpoint, canonical string) {
	t.Helper()
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	registry := packTestRegistryID + "|/eregion-registry|" + RegistryImage + `|true|{"5000/tcp":[{"HostIp":"127.0.0.1","HostPort":"` + port + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "registry-inspect"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	images := packTestImageID + `|["` + canonical + `"]`
	if err := os.WriteFile(filepath.Join(dir, "image-inspect"), []byte(images), 0o600); err != nil {
		t.Fatal(err)
	}
	pulled := packTestImageID + `|linux|` + runtime.GOARCH + `|["` + canonical + `"]`
	if err := os.WriteFile(filepath.Join(dir, "pulled-image-inspect"), []byte(pulled), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildNetworkNameIsScopedToTheAttempt(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	first := buildNetworkName(operationID, stepID, 1)
	if first != buildNetworkName(operationID, stepID, 1) {
		t.Fatal("the same build attempt must use one network name")
	}
	if first == buildNetworkName(operationID, stepID, 2) {
		t.Fatal("a retry must not reuse an earlier attempt's network")
	}
	if !strings.HasPrefix(first, "eregion-build-") {
		t.Fatalf("network name %q is not in the eregion build namespace", first)
	}
}

func TestPackBuildPublishesThroughTheOwnedBridgeAndTagsLocally(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	network := buildNetworkName(operationID, stepID, 1)
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	packLog := filepath.Join(dir, "pack-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
printf '%s\n' "$*" >> "`+packLog+`"
exit 0
`)
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Docker-Content-Digest", packTestManifestDigest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}))
	defer server.Close()
	endpoint := server.Listener.Addr().String()
	transient := net.JoinHostPort("172.18.0.2", strconv.Itoa(RegistryContainerPort))
	canonical := endpoint + "/eregion/api-production@" + packTestManifestDigest
	writePackRegistryFixture(t, dir, endpoint, canonical)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepWithRegistry(t, operationID, stepID, t.TempDir(), endpoint)
	root := t.TempDir()
	result, err := step.Apply(context.Background(), engine.Runtime{Root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, ok := result.Result.(map[string]any)
	if !ok || fields["image"] != "eregion/api-production:abc" || fields["image_id"] != packTestImageID ||
		fields["reference"] != canonical || fields["digest"] != packTestManifestDigest {
		t.Fatalf("build result = %#v", result.Result)
	}
	packCalls, err := os.ReadFile(packLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"build " + transient + "/eregion/api-production:abc --publish",
		"--platform linux/" + runtime.GOARCH,
		"--insecure-registry " + transient,
		"--network " + network,
		"--cache type=build;format=volume;name=" + BuildpackCacheVolume("api", "production"),
		"--cache type=launch;format=volume;name=" + BuildpackLaunchCacheVolume("api", "production"),
	} {
		if !strings.Contains(string(packCalls), want) {
			t.Fatalf("pack invocation lacks %q: %s", want, packCalls)
		}
	}
	calls := dockerCallLog(t, dockerLog)
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"network create ",
		"network connect " + network + " " + packTestRegistryID,
		"network disconnect " + network + " " + packTestRegistryID,
		"network rm " + network,
		"pull --platform linux/" + runtime.GOARCH + " " + canonical,
		"tag " + canonical + " eregion/api-production:abc",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker calls lack %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "network inspect --format {{json .}} "+network) {
		t.Fatalf("docker calls did not inspect the owned network detail:\n%s", joined)
	}
	if strings.Contains(joined, "stop eregion-registry") || strings.Contains(joined, "rm -f "+packTestRegistryID) {
		t.Fatalf("network cleanup touched the shared registry container:\n%s", joined)
	}
	if _, err := os.Stat(packCompletionPath(root, operationID, stepID)); err != nil {
		t.Fatalf("durable build completion was not written: %v", err)
	}
}

func TestPackBuildCancellationRemovesOnlyItsOwnNetworkContainers(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	network := buildNetworkName(operationID, stepID, 1)
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	packLog := filepath.Join(dir, "pack-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
printf '%s\n' "$*" >> "`+packLog+`"
exec sleep 10
`)
	fakeBuildDocker(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "pack-build-container\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := step.Apply(ctx, engine.Runtime{Root: t.TempDir()}, nil)
		done <- err
	}()
	waitForFileContains(t, packLog, "--network "+network)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, engine.ErrInterrupted) {
			t.Fatalf("cancelled build error = %v, want ErrInterrupted", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled build did not return")
	}

	calls := dockerCallLog(t, dockerLog)
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"network create ",
		"network connect " + network + " " + packTestRegistryID,
		"ps -aq --filter network=" + network + " --filter label=author=pack",
		"rm -f pack-build-container",
		"network disconnect " + network + " " + packTestRegistryID,
		"network rm " + network,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cancelled build docker calls lack %q:\n%s", want, joined)
		}
	}
	for key, value := range buildNetworkLabels(operationID, stepID, 1) {
		if !strings.Contains(joined, "--label "+key+"="+value) {
			t.Fatalf("network create lacks ownership label %s=%s:\n%s", key, value, joined)
		}
	}
	if strings.Contains(joined, "stop eregion-registry") || strings.Contains(joined, "rm -f "+packTestRegistryID) {
		t.Fatalf("cancelled build cleanup touched the shared registry:\n%s", joined)
	}
}

func TestPackBuildCancellationKeepsErrInterruptedWhenCleanupFails(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	network := buildNetworkName(operationID, stepID, 1)
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	packLog := filepath.Join(dir, "pack-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
printf '%s\n' "$*" >> "`+packLog+`"
exec sleep 10
`)
	labelsPath, containersPath := writeBuildDockerFixture(t, dir, buildNetworkLabels(operationID, stepID, 1), "pack-build-container\n")
	builderPath := filepath.Join(dir, "builder-metadata.json")
	registryPath := filepath.Join(dir, "registry-inspect")
	imagesPath := filepath.Join(dir, "image-inspect")
	stalePath := filepath.Join(dir, "stale-networks")
	inspectCount := filepath.Join(dir, "inspect-count")
	writePackTestExecutable(t, dir, "docker", `#!/bin/sh
printf '%s\n' "$*" >> "`+dockerLog+`"
case "$1 $2" in
  "network create") exit 0;;
  "network ls") cat "`+stalePath+`"; exit 0;;
  "network connect") exit 0;;
  "network inspect")
    count=0
    if [ -f "`+inspectCount+`" ]; then count=$(cat "`+inspectCount+`"); fi
    count=$((count+1)); printf '%s' "$count" > "`+inspectCount+`"
    if [ "$count" -ge 2 ]; then echo "permission denied" >&2; exit 1; fi
    cat "`+labelsPath+`"; exit 0;;
  "ps -aq") cat "`+containersPath+`"; exit 0;;
  "image inspect")
    case "$*" in *io.buildpacks.builder.metadata*) cat "`+builderPath+`"; exit 0;; esac
    cat "`+imagesPath+`"; exit 0;;
  inspect*) cat "`+registryPath+`"; exit 0;;
esac
exit 0
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	var warnings []string
	report := func(event engine.Event) {
		if event.Kind == "warning" {
			warnings = append(warnings, event.Message)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := step.Apply(ctx, engine.Runtime{Root: t.TempDir()}, report)
		done <- err
	}()
	waitForFileContains(t, packLog, "--network "+network)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, engine.ErrInterrupted) {
			t.Fatalf("cancelled build error = %v, want ErrInterrupted", err)
		}
		if !strings.Contains(err.Error(), "cleaning the build network failed") {
			t.Fatalf("cancelled build error = %v, want the cleanup failure named", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled build did not return")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "inspecting build network") {
		t.Fatalf("warning events = %q, want one cleanup warning", warnings)
	}
}

func TestPackBuildAmbiguousNetworkCreateCleansOwnedNetwork(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	network := buildNetworkName(operationID, stepID, 1)
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	packLog := filepath.Join(dir, "pack-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
printf '%s\n' "$*" >> "`+packLog+`"
exit 0
`)
	fakeBuildDockerWithCreateError(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "", "Error response from daemon: connection reset by peer")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	_, err := step.Apply(context.Background(), engine.Runtime{Root: t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("ambiguous create error = %v, want the create failure", err)
	}
	if _, statErr := os.Stat(packLog); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pack ran after a failed network create: %v", statErr)
	}
	calls := dockerCallLog(t, dockerLog)
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"network create ",
		"network inspect --format {{json .}} " + network,
		"ps -aq --filter network=" + network + " --filter label=author=pack",
		"network disconnect " + network + " " + packTestRegistryID,
		"network rm " + network,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ambiguous create docker calls lack %q:\n%s", want, joined)
		}
	}
}

func TestPackBuildCancelledNetworkCreateCleansOwnedNetwork(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	network := buildNetworkName(operationID, stepID, 1)
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	packLog := filepath.Join(dir, "pack-calls")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
printf '%s\n' "$*" >> "`+packLog+`"
exit 0
`)
	fakeBuildDockerWithHangingCreate(t, dir, dockerLog, buildNetworkLabels(operationID, stepID, 1), "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := step.Apply(ctx, engine.Runtime{Root: t.TempDir()}, nil)
		done <- err
	}()
	waitForFileContains(t, dockerLog, "network create")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, engine.ErrInterrupted) {
			t.Fatalf("cancelled create error = %v, want ErrInterrupted", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled network create did not return")
	}
	if _, statErr := os.Stat(packLog); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pack ran after a cancelled network create: %v", statErr)
	}
	calls := dockerCallLog(t, dockerLog)
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"network create ",
		"network inspect --format {{json .}} " + network,
		"ps -aq --filter network=" + network + " --filter label=author=pack",
		"network disconnect " + network + " " + packTestRegistryID,
		"network rm " + network,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cancelled create docker calls lack %q:\n%s", want, joined)
		}
	}
}

func TestPackBuildAmbiguousNetworkCreateDoesNotCleanForeignNetwork(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	foreign := buildNetworkLabels(operationID, stepID, 1)
	foreign[buildNetworkOwnerLabel] = "another-build"
	fakeBuildDockerWithCreateError(t, dir, dockerLog, foreign, "serving-container\n", "Error response from daemon: connection reset by peer")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	var warnings []string
	report := func(event engine.Event) {
		if event.Kind == "warning" {
			warnings = append(warnings, event.Message)
		}
	}
	_, err := step.Apply(context.Background(), engine.Runtime{Root: t.TempDir()}, report)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("ambiguous create error = %v, want the create failure", err)
	}
	if !strings.Contains(err.Error(), "refusing to clean") {
		t.Fatalf("ambiguous create error = %v, want the foreign network refusal", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "refusing to clean") {
		t.Fatalf("warning events = %q, want the foreign network refusal", warnings)
	}
	calls := dockerCallLog(t, dockerLog)
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "network create ") || !strings.Contains(joined, "network inspect --format {{json .}} ") {
		t.Fatalf("docker calls = %q, want create and ownership inspection", calls)
	}
	if strings.Contains(joined, "ps -aq") || strings.Contains(joined, "network rm ") || strings.Contains(joined, "network disconnect ") {
		t.Fatalf("a foreign network was partly cleaned: %q", calls)
	}
}

func TestPackBuildNetworkCleanupRefusesAForeignNetwork(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	labels := buildNetworkLabels(operationID, stepID, 1)
	labels[buildNetworkOwnerLabel] = "another-build"
	fakeBuildDocker(t, dir, dockerLog, labels, "serving-container\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := removeBuildNetwork(context.Background(), engine.Runtime{}, nil, buildNetworkName(operationID, stepID, 1), operationID, stepID, 1, "")
	if err == nil || !strings.Contains(err.Error(), "refusing to clean") {
		t.Fatalf("foreign network cleanup error = %v, want a refusal", err)
	}
	calls := dockerCallLog(t, dockerLog)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "network inspect ") {
		t.Fatalf("docker calls = %q, want only the ownership inspection", calls)
	}
}

func TestPackBuildNetworkCleanupTreatsAMissingNetworkAsDone(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-calls")
	fakeBuildDocker(t, dir, dockerLog, nil, "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := removeBuildNetwork(context.Background(), engine.Runtime{}, nil, buildNetworkName(operationID, stepID, 1), operationID, stepID, 1, ""); err != nil {
		t.Fatalf("missing network cleanup failed: %v", err)
	}
	calls := dockerCallLog(t, dockerLog)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "network inspect ") {
		t.Fatalf("docker calls = %q, want only the ownership inspection", calls)
	}
}

func TestPackBuildCancellationSendsTermBeforeKill(t *testing.T) {
	dir := t.TempDir()
	packLog := filepath.Join(dir, "pack-calls")
	termMarker := filepath.Join(dir, "pack-term")
	writePackTestExecutable(t, dir, "pack", `#!/bin/sh
trap 'echo term > "`+termMarker+`"; exit 143' TERM
printf '%s\n' "$*" >> "`+packLog+`"
i=0
while [ "$i" -lt 10 ]; do sleep 1; i=$((i+1)); done
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runArgvCleanPack(ctx, []string{"pack", "build", "image", "--builder", "builder"}, nil, engine.Runtime{}, "platform-pack-01-v1", nil)
		done <- err
	}()
	waitForFileContains(t, packLog, "build image")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled pack error = %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled pack did not return")
	}
	if _, err := os.Stat(termMarker); err != nil {
		t.Fatalf("pack did not receive SIGTERM before being killed: %v", err)
	}
}
