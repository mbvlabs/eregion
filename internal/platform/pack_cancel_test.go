package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func packCancelTestIDs() (contract.OperationID, string) {
	return contract.OperationID("op_" + strings.Repeat("a", 32)), "platform-pack-01-v1"
}

func writePackTestExecutable(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// writeBuildDockerFixture writes the label and container response files used by
// the fake Docker script and returns their paths.
func writeBuildDockerFixture(t *testing.T, dir string, labels map[string]string, containers string) (string, string) {
	t.Helper()
	labelsPath := filepath.Join(dir, "network-labels.json")
	if labels != nil {
		encoded, err := json.Marshal(labels)
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
	return labelsPath, containersPath
}

// writeFakeBuildDocker writes a docker script that logs calls and answers the
// build network lifecycle. createCase is the shell fragment after
// `"network create")` and must include its own exit.
func writeFakeBuildDocker(t *testing.T, dir, log, labelsPath, containersPath, createCase string) {
	t.Helper()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + log + `"
case "$1 $2" in
  "network create") ` + createCase + `
  "network inspect")
    if [ -f "` + labelsPath + `" ]; then cat "` + labelsPath + `"; exit 0; fi
    echo "Error response from daemon: network not found" >&2; exit 1;;
  "ps -aq") cat "` + containersPath + `"; exit 0;;
  "network rm") exit 0;;
  "rm -f") exit 0;;
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
	payload := PackPayload{
		Step: stepID, Image: "eregion/api-production:abc", Builder: "paketobuildpacks/builder-jammy-base",
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

func TestPackBuildUsesAnOwnedNetworkAndRemovesItAfterSuccess(t *testing.T) {
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
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	step := packStepForTest(t, operationID, stepID, t.TempDir())
	result, err := step.Apply(context.Background(), engine.Runtime{Root: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, ok := result.Result.(map[string]any)
	if !ok || fields["image"] != "eregion/api-production:abc" {
		t.Fatalf("build result = %#v", result.Result)
	}

	packCalls, err := os.ReadFile(packLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--network " + network,
		"--cache type=build;format=volume;name=" + BuildpackCacheVolume("api", "production"),
		"--cache type=launch;format=volume;name=" + BuildpackLaunchCacheVolume("api", "production"),
	} {
		if !strings.Contains(string(packCalls), want) {
			t.Fatalf("pack invocation lacks %q: %s", want, packCalls)
		}
	}

	calls := dockerCallLog(t, dockerLog)
	if len(calls) != 4 {
		t.Fatalf("docker calls = %q, want create, inspect, list and remove", calls)
	}
	if !strings.HasPrefix(calls[0], "network create ") || !strings.HasSuffix(calls[0], " "+network) {
		t.Fatalf("network create call = %q", calls[0])
	}
	for key, value := range buildNetworkLabels(operationID, stepID, 1) {
		if !strings.Contains(calls[0], "--label "+key+"="+value) {
			t.Fatalf("network create call %q lacks ownership label %s=%s", calls[0], key, value)
		}
	}
	if calls[1] != "network inspect --format {{json .Labels}} "+network {
		t.Fatalf("network inspect call = %q", calls[1])
	}
	if calls[2] != "ps -aq --filter network="+network+" --filter label=author=pack" {
		t.Fatalf("container list call = %q", calls[2])
	}
	if calls[3] != "network rm "+network {
		t.Fatalf("network remove call = %q", calls[3])
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
	if len(calls) != 5 {
		t.Fatalf("docker calls = %q, want create and four cleanup calls", calls)
	}
	if !strings.HasPrefix(calls[0], "network create ") || !strings.HasSuffix(calls[0], " "+network) {
		t.Fatalf("network create call = %q", calls[0])
	}
	for key, value := range buildNetworkLabels(operationID, stepID, 1) {
		if !strings.Contains(calls[0], "--label "+key+"="+value) {
			t.Fatalf("network create call %q lacks ownership label %s=%s", calls[0], key, value)
		}
	}
	if calls[1] != "network inspect --format {{json .Labels}} "+network {
		t.Fatalf("network inspect call = %q", calls[1])
	}
	if calls[2] != "ps -aq --filter network="+network+" --filter label=author=pack" {
		t.Fatalf("container list call = %q", calls[2])
	}
	if calls[3] != "rm -f pack-build-container" {
		t.Fatalf("container remove call = %q", calls[3])
	}
	if calls[4] != "network rm "+network {
		t.Fatalf("network remove call = %q", calls[4])
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
	writePackTestExecutable(t, dir, "docker", `#!/bin/sh
printf '%s\n' "$*" >> "`+dockerLog+`"
case "$1 $2" in
  "network create") exit 0;;
  "network inspect") echo "permission denied" >&2; exit 1;;
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
	if len(calls) != 4 {
		t.Fatalf("docker calls = %q, want create, inspect, list and remove", calls)
	}
	if !strings.HasPrefix(calls[0], "network create ") {
		t.Fatalf("network create call = %q", calls[0])
	}
	if calls[1] != "network inspect --format {{json .Labels}} "+network {
		t.Fatalf("network inspect call = %q", calls[1])
	}
	if calls[2] != "ps -aq --filter network="+network+" --filter label=author=pack" {
		t.Fatalf("container list call = %q", calls[2])
	}
	if calls[3] != "network rm "+network {
		t.Fatalf("network remove call = %q", calls[3])
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
	if len(calls) != 4 {
		t.Fatalf("docker calls = %q, want create, inspect, list and remove after cancellation", calls)
	}
	if !strings.HasPrefix(calls[0], "network create ") {
		t.Fatalf("network create call = %q", calls[0])
	}
	if calls[1] != "network inspect --format {{json .Labels}} "+network {
		t.Fatalf("network inspect call = %q", calls[1])
	}
	if calls[2] != "ps -aq --filter network="+network+" --filter label=author=pack" {
		t.Fatalf("container list call = %q", calls[2])
	}
	if calls[3] != "network rm "+network {
		t.Fatalf("network remove call = %q", calls[3])
	}
}

func TestPackBuildAmbiguousNetworkCreateDoesNotCleanForeignNetwork(t *testing.T) {
	operationID, stepID := packCancelTestIDs()
	network := buildNetworkName(operationID, stepID, 1)
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
	if len(calls) != 2 {
		t.Fatalf("docker calls = %q, want only create and ownership inspection", calls)
	}
	if !strings.HasPrefix(calls[0], "network create ") {
		t.Fatalf("network create call = %q", calls[0])
	}
	if calls[1] != "network inspect --format {{json .Labels}} "+network {
		t.Fatalf("network inspect call = %q", calls[1])
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

	err := removeBuildNetwork(context.Background(), engine.Runtime{}, nil, buildNetworkName(operationID, stepID, 1), operationID, stepID, 1)
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

	if err := removeBuildNetwork(context.Background(), engine.Runtime{}, nil, buildNetworkName(operationID, stepID, 1), operationID, stepID, 1); err != nil {
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
