package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/retention"
)

func retentionTestExecution(step string) engine.Execution {
	return engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: step}
}

func retentionRemoveStepFor(t *testing.T, kind retention.ItemKind, reference string, extra func(*RetentionRemovePayload)) engine.Step {
	t.Helper()
	payload := RetentionRemovePayload{
		Step: RetentionRemoveStepID(kind, reference), Kind: string(kind), Reference: reference,
	}
	if extra != nil {
		extra(&payload)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	step, err := buildRetentionRemoveStep(retentionTestExecution(payload.Step), raw)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func TestRetentionRemoveStepIDIsStablePerItem(t *testing.T) {
	first := RetentionRemoveStepID(retention.ItemContainer, "eregion-api-production-abc")
	if first != RetentionRemoveStepID(retention.ItemContainer, "eregion-api-production-abc") {
		t.Fatal("the same item must always produce the same effect identity")
	}
	if first == RetentionRemoveStepID(retention.ItemContainer, "eregion-api-production-def") {
		t.Fatal("two containers share one effect identity")
	}
	if first == RetentionRemoveStepID(retention.ItemDanglingImage, "eregion-api-production-abc") {
		t.Fatal("two kinds share one effect identity")
	}
	if !retentionRemoveStepPattern.MatchString(first) {
		t.Fatalf("the derived step ID is not a retention step: %q", first)
	}
}

func TestRegistryManifestRemovalIsRepositoryScoped(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	first := retentionRemoveStepFor(t, retention.ItemRegistryArtifact, "demo/app@"+digest,
		func(payload *RetentionRemovePayload) { payload.Registry = "127.0.0.1:5000" })
	second := retentionRemoveStepFor(t, retention.ItemRegistryArtifact, "other/repo@"+digest,
		func(payload *RetentionRemovePayload) { payload.Registry = "127.0.0.1:5000" })
	if first.ID() == second.ID() {
		t.Fatal("the same digest in two repositories must be two effects")
	}
	firstURL, err := first.(*RetentionRemoveStep).registryURL()
	if err != nil {
		t.Fatal(err)
	}
	secondURL, err := second.(*RetentionRemoveStep).registryURL()
	if err != nil {
		t.Fatal(err)
	}
	if firstURL != "http://127.0.0.1:5000/v2/demo/app/manifests/"+digest {
		t.Fatalf("first delete URL = %q", firstURL)
	}
	if secondURL != "http://127.0.0.1:5000/v2/other/repo/manifests/"+digest {
		t.Fatalf("second delete URL = %q", secondURL)
	}
}

func TestRetentionRemovePayloadValidation(t *testing.T) {
	valid := []RetentionRemovePayload{
		{Kind: string(retention.ItemContainer), Reference: "eregion-api-production-abc"},
		{Kind: string(retention.ItemRelease), Reference: "sha256:" + strings.Repeat("b", 64)},
		{Kind: string(retention.ItemDanglingImage), Reference: "eregion/api-production:abc"},
		{Kind: string(retention.ItemBuildpackCache), Reference: testBuildCacheVolume},
		{Kind: string(retention.ItemBuildpackCache), Reference: testLaunchCacheVolume},
		{Kind: string(retention.ItemBuildpackCache), Reference: "pack-cache-0123456789abcdef.build"},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "demo/app@sha256:" + strings.Repeat("c", 64)},
	}
	for _, payload := range valid {
		payload.Step = RetentionRemoveStepID(retention.ItemKind(payload.Kind), payload.Reference)
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildRetentionRemoveStep(retentionTestExecution(payload.Step), raw); err != nil {
			t.Errorf("%s %s was refused: %v", payload.Kind, payload.Reference, err)
		}
	}

	invalid := []RetentionRemovePayload{
		{Kind: "unknown", Reference: "x"},
		{Kind: string(retention.ItemContainer), Reference: "bad name"},
		{Kind: string(retention.ItemBuildpackCache), Reference: "some-other-volume"},
		{Kind: string(retention.ItemBuildpackCache), Reference: "pack-cache-random"},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "no-digest"},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "Demos/app@sha256:" + strings.Repeat("c", 64)},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "demo/app@sha256:short"},
	}
	for _, payload := range invalid {
		payload.Step = RetentionRemoveStepID(retention.ItemKind(payload.Kind), payload.Reference)
		raw, _ := json.Marshal(payload)
		if _, err := buildRetentionRemoveStep(retentionTestExecution(payload.Step), raw); err == nil {
			t.Errorf("%s %s was accepted", payload.Kind, payload.Reference)
		}
	}

	payload := RetentionRemovePayload{
		Step: RetentionRemoveStepID(retention.ItemRegistryArtifact, "demo/app@sha256:"+strings.Repeat("c", 64)),
		Kind: string(retention.ItemRegistryArtifact), Reference: "demo/app@sha256:" + strings.Repeat("c", 64),
		Registry: "registry.example.com:5000",
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildRetentionRemoveStep(retentionTestExecution(payload.Step), raw); err == nil {
		t.Fatal("a non-loopback registry was accepted")
	}
}

