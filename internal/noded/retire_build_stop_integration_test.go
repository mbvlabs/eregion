package noded

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestIntegrationRetireBuildStopMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	exec := engine.Execution{OperationID: contract.OperationID("op_0123456789abcdef0123456789abcdef"), Attempt: 1, Step: "platform-build-stop-01-v1"}
	buildID := "op_1123456789abcdef0123456789abcdef"
	payload, err := json.Marshal(platform.BuildStopPayload{Step: exec.Step, OperationID: buildID, PackStep: "platform-pack-01-v1"})
	if err != nil {
		t.Fatal(err)
	}
	step, err := node.DefaultRegistry().Build(exec, payload)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	hash := engine.InputsHash(exec.Step, inputs)
	local, err := node.NewLocal(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Node: "builder", StateDir: dir}
	retire := node.Request{Method: node.MethodRetireBuildStopMarker, Execution: exec, InputsHash: hash}
	marker := filepath.Join(dir, "builds", buildID+"-platform-pack-01-v1.cancel")

	release, err := engine.TryEffectLock(ctx, dir, exec)
	if err != nil {
		t.Fatal(err)
	}
	response := Handle(config, node.DefaultRegistry(), retire)
	release()
	if response.Status != engine.StatusRunning {
		t.Fatalf("locked retirement: %+v", response)
	}
	response = Handle(config, node.DefaultRegistry(), retire)
	if response.Status != engine.StatusCompleted || response.InputsHash != hash {
		t.Fatalf("retirement: %+v", response)
	}
	response, err = local.Call(ctx, retire, nil)
	if err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("repeat retirement: %+v, %v", response, err)
	}
	wrong := retire
	wrong.Execution.Attempt++
	response = Handle(config, node.DefaultRegistry(), wrong)
	if response.Status != engine.StatusStale {
		t.Fatalf("different attempt: %+v", response)
	}
	wrong = retire
	wrong.InputsHash = strings.Repeat("0", 64)
	response = Handle(config, node.DefaultRegistry(), wrong)
	if response.Status != engine.StatusStale {
		t.Fatalf("different hash: %+v", response)
	}
	wrong = retire
	wrong.Execution.Step = "platform-build-stop-02-v1"
	if response = Handle(config, node.DefaultRegistry(), wrong); response.Status != engine.StatusFailed {
		t.Fatalf("unsupported step: %+v", response)
	}
	run := node.Request{Method: node.MethodRun, Execution: exec, Payload: payload}
	response, err = local.Call(ctx, run, nil)
	if err != nil || response.Status != engine.StatusFailed || !strings.Contains(response.Detail, "retired") {
		t.Fatalf("late local run: %+v, %v", response, err)
	}
	response = Handle(config, node.DefaultRegistry(), run)
	if response.Status != engine.StatusFailed || !strings.Contains(response.Detail, "retired") {
		t.Fatalf("late node run: %+v", response)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("late run wrote marker: %v", err)
	}
}

func TestIntegrationRetireBuildStopMarkerInterruptedJournal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	exec := engine.Execution{OperationID: contract.OperationID("op_4123456789abcdef0123456789abcdef"), Attempt: 1, Step: "platform-build-stop-01-v1"}
	hash := strings.Repeat("a", 64)
	journal := engine.NewJournal("builder")
	journal.Tasks[exec.EffectKey()] = engine.Task{
		OperationID: string(exec.OperationID), Step: exec.Step, Attempt: exec.Attempt,
		ExecutionID: exec.ExecutionID(), InputsHash: hash, Status: engine.TaskRunning, Phase: engine.PhaseApplying,
	}
	if err := journal.Save(engine.JournalPath(dir), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong := exec
	wrong.Attempt++
	status, err := e.RetireBuildStopMarker(ctx, wrong, hash)
	if err != nil || status.Status != engine.StatusStale {
		t.Fatalf("mismatched journal: %+v, %v", status, err)
	}
	retired, err := e.IsRetired(exec)
	if err != nil || retired {
		t.Fatalf("mismatch fenced effect: %v, %v", retired, err)
	}
	status, err = e.RetireBuildStopMarker(ctx, exec, hash)
	if err != nil || status.Status != engine.StatusCompleted {
		t.Fatalf("interrupted synchronous marker: %+v, %v", status, err)
	}
}

func TestIntegrationRetireBuildStopMarkerAfterRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	exec := engine.Execution{OperationID: contract.OperationID("op_2123456789abcdef0123456789abcdef"), Attempt: 1, Step: "platform-build-stop-01-v1"}
	buildID := "op_3123456789abcdef0123456789abcdef"
	payload, err := json.Marshal(platform.BuildStopPayload{Step: exec.Step, OperationID: buildID, PackStep: "platform-pack-01-v1"})
	if err != nil {
		t.Fatal(err)
	}
	step, err := node.DefaultRegistry().Build(exec, payload)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	hash := engine.InputsHash(exec.Step, inputs)
	local, err := node.NewLocal(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := local.Call(ctx, node.Request{Method: node.MethodRun, Execution: exec, Payload: payload}, nil)
	if err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("run: %+v, %v", response, err)
	}
	marker := filepath.Join(dir, "builds", buildID+"-platform-pack-01-v1.cancel")
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "cancelled\n" {
		t.Fatalf("marker: %q, %v", contents, err)
	}
	retire := node.Request{Method: node.MethodRetireBuildStopMarker, Execution: exec, InputsHash: strings.Repeat("0", 64)}
	response, err = local.Call(ctx, retire, nil)
	if err != nil || response.Status != engine.StatusStale {
		t.Fatalf("mismatched record: %+v, %v", response, err)
	}
	retire.InputsHash = hash
	response, err = local.Call(ctx, retire, nil)
	if err != nil || response.Status != engine.StatusCompleted || !strings.Contains(response.Detail, "prior marker outcome is not established") {
		t.Fatalf("retire after marker: %+v, %v", response, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("retirement removed prior marker: %v", err)
	}
}
