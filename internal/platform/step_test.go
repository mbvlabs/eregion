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

// returned separately by the fake Docker inspect format call.
func TestContainerStepAcceptsBareAndPrefixedLabels(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-container-01-v1"}
	payload := ContainerPayload{
		Step: "platform-container-01-v1", Name: "eregion-test-candidate", Image: "sha256:" + strings.Repeat("b", 64),
		ContainerPort: 8080, HostPort: 30000, Bind: "127.0.0.1",
		Labels: map[string]string{
			"io.eregion.app": "api", "io.eregion.environment": "production",
			"io.eregion.release": "rel_" + strings.Repeat("c", 32), "io.eregion.role": "candidate",
			"com.example/team": "platform",
		},
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err != nil {
		t.Fatalf("valid labels were refused: %v", err)
	}
	payload.Labels = map[string]string{"bad label": "x"}
	raw, _ = json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err == nil {
		t.Fatal("a label with whitespace was accepted")
	}
	payload.Labels = map[string]string{"bad_prefix/name": "x"}
	raw, _ = json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err == nil {
		t.Fatal("an invalid DNS prefix was accepted")
	}
}

func TestProcessCommandArgsUseTheLauncherOnlyForBuildpacksImages(t *testing.T) {
	for _, tc := range []struct {
		command    []string
		buildpacks bool
		want       string
	}{
		{nil, false, "img"},
		{[]string{"web"}, true, "--entrypoint /cnb/lifecycle/launcher img -- web"},
		{[]string{"/app/bin/queue", "--workers", "4"}, false, "--entrypoint /app/bin/queue img --workers 4"},
	} {
		if got := strings.Join(processCommandArgs("img", tc.command, tc.buildpacks), " "); got != tc.want {
			t.Errorf("processCommandArgs(%q, %v) = %q, want %q", tc.command, tc.buildpacks, got, tc.want)
		}
	}
}

func TestPackPayloadAcceptsGitContextPathOnlyWithAGitSource(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-pack-01-v1"}
	payload := PackPayload{
		Step: "platform-pack-01-v1", Image: "eregion/app-production:abc", Builder: DefaultBuilder,
		GitURL: "https://github.com/acme/app.git", GitRef: "main", GitContextPath: "apps/web",
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err != nil {
		t.Fatalf("a git context path was refused: %v", err)
	}
	payload.GitURL = ""
	payload.Path = t.TempDir()
	raw, _ = json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil || !strings.Contains(err.Error(), "requires a git source") {
		t.Fatalf("a directory source accepted a git context path: %v", err)
	}
	payload.GitURL = "https://github.com/acme/app.git"
	payload.Path = ""
	payload.GitContextPath = "../outside"
	raw, _ = json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil {
		t.Fatal("a context path that leaves the repository was accepted")
	}
}

func TestResolveGitContextPathStaysInsideTheCheckout(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "apps", "web"), 0o700); err != nil {
		t.Fatal(err)
	}
	realCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "outside"), filepath.Join(checkout, "apps", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("web", filepath.Join(checkout, "apps", "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "apps", "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		contextPath string
		want        string
		wantErr     string
	}{
		{contextPath: "", want: realCheckout},
		{contextPath: ".", want: realCheckout},
		{contextPath: "apps/web", want: filepath.Join(realCheckout, "apps", "web")},
		{contextPath: "apps/inside", want: filepath.Join(realCheckout, "apps", "web")},
		{contextPath: "apps/missing", wantErr: "not present"},
		{contextPath: "apps/file", wantErr: "not a directory"},
		{contextPath: "apps/escape", wantErr: "outside the repository"},
		{contextPath: "../outside", wantErr: "outside the repository"},
	} {
		got, err := resolveGitContextPath(checkout, tc.contextPath)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), tc.contextPath) {
				t.Fatalf("context %q: got %q, %v; want an error naming the path and containing %q", tc.contextPath, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("context %q: %v", tc.contextPath, err)
		}
		if got != tc.want {
			t.Fatalf("context %q resolved to %q, want %q", tc.contextPath, got, tc.want)
		}
	}
}

