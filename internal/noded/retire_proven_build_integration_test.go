package noded

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type provenBuildCase struct {
	name    string
	setup   func(*testing.T, *engine.Engine)
	want    engine.Status
	wantErr bool
}

func TestIntegrationRetireProvenBuild(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_4123456789abcdef0123456789abcdef"), Attempt: 1, Step: "platform-pack-03-v1"}
	payload, err := json.Marshal(platform.PackPayload{Step: exec.Step, Image: "localhost:5000/app:test", Builder: "example.com/builder:test", Registry: "127.0.0.1:5000", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	hash := engine.InputsHash(exec.Step, payload)

	for _, tc := range []provenBuildCase{
		{name: "missing", want: engine.StatusStale},
		{name: "terminal task alone", setup: func(t *testing.T, e *engine.Engine) {
			j := engine.NewJournal("builder")
			j.Tasks[exec.EffectKey()] = engine.Task{OperationID: string(exec.OperationID), Step: exec.Step, Attempt: exec.Attempt, ExecutionID: exec.ExecutionID(), InputsHash: hash, Status: engine.TaskCompleted}
			if err := j.Save(e.JournalPath(), &secrets.Registry{}); err != nil {
				t.Fatal(err)
			}
		}, want: engine.StatusStale},
		{name: "matching effect", want: engine.StatusCompleted},
		{name: "legacy matching effect", want: engine.StatusStale},
		{name: "wrong caller input", want: engine.StatusStale},
		{name: "missing caller input", wantErr: true},
		{name: "wrong effect input", want: engine.StatusStale},
		{name: "wrong execution", want: engine.StatusStale},
		{name: "conflicting journal", want: engine.StatusStale},
		{name: "corrupt journal", wantErr: true},
		{name: "corrupt effect", wantErr: true},
		{name: "uncertain marker", want: engine.StatusStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRetireProvenBuild(t, tc, exec, payload, hash)
		})
	}
}

func testRetireProvenBuild(t *testing.T, tc provenBuildCase, exec engine.Execution, payload []byte, hash string) {
	t.Helper()
	e := setupProvenBuild(t, tc, exec, hash)
	dir := e.StateDir
	proofHash := hash
	if tc.name == "wrong caller input" {
		proofHash = strings.Repeat("b", 64)
	}
	if tc.name == "missing caller input" {
		proofHash = ""
	}
	status, err := node.RetireProvenBuild(context.Background(), e, exec, proofHash)
	if (err != nil) != tc.wantErr || (!tc.wantErr && status.Status != tc.want) {
		t.Fatalf("retire: %+v, %v", status, err)
	}
	retired, err := e.IsRetired(exec)
	if err != nil || retired != (tc.want == engine.StatusCompleted && !tc.wantErr) {
		t.Fatalf("retirement fence: %v, %v", retired, err)
	}
	if tc.want == engine.StatusCompleted {
		local, err := node.NewLocal(dir, "builder", nil)
		if err != nil {
			t.Fatal(err)
		}
		late := exec
		late.Attempt++
		response, err := local.Call(context.Background(), node.Request{Method: node.MethodRun, Execution: late, Payload: payload}, nil)
		if err != nil || response.Status != engine.StatusFailed || !strings.Contains(response.Detail, "retired") {
			t.Fatalf("late run: %+v, %v", response, err)
		}
	}
}

func setupProvenBuild(t *testing.T, tc provenBuildCase, exec engine.Execution, hash string) *engine.Engine {
	t.Helper()
	dir := t.TempDir()
	if tc.name == "legacy matching effect" {
		if err := engine.NewJournal("builder").Save(engine.JournalPath(dir), &secrets.Registry{}); err != nil {
			t.Fatal(err)
		}
	}
	e, err := engine.New(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tc.setup != nil {
		tc.setup(t, e)
	}
	if tc.name != "missing" && tc.name != "terminal task alone" {
		record := engine.EffectRecord{OperationID: string(exec.OperationID), Step: exec.Step, ExecutionID: exec.ExecutionID(), InputsHash: hash, AppliedAttempt: exec.Attempt, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if tc.name == "wrong effect input" {
			record.InputsHash = strings.Repeat("a", 64)
		}
		if tc.name == "wrong execution" {
			record.ExecutionID = "other"
		}
		if err := os.MkdirAll(filepath.Dir(e.EffectRecordPath(exec)), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.SaveEffectRecord(e.EffectRecordPath(exec), record); err != nil {
			t.Fatal(err)
		}
	}
	modifyProvenBuild(t, tc.name, e, exec, hash)
	return e
}

func modifyProvenBuild(t *testing.T, name string, e *engine.Engine, exec engine.Execution, hash string) {
	t.Helper()
	switch name {
	case "conflicting journal":
		j := engine.NewJournal("builder")
		j.Tasks[exec.EffectKey()] = engine.Task{OperationID: "other", Step: exec.Step, Attempt: exec.Attempt, ExecutionID: exec.ExecutionID(), InputsHash: hash, Status: engine.TaskCompleted}
		if err := j.Save(e.JournalPath(), &secrets.Registry{}); err != nil {
			t.Fatal(err)
		}
	case "corrupt journal":
		if err := os.WriteFile(e.JournalPath(), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "corrupt effect":
		if err := os.WriteFile(e.EffectRecordPath(exec), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "uncertain marker":
		sum := sha256.Sum256([]byte(exec.EffectKey()))
		if err := os.WriteFile(filepath.Join(e.StateDir, "build-applying-"+hex.EncodeToString(sum[:])), []byte("uncertain"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegrationRetireProvenBuildLockAndPreApplyHistory(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_5123456789abcdef0123456789abcdef"), Attempt: 1, Step: "platform-pack-03-v1"}
	hash := engine.InputsHash(exec.Step, []byte("inputs"))
	for _, legacy := range []bool{false, true} {
		testProvenBuildLockAndHistory(t, exec, hash, legacy)
	}
}

func testProvenBuildLockAndHistory(t *testing.T, exec engine.Execution, hash string, legacy bool) {
	t.Helper()
	dir := t.TempDir()
	if legacy {
		if err := engine.NewJournal("builder").Save(engine.JournalPath(dir), &secrets.Registry{}); err != nil {
			t.Fatal(err)
		}
	}
	e, err := engine.New(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := engine.NewJournal("builder")
	j.Tasks[exec.EffectKey()] = engine.Task{OperationID: string(exec.OperationID), Step: exec.Step, Attempt: exec.Attempt, ExecutionID: exec.ExecutionID(), InputsHash: hash, Status: engine.TaskRunning, Phase: engine.PhaseChecking}
	if err := j.Save(e.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	release, err := engine.TryEffectLock(context.Background(), dir, exec)
	if err != nil {
		t.Fatal(err)
	}
	status, err := e.RetireProvenBuild(context.Background(), exec, hash)
	release()
	if err != nil || status.Status != engine.StatusRunning {
		t.Fatalf("held lock: %+v, %v", status, err)
	}
	retired, err := e.IsRetired(exec)
	if err != nil || retired {
		t.Fatalf("fenced while locked: %v, %v", retired, err)
	}
	status, err = e.RetireProvenBuild(context.Background(), exec, hash)
	want := engine.StatusCompleted
	if legacy {
		want = engine.StatusStale
	}
	if err != nil || status.Status != want {
		t.Fatalf("legacy=%v: %+v, %v", legacy, status, err)
	}
}

func TestIntegrationRetireProvenBuildEngineRecord(t *testing.T) {
	dir := t.TempDir()
	e, err := engine.New(dir, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.Execution{OperationID: contract.OperationID("op_6123456789abcdef0123456789abcdef"), Attempt: 1, Step: node.DiagnosticStepID(1)}
	step, err := node.NewDiagnosticStep(exec, node.DiagnosticPayload{Index: 1, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	status, err := e.Run(context.Background(), step, exec, nil)
	if err != nil || status.Status != engine.StatusCompleted {
		t.Fatalf("run: %+v, %v", status, err)
	}
	status, err = e.RetireProvenBuild(context.Background(), exec, engine.InputsHash(exec.Step, inputs))
	if err != nil || status.Status != engine.StatusCompleted {
		t.Fatalf("retire engine-produced record: %+v, %v", status, err)
	}
	exec.Attempt++
	_, err = e.Run(context.Background(), step, exec, nil)
	if err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("late engine run: %v", err)
	}
}
