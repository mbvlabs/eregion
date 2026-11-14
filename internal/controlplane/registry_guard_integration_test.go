package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
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

type registryMarkerStep struct {
	id    string
	value string
}

func (s *registryMarkerStep) ID() string                    { return s.id }
func (*registryMarkerStep) Describe() string                { return "Exercise node result serialization" }
func (*registryMarkerStep) Inputs() ([]byte, error)         { return []byte("fixed-input"), nil }
func (*registryMarkerStep) CancelBoundary() engine.Boundary { return engine.BoundaryBeforeApply }
func (*registryMarkerStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{}, nil
}
func (s *registryMarkerStep) Apply(_ context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.id, Message: "marker serialization observed"})
	}
	return engine.ApplyResult{Result: map[string]any{"redaction_context": s.value, "stdout": "private-original-output"}}, nil
}

// This uses an actual dispatcher, node journal and PostgreSQL event service;
// the registered step only supplies a result shape unavailable in this branch.
func TestIntegrationNodePreservesValidatedRedactionContext(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, f.op.ID); err != nil {
		t.Fatal(err)
	}
	registry := node.NewStepRegistry()
	registry.Register(func(id string) bool { return strings.HasPrefix(id, "integration-marker-") }, func(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
		var payload struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		return &registryMarkerStep{id: exec.Step, value: payload.Value}, nil
	})
	dispatcher, err := node.NewDispatcherWithSteps(t.TempDir(), "local", &secrets.Registry{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	for index, value := range []string{"original", "not_required", "unavailable", "invalid-original"} {
		execution := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: fmt.Sprintf("integration-marker-%d-v1", index)}
		raw, _ := json.Marshal(map[string]string{"value": value})
		var reportErr error
		response, err := dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw, Secrets: map[string]string{"FIRST": "original", "SECOND": "a"}}, func(event node.Event) {
			if reportErr == nil {
				reportErr = f.service.appendNodeEvents(ctx, f.op.ID, 1, []node.Event{event})
			}
		})
		var result struct {
			Context string `json:"redaction_context"`
			Stdout  string `json:"stdout"`
		}
		if err != nil || reportErr != nil || response.Status != engine.StatusCompleted || json.Unmarshal(response.Result, &result) != nil {
			t.Fatalf("response=%+v err=%v report=%v", response, err, reportErr)
		}
		if index < 3 && result.Context != value || index == 3 && strings.Contains(result.Context, "original") || strings.Contains(result.Stdout, "original") {
			t.Fatalf("unprotected result=%+v", result)
		}
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1 AND event->>'kind'='log'`, contract.OperationID(f.op.ID)).Scan(&count); err != nil || count == 0 {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

// Exercise real PostgreSQL event writes and the same registered node steps and
// engine used by the dispatcher. The shared registry models an engine runtime
// retaining an earlier registered value; Dispatcher.Call itself constructs a
// fresh registry for each request.
func TestIntegrationSharedRegistryBlocksSplitLogsAndDockerTail(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	other := platformSubmit(t, f.service, platform.KindDeploy, "env/shop/production", "registry-guard-second", map[string]any{})
	for _, id := range []string{string(f.op.ID), string(other.ID)} {
		if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	plan, err := platform.PlanDeployment(f.env.App, f.env.Name, f.env.Revision.RevisionState, f.release, f.dep)
	if err != nil || len(plan) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	item := plan[0]
	values := []any{strings.Repeat("a", 64), "/" + item.Name, "sha256:" + strings.Repeat("b", 64), item.Labels, map[string]any{"Status": "exited", "Running": false, "StartedAt": "2026-11-14T12:00:00Z", "FinishedAt": "2026-11-14T12:00:01Z", "ExitCode": 7, "OOMKilled": false}}
	var lines []string
	for _, value := range values {
		raw, _ := json.Marshal(value)
		lines = append(lines, string(raw))
	}
	releaseLabels, _ := json.Marshal(platform.ReleaseCommandLabels(f.env.App, f.env.Name, f.release, f.dep))
	releaseState := `{"Status":"exited","Running":false,"StartedAt":"2026-11-14T12:00:00Z","FinishedAt":"2026-11-14T12:00:01Z","ExitCode":0,"OOMKilled":false}`
	for name, data := range map[string]string{
		"inspect":         strings.Join(lines, "\n") + "\n",
		"whole":           "known-prefix\nknown-suffix\n",
		"tail":            "known-suffix\n",
		"release-inspect": fmt.Sprintf("%q\n%s\n%s\n", strings.Repeat("a", 64), releaseLabels, releaseState),
		"docker": `#!/bin/sh
