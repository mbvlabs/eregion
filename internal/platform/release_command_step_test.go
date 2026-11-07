package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/containerresolver"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// fakeDocker puts a docker script on PATH that records each invocation and
// behaves per FAKE_DOCKER_RUN: "ok", "hang" or "fail". The image inspect probe
// reports a Buildpacks label value by default.
func fakeDocker(t *testing.T, run string) string {
	t.Helper()
	return fakeDockerImage(t, run, "{}", "")
}

// fakeDockerImage is fakeDocker with an explicit Buildpacks label value. An
// empty value is a plain image. When inspectError is nonempty, image inspect
// prints it to stderr and exits 1.
func fakeDockerImage(t *testing.T, run, imageLabel, inspectError string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1" in
inspect)
  if [ -n "$FAKE_DOCKER_INSPECT_ERROR" ]; then
    printf '%s\n' "$FAKE_DOCKER_INSPECT_ERROR" >&2
    exit 1
  fi
  if [ "$2" = "--type=image" ]; then
    printf '%s' "$FAKE_DOCKER_IMAGE_LABEL"
    exit 0
  fi
  echo "unexpected inspect" >&2
  exit 1;;
rm) exit 0;;
run)
  echo "run-env PATH=$PATH" >> "` + log + `"
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
	t.Setenv("FAKE_DOCKER_IMAGE_LABEL", imageLabel)
	t.Setenv("FAKE_DOCKER_INSPECT_ERROR", inspectError)
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

func dockerRunCall(t *testing.T, log string) string {
	t.Helper()
	for _, call := range calls(t, log) {
		if strings.HasPrefix(call, "run ") {
			return call
		}
	}
	t.Fatalf("no docker run call in %q", log)
	return ""
}

func dockerRunPATH(t *testing.T, log string) string {
	t.Helper()
	for _, call := range calls(t, log) {
		if strings.HasPrefix(call, "run-env PATH=") {
			return strings.TrimPrefix(call, "run-env PATH=")
		}
	}
	return ""
}

func TestReleaseCommandRunsThroughTheBuildpacksLauncher(t *testing.T) {
	source := withTestResolver(t)
	log := fakeDocker(t, "ok")
	if _, err := releaseCommandStepFor(t, 10).Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	// Without the launcher a Buildpacks image starts its default web process
	// with the command appended, which never exits. The launcher sets the
	// buildpack environment and then execs the mounted resolver.
	run := dockerRunCall(t, log)
	for _, want := range []string{
		"--mount type=bind,source=" + source + ",destination=/run/eregion/eregion,readonly=true",
		"--entrypoint /cnb/lifecycle/launcher",
		" -- /run/eregion/eregion __eregion_container_exec demo release",
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("docker %s lacks %s", run, want)
		}
	}
}

func TestReleaseCommandPlainImageBareCommandUsesResolverMount(t *testing.T) {
	source := withTestResolver(t)
	log := fakeDockerImage(t, "ok", "", "")
	step := releaseCommandStepForCommand(t, []string{"migrate", "--yes"}, map[string]string{"APP_VERSION": "v1"}, nil)
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	run := dockerRunCall(t, log)
	for _, want := range []string{
		"-e APP_VERSION ",
		"--mount type=bind,source=" + source + ",destination=/run/eregion/eregion,readonly=true",
		"--entrypoint /run/eregion/eregion ",
		" __eregion_container_exec migrate --yes",
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("docker %s lacks %s", run, want)
		}
	}
	if strings.Contains(run, "/bin/sh") || strings.Contains(run, "sh -c") {
		t.Fatalf("resolver used a shell: docker %s", run)
	}
	if strings.Contains(run, "-e PATH") {
		t.Fatalf("bare command synthesized a PATH flag: docker %s", run)
	}
}

