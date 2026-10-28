package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/viewer"
	"github.com/spf13/cobra"
)

func TestFollowErrorClassifiesOutcomes(t *testing.T) {
	for _, test := range []struct {
		name     string
		state    contract.State
		code     contract.Code
		detached bool
		err      error
		want     int
		message  string
	}{
		{name: "success", state: contract.Succeeded, want: 0},
		{name: "failed with code", state: contract.Failed, code: contract.Unavailable, want: 6, message: "operation failed"},
		{name: "failed without code", state: contract.Failed, want: 5, message: "operation failed"},
		{name: "failed unknown code", state: contract.Failed, code: "future_code", want: 5, message: "operation failed"},
		{name: "failed is not detach", state: contract.Failed, code: contract.Cancelled, want: 5, message: "operation failed"},
		{name: "remote cancelled", state: contract.Canceled, want: 130, message: "operation was cancelled"},
		{name: "detached", state: contract.Running, detached: true, err: context.Canceled, want: 130, message: "viewer detached; the operation was not cancelled"},
		{name: "observation failure", state: contract.Failed, err: contract.NewFault(contract.Unavailable, "poll unavailable", "Retry observation."), want: 6, message: "poll unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			summary := viewer.Summary{OperationID: followTestID, State: test.state, ErrorCode: test.code, Detached: test.detached}
			err := followError(summary, test.err)
			if got := contract.ExitCode(err); got != test.want {
				t.Fatalf("exit=%d want=%d, error=%v", got, test.want, err)
			}
			if err != nil {
				fault := contract.Classify(err)
				if fault.Message != test.message || fault.Details["operation_id"] != string(followTestID) {
					t.Fatalf("fault=%+v", fault)
				}
			}
		})
	}
}

// Exercise the actual runner and all three noninteractive presenters using an
// in-memory backend. Nothing opens a socket or starts a fixture.
func TestRunnerFollowReturnsTerminalOutcome(t *testing.T) {
	for _, state := range []contract.State{contract.Succeeded, contract.Failed, contract.Canceled} {
		for _, mode := range []string{"plain", "json", "ndjson"} {
			t.Run(string(state)+"/"+mode, func(t *testing.T) {
				at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
				op := guidedOperation(followTestID, platform.KindDeploy, "env/api/production", state, 1, at)
				wantExit := 0
				if state == contract.Failed {
					op.ErrorCode = contract.PreconditionFailed
					wantExit = 5
				}
				if state == contract.Canceled {
					wantExit = 130
				}
				backend := &guidedCreateBackend{
					operations: map[contract.OperationID]contract.Operation{op.ID: op},
					events:     map[contract.OperationID][]contract.Event{op.ID: {guidedStateEvent(op, 1, state, at)}},
				}
				a := newApp()
				cmd, _, err := a.root.Find([]string{"operations", "follow"})
				if err != nil {
					t.Fatal(err)
				}
				cmd.RunE = func(cmd *cobra.Command, _ []string) error {
					a.started = true
					res, err := a.followOperation(cmd, backend, op.ID)
					res.command = cmd.CommandPath()
					a.result = res
					return err
				}
				args := []string{"operations", "follow", string(op.ID), "--output", mode}
				var stdout, stderr bytes.Buffer
				exit := runApp(context.Background(), args, strings.NewReader(""), &stdout, &stderr, &secrets.Registry{}, a)
				if exit != wantExit {
					t.Fatalf("exit=%d want=%d, stdout=%s stderr=%s", exit, wantExit, stdout.String(), stderr.String())
				}
				if mode == "plain" {
					if !strings.Contains(stdout.String(), string(op.ID)) || !strings.Contains(stdout.String(), string(state)) {
						t.Fatalf("terminal summary missing: %s", stdout.String())
					}
					return
				}
				var envelope contract.Envelope
				if mode == "json" {
					envelope = decodeOne(t, stdout.String())
				} else {
					decoder := json.NewDecoder(&stdout)
					results := 0
					for {
						var record struct {
							Type   string            `json:"type"`
							Result contract.Envelope `json:"result"`
						}
						err := decoder.Decode(&record)
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						if record.Type == "result" {
							results++
							envelope = record.Result
						}
					}
					if results != 1 {
						t.Fatalf("got %d result records", results)
					}
				}
				if envelope.OK != (state == contract.Succeeded) {
					t.Fatalf("envelope=%+v", envelope)
				}
				if state != contract.Succeeded {
					if envelope.Error == nil || envelope.Error.Details["operation_id"] != string(op.ID) || envelope.Error.Details["state"] != string(state) {
						t.Fatalf("missing terminal failure identity: %+v", envelope)
					}
					if state == contract.Canceled && strings.Contains(envelope.Error.Message, "detached") {
						t.Fatalf("remote cancellation described as detach: %+v", envelope.Error)
					}
				}
				if stderr.Len() != 0 {
					t.Fatalf("machine output wrote stderr: %s", stderr.String())
				}
			})
		}
	}
}

func TestFailedBuildLogFollowKeepsTerminalSummary(t *testing.T) {
	for _, state := range []contract.State{contract.Failed, contract.Canceled} {
		t.Run(string(state), func(t *testing.T) {
			at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			op := guidedOperation(followTestID, platform.KindDeploy, "env/api/production", state, 1, at)
			wantExit := 130
			if state == contract.Failed {
				op.ErrorCode = contract.PreconditionFailed
				wantExit = 5
			}
			backend := &guidedCreateBackend{
				operations: map[contract.OperationID]contract.Operation{op.ID: op},
				events:     map[contract.OperationID][]contract.Event{op.ID: {guidedStateEvent(op, 1, state, at)}},
			}
			cmd, out, errOut := guidedCreateCommand(t)
			a := &app{secrets: &secrets.Registry{}}
			res, err := a.followBuildLogs(cmd, backend, platform.BuildRecord{OperationID: op.ID})
			if contract.ExitCode(err) != wantExit || !res.streamed {
				t.Fatalf("exit=%d streamed=%v error=%v", contract.ExitCode(err), res.streamed, err)
			}
			if !strings.Contains(out.String(), string(state)) || !strings.Contains(out.String(), string(op.ID)) {
				t.Fatalf("terminal summary missing: %s", out.String())
			}
			if errOut.Len() != 0 || strings.Contains(out.String(), "resume:") {
				t.Fatalf("terminal build shown as observation failure: stdout=%s stderr=%s", out.String(), errOut.String())
			}
		})
	}
}
