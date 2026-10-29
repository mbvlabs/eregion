package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// fakeDocker puts a docker script on PATH that records each invocation and
// behaves per FAKE_DOCKER_RUN: "ok", "hang" or "fail". The image inspect probe
// reports a Buildpacks label.
func fakeDocker(t *testing.T, run string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1" in
inspect) echo '{"buildpacks":[]}'; exit 0;;
rm) exit 0;;
run)
  case "$FAKE_DOCKER_RUN" in
  hang) exec sleep 30;;
  fail) echo "migration failed" >&2; exit 3;;
  esac
  echo "release ran"; exit 0;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_RUN", run)
	return log
}

func releaseCommandStepFor(t *testing.T, timeout int) engine.Step {
	t.Helper()
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-command-01-v1"}
	raw, _ := json.Marshal(ReleaseCommandPayload{
		Step: "platform-command-01-v1", Name: "eregion-demo-production-release-abc",
		Image: "sha256:" + strings.Repeat("b", 64), Network: "er-env_demo",
		Command: []string{"demo", "release"}, Env: map[string]string{"APP_VERSION": "v1"},
		TimeoutSeconds: timeout,
	})
	step, err := buildReleaseCommandStep(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestReleaseCommandRunsThroughTheBuildpacksLauncher(t *testing.T) {
	log := fakeDocker(t, "ok")
	if _, err := releaseCommandStepFor(t, 10).Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	var run string
	for _, call := range calls(t, log) {
		if strings.HasPrefix(call, "run ") {
			run = call
		}
	}
	// Without the launcher a Buildpacks image starts its default web process
	// with the command appended, which never exits.
	want := "run --rm --name eregion-demo-production-release-abc --pull never --network er-env_demo -e APP_VERSION --entrypoint /cnb/lifecycle/launcher sha256:" + strings.Repeat("b", 64) + " -- demo release"
	if run != want {
		t.Fatalf("docker %s\nwant docker %s", run, want)
	}
}

func TestReleaseCommandTimeoutRemovesItsContainerAndIsTerminal(t *testing.T) {
	log := fakeDocker(t, "hang")
	_, err := releaseCommandStepFor(t, 1).Apply(context.Background(), engine.Runtime{}, nil)
	if !errors.Is(err, engine.ErrPermanent) || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("timeout error = %v, want a permanent timeout", err)
	}
	got := calls(t, log)
	if last := got[len(got)-1]; last != "rm -f eregion-demo-production-release-abc" {
		t.Fatalf("last docker call %q, want the container removed; calls %q", last, got)
	}
}

func TestReleaseCommandFailureIsTerminal(t *testing.T) {
	fakeDocker(t, "fail")
	_, err := releaseCommandStepFor(t, 10).Apply(context.Background(), engine.Runtime{}, nil)
	if !errors.Is(err, engine.ErrPermanent) || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("failure error = %v, want a permanent exit status 3", err)
	}
}

func TestReleaseCommandContainerCarriesOwnershipLabels(t *testing.T) {
	log := fakeDocker(t, "ok")
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-command-01-v1"}
	raw, _ := json.Marshal(ReleaseCommandPayload{
		Step: "platform-command-01-v1", Name: "eregion-demo-production-release-abc",
		Image: "sha256:" + strings.Repeat("b", 64), Network: "er-env_demo", Command: []string{"demo", "release"},
		Labels: ReleaseCommandLabels("demo", "production", "rel_x", "dep_y"),
	})
	step, err := buildReleaseCommandStep(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	var run string
	for _, call := range calls(t, log) {
		if strings.HasPrefix(call, "run ") {
			run = call
		}
	}
	// app and environment deletion and status select containers by these.
	for _, label := range []string{"io.eregion.app=demo", "io.eregion.environment=production", "io.eregion.process.kind=release", "io.eregion.deployment=dep_y"} {
		if !strings.Contains(run, "--label "+label) {
			t.Fatalf("docker %s lacks --label %s", run, label)
		}
	}
}

func TestObservationListsAReleaseCommandContainerApart(t *testing.T) {
	env := Environment{App: "demo", Name: "production", Processes: []ProcessSpec{{Name: "web", Kind: "web"}}}
	observation := BuildEnvironmentObservation(env, nil, []ContainerObservation{
		{App: "demo", Environment: "production", Kind: ReleaseCommandKind, Name: "eregion-demo-production-release-abc", State: "running"},
	}, nil, nil)
	if len(observation.OtherContainers) != 1 {
		t.Fatalf("other containers %+v, want the release command container", observation.OtherContainers)
	}
	for _, process := range observation.Processes {
		for _, replica := range process.Replicas {
			if replica.Container == "eregion-demo-production-release-abc" {
				t.Fatalf("process %s claims the release command container", process.Name)
			}
		}
	}
}

func TestReleaseCommandStepIsItsOwnFamily(t *testing.T) {
	if argvStepPattern.MatchString("platform-command-01-v1") {
		t.Fatal("the release command step still matches the generic argv family")
	}
	if !releaseCommandStepPattern.MatchString("platform-command-01-v1") {
		t.Fatal("the release command step does not match its family")
	}
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-command-01-v1"}
	raw, _ := json.Marshal(ReleaseCommandPayload{
		Step: "platform-command-01-v1", Name: "eregion-demo-production-release-abc",
		Image: "sha256:" + strings.Repeat("b", 64), Network: "er-env_demo", Command: []string{"demo", "release"},
	})
	step, err := buildReleaseCommandStep(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := step.(*ReleaseCommandStep).payload.TimeoutSeconds; got != 120 {
		t.Fatalf("default timeout %d, want 120", got)
	}
	raw, _ = json.Marshal(ReleaseCommandPayload{
		Step: "platform-command-01-v1", Name: "eregion-demo-production-release-abc",
		Image: "sha256:" + strings.Repeat("b", 64), Network: "er-env_demo",
	})
	if _, err := buildReleaseCommandStep(exec, raw); err == nil {
		t.Fatal("an empty release command was accepted")
	}
}