// fakeDocker puts a docker script on PATH. missing makes every inspect and
// removal report the daemon's already-gone answer. containerInspect answers a
// container ownership probe.
func fakeRetentionDocker(t *testing.T, missing bool, containerInspect string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
  "image inspect"|"image rm")
    if [ "$FAKE_MISSING" = 1 ]; then echo "Error response from daemon: No such image" >&2; exit 1; fi
    exit 0;;
  "volume inspect"|"volume rm")
    if [ "$FAKE_MISSING" = 1 ]; then echo "Error response from daemon: no such volume" >&2; exit 1; fi
    exit 0;;
  "inspect --type=container")
    if [ "$FAKE_MISSING" = 1 ]; then echo "Error: No such object" >&2; exit 1; fi
    printf '%s' "$FAKE_CONTAINER_INSPECT"; exit 0;;
  "rm --force")
    exit 0;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_MISSING", map[bool]string{true: "1", false: "0"}[missing])
	t.Setenv("FAKE_CONTAINER_INSPECT", containerInspect)
	return log
}

func TestRetentionRemoveStepTreatsAlreadyGoneAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		kind      retention.ItemKind
		reference string
		want      string
	}{
		{retention.ItemRelease, "sha256:" + strings.Repeat("b", 64), "image rm"},
		{retention.ItemDanglingImage, "eregion/api-production:old", "image rm"},
		{retention.ItemContainer, "eregion-api-production-abc", "inspect --type=container"},
		{retention.ItemBuildpackCache, testBuildCacheVolume, "volume rm"},
		{retention.ItemBuildpackCache, "pack-cache-0123456789abcdef.build", "volume rm"},
	} {
		t.Run(string(tc.kind)+"/"+tc.reference, func(t *testing.T) {
			log := fakeRetentionDocker(t, true, "")
			step := retentionRemoveStepFor(t, tc.kind, tc.reference, nil)
			check, err := step.Check(context.Background(), engine.Runtime{})
			if err != nil {
				t.Fatal(err)
			}
			if !check.Complete {
				t.Fatalf("an already-gone item is pending removal: %+v", check)
			}
			result, err := step.Apply(context.Background(), engine.Runtime{}, nil)
			if err != nil {
				t.Fatalf("an already-gone item failed removal: %v", err)
			}
			data, _ := json.Marshal(result.Result)
			if !strings.Contains(string(data), `"already_gone":true`) {
				t.Fatalf("the already-gone result is not reported: %s", data)
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Fatalf("the expected probe/removal command was not rendered: %s", raw)
			}
		})
	}
}

