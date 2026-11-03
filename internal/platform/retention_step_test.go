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
	exec := retentionTestExecution(payload.Step)
	step, err := buildRetentionRemoveStep(exec, raw)
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
		{Kind: string(retention.ItemBuildpackCache), Reference: "eregion-build-cache-api-production"},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "demo/app@sha256:" + strings.Repeat("c", 64)},
	}
	for _, payload := range valid {
		payload.Step = RetentionRemoveStepID(retention.ItemKind(payload.Kind), payload.Reference)
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		exec := retentionTestExecution(payload.Step)
		if _, err := buildRetentionRemoveStep(exec, raw); err != nil {
			t.Errorf("%s %s was refused: %v", payload.Kind, payload.Reference, err)
		}
	}

	invalid := []RetentionRemovePayload{
		{Kind: "unknown", Reference: "x"},
		{Kind: string(retention.ItemContainer), Reference: "bad name"},
		{Kind: string(retention.ItemBuildpackCache), Reference: "some-other-volume"},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "no-digest"},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "Demos/app@sha256:" + strings.Repeat("c", 64)},
		{Kind: string(retention.ItemRegistryArtifact), Reference: "demo/app@sha256:short"},
	}
	for _, payload := range invalid {
		payload.Step = RetentionRemoveStepID(retention.ItemKind(payload.Kind), payload.Reference)
		raw, _ := json.Marshal(payload)
		exec := retentionTestExecution(payload.Step)
		if _, err := buildRetentionRemoveStep(exec, raw); err == nil {
			t.Errorf("%s %s was accepted", payload.Kind, payload.Reference)
		}
	}

	payload := RetentionRemovePayload{
		Step: RetentionRemoveStepID(retention.ItemRegistryArtifact, "demo/app@sha256:"+strings.Repeat("c", 64)),
		Kind: string(retention.ItemRegistryArtifact), Reference: "demo/app@sha256:" + strings.Repeat("c", 64),
		Registry: "registry.example.com:5000",
	}
	raw, _ := json.Marshal(payload)
	exec := retentionTestExecution(payload.Step)
	if _, err := buildRetentionRemoveStep(exec, raw); err == nil {
		t.Fatal("a non-loopback registry was accepted")
	}
}

// fakeDockerRemoval puts a docker script on PATH. When missing is true every
// removal reports the daemon's already-gone answer, which retention retries
// must treat as success.
func fakeDockerRemoval(t *testing.T, missing bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
`
	if missing {
		script += `case "$1 $2" in
  "image inspect"|"image rm") echo "Error response from daemon: No such image: $4" >&2; exit 1;;
  "volume inspect"|"volume rm") echo "Error response from daemon: no such volume: $4" >&2; exit 1;;
  "inspect --type=container") echo "Error: No such object: $4" >&2; exit 1;;
  "rm --force") echo "Error response from daemon: No such container: $3" >&2; exit 1;;
esac
`
	}
	script += "exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
		{retention.ItemContainer, "eregion-api-production-abc", "rm --force"},
		{retention.ItemBuildpackCache, "eregion-build-cache-api-production", "volume rm"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			log := fakeDockerRemoval(t, true)
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
				t.Fatalf("the removal command was not rendered: %s", raw)
			}
		})
	}
}

func TestRetentionRemoveStepRendersTheRemovalCommands(t *testing.T) {
	log := fakeDockerRemoval(t, false)
	for _, tc := range []struct {
		kind      retention.ItemKind
		reference string
		want      string
	}{
		{retention.ItemContainer, "eregion-api-production-abc", "rm --force eregion-api-production-abc"},
		{retention.ItemBuildpackCache, "eregion-build-cache-api-production", "volume rm eregion-build-cache-api-production"},
		{retention.ItemRelease, "sha256:" + strings.Repeat("d", 64), "image rm sha256:" + strings.Repeat("d", 64)},
	} {
		step := retentionRemoveStepFor(t, tc.kind, tc.reference, nil)
		if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"rm --force eregion-api-production-abc",
		"volume rm eregion-build-cache-api-production",
		"image rm sha256:" + strings.Repeat("d", 64),
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing command %q in:\n%s", want, raw)
		}
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

func TestRetentionGCScriptStopsRestartsAndNeverDeletesUntaggedManifests(t *testing.T) {
	payload := RetentionGCPayload{
		Step: "platform-retention-gc-01-v1", RegistryDir: "/srv/registry",
		ConfigPath: "/etc/eregion/registry/config.yml", Unit: "eregion-registry.service",
	}
	raw, _ := json.Marshal(payload)
	exec := retentionTestExecution(payload.Step)
	built, err := buildRetentionGCStep(exec, raw)
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
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("garbage collection must run when requested: %+v, %v", check, err)
	}

	invalid := payload
	invalid.Step = payload.Step
	invalid.Unit = "not-a-unit"
	raw, _ = json.Marshal(invalid)
	if _, err := buildRetentionGCStep(exec, raw); err == nil {
		t.Fatal("an invalid registry unit was accepted")
	}
	invalid = payload
	invalid.Image = "registry:2.8.3"
	raw, _ = json.Marshal(invalid)
	if _, err := buildRetentionGCStep(exec, raw); err == nil {
		t.Fatal("an unpinned registry image was accepted")
	}
}

func TestPackBuildUsesTheExplicitCacheVolume(t *testing.T) {
	payload := PackPayload{
		Image: "eregion/api-production:abc", Builder: DefaultBuilder, Path: t.TempDir(),
		Env: map[string]string{"BP_JVM_VERSION": "21"}, Cache: BuildpackCacheVolume("api", "production"),
	}
	args := strings.Join(packBuildArgs(payload, "/src"), " ")
	if !strings.Contains(args, "--cache type=build;format=volume;name=eregion-build-cache-api-production") {
		t.Fatalf("pack build has no explicit cache volume: %s", args)
	}
	payload.Cache = ""
	if strings.Contains(strings.Join(packBuildArgs(payload, "/src"), " "), "--cache") {
		t.Fatal("an unset cache must keep the builder's own default")
	}

	exec := retentionTestExecution("platform-pack-01-v1")
	payload.Step = "platform-pack-01-v1"
	payload.Cache = "some-other-volume"
	raw, _ := json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil {
		t.Fatal("an unattributable cache volume was accepted")
	}
	payload.Cache = BuildpackCacheVolume("api", "production")
	raw, _ = json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err != nil {
		t.Fatalf("the eregion cache volume was refused: %v", err)
	}
}
