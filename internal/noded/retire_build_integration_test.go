package noded

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestIntegrationRetireBuildFencesLocalAndNodeAgent(t *testing.T) {
	dir := t.TempDir()
	exec := engine.Execution{OperationID: contract.OperationID("op_0123456789abcdef0123456789abcdef"), Attempt: 1, Step: "platform-pack-03-v1"}
	local, err := node.NewLocal(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	retire := node.Request{Method: node.MethodRetireBuild, Execution: exec}
	response, err := local.Call(context.Background(), retire, nil)
	if err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("retire: %+v, %v", response, err)
	}
	payload, err := json.Marshal(platform.PackPayload{
		Step: exec.Step, Image: "localhost:5000/app:test", Builder: "example.com/builder:test",
		Registry: "127.0.0.1:5000", Path: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	exec.Attempt++
	run := node.Request{Method: node.MethodRun, Execution: exec, Payload: payload}
	response, err = local.Call(context.Background(), run, nil)
	if err != nil || response.Status != engine.StatusFailed || !strings.Contains(response.Detail, "retired") {
		t.Fatalf("local late run: %+v, %v", response, err)
	}
	response = Handle(Config{Node: "builder", StateDir: dir}, node.DefaultRegistry(), run)
	if response.Status != engine.StatusFailed || !strings.Contains(response.Detail, "retired") {
		t.Fatalf("node agent late run: %+v", response)
	}
	retire.Execution = exec
	response = Handle(Config{Node: "builder", StateDir: dir}, node.DefaultRegistry(), retire)
	if response.Status != engine.StatusCompleted {
		t.Fatalf("repeated retirement: %+v", response)
	}
}

func TestIntegrationRetireBuildDoesNotTrustInterruptedApply(t *testing.T) {
	dir := t.TempDir()
	e, err := engine.New(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: contract.OperationID("op_1123456789abcdef0123456789abcdef"), Attempt: 1, Step: "platform-pack-03-v1"}
	journal := engine.NewJournal("builder")
	journal.Tasks[exec.EffectKey()] = engine.Task{
		OperationID: string(exec.OperationID), Step: exec.Step, Status: engine.TaskRunning,
		Phase: engine.PhaseApplying, StartedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if err := journal.Save(e.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Compact(context.Background(), dir, engine.Retention{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	response, err := node.RetireBuild(context.Background(), e, exec)
	if err != nil || response.Status != engine.StatusStale {
		t.Fatalf("interrupted apply: %+v, %v", response, err)
	}
	retired, err := e.IsRetired(exec)
	if err != nil || !retired {
		t.Fatalf("uncertain build not fenced: %v, %v", retired, err)
	}
}

func TestIntegrationRetireBuildWaitsForEffectLock(t *testing.T) {
	dir := t.TempDir()
	e, err := engine.New(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: contract.OperationID("op_2123456789abcdef0123456789abcdef"), Step: "platform-pack-03-v1"}
	release, err := engine.TryEffectLock(context.Background(), dir, exec)
	if err != nil {
		t.Fatal(err)
	}
	response, err := node.RetireBuild(context.Background(), e, exec)
	release()
	if err != nil || response.Status != engine.StatusRunning {
		t.Fatalf("locked retirement: %+v, %v", response, err)
	}
	response, err = node.RetireBuild(context.Background(), e, exec)
	if err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("unlocked retirement: %+v, %v", response, err)
	}
}

func TestIntegrationRetireBuildDoesNotTrustLegacyMissingHistory(t *testing.T) {
	dir := t.TempDir()
	if err := engine.NewJournal("builder").Save(engine.JournalPath(dir), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: contract.OperationID("op_3123456789abcdef0123456789abcdef"), Step: "platform-pack-03-v1"}
	response, err := node.RetireBuild(context.Background(), e, exec)
	if err != nil || response.Status != engine.StatusStale {
		t.Fatalf("legacy missing history: %+v, %v", response, err)
	}
}