func TestRetentionRemoveStepRendersTheRemovalCommands(t *testing.T) {
	log := fakeRetentionDocker(t, false, "cid123|api|production|/eregion-api-production-abc")
	for _, tc := range []struct {
		kind      retention.ItemKind
		reference string
		extra     func(*RetentionRemovePayload)
		want      string
	}{
		{retention.ItemContainer, "eregion-api-production-abc", func(payload *RetentionRemovePayload) {
			payload.App, payload.Environment = "api", "production"
		}, "rm --force cid123"},
		{retention.ItemBuildpackCache, testBuildCacheVolume, nil, "volume rm " + testBuildCacheVolume},
		{retention.ItemRelease, "sha256:" + strings.Repeat("d", 64), nil, "image rm sha256:" + strings.Repeat("d", 64)},
	} {
		step := retentionRemoveStepFor(t, tc.kind, tc.reference, tc.extra)
		if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"rm --force cid123",
		"volume rm " + testBuildCacheVolume,
		"image rm sha256:" + strings.Repeat("d", 64),
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing command %q in:\n%s", want, raw)
		}
	}
}

func TestRetentionRemoveContainerRequiresOwnershipAtExecutionTime(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inspect     string
		app         string
		environment string
		wantErr     string
		wantRemoved bool
	}{
		{
			name: "owned", inspect: "cid123|api|production|/eregion-api-production-abc",
			app: "api", environment: "production", wantRemoved: true,
		},
		{
			name: "registry container", inspect: "cid123|||/eregion-registry",
			wantErr: "infrastructure container",
		},
		{
			name: "unlabeled container", inspect: "cid123|<no value>|<no value>|/other",
			wantErr: "ownership label",
		},
		{
			name: "other app", inspect: "cid123|other|production|/eregion-other-production-abc",
			app: "api", wantErr: "belongs to app",
		},
		{
			name: "other environment", inspect: "cid123|api|staging|/eregion-api-staging-abc",
			app: "api", environment: "production", wantErr: "belongs to environment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := fakeRetentionDocker(t, false, tc.inspect)
			step := retentionRemoveStepFor(t, retention.ItemContainer, "eregion-api-production-abc", func(payload *RetentionRemovePayload) {
				payload.App, payload.Environment = tc.app, tc.environment
			})
			_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
			raw, _ := os.ReadFile(log)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(string(raw), "rm --force") {
					t.Fatalf("an unowned container was removed:\n%s", raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantRemoved || !strings.Contains(string(raw), "rm --force cid123") {
				t.Fatalf("the owned container was not removed by its immutable ID:\n%s", raw)
			}
		})
	}
}

