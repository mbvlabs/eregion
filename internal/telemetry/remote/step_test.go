package remote

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func stepExecution(step string) engine.Execution {
	return engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: step}
}

func TestFileStepRefusesEscapesAndChangedContent(t *testing.T) {
	root := t.TempDir()
	step := "telemetry-file-01-v1"
	sum := sha256.Sum256([]byte("nice"))
	payload := FilePayload{
		Step: step, Root: root, Path: filepath.Join(root, "stack", "config.yaml"), Mode: 0600,
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("nice")),
		SHA256:        hex.EncodeToString(sum[:]),
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildFileStep(stepExecution(step), raw); err != nil {
		t.Fatal(err)
	}
	payload.SHA256 = strings.Repeat("0", 64)
	raw, _ = json.Marshal(payload)
	if _, err := buildFileStep(stepExecution(step), raw); err == nil {
		t.Fatal("a mismatched content hash was accepted")
	}
	payload.SHA256 = hex.EncodeToString(sum[:])
	payload.Path = filepath.Join(root, "..", "escape")
	raw, _ = json.Marshal(payload)
	if _, err := buildFileStep(stepExecution(step), raw); err == nil {
		t.Fatal("a path outside the declared root was accepted")
	}
	payload.Path = filepath.Join(root, "stack", "config.yaml")
	raw, _ = json.Marshal(payload)
	fileStep, err := buildFileStep(stepExecution(step), raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = fileStep
}

func TestArgvStepIsLimitedToCompose(t *testing.T) {
	root := t.TempDir()
	compose := filepath.Join(root, "stack", "compose.yaml")
	if err := os.MkdirAll(filepath.Dir(compose), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0640); err != nil {
		t.Fatal(err)
	}
	good := ArgvPayload{Step: "telemetry-argv-01-v1", Root: root, Argv: []string{"docker", "compose", "--file", compose, "up", "--detach"}}
	raw, _ := json.Marshal(good)
	if _, err := buildArgvStep(stepExecution(good.Step), raw); err != nil {
		t.Fatal(err)
	}
	bad := []ArgvPayload{
		{Step: good.Step, Root: root, Argv: []string{"sh", "-c", "rm -rf /"}},
		{Step: good.Step, Root: root, Argv: []string{"docker", "compose", "--file", "/etc/passwd", "up"}},
		{Step: good.Step, Root: root, Argv: []string{"docker", "compose", "--file", compose, "down"}},
		{Step: good.Step, Root: root, Argv: []string{"docker", "compose", "--file", compose, "up", "--privileged"}},
	}
	for _, value := range bad {
		raw, _ := json.Marshal(value)
		if _, err := buildArgvStep(stepExecution(good.Step), raw); err == nil {
			t.Fatalf("unsafe argv was accepted: %+v", value.Argv)
		}
	}
}

func TestMkdirStepRefusesTheFilesystemRoot(t *testing.T) {
	payload := MkdirPayload{Step: "telemetry-mkdir-01-v1", Root: "/", Path: "/tmp/x", Mode: 0750}
	raw, _ := json.Marshal(payload)
	if _, err := buildMkdirStep(stepExecution(payload.Step), raw); err == nil {
		t.Fatal("the filesystem root was accepted as a telemetry root")
	}
	// The declared root itself is a valid mkdir target; subdirectories are too.
	root := t.TempDir()
	payload = MkdirPayload{Step: "telemetry-mkdir-01-v1", Root: root, Path: root, Mode: 0750}
	raw, _ = json.Marshal(payload)
	step, err := buildMkdirStep(stepExecution(payload.Step), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
}