func TestClassifyImageInspectError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stderr    string
		permanent bool
	}{
		{"missing image", "Error response from daemon: No such image: sha256:abc", true},
		{"image not found", "Error: image not found", true},
		{"template failure", `template: image:1: function "unsupported" not defined`, true},
		{"permission denied", "permission denied while trying to connect to the Docker daemon socket", true},
		{"invalid reference", "invalid reference format", true},
		{"daemon unavailable", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", false},
		{"generic failure", "temporary read failure", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyImageInspectError(context.Background(), errors.New("inspect failed"), tc.stderr)
			if got := errors.Is(err, engine.ErrPermanent); got != tc.permanent {
				t.Fatalf("classifyImageInspectError() permanent = %v, want %v; error %v", got, tc.permanent, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := classifyImageInspectError(ctx, errors.New("inspect failed"), "No such image"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled inspect error = %v, want context cancellation", err)
	}
}

func withTestResolver(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "eregion")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := resolverExecutable
	resolverExecutable = func() (string, error) { return source, nil }
	t.Cleanup(func() { resolverExecutable = previous })
	return source
}

func containerStepFor(t *testing.T, command []string, env map[string]string, secretKeys []string) engine.Step {
	t.Helper()
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-container-01-v1"}
	raw, _ := json.Marshal(ContainerPayload{
		Step: "platform-container-01-v1", Name: "eregion-test-candidate",
		Image: "sha256:" + strings.Repeat("b", 64), Command: command, Env: env, SecretEnvKeys: secretKeys, Restart: "no",
	})
	step, err := buildContainerStep(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	return step
}
func TestContainerStepInspectFailureDoesNotRemoveExistingContainer(t *testing.T) {
	log := fakeDockerImage(t, "ok", "", "Error response from daemon: No such image: sha256:"+strings.Repeat("b", 64))
	step := containerStepFor(t, []string{"migrate"}, nil, nil)
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if !errors.Is(err, engine.ErrPermanent) {
		t.Fatalf("inspect error = %v, want a permanent image failure", err)
	}
	for _, call := range calls(t, log) {
		if strings.HasPrefix(call, "rm ") {
			t.Fatalf("an inspect failure removed the existing container: %q", call)
		}
	}
}

func TestContainerStepWithoutCommandKeepsImageEntrypoint(t *testing.T) {
	log := fakeDockerImage(t, "ok", "", "")
	step := containerStepFor(t, nil, nil, nil)
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls(t, log) {
		if strings.HasPrefix(call, "inspect ") {
			t.Fatalf("a commandless container inspected its image: %q", call)
		}
	}
	run := dockerRunCall(t, log)
	if strings.Contains(run, "--entrypoint") || strings.Contains(run, "--mount") || strings.Contains(run, "PATH") {
		t.Fatalf("a commandless container changed the image entrypoint or PATH: docker %s", run)
	}
}

func TestContainerStepBareCommandUsesResolverMount(t *testing.T) {
	source := withTestResolver(t)
	log := fakeDockerImage(t, "ok", "", "")
	step := containerStepFor(t, []string{"migrate", "--yes"}, map[string]string{"APP_VERSION": "v1"}, nil)
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

func TestContainerStepBuildpacksBareCommandUsesLauncherResolver(t *testing.T) {
	source := withTestResolver(t)
	log := fakeDocker(t, "ok")
	step := containerStepFor(t, []string{"migrate"}, nil, nil)
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	run := dockerRunCall(t, log)
	for _, want := range []string{
		"--mount type=bind,source=" + source + ",destination=/run/eregion/eregion,readonly=true",
		"--entrypoint /cnb/lifecycle/launcher",
		" -- /run/eregion/eregion __eregion_container_exec migrate",
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("docker %s lacks %s", run, want)
		}
	}
}

func TestContainerStepExplicitPathHasNoResolver(t *testing.T) {
	log := fakeDockerImage(t, "ok", "", "")
	step := containerStepFor(t, []string{"./migrate"}, nil, nil)
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

func TestContainerStepConfiguredAndSecretEnvArePreserved(t *testing.T) {
	withTestResolver(t)
	log := fakeDockerImage(t, "ok", "", "")
	step := containerStepFor(t, []string{"migrate"}, map[string]string{"PATH": "/configured/bin"}, []string{"PATH"})
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

func TestContainerAlreadyGone(t *testing.T) {
	for _, output := range []string{
		"Error response from daemon: No such container: eregion-demo-production-web-1",
		"error: no such object: eregion-demo-production-web-1",
	} {
		if !containerAlreadyGone(output) {
			t.Errorf("missing container output was not classified: %q", output)
		}
	}
	for _, output := range []string{
		"Error response from daemon: container is in use",
		"permission denied while trying to connect to the Docker daemon socket",
		"",
	} {
		if containerAlreadyGone(output) {
			t.Errorf("real docker rm failure was classified as missing: %q", output)
		}
	}
}

func TestValidateResolverMountOption(t *testing.T) {
	source := withTestResolver(t)
	valid := "type=bind,source=" + source + ",destination=" + containerresolver.TargetPath + ",readonly=true"
	if err := validateResolverMountOption(valid); err != nil {
		t.Fatalf("valid resolver mount refused: %v", err)
	}
	for _, value := range []string{
		"type=volume,source=" + source + ",destination=" + containerresolver.TargetPath + ",readonly=true",
		"type=bind,source=relative,destination=" + containerresolver.TargetPath + ",readonly=true",
		"type=bind,source=" + source + ",destination=/tmp/other,readonly=true",
		"type=bind,source=" + source + ",destination=" + containerresolver.TargetPath + ",readonly=false",
		"type=bind,source=" + source + ",destination=" + containerresolver.TargetPath,
		"type=bind,source=" + source + ",destination=" + containerresolver.TargetPath + ",readonly=true,extra=1",
		"type=bind,source=/usr/local/bin/other,destination=" + containerresolver.TargetPath + ",readonly=true",
	} {
		if err := validateResolverMountOption(value); err == nil {
			t.Errorf("unsafe resolver mount accepted: %q", value)
		}
	}
}

func TestContainerResolverSourceRejectsUnsafeBinary(t *testing.T) {
	root := t.TempDir()
	nonExecutable := filepath.Join(root, "non-executable")
	if err := os.WriteFile(nonExecutable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	comma := filepath.Join(root, "with,comma")
	if err := os.WriteFile(comma, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{nonExecutable, directory, comma} {
		previous := resolverExecutable
		resolverExecutable = func() (string, error) { return source, nil }
		_, err := containerResolverSource()
		resolverExecutable = previous
		if !errors.Is(err, engine.ErrPermanent) {
			t.Errorf("containerResolverSource(%q) = %v, want permanent error", source, err)
		}
	}
}