func TestRetentionRemovalRunsUnderTheHostLock(t *testing.T) {
	for _, step := range []engine.Step{
		retentionRemoveStepFor(t, retention.ItemContainer, "eregion-api-production-abc", nil),
		retentionRemoveStepFor(t, retention.ItemDanglingImage, "sha256:"+strings.Repeat("b", 64), nil),
	} {
		if _, ok := step.(engine.HostLockIndependent); ok {
			t.Fatalf("%s must not opt out of the node host lock", step.ID())
		}
	}
	gc, err := buildRetentionGCStep(retentionTestExecution("platform-retention-gc-01-v1"), []byte(`{"step":"platform-retention-gc-01-v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gc.(engine.HostLockIndependent); ok {
		t.Fatal("garbage collection must not opt out of the node host lock")
	}
}

func TestRetentionGCPassIDsAreDistinctAndValidated(t *testing.T) {
	first := RetentionGCStepID(1)
	second := RetentionGCStepID(2)
	if first == second || first == RetentionGCStepID(0) || second == RetentionGCStepID(0) {
		t.Fatalf("GC pass IDs are not distinct: %q %q %q", RetentionGCStepID(0), first, second)
	}
	payload, _ := json.Marshal(RetentionGCPayload{Step: second, Generation: 2})
	if _, err := buildRetentionGCStep(retentionTestExecution(second), payload); err != nil {
		t.Fatalf("a generated GC pass was refused: %v", err)
	}
	payload, _ = json.Marshal(RetentionGCPayload{Step: second, Generation: 1})
	if _, err := buildRetentionGCStep(retentionTestExecution(second), payload); err == nil {
		t.Fatal("a GC step with the wrong generation was accepted")
	}
}

func TestRetentionRemoveStepDeletesARegistryManifestByDigest(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()

	reference := "demo/app@sha256:" + strings.Repeat("e", 64)
	digest := "sha256:" + strings.Repeat("e", 64)
	step := retentionRemoveStepFor(t, retention.ItemRegistryArtifact, reference, func(payload *RetentionRemovePayload) {
		payload.Registry = server.Listener.Addr().String()
	})
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("check = %+v, err = %v", check, err)
	}
	result, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result.Result)
	if !strings.Contains(string(data), `"removed":true`) {
		t.Fatalf("result = %s", data)
	}
	want := "/v2/demo/app/manifests/" + digest
	found := false
	for _, path := range paths {
		if path == "DELETE "+want {
			found = true
		}
	}
	if !found {
		t.Fatalf("DELETE %s was not requested: %v", want, paths)
	}
}

func TestRetentionRemoveRegistryDeleteHandlesGoneAndDisabledDeletes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantErr  string
		wantGone bool
	}{
		{name: "already gone", status: http.StatusNotFound, wantGone: true},
		{name: "deletes disabled", status: http.StatusMethodNotAllowed, wantErr: "host-registry-v2"},
		{name: "server failure", status: http.StatusInternalServerError, wantErr: "registry"},
		{name: "forbidden", status: http.StatusForbidden, wantErr: "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			step := retentionRemoveStepFor(t, retention.ItemRegistryArtifact, "demo/app@sha256:"+strings.Repeat("f", 64), func(payload *RetentionRemovePayload) {
				payload.Registry = server.Listener.Addr().String()
			})
			result, err := step.Apply(context.Background(), engine.Runtime{}, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(result.Result)
			if tc.wantGone && !strings.Contains(string(data), `"already_gone":true`) {
				t.Fatalf("result = %s", data)
			}
		})
	}
}

func gcStepForTest(t *testing.T) *RetentionGCStep {
	t.Helper()
	payload := RetentionGCPayload{
		Step: "platform-retention-gc-01-v1", RegistryDir: "/srv/registry",
		ConfigPath: "/etc/eregion/registry/config.yml", Unit: "eregion-registry.service",
	}
	raw, _ := json.Marshal(payload)
	built, err := buildRetentionGCStep(retentionTestExecution(payload.Step), raw)
	if err != nil {
		t.Fatal(err)
	}
	return built.(*RetentionGCStep)
}

func TestRetentionGCScriptUsesOnlyThePolkitAllowedSystemdVerbs(t *testing.T) {
	script := gcStepForTest(t).script()
	allowed := map[string]bool{"start": true, "stop": true, "restart": true, "is-active": true}
	for _, line := range strings.Split(script, "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field != "systemctl" || i+1 >= len(fields) {
				continue
			}
			verb := fields[i+1]
			if !allowed[verb] {
				t.Fatalf("the GC script calls systemctl %s, which host-registry-gc-polkit-v1 does not allow:\n%s", verb, line)
			}
		}
	}
	if strings.Contains(script, "sudo ") || strings.Contains(script, "pkexec") {
		t.Fatalf("the GC script tries to elevate privileges instead of using the narrow polkit rule:\n%s", script)
	}
}

func TestRetentionGCScriptStopsRestartsAndFailsClosed(t *testing.T) {
	step := gcStepForTest(t)
	script := step.script()
	for _, want := range []string{
		`unit='eregion-registry.service'`, "collector=eregion-registry-gc",
		`systemctl stop "$unit"`, `systemctl start "$unit"`,
		"garbage-collect /etc/docker/registry/config.yml",
		`docker container inspect "$collector"`,
		`docker rm --force "$collector"`,
		`docker create --name "$collector"`,
		`docker start --attach "$collector"`,
		"no registry garbage-collect process is running",
		"garbage-collect process is still running",
		"is left stopped",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("the GC script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "--delete-untagged") {
		t.Fatalf("untagged manifests can still be live releases; they must never be deleted:\n%s", script)
	}
	if !strings.Contains(script, "--network none") || !strings.Contains(script, "--volume '/srv/registry':/var/lib/registry") {
		t.Fatalf("the collection container is not isolated or is missing the registry storage:\n%s", script)
	}
	if !strings.Contains(script, RegistryImage) {
		t.Fatalf("the collection container does not use the pinned registry image:\n%s", script)
	}
	if strings.Contains(script, `rm --force "$collector" >/dev/null 2>&1 || true`) {
		t.Fatalf("an orphan removal failure is still ignored:\n%s", script)
	}
	// The trap may only be armed after the stale collector and any running
	// garbage-collect process are verified gone, and before our own collector
	// starts.
	verify := strings.Index(script, "garbage-collect process is still running")
	create := strings.Index(script, `docker create --name "$collector"`)
	restore := strings.Index(script, "restore_registry()")
	trap := strings.Index(script, "trap restore_registry EXIT")
	start := strings.Index(script, `docker start --attach "$collector"`)
	if verify < 0 || create < 0 || restore < 0 || trap < 0 || start < 0 || !(verify < create && create < restore && restore < trap && trap < start) {
		t.Fatalf("the restore trap is not armed between stale-collector verification and our collector start:\n%s", script)
	}
	stop := strings.Index(script, `systemctl stop "$unit"`)
	if stop < 0 || strings.Contains(script[stop:restore], "systemctl start") {
		t.Fatalf("the stale-collector path can start the registry before the collector is created:\n%s", script)
	}
	if !strings.Contains(script, "failed to restart $unit after garbage collection") ||
		!strings.Contains(script, `exit "$status"`) {
		t.Fatalf("a registry restart failure in the trap is suppressed:\n%s", script)
	}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("garbage collection must run when requested: %+v, %v", check, err)
	}

	invalid := RetentionGCPayload{
		Step: "platform-retention-gc-01-v1", Unit: "not-a-unit",
	}
	raw, _ := json.Marshal(invalid)
	if _, err := buildRetentionGCStep(retentionTestExecution(invalid.Step), raw); err == nil {
		t.Fatal("an invalid registry unit was accepted")
	}
	invalid = RetentionGCPayload{Step: "platform-retention-gc-01-v1", Image: "registry:2.8.3"}
	raw, _ = json.Marshal(invalid)
	if _, err := buildRetentionGCStep(retentionTestExecution(invalid.Step), raw); err == nil {
		t.Fatal("an unpinned registry image was accepted")
	}
}

type gcScriptResult struct {
	exitCode     int
	stderr       string
	dockerCalls  []string
	serviceCalls []string
}

// runRetentionGCScript runs the rendered script with fake docker and systemctl
// programs. The script's absolute /usr/bin/docker is redirected to the fake.
func runRetentionGCScript(t *testing.T, script string, env map[string]string) gcScriptResult {
	t.Helper()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker.log")
	serviceLog := filepath.Join(dir, "systemctl.log")
	dockerPath := filepath.Join(dir, "docker")
	dockerScript := `#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_LOG"
case "$1 $2" in
  "container inspect")
    if [ -f "$FAKE_ORPHAN" ]; then exit 0; fi
    exit 1;;
  "rm --force")
    if [ "$3" = "eregion-registry-gc" ] && [ -f "$FAKE_ORPHAN" ] && [ "$FAKE_ORPHAN_RM" != "sticky" ]; then
      case "$FAKE_ORPHAN_RM" in
      fail) exit 1;;
      esac
      rm -f "$FAKE_ORPHAN"
    fi
    exit 0;;
  "ps --quiet")
    if [ -n "$FAKE_RUNNING" ]; then printf '%s\n' "$FAKE_RUNNING"; fi
    exit 0;;
  "inspect --type=container")
    printf '%s\n' "$FAKE_RUNNING_COMMANDS"
    exit 0;;
  "create --name")
    exit "$FAKE_CREATE";;
  "start --attach")
    exit "$FAKE_START";;
