package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestRetentionGCStopsRestartsAndRecoversAnInterruptedAttempt(t *testing.T) {
	payload := RetentionGCPayload{
		Step: "platform-retention-gc-01-v1", RegistryDir: "/srv/registry",
		ConfigPath: "/etc/eregion/registry/config.yml", Unit: "eregion-registry.service",
	}
	raw, _ := json.Marshal(payload)
	built, err := buildRetentionGCStep(retentionTestExecution(payload.Step), raw)
	if err != nil {
		t.Fatal(err)
	}
	step := built.(*RetentionGCStep)
	script := step.script()
	if !strings.Contains(script, "systemctl stop 'eregion-registry.service'") ||
		!strings.Contains(script, "systemctl start 'eregion-registry.service'") {
		t.Fatalf("the registry is not stopped and restarted:\n%s", script)
	}
	if !strings.Contains(script, "garbage-collect /etc/docker/registry/config.yml") {
		t.Fatalf("the collection command is missing:\n%s", script)
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
	if !strings.Contains(script, "--name eregion-registry-gc") ||
		!strings.Contains(script, "docker container inspect eregion-registry-gc") ||
		!strings.Contains(script, "docker rm --force eregion-registry-gc") {
		t.Fatalf("a retry cannot detect and remove an orphaned collection container:\n%s", script)
	}
	if !strings.Contains(script, "failed to restart eregion-registry.service after garbage collection") ||
		!strings.Contains(script, `exit "$status"`) {
		t.Fatalf("a registry restart failure in the trap is suppressed:\n%s", script)
	}
	if strings.Contains(script, "systemctl start 'eregion-registry.service' >/dev/null 2>&1 || true") {
		t.Fatalf("the trap still suppresses a restart failure:\n%s", script)
	}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("garbage collection must run when requested: %+v, %v", check, err)
	}

	invalid := payload
	invalid.Unit = "not-a-unit"
	raw, _ = json.Marshal(invalid)
	if _, err := buildRetentionGCStep(retentionTestExecution(payload.Step), raw); err == nil {
		t.Fatal("an invalid registry unit was accepted")
	}
	invalid = payload
	invalid.Image = "registry:2.8.3"
	raw, _ = json.Marshal(invalid)
	if _, err := buildRetentionGCStep(retentionTestExecution(payload.Step), raw); err == nil {
		t.Fatal("an unpinned registry image was accepted")
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
