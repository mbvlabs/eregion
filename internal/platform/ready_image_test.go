package platform

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func readyImageStepForTest(t *testing.T) engine.Step {
	t.Helper()
	payload := ReadyImagePayload{
		Step: "platform-ready-image-01-v1", Image: packTestCanonical, ImageID: packTestImageID,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	step, err := buildReadyImageStep(engine.Execution{
		OperationID: contract.OperationID("op_" + strings.Repeat("c", 32)),
		Attempt:     1, Step: payload.Step,
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func writeReadyImageDocker(t *testing.T, dir, log, pulledID string, present bool) {
	t.Helper()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + log + `"
case "$1 $2" in
  "pull") exit 0;;
  "image inspect")
    case "$*" in
      *"{{.Id}}|{{json .RepoDigests}}"*) printf '%s|["%s"]\n' "` + pulledID + `" "` + packTestCanonical + `"; exit 0;;
    esac
    if [ "` + boolText(present) + `" = "true" ]; then printf '%s\n' "` + packTestImageID + `"; exit 0; fi
    echo "Error response from daemon: No such image" >&2; exit 1;;
esac
exit 0
`
	writePackTestExecutable(t, dir, "docker", script)
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestReadyImageCheckDoesNotPullWhenTheRecordedImageIsPresent(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	writeReadyImageDocker(t, dir, log, packTestImageID, true)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	check, err := readyImageStepForTest(t).Check(context.Background(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !check.Complete {
		t.Fatalf("present image check = %+v, want complete", check)
	}
	raw, _ := os.ReadFile(log)
	if strings.Contains(string(raw), "pull ") {
		t.Fatalf("a present image was pulled: %s", raw)
	}
}

func TestReadyImagePullsOnlyTheCanonicalDigestWhenTheLocalImageIsMissing(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	writeReadyImageDocker(t, dir, log, packTestImageID, false)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	step := readyImageStepForTest(t)

	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete {
		t.Fatal("a missing image passed Check")
	}
	result, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, ok := result.Result.(map[string]any)
	if !ok || fields["image_id"] != packTestImageID || fields["image"] != packTestCanonical {
		t.Fatalf("ready image result = %#v", result.Result)
	}
	raw, _ := os.ReadFile(log)
	want := "pull --platform linux/" + runtime.GOARCH + " " + packTestCanonical
	if !strings.Contains(string(raw), want) {
		t.Fatalf("pull did not use the canonical digest and node platform %q:\n%s", want, raw)
	}
	if strings.Contains(string(raw), "docker pull 127.0.0.1:5000") {
		t.Fatalf("pull used a mutable tag: %s", raw)
	}
}

func TestReadyImageRefusesAConfigDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	writeReadyImageDocker(t, dir, log, "sha256:"+strings.Repeat("f", 64), false)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := readyImageStepForTest(t).Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched pull = %v, want an explicit identity refusal", err)
	}
}

func TestReadyImagePayloadRequiresAnImmutableReference(t *testing.T) {
	payload := ReadyImagePayload{Step: "platform-ready-image-01-v1", Image: "127.0.0.1:5000/eregion/api:tag", ImageID: packTestImageID}
	raw, _ := json.Marshal(payload)
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("d", 32)), Attempt: 1, Step: payload.Step}
	if _, err := buildReadyImageStep(exec, raw); err == nil {
		t.Fatal("a mutable ready-release reference was accepted")
	}
}