func TestReleaseCommandExplicitPathHasNoResolver(t *testing.T) {
	log := fakeDockerImage(t, "ok", "", "")
	step := releaseCommandStepForCommand(t, []string{"./migrate", "--yes"}, nil, nil)
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	run := dockerRunCall(t, log)
	if strings.Contains(run, "--mount") || strings.Contains(run, containerresolver.Invocation) {
		t.Fatalf("explicit path used the resolver: docker %s", run)
	}
	if !strings.Contains(run, "--entrypoint ./migrate ") {
		t.Fatalf("explicit path was rewritten: docker %s", run)
	}
}

func TestReleaseCommandConfiguredAndSecretEnvArePreserved(t *testing.T) {
	withTestResolver(t)
	log := fakeDockerImage(t, "ok", "", "")
	step := releaseCommandStepForCommand(t, []string{"migrate"}, map[string]string{"PATH": "/configured/bin"}, []string{"PATH"})
	rt := engine.Runtime{SecretValues: map[string]string{"PATH": "/secret/bin"}}
	if _, err := step.Apply(context.Background(), rt, nil); err != nil {
		t.Fatal(err)
	}
	run := dockerRunCall(t, log)
	if strings.Contains(run, "/configured/bin") || strings.Contains(run, "/secret/bin") {
		t.Fatalf("a PATH value entered argv: docker %s", run)
	}
	if got := dockerRunPATH(t, log); got != "/secret/bin" {
		t.Fatalf("container PATH = %q, want the secret PATH unchanged", got)
	}
}

func TestReleaseCommandResolverSourceErrorIsTerminal(t *testing.T) {
	source := filepath.Join(t.TempDir(), "bad,source")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := resolverExecutable
	resolverExecutable = func() (string, error) { return source, nil }
	t.Cleanup(func() { resolverExecutable = previous })
	fakeDockerImage(t, "ok", "", "")
	step := releaseCommandStepForCommand(t, []string{"migrate"}, nil, nil)
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if !errors.Is(err, engine.ErrPermanent) || !strings.Contains(err.Error(), "bind-mounted safely") {
		t.Fatalf("resolver source error = %v, want a permanent mount-path error", err)
	}
}

func TestReleaseCommandImageInspectFailureIsTerminal(t *testing.T) {
	log := fakeDockerImage(t, "ok", "{}", "Error response from daemon: No such image: sha256:"+strings.Repeat("b", 64))
	_, err := releaseCommandStepFor(t, 10).Apply(context.Background(), engine.Runtime{}, nil)
	if !errors.Is(err, engine.ErrPermanent) || !strings.Contains(err.Error(), "inspect image configuration") {
		t.Fatalf("inspect error = %v, want a permanent image configuration failure", err)
	}
	if !strings.Contains(strings.Join(calls(t, log), "\n"), "inspect --type=image") {
		t.Fatal("the release command did not inspect the image")
	}
}

func TestReleaseCommandImageInspectDaemonFailureIsRetryable(t *testing.T) {
	fakeDockerImage(t, "ok", "{}", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
	_, err := releaseCommandStepFor(t, 10).Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil || errors.Is(err, engine.ErrPermanent) {
		t.Fatalf("inspect error = %v, want a retryable daemon failure", err)
	}
}

func TestReleaseCommandMissingSecretPathIsTerminal(t *testing.T) {
	withTestResolver(t)
	fakeDockerImage(t, "ok", "", "")
	step := releaseCommandStepForCommand(t, []string{"migrate"}, nil, []string{"PATH"})
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if !errors.Is(err, engine.ErrPermanent) || !strings.Contains(err.Error(), "secret value for PATH") {
		t.Fatalf("missing secret PATH error = %v, want a permanent secret error", err)
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

func releaseCommandStepForCommand(t *testing.T, command []string, env map[string]string, secretKeys []string) engine.Step {
	t.Helper()
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-command-01-v1"}
	raw, _ := json.Marshal(ReleaseCommandPayload{
		Step: "platform-command-01-v1", Name: "eregion-demo-production-release-abc",
		Image: "sha256:" + strings.Repeat("b", 64), Network: "er-env_demo",
		Command: command, Env: env, SecretEnvKeys: secretKeys, TimeoutSeconds: 10,
	})
	step, err := buildReleaseCommandStep(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	return step
}
