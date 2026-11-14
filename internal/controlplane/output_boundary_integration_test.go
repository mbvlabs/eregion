package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestIntegrationOutputBoundary(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "output-boundary", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = run.Service.mutate(ctx, op.ID, func(tx pgx.Tx, current *contract.Operation) error {
		return appendEvent(ctx, tx, current, contract.Event{Kind: "log", Stage: "status", Message: "stdout secret"})
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err = run.Service.Get(ctx, testActor, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	event := page.Events[len(page.Events)-1]
	event.NodeEvidence = &contract.NodeExecutionEvidence{ExecutionID: string(op.ID) + ":1:status-v1", Stage: "status", Stdout: "stdout", Stderr: "status", Container: &contract.NodeContainerEvidence{Logs: "stdout"}}
	event.Health = &contract.HealthObservation{Detail: "status"}
	event.Result = &contract.ActionResult{Message: "stdout", Excerpt: "status", History: contract.Cursor{OperationID: op.ID, After: 9007199254740993}}
	op.LatestResult = event.Result
	op.LatestResult.Data = json.RawMessage(`{"status":"stdout","number":9007199254740993}`)
	op.LastSequence = 9007199254740993
	reg := &secrets.Registry{}
	reg.Add("a", "stdout", "status")
	var buf bytes.Buffer
	w := output.Writer{Out: &buf, Mode: output.JSON, Secrets: reg}
	if err := w.Write(contract.Success("operations get", op, nil), ""); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data contract.Operation `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.ID != op.ID || response.Data.LastSequence != op.LastSequence || response.Data.LatestResult.History.After != 9007199254740993 ||
		response.Data.LatestResult.Message != secrets.Placeholder || response.Data.LatestResult.Excerpt != secrets.Placeholder {
		t.Fatalf("operation boundary: %s", buf.String())
	}
	if op.LatestResult.Message != "stdout" {
		t.Fatal("mutated input")
	}
	if string(response.Data.LatestResult.Data) != string(op.LatestResult.Data) {
		t.Fatalf("source-sanitized data changed: %s", buf.String())
	}
	buf.Reset()
	stream := output.NewStream(&buf, reg)
	if err := stream.Event(event); err != nil {
		t.Fatal(err)
	}
	var record output.Record
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	got := record.Event
	if got.OperationID != op.ID || got.Stage != "status" || got.NodeEvidence.ExecutionID != event.NodeEvidence.ExecutionID ||
		got.NodeEvidence.Stdout != secrets.Placeholder || got.NodeEvidence.Stderr != secrets.Placeholder ||
		got.NodeEvidence.Container.Logs != secrets.Placeholder || got.Health.Detail != secrets.Placeholder || got.Result.History.After != 9007199254740993 {
		t.Fatalf("event boundary: %s", buf.String())
	}
	event.Sequence++
	event.Kind, event.Stage = "step", "action-output"
	event.ActionOutputStatus = "available"
	event.ActionOutput = json.RawMessage(`{"status":"stdout","number":9007199254740993}`)
	if err := stream.Event(event); err != nil {
		t.Fatal(err)
	}
	if err := stream.Finish(contract.Success("follow", output.FollowResult{Operation: op, Cursor: 9007199254740993}, nil)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var actionRecord output.Record
	if len(lines) != 3 {
		t.Fatalf("stream records: %s", buf.String())
	}
	if err := json.Unmarshal([]byte(lines[1]), &actionRecord); err != nil || string(actionRecord.Event.ActionOutput) != string(event.ActionOutput) {
		t.Fatalf("action output boundary: %s %v", lines[1], err)
	}
	var finished struct {
		Result struct {
			Data output.FollowResult `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &finished); err != nil || finished.Result.Data.Operation.ID != op.ID || finished.Result.Data.Cursor != 9007199254740993 {
		t.Fatalf("stream result boundary: %s %v", lines[2], err)
	}
	buf.Reset()
	if err := w.Write(contract.Success("operations list", operations.ListPage{Items: []contract.Operation{op}, Limit: 1}, nil), ""); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data operations.ListPage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &listed); err != nil || len(listed.Data.Items) != 1 || listed.Data.Items[0].ID != op.ID {
		t.Fatalf("list boundary: %s %v", buf.String(), err)
	}
	buf.Reset()
	follow := output.FollowResult{Operation: op, State: op.State, Cursor: 9007199254740993}
	if err := w.Write(contract.Success("follow", follow, nil), ""); err != nil {
		t.Fatal(err)
	}
	var followed struct {
		Data output.FollowResult `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &followed); err != nil || followed.Data.Operation.ID != op.ID || followed.Data.Cursor != follow.Cursor {
		t.Fatalf("follow boundary: %s %v", buf.String(), err)
	}
	buf.Reset()
	known := map[string]any{"operation_id": string(op.ID), "state": op.State, "result": op.LatestResult, "cursor": "9007199254740993"}
	details := make(map[string]any)
	for k, v := range known {
		details[k] = v
	}
	details["stdout"] = "status"
	fault := &contract.Fault{Code: contract.PreconditionFailed, Message: "stdout", Details: details, FollowDetails: &contract.FollowFaultDetails{Values: known}}
	if err := w.Write(contract.Failure("follow", fault), ""); err != nil {
		t.Fatal(err)
	}
	var failed struct {
		Error struct {
			Details map[string]json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	if string(failed.Error.Details["operation_id"]) != `"`+string(op.ID)+`"` || string(failed.Error.Details["cursor"]) != `"9007199254740993"` ||
		failed.Error.Details["stdout"] != nil || failed.Error.Details["result"] == nil {
		t.Fatalf("follow fault boundary: %s", buf.String())
	}
	buf.Reset()
	if err := w.Write(contract.Success("unknown", map[string]any{"stdout": "status"}, nil), ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"stdout"`) || strings.Contains(buf.String(), `"status"`) {
		t.Fatalf("unknown was trusted: %s", buf.String())
	}
}