esac
exit 0
`
	if err := os.WriteFile(dockerPath, []byte(dockerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	servicePath := filepath.Join(dir, "systemctl")
	serviceScript := `#!/bin/sh
echo "$*" >> "$FAKE_SYSTEMCTL_LOG"
case "$1" in
  stop)
    : > "$FAKE_SERVICE_STOPPED"
    exit 0;;
  start)
    if [ "$FAKE_SYSTEMCTL_START" = "0" ]; then rm -f "$FAKE_SERVICE_STOPPED"; fi
    exit "$FAKE_SYSTEMCTL_START";;
  is-active)
    if [ -f "$FAKE_SERVICE_STOPPED" ]; then exit 1; fi
    exit "$FAKE_SYSTEMCTL_ACTIVE";;
esac
exit 0
`
	if err := os.WriteFile(servicePath, []byte(serviceScript), 0o700); err != nil {
		t.Fatal(err)
	}

	script = strings.ReplaceAll(script, "/usr/bin/docker", dockerPath)
	command := exec.Command("bash", "-c", script)
	values := map[string]string{
		"PATH":            dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_DOCKER_LOG": dockerLog, "FAKE_SYSTEMCTL_LOG": serviceLog,
		"FAKE_SERVICE_STOPPED": filepath.Join(dir, "stopped"),
		"FAKE_ORPHAN_RM":       "remove", "FAKE_CREATE": "0", "FAKE_START": "0",
		"FAKE_SYSTEMCTL_START": "0", "FAKE_SYSTEMCTL_ACTIVE": "0",
	}
	for key, value := range env {
		values[key] = value
	}
	if values["FAKE_ORPHAN"] == "1" {
		orphanPath := filepath.Join(dir, "orphan")
		if err := os.WriteFile(orphanPath, []byte("orphan\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		values["FAKE_ORPHAN"] = orphanPath
	}
	command.Env = make([]string, 0, len(values))
	for key, value := range values {
		command.Env = append(command.Env, key+"="+value)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	result := gcScriptResult{stderr: stderr.String()}
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		result.exitCode = exitErr.ExitCode()
	}
	result.dockerCalls = readLines(t, dockerLog)
	result.serviceCalls = readLines(t, serviceLog)
	return result
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func firstCallIndex(calls []string, prefix string) int {
	for index, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return index
		}
	}
	return -1
}

func serviceStarted(calls []string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, "start ") {
			return true
		}
	}
	return false
}

func TestRetentionGCScriptNormalPath(t *testing.T) {
	result := runRetentionGCScript(t, gcStepForTest(t).script(), nil)
	if result.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", result.exitCode, result.stderr)
	}
	if !serviceStarted(result.serviceCalls) {
		t.Fatalf("the registry was not restarted: %v", result.serviceCalls)
	}
	create := firstCallIndex(result.dockerCalls, "create --name")
	start := firstCallIndex(result.dockerCalls, "start --attach")
	if create < 0 || start < 0 || create > start {
		t.Fatalf("collector calls = %v", result.dockerCalls)
	}
	if rm := firstCallIndex(result.dockerCalls, "rm --force eregion-registry-gc"); rm >= 0 && rm < create {
		t.Fatalf("the normal path removed a collector before creating one: %v", result.dockerCalls)
	}
}

func TestRetentionGCScriptRemovesAStaleCollector(t *testing.T) {
	result := runRetentionGCScript(t, gcStepForTest(t).script(), map[string]string{"FAKE_ORPHAN": "1"})
	if result.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", result.exitCode, result.stderr)
	}
	rm := firstCallIndex(result.dockerCalls, "rm --force eregion-registry-gc")
	create := firstCallIndex(result.dockerCalls, "create --name")
	if rm < 0 || create < 0 || rm > create {
		t.Fatalf("a stale collector was not removed before the new one: %v", result.dockerCalls)
	}
	if !serviceStarted(result.serviceCalls) {
		t.Fatalf("the registry was not restarted: %v", result.serviceCalls)
	}
}

func TestRetentionGCScriptFailsClosedWhenAStaleCollectorCannotBeRemoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "removal fails", env: map[string]string{"FAKE_ORPHAN": "1", "FAKE_ORPHAN_RM": "fail"}, want: "could not be removed"},
		{name: "still present", env: map[string]string{"FAKE_ORPHAN": "1", "FAKE_ORPHAN_RM": "sticky"}, want: "still present after removal"},
		{name: "process still running", env: map[string]string{
			"FAKE_RUNNING":          "abc123",
			"FAKE_RUNNING_COMMANDS": "[] [/bin/registry garbage-collect /etc/docker/registry/config.yml]",
		}, want: "process is still running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := runRetentionGCScript(t, gcStepForTest(t).script(), tc.env)
			if result.exitCode == 0 {
				t.Fatalf("the step succeeded: stderr=%s", result.stderr)
			}
			if serviceStarted(result.serviceCalls) {
				t.Fatalf("the registry was started while a stale collector may still write: %v", result.serviceCalls)
			}
			if !strings.Contains(result.stderr, tc.want) || !strings.Contains(result.stderr, "left stopped") {
				t.Fatalf("stderr = %q, want %q and a recovery instruction", result.stderr, tc.want)
			}
			if firstCallIndex(result.dockerCalls, "create --name") >= 0 {
				t.Fatalf("a collector was created after the stale one could not be verified gone: %v", result.dockerCalls)
			}
		})
	}
}

func TestRetentionGCScriptRestartsAfterItsOwnCollectorFails(t *testing.T) {
	result := runRetentionGCScript(t, gcStepForTest(t).script(), map[string]string{"FAKE_START": "3"})
	if result.exitCode == 0 {
		t.Fatal("a failed collection succeeded")
	}
	if !serviceStarted(result.serviceCalls) {
		t.Fatalf("a failed collection did not restart the registry: %v", result.serviceCalls)
	}
}

func TestRetentionGCScriptFailsWhenARestartFails(t *testing.T) {
	result := runRetentionGCScript(t, gcStepForTest(t).script(), map[string]string{"FAKE_SYSTEMCTL_START": "1"})
	if result.exitCode == 0 {
		t.Fatal("a failed registry restart succeeded")
	}
	if !strings.Contains(result.stderr, "failed to restart") {
		t.Fatalf("stderr = %q", result.stderr)
	}
}

func TestRetentionGCScriptFailsWhenItsCollectorCannotBeCreated(t *testing.T) {
	result := runRetentionGCScript(t, gcStepForTest(t).script(), map[string]string{"FAKE_CREATE": "1"})
	if result.exitCode == 0 {
		t.Fatal("a collector creation failure succeeded")
	}
	if serviceStarted(result.serviceCalls) {
		t.Fatalf("the registry was started without a collector: %v", result.serviceCalls)
	}
	if !strings.Contains(result.stderr, "could not create") {
		t.Fatalf("stderr = %q", result.stderr)
	}
}

func TestPackBuildUsesTheExplicitBuildAndLaunchCaches(t *testing.T) {
	payload := PackPayload{
		Image: "eregion/api-production:abc", Builder: DefaultBuilder, Path: t.TempDir(),
		Env:         map[string]string{"BP_JVM_VERSION": "21"},
		Cache:       BuildpackCacheVolume("api", "production"),
		LaunchCache: BuildpackLaunchCacheVolume("api", "production"),
	}
	args := packBuildArgs(payload, "/src")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cache type=build;format=volume;name="+testBuildCacheVolume) {
		t.Fatalf("pack build has no explicit build cache: %s", joined)
	}
	if !strings.Contains(joined, "--cache type=launch;format=volume;name="+testLaunchCacheVolume) {
		t.Fatalf("pack build has no explicit launch cache: %s", joined)
	}
	if strings.Count(joined, "--cache ") != 2 {
		t.Fatalf("pack 0.40 needs one cache type per --cache flag: %s", joined)
	}
	payload.Cache, payload.LaunchCache = "", ""
	if strings.Contains(strings.Join(packBuildArgs(payload, "/src"), " "), "--cache") {
		t.Fatal("unset caches must keep the builder's own defaults")
	}

	exec := retentionTestExecution("platform-pack-01-v1")
	payload.Step = "platform-pack-01-v1"
	payload.Cache = "some-other-volume"
	raw, _ := json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil {
		t.Fatal("an unattributable cache volume was accepted")
	}
	payload.Cache = BuildpackCacheVolume("api", "production")
	payload.LaunchCache = BuildpackLaunchCacheVolume("api", "production")
	raw, _ = json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err != nil {
		t.Fatalf("the eregion cache volumes were refused: %v", err)
	}
}
