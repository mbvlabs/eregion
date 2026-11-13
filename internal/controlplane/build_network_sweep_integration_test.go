package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// sweepRetentionTransport runs the actual GC node effect through the engine.
// Other retention effects and inventory use the existing service fixture.
type sweepRetentionTransport struct {
	*retentionFakeTransport
	engine *engine.Engine
	status engine.TaskStatus
	gcErr  error
}

var errScriptRefused = errors.New("fixture refuses all script execution")

// refusingScriptExecutor never starts a subprocess, regardless of script or step.
type refusingScriptExecutor struct {
	steps []string
}

func (r *refusingScriptExecutor) Run(_ context.Context, step, _ string, _ map[string]string, _ engine.Reporter) error {
	r.steps = append(r.steps, step)
	return errScriptRefused
}

func (t *sweepRetentionTransport) Call(ctx context.Context, request node.Request, report func(node.Event)) (node.Response, error) {
	if !strings.HasPrefix(request.Execution.Step, "platform-retention-gc-") {
		return t.retentionFakeTransport.Call(ctx, request, report)
	}
	if request.Method == node.MethodStatus {
		status, err := t.engine.Status(ctx, request.Execution)
		if err != nil {
			return node.Response{}, err
		}
		return node.Response{Status: status.Status, InputsHash: status.InputsHash, ExecutionID: status.ExecutionID, Result: status.Result}, nil
	}
	step, err := node.DefaultRegistry().Build(request.Execution, request.Payload)
	if err != nil {
		return node.Response{}, err
	}
	status, err := t.engine.Run(ctx, step, request.Execution, nil)
	t.status = status
	t.gcErr = err
	return node.Response{
		Status: status.Status, ExecutionID: status.ExecutionID, InputsHash: status.InputsHash,
		Attempt: status.Attempt, ErrorCode: status.ErrorCode, Retryable: status.Retryable, Detail: status.Detail,
	}, nil
}