printf '%s\n' "$1" >> "$EVIDENCE_FIXTURE/trace"
case "$1" in
version) cat "$EVIDENCE_FIXTURE/whole";;
inspect) if [ "$2" = "--type=image" ]; then echo '<no value>'; elif [ "$2" = "--type=container" ]; then cat "$EVIDENCE_FIXTURE/release-inspect"; else cat "$EVIDENCE_FIXTURE/inspect"; fi;;
create) printf '%064d\n' 0 | tr 0 a;;
start) echo ok;;
rm) exit 0;;
logs) cat "$EVIDENCE_FIXTURE/tail";;
*) exit 1;;
esac
`,
	} {
		mode := os.FileMode(0600)
		if name == "docker" {
			mode = 0700
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("EVIDENCE_FIXTURE", dir)
	registry := &secrets.Registry{}
	registry.Add("known-prefix\nknown-suffix")
	stateDir := t.TempDir()
	runner, err := engine.New(stateDir, "local", registry)
	if err != nil {
		t.Fatal(err)
	}
	steps := node.DefaultRegistry()
	for index, id := range []engine.Execution{
		{OperationID: f.op.ID, Attempt: 1, Step: "platform-argv-registryone-v1"},
		{OperationID: other.ID, Attempt: 1, Step: "platform-argv-registrytwo-v1"},
	} {
		payload, _ := json.Marshal(platform.ArgvPayload{Step: id.Step, Argv: []string{"docker", "version"}})
		step, err := steps.Build(id, payload)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			runner.SecretValues = map[string]string{"OLD": "known-prefix\nknown-suffix"}
		} else {
			runner.SecretValues = map[string]string{}
		}
		var reportErr error
		status, err := runner.Run(ctx, step, id, func(event engine.Event) {
			if reportErr == nil {
				reportErr = f.service.appendNodeEvents(ctx, id.OperationID, 1, []node.Event{{Kind: event.Kind, Step: event.Step, ExecutionID: id.ExecutionID(), Message: event.Message}})
			}
		})
		if err != nil || reportErr != nil || status.Status != engine.StatusCompleted || strings.Contains(string(status.Result), "known-prefix") || strings.Contains(string(status.Result), "known-suffix") {
			t.Fatalf("execution %d status=%+v err=%v report=%v", index, status, err, reportErr)
		}
	}
	capture := engine.Execution{OperationID: other.ID, Attempt: 1, Step: "platform-candidate-diagnostic-91-v1"}
	payload := platform.CandidateDiagnosticPayload{Step: capture.Step, App: f.env.App, Environment: f.env.Name, ReleaseID: f.release, DeploymentID: f.dep, Deadline: time.Now().Add(time.Minute), Containers: []platform.CandidateDiagnosticContainer{{Name: item.Name, Process: item.Process.Name, Kind: item.Process.Kind, Replica: item.Replica}}}
	raw, _ := json.Marshal(payload)
	step, err := steps.Build(capture, raw)
	if err != nil {
		t.Fatal(err)
	}
	status, err := runner.Run(ctx, step, capture, nil)
	var result platform.CandidateDiagnosticResult
	if err != nil || status.Status != engine.StatusCompleted || json.Unmarshal(status.Result, &result) != nil || !result.Unavailable || len(result.Observations) != 1 || len(result.Lines) != 0 {
		t.Fatalf("tail status=%+v result=%+v err=%v", status, result, err)
	}
	command := engine.Execution{OperationID: other.ID, Attempt: 1, Step: "platform-command-01-v1"}
	commandPayload := platform.ReleaseCommandPayload{Step: command.Step, Name: "release-registry-test", Image: "sha256:" + strings.Repeat("b", 64), Network: "release-registry-network", Command: []string{"/bin/true"}, Labels: platform.ReleaseCommandLabels(f.env.App, f.env.Name, f.release, f.dep)}
	raw, _ = json.Marshal(commandPayload)
	step, err = steps.Build(command, raw)
	if err != nil {
		t.Fatal(err)
	}
	commandStatus, err := runner.Run(ctx, step, command, nil)
	var commandResult platform.StepExecutionEvidence
	if err != nil || commandStatus.Status != engine.StatusCompleted || json.Unmarshal(commandStatus.Result, &commandResult) != nil || commandResult.Container == nil || commandResult.Container.Availability != "partial" || commandResult.Container.Logs != "" {
		t.Fatalf("release tail status=%+v result=%+v err=%v", commandStatus, commandResult, err)
	}
	trace, err := os.ReadFile(filepath.Join(dir, "trace"))
	if err != nil || strings.Contains(string(trace), "logs\n") {
		t.Fatalf("unsafe docker logs call: %s err=%v", trace, err)
	}
	journal, err := os.ReadFile(engine.JournalPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	var events string
	if err := f.pool.QueryRow(ctx, `SELECT coalesce(string_agg(event::text,E'\n'),'') FROM eregion_operation_events WHERE operation_id=ANY($1)`, []string{string(f.op.ID), string(other.ID)}).Scan(&events); err != nil {
		t.Fatal(err)
	}
	for label, text := range map[string]string{"journal": string(journal), "events": events, "capture": string(status.Result), "command": string(commandStatus.Result)} {
		if strings.Contains(text, "known-prefix") || strings.Contains(text, "known-suffix") {
			t.Fatalf("%s leaked a registered fragment: %s", label, text)
		}
	}
}