// TestIntegrationRetentionSweepOwnedBuildNetworks runs an accepted retention
// operation through the real worker and GC effect. The test-owned executable
// models Docker metadata only. An independent executor refuses every script,
// including the GC shell, after the sweep without relying on cancellation.
func TestIntegrationRetentionSweepOwnedBuildNetworks(t *testing.T) {
	fixture := newRetentionFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	eng, err := engine.New(root, "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	scripts := &refusingScriptExecutor{}
	eng.Scripts = scripts
	type candidate struct {
		execution      engine.Execution
		status         string
		networkAttempt uint32
		recordHash     string
		owner          string
	}
	var candidates []candidate
	for i, state := range []string{engine.TaskCompleted, engine.TaskCompleted, engine.TaskFailed, engine.TaskDeferred, engine.TaskRunning, engine.TaskCompleted, engine.TaskCompleted, engine.TaskCompleted} {
		releaseID := insertRetentionWorkerRelease(t, fixture.pool, fixture.app, fixture.env,
			fmt.Sprintf("127.0.0.1:5000/test:image%d", i), "sha256:"+strings.Repeat("a", 64), time.Now().UTC())
		var op contract.OperationID
		if err := fixture.pool.QueryRow(ctx, `SELECT operation_id FROM eregion_platform_releases WHERE id=$1`, releaseID).Scan(&op); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate{execution: engine.Execution{OperationID: op, Step: "platform-pack-02-v1", Attempt: 1}, status: state, networkAttempt: 1, recordHash: strings.Repeat("a", 64), owner: "pack"})
	}
	candidates[5].networkAttempt = 2                   // newer than completed attempt
	candidates[6].recordHash = strings.Repeat("b", 64) // inconsistent durable record
	candidates[7].owner = "foreign"
	journal := engine.NewJournal("local")
	for _, item := range candidates {
		execution := item.execution
		journal.Tasks[execution.EffectKey()] = engine.Task{
			OperationID: string(execution.OperationID), Step: execution.Step, Attempt: 1,
			ExecutionID: execution.ExecutionID(), InputsHash: strings.Repeat("a", 64),
			Status: item.status, AppliedAttempt: 1, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		record := engine.EffectRecord{OperationID: string(execution.OperationID), Step: execution.Step,
			InputsHash: item.recordHash, AppliedAttempt: 1, ExecutionID: execution.ExecutionID(), CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		path := eng.EffectRecordPath(execution)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.SaveEffectRecord(path, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Save(eng.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	locked, err := engine.TryEffectLock(ctx, root, candidates[1].execution)
	if err != nil {
		t.Fatal(err)
	}
	defer locked()
	name := func(item candidate) string {
		sum := sha256.Sum256([]byte(string(item.execution.OperationID) + "\x00" + item.execution.Step + "\x00" + strconv.FormatUint(uint64(item.networkAttempt), 10)))
		return "eregion-build-" + hex.EncodeToString(sum[:8])
	}
	registryID, foreignID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
	bin := t.TempDir()
	logPath := filepath.Join(bin, "effects.log")
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncase \"$1:$2\" in\n")
	fmt.Fprintf(&script, "inspect:--type=container) printf '%%s\\n' %s ;;\n", quote(registryID+"|/eregion-registry|"+platform.RegistryImage))
	script.WriteString("network:ls) printf '%s\\n'")
	for _, item := range candidates {
		fmt.Fprintf(&script, " %s", quote(name(item)))
	}
	// The sentinel fails its metadata read after all candidates, exercising
	// the incomplete-sweep path without controlling whether the shell runs.
	script.WriteString(" sentinel ;;\nnetwork:inspect) case \"$5\" in\n")
	for _, item := range candidates {
		details, err := json.Marshal(map[string]any{
			"Name": name(item), "Labels": map[string]string{
				"io.eregion.build.owner": item.owner, "io.eregion.build.operation": string(item.execution.OperationID),
				"io.eregion.build.step": item.execution.Step, "io.eregion.build.attempt": strconv.FormatUint(uint64(item.networkAttempt), 10),
			},
			"Containers": map[string]any{registryID: map[string]string{"Name": "eregion-registry"}, foreignID: map[string]string{"Name": "foreign"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&script, "%s) printf '%%s\\n' %s ;;\n", quote(name(item)), quote(string(details)))
	}
	script.WriteString("*) exit 9 ;; esac ;;\nps:-aq) exit 0 ;;\n")
	fmt.Fprintf(&script, "network:disconnect) printf 'disconnect %%s %%s\\n' \"$3\" \"$4\" >> %s ;;\n", quote(logPath))
	fmt.Fprintf(&script, "network:rm) printf 'remove %%s\\n' \"$3\" >> %s ;;\n", quote(logPath))
	script.WriteString("*) exit 9 ;; esac\n")
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	transport := &sweepRetentionTransport{retentionFakeTransport: fixture.transport, engine: eng}
	fixture.service.transport = transport
	op := submitRetentionWorkerPrune(t, fixture.service, "owned-network-sweep")
	err = (&retentionWorker{service: fixture.service}).Work(ctx, retentionWorkerJob(op.ID, 1, 8))
	if err == nil {
		t.Fatal("retryable refused GC unexpectedly settled without a retry")
	}
	if errors.Is(err, errNodeStepPermanentFailure) {
		t.Fatalf("retryable GC failure classified as permanent: %v", err)
	}
	if !errors.Is(transport.gcErr, errScriptRefused) || transport.status.Status != engine.StatusFailed || !transport.status.Retryable {
		t.Fatalf("GC refusal not reported as retryable failure: status=%+v error=%v", transport.status, transport.gcErr)
	}
	if len(scripts.steps) != 1 || scripts.steps[0] != platform.RetentionGCStepID(1) {
		t.Fatalf("unexpected script attempts: %v", scripts.steps)
	}
	run, exists, err := fixture.service.retentionRunForOperation(ctx, op.ID)
	if err != nil || !exists || run.Status != retention.RunFailed || run.GCGeneration != 1 {
		t.Fatalf("retention run not failed: exists=%v run=%+v err=%v", exists, run, err)
	}
	var opState contract.State
	if err := fixture.pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, op.ID).Scan(&opState); err != nil || opState != contract.Running {
		t.Fatalf("retryable operation state=%q err=%v", opState, err)
	}
	var gcFailure bool
	for _, item := range run.Errors {
		if item.Stage == "registry_gc" {
			gcFailure = true
		}
	}
	for _, item := range run.Removed {
		if item.Kind == retention.ItemRegistryGC {
			t.Fatal("refused GC was recorded as completed")
		}
	}
	pending, err := fixture.service.retentionGCPending(ctx)
	if err != nil || !pending || !gcFailure {
		t.Fatalf("GC failure/pending not retained: failure=%v pending=%v err=%v", gcFailure, pending, err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "disconnect " + name(candidates[0]) + " " + registryID + "\nremove " + name(candidates[0]) + "\n"
	if string(logged) != want {
		t.Fatalf("sweep touched an ineligible or foreign resource: got %q want %q", logged, want)
	}
}
