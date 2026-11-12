package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// followingCommands is the product list of commands that follow an accepted
// operation. The table keeps the metadata gate and the runner selection in
// lockstep; a new follow command must be added here and marked with
// followingMeta.
var followingCommands = []struct {
	name string
	path string
	args []string
}{
	{name: "definition apply", path: "eregion apply", args: []string{"apply", "--from", "testdata/definition.toml", "--app", "api", "--environment", "production", "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key"}},
	{name: "env update definition", path: "eregion env update", args: []string{"env", "update", "api", "production", "--from", "testdata/definition.toml", "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key"}},
	{name: "deploy", path: "eregion deploy", args: []string{"deploy", "api", "production"}},
	{name: "prune", path: "eregion prune", args: []string{"prune", "--confirm", "main"}},
	{name: "rollback", path: "eregion rollback", args: []string{"rollback", "api", "production", "--confirm", "api/production"}},
	{name: "restart", path: "eregion restart", args: []string{"restart", "api", "production"}},
	{name: "diagnose", path: "eregion diagnose", args: []string{"diagnose", "op_" + strings.Repeat("b", 32), "--execute", "--confirm", "op_" + strings.Repeat("b", 32), "--", "/app/check"}},
	{name: "build stop", path: "eregion build stop", args: []string{"build", "stop", "bld_" + strings.Repeat("a", 32)}},
	{name: "build retry", path: "eregion build retry", args: []string{"build", "retry", "bld_" + strings.Repeat("a", 32)}},
	{name: "build logs", path: "eregion build logs", args: []string{"build", "logs", "bld_" + strings.Repeat("a", 32)}},
	{name: "app create", path: "eregion app create", args: []string{"app", "create", "--name", "api", "--node", "main", "--git-url", "https://github.com/acme/api"}},
	{name: "app delete", path: "eregion app delete", args: []string{"app", "delete", "api", "--confirm", "api"}},
	{name: "env create", path: "eregion env create", args: []string{"env", "create", "--app", "api", "--name", "production", "--node", "main", "--git-url", "https://github.com/acme/api"}},
	{name: "env delete", path: "eregion env delete", args: []string{"env", "delete", "api", "production", "--confirm", "api/production"}},
	{name: "env protect", path: "eregion env protect", args: []string{"env", "protect", "api", "production"}},
	{name: "env unprotect", path: "eregion env unprotect", args: []string{"env", "unprotect", "api", "production"}},
	{name: "env promote", path: "eregion env promote", args: []string{"env", "promote", "api", "staging"}},
	{name: "domain add", path: "eregion domain add", args: []string{"domain", "add", "api", "production", "hello.example.com"}},
	{name: "domain remove", path: "eregion domain remove", args: []string{"domain", "remove", "api", "production", "hello.example.com", "--confirm", "hello.example.com"}},
	{name: "process scale", path: "eregion process scale", args: []string{"process", "scale", "api", "production", "web", "2"}},
	{name: "resource apply", path: "eregion resource apply", args: []string{"resource", "apply", "api", "production", "db", "--image", "redis:7", "--health-command", "redis-cli", "--health-command", "ping"}},
	{name: "resource attach", path: "eregion resource attach", args: []string{"resource", "attach", "api", "production", "db"}},
	{name: "resource detach", path: "eregion resource detach", args: []string{"resource", "detach", "api", "production", "db"}},
	{name: "resource destroy", path: "eregion resource destroy", args: []string{"resource", "destroy", "api", "production", "db", "--confirm", "api/production/db"}},
	{name: "operations follow", path: "eregion operations follow", args: []string{"operations", "follow", "op_" + strings.Repeat("b", 32)}},
	{name: "logs", path: "eregion logs", args: []string{"logs", "api", "production"}},
}

// Follow flags belong before the diagnostic executable's argv separator.
func followingArgs(args []string, flags ...string) []string {
	for i, arg := range args {
		if arg == "--" {
			result := append([]string{}, args[:i]...)
			result = append(result, flags...)
			return append(result, args[i:]...)
		}
	}
	return append(append([]string{}, args...), flags...)
}

func TestFollowingCommandsAdvertiseNDJSON(t *testing.T) {
	a := newApp()
	for _, test := range followingCommands {
		t.Run(test.name, func(t *testing.T) {
			cmd, err := a.resolve(strings.Fields(test.path)[1:])
			if err != nil {
				t.Fatal(err)
			}
			meta := a.meta[cmd]
			if !containsValue(meta.OutputModes, "ndjson") {
				t.Fatalf("%s does not advertise ndjson: %v", test.path, meta.OutputModes)
			}
			selection := a.followSelection(followingArgs(test.args, "--output", "ndjson"))
			if !selection.Active || !selection.NDJSON {
				t.Fatalf("%s: selection = %+v", test.args, selection)
			}
		})
	}
	ordinary, err := a.resolve([]string{"operations", "get"})
	if err != nil {
		t.Fatal(err)
	}
	if containsValue(a.meta[ordinary].OutputModes, "ndjson") {
		t.Fatalf("ordinary command advertises ndjson: %v", a.meta[ordinary].OutputModes)
	}
	if a.followSelection([]string{"operations", "get", "op_x", "--output", "ndjson"}).Active {
		t.Fatal("operations get selected ndjson")
	}
	allowed := map[string]bool{}
	for _, test := range followingCommands {
		allowed[test.path] = true
	}
	for _, cmd := range walk(a.root) {
		if commandHidden(cmd) {
			continue
		}
		if containsValue(a.meta[cmd].OutputModes, "ndjson") && !allowed[cmd.CommandPath()] {
			t.Fatalf("%s advertises ndjson without appearing in the follow table", cmd.CommandPath())
		}
	}
}

func TestCommandCatalogListsOutputModes(t *testing.T) {
	code, stdout, stderr := invoke(t, "commands", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Command{}
	for _, command := range data.Commands {
		byPath[command.Path] = command
	}
	for _, test := range followingCommands {
		command, ok := byPath[test.path]
		if !ok {
			t.Fatalf("catalog is missing %s", test.path)
		}
		if !containsValue(command.OutputModes, "ndjson") {
			t.Fatalf("%s output_modes = %v", test.path, command.OutputModes)
		}
	}
	if containsValue(byPath["eregion operations get"].OutputModes, "ndjson") {
		t.Fatalf("operations get output_modes = %v", byPath["eregion operations get"].OutputModes)
	}
}

func TestFollowingCommandsRefuseJSONWithNDJSON(t *testing.T) {
	for _, test := range followingCommands {
		t.Run(test.name, func(t *testing.T) {
			args := followingArgs(test.args, "--json", "--output", "ndjson")
			code, stdout, stderr := invoke(t, args...)
			if code != 2 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			envelope := decodeOne(t, stdout)
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.InvalidOutputMode {
				t.Fatalf("envelope=%+v", envelope)
			}
			if !strings.Contains(envelope.Error.Message, "conflicting output modes") || stderr != "" {
				t.Fatalf("stderr=%q fault=%+v", stderr, envelope.Error)
			}
		})
	}
}

func TestNDJSONGuidedCommandsNeverPrompt(t *testing.T) {
	for _, args := range [][]string{
		{"app", "create", "--output", "ndjson"},
		{"env", "create", "--output", "ndjson"},
		{"app", "delete", "api", "--confirm", "api", "--output", "ndjson"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// invoke uses a stdin reader that panics if a prompt reads it.
			code, stdout, stderr := invoke(t, args...)
			if code != 2 || stderr != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			envelope := decodeNDJSONResult(t, stdout)
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
				t.Fatalf("envelope=%+v", envelope)
			}
		})
	}
}

func TestFollowingCommandJSONReturnsAcceptedOperation(t *testing.T) {
	backend := newNDJSONBackend()
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{"deploy", "api", "production", "--json"}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	envelope := decodeOne(t, stdout.String())
	if !envelope.OK {
		t.Fatalf("envelope=%+v", envelope)
	}
	data, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), string(followTestID)) {
		t.Fatalf("accepted operation is missing from data: %s", data)
	}
	if backend.gets != 0 {
		t.Fatalf("--json waited: %d snapshot reads", backend.gets)
	}
}

// ndjsonFollowBackend supplies the accepted operation, its events and the
// preview the real command submits. It never opens a socket or a fixture.
type ndjsonFollowBackend struct {
	Backend
	operations  map[contract.OperationID]contract.Operation
	events      map[contract.OperationID][]contract.Event
	inventory   map[string]json.RawMessage
	firstDeploy bool
	submitted   []string
	gets        int
	state       contract.State
	errorCode   contract.Code
	cancelOn    contract.OperationID
	cancel      context.CancelFunc
	replay      *contract.Operation
	calls       int
	dryRuns     int
}

func newNDJSONBackend() *ndjsonFollowBackend {
	return &ndjsonFollowBackend{
		operations: map[contract.OperationID]contract.Operation{},
		events:     map[contract.OperationID][]contract.Event{},
		inventory:  map[string]json.RawMessage{},
	}
}

// runWithBackend runs the real command tree with the in-memory operator
// backend. It never resolves a saved workspace or touches a server.
func runWithBackend(t *testing.T, ctx context.Context, backend *ndjsonFollowBackend, args []string) (int, string, string) {
	t.Helper()
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(ctx, args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	return exit, stdout.String(), stderr.String()
}

// prepareFollowingBackend supplies the inventory a command reads before it
// submits or follows one operation.
func prepareFollowingBackend(name string, backend *ndjsonFollowBackend) {
	switch name {
	case "deploy":
		raw, _ := json.Marshal(map[string]any{"items": []platform.Environment{{
			App: "api", Name: "production", Node: "main",
		}}})
		backend.inventory["environments"] = raw
	case "env create":
		raw, _ := json.Marshal(map[string]any{"items": []platform.App{{Name: "api"}}})
		backend.inventory["apps"] = raw
	case "build logs":
		raw, _ := json.Marshal(map[string]any{"items": []platform.BuildRecord{{
			ID:          platform.BuildID("bld_" + strings.Repeat("a", 32)),
			OperationID: followTestID,
			App:         "api",
			Environment: "production",
			State:       platform.BuildSucceeded,
			CreatedAt:   time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		}}})
		backend.inventory["builds"] = raw
	case "logs":
		raw, _ := json.Marshal(map[string]any{"items": []platform.Environment{{
			App: "api", Name: "production", ServingDeploymentID: platform.DeploymentID("dep_" + strings.Repeat("a", 32)),
		}}})
		backend.inventory["environments"] = raw
	case "env delete":
		raw, _ := json.Marshal(map[string]any{"items": []platform.Environment{{
			App: "api", Name: "production", Node: "main", Protected: true,
		}, {
			App: "api", Name: "staging", Node: "main", Protected: true,
		}}})
		backend.inventory["environments"] = raw
	case "env protect", "env unprotect":
		raw, _ := json.Marshal(map[string]any{"items": []platform.Environment{{
			App: "api", Name: "production", Node: "main", Protected: false,
		}}})
		backend.inventory["environments"] = raw
	}
}

func followingSubmits(name string) bool {
	switch name {
	case "definition apply", "env update definition", "deploy", "rollback", "restart", "diagnose", "build stop", "build retry",
		"app create", "env create", "env protect", "env unprotect", "domain add", "domain remove", "process scale", "prune",
		"resource apply", "resource attach", "resource detach", "resource destroy":
		return true
	}
	return false
}

func (f *ndjsonFollowBackend) PlanDefinition(_ context.Context, input api.DefinitionPlanInput) (api.DefinitionPlan, error) {
	f.calls++
	f.dryRuns++
	return api.DefinitionPlan{App: input.App, Environment: input.Environment, CanApply: true, Digest: "digest", PlanNonce: "nonce"}, nil
}

func (f *ndjsonFollowBackend) ApplyDefinition(_ context.Context, input api.DefinitionApplyInput) (operations.SubmissionResult, error) {
	f.calls++
	raw, err := f.accept("definition.apply", "env/"+input.Plan.App+"/"+input.Plan.Environment)
	if err != nil {
		return operations.SubmissionResult{}, err
	}
	var op contract.Operation
	err = json.Unmarshal(raw, &op)
	return operations.SubmissionResult{Operation: op}, err
}

func (f *ndjsonFollowBackend) SubmitAction(_ context.Context, kind, target string, _ json.RawMessage, _, _ string, dryRun bool) (json.RawMessage, error) {
	f.calls++
	if dryRun {
		f.dryRuns++
		preview := platform.Preview{Kind: kind, Target: target, DryRun: true, Summary: "plan"}
		if f.replay != nil && kind == f.replay.Kind && target == f.replay.Target {
			preview.Replay = true
			preview.Operation = f.replay
		}
		return json.Marshal(preview)
	}
	if f.replay != nil && kind == f.replay.Kind && target == f.replay.Target {
		f.submitted = append(f.submitted, kind)
		return json.Marshal(*f.replay)
	}
	return f.accept(kind, target)
}

func (f *ndjsonFollowBackend) SubmitActionWithSecrets(ctx context.Context, kind, target string, input json.RawMessage, key, confirm string, dryRun bool, _ map[string]string) (json.RawMessage, error) {
	return f.SubmitAction(ctx, kind, target, input, key, confirm, dryRun)
}

func (f *ndjsonFollowBackend) DiagnosticExecution(_ context.Context, id contract.OperationID, _ []string, _, _ string, _ bool) (json.RawMessage, error) {
	f.calls++
	raw, err := f.accept(platform.KindDiagnosticExecution, "operation/"+string(id))
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Operation json.RawMessage `json:"operation"`
	}{Operation: raw})
}

func (f *ndjsonFollowBackend) NodeInfo(context.Context) (rpcNodeInfoResult, error) {
	return rpcNodeInfoResult{Node: "main", Hostname: "main", Platform: "linux/amd64"}, nil
}

func (f *ndjsonFollowBackend) RetentionPlan(context.Context, string, string, string) (retention.PlanResult, error) {
	f.calls++
	return retention.PlanResult{Node: "main", Items: []retention.Item{}}, nil
}

func (f *ndjsonFollowBackend) accept(kind, target string) (json.RawMessage, error) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	state := f.state
	if state == "" {
		state = contract.Succeeded
	}
	op := guidedOperation(followTestID, kind, target, state, 2, at)
	op.ErrorCode = f.errorCode
	if f.firstDeploy && (kind == platform.KindAppCreate || kind == platform.KindEnvironmentCreate) {
		op.FirstDeployOperationID = ndjsonDeployID
		deploy := guidedOperation(ndjsonDeployID, platform.KindDeploy, "env/api/production", contract.Succeeded, 2, at.Add(time.Second))
		f.operations[ndjsonDeployID] = deploy
		f.events[ndjsonDeployID] = []contract.Event{
			guidedStateEvent(deploy, 1, contract.Queued, at),
			guidedStateEvent(deploy, 2, contract.Succeeded, at.Add(time.Second)),
		}
	}
	f.operations[op.ID] = op
	f.events[op.ID] = []contract.Event{
		guidedStateEvent(op, 1, contract.Queued, at.Add(-time.Second)),
		guidedStateEvent(op, 2, state, at),
	}
	f.submitted = append(f.submitted, kind)
	return json.Marshal(op)
}

func (f *ndjsonFollowBackend) Get(_ context.Context, id contract.OperationID) (json.RawMessage, error) {
	f.gets++
	op, ok := f.operations[id]
	if !ok {
		return nil, contract.NewFault(contract.NotFound, "operation not found", "Inspect the operation list.")
	}
	raw, err := json.Marshal(op)
	if id == f.cancelOn && f.cancel != nil {
		f.cancel()
	}
	return raw, err
}

func (f *ndjsonFollowBackend) Events(_ context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	var page []contract.Event
	for _, event := range f.events[cursor.OperationID] {
		if event.Sequence <= cursor.After {
			continue
		}
		if len(page) >= limit {
			break
		}
		page = append(page, event)
	}
	after := cursor.After
	if len(page) > 0 {
		after = page[len(page)-1].Sequence
	}
	return json.Marshal(operations.EventPage{
		Events: page,
		Cursor: contract.Cursor{OperationID: cursor.OperationID, After: after},
	})
}

func (f *ndjsonFollowBackend) PlatformInventory(_ context.Context, collection string, _ url.Values) (json.RawMessage, error) {
	if raw, ok := f.inventory[collection]; ok {
		return raw, nil
	}
	return json.RawMessage(`{"items":[]}`), nil
}

func (f *ndjsonFollowBackend) Logs(_ context.Context, query platform.LogQuery, emit func(platform.LogLine) error) (platform.LogResult, error) {
	line := platform.LogLine{
		App: query.App, Environment: query.Environment,
		Process: "web", Replica: "1", Message: "hello",
	}
	if err := emit(line); err != nil {
		return platform.LogResult{}, err
	}
	return platform.LogResult{Lines: []platform.LogLine{line}, Count: 1}, nil
}

var ndjsonDeployID = contract.OperationID("op_" + strings.Repeat("d", 32))
var ndjsonReplayID = contract.OperationID("op_" + strings.Repeat("e", 32))

// TestFollowingCommandsStreamNDJSON runs every real command through the
// runner with an in-memory backend. Each successful follow must emit the same
// event records as `operations follow --output ndjson`, followed by exactly one
// terminal result for the command that was invoked.
func TestFollowingCommandsStreamNDJSON(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range followingCommands {
		t.Run(test.name, func(t *testing.T) {
			backend := newNDJSONBackend()
			backend.operations[followTestID] = guidedOperation(followTestID, platform.KindDeploy, "env/api/production", contract.Succeeded, 2, at)
			backend.events[followTestID] = []contract.Event{
				guidedStateEvent(backend.operations[followTestID], 1, contract.Queued, at.Add(-time.Second)),
				guidedStateEvent(backend.operations[followTestID], 2, contract.Succeeded, at),
			}
			prepareFollowingBackend(test.name, backend)
			if test.name == "app create" || test.name == "env create" {
				backend.firstDeploy = true
			}
			a := newApp()
			a.backend = backend
			args := followingArgs(test.args, "--output", "ndjson")
			var stdout, stderr bytes.Buffer
			exit := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
			if test.name == "app delete" || test.name == "env delete" {
				// NDJSON never prompts for the authenticator code.
				if exit != 2 {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
				}
				envelope := decodeNDJSONResult(t, stdout.String())
				if envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
					t.Fatalf("fault=%+v", envelope.Error)
				}
				return
			}
			if test.name == "logs" {
				if exit != 0 || stderr.Len() != 0 {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
				}
				return
			}
			if exit != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
			}
			result, eventOps := decodeNDJSONStream(t, stdout.String())
			if result.Command != test.path || !result.OK {
				t.Fatalf("result=%+v", result)
			}
			switch test.name {
			case "operations follow", "build logs", "logs":
				// These read an operation; they never submit one.
			default:
				if len(backend.submitted) == 0 {
					t.Fatalf("%s followed without submitting an operation", test.path)
				}
			}
			want := []contract.OperationID{followTestID}
			if test.name == "app create" || test.name == "env create" {
				want = append(want, ndjsonDeployID)
			}
			for _, id := range want {
				if eventOps[id] == 0 {
					t.Fatalf("%s emitted no events for %s: %s", test.path, id, stdout.String())
				}
			}
		})
	}
}

func TestFollowingSubmitValidatesViewerBeforeSubmit(t *testing.T) {
	app := newApp()
	wantExit := contract.ExitCode(contract.NewFault(contract.ValidationFailed, "invalid follow option", "fix it"))
	for _, test := range followingCommands {
		if !followingSubmits(test.name) {
			continue
		}
		cmd, err := app.resolve(strings.Fields(test.path)[1:])
		if err != nil {
			t.Fatal(err)
		}
		if flagFor(cmd, "poll-interval") == nil {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			backend := newNDJSONBackend()
			prepareFollowingBackend(test.name, backend)
			args := followingArgs(test.args, "--output", "ndjson", "--poll-interval", "0")
			exit, stdout, stderr := runWithBackend(t, context.Background(), backend, args)
			if exit != wantExit || stderr != "" {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			envelope := decodeNDJSONResult(t, stdout)
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.ValidationFailed {
				t.Fatalf("envelope=%+v", envelope)
			}
			if backend.calls != 0 || len(backend.submitted) != 0 {
				t.Fatalf("invalid follow options reached the backend: calls=%d submitted=%v", backend.calls, backend.submitted)
			}
		})
	}
}

func TestFollowingSubmitNDJSONOutcomes(t *testing.T) {
	for _, outcome := range []struct {
		name      string
		state     contract.State
		errorCode contract.Code
		wantCode  contract.Code
		wantExit  int
	}{
		{name: "failed", state: contract.Failed, errorCode: contract.PreconditionFailed, wantCode: contract.PreconditionFailed, wantExit: contract.ExitCode(contract.NewFault(contract.PreconditionFailed, "failed", "inspect"))},
		{name: "cancelled", state: contract.Canceled, wantCode: contract.Cancelled, wantExit: 130},
	} {
		for _, test := range followingCommands {
			if !followingSubmits(test.name) {
				continue
			}
			t.Run(test.name+"/"+outcome.name, func(t *testing.T) {
				backend := newNDJSONBackend()
				backend.state, backend.errorCode = outcome.state, outcome.errorCode
				prepareFollowingBackend(test.name, backend)
				args := followingArgs(test.args, "--output", "ndjson")
				exit, stdout, stderr := runWithBackend(t, context.Background(), backend, args)
				if exit != outcome.wantExit || stderr != "" {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
				}
				envelope, _ := decodeNDJSONStream(t, stdout)
				if envelope.Command != test.path || envelope.OK || envelope.Error == nil {
					t.Fatalf("envelope=%+v", envelope)
				}
				if envelope.Error.Code != outcome.wantCode || envelope.Error.Details["operation_id"] != string(followTestID) || envelope.Error.Details["state"] != string(outcome.state) {
					t.Fatalf("fault=%+v", envelope.Error)
				}
			})
		}
	}
}

func TestFollowingSubmitNDJSONDetach(t *testing.T) {
	for _, test := range followingCommands {
		if !followingSubmits(test.name) {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backend := newNDJSONBackend()
			backend.state = contract.Running
			backend.cancelOn, backend.cancel = followTestID, cancel
			prepareFollowingBackend(test.name, backend)
			args := followingArgs(test.args, "--output", "ndjson")
			exit, stdout, stderr := runWithBackend(t, ctx, backend, args)
			if exit != 130 || stderr != "" {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			envelope, _ := decodeNDJSONStream(t, stdout)
			if envelope.Command != test.path || envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.Cancelled {
				t.Fatalf("envelope=%+v", envelope)
			}
			if !strings.Contains(envelope.Error.Message, "viewer detached") {
				t.Fatalf("detach message = %q", envelope.Error.Message)
			}
			resume, _ := envelope.Error.Details["resume"].(string)
			if envelope.Error.Details["operation_id"] != string(followTestID) || !strings.Contains(resume, resumeCommand) {
				t.Fatalf("detach details = %+v", envelope.Error.Details)
			}
		})
	}
}

func TestGuidedCreateNDJSONDetachNamesFirstDeploy(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "app create", args: []string{"app", "create", "--name", "api", "--node", "main", "--git-url", "https://github.com/acme/api"}},
		{name: "env create", args: []string{"env", "create", "--app", "api", "--name", "production", "--node", "main", "--git-url", "https://github.com/acme/api"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backend := newNDJSONBackend()
			backend.state = contract.Running
			backend.firstDeploy = true
			backend.cancelOn, backend.cancel = followTestID, cancel
			prepareFollowingBackend(test.name, backend)
			args := append(append([]string{}, test.args...), "--output", "ndjson")
			exit, stdout, stderr := runWithBackend(t, ctx, backend, args)
			if exit != 130 || stderr != "" {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			envelope, _ := decodeNDJSONStream(t, stdout)
			if envelope.Error == nil || envelope.Error.Code != contract.Cancelled {
				t.Fatalf("envelope=%+v", envelope)
			}
			if envelope.Error.Details["first_deploy_operation_id"] != string(ndjsonDeployID) || envelope.Error.Details["operation_id"] != string(followTestID) {
				t.Fatalf("detach details = %+v", envelope.Error.Details)
			}
			if !strings.Contains(envelope.Error.Hint, "first deploy is queued") {
				t.Fatalf("detach hint = %q", envelope.Error.Hint)
			}
		})
	}
}

func TestFollowingSubmitNDJSONDryRun(t *testing.T) {
	app := newApp()
	for _, test := range followingCommands {
		cmd, err := app.resolve(strings.Fields(test.path)[1:])
		if err != nil {
			t.Fatal(err)
		}
		if flagFor(cmd, "dry-run") == nil {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			backend := newNDJSONBackend()
			prepareFollowingBackend(test.name, backend)
			args := followingArgs(test.args, "--dry-run", "--output", "ndjson")
			exit, stdout, stderr := runWithBackend(t, context.Background(), backend, args)
			if test.name == "diagnose" {
				envelope := decodeNDJSONResult(t, stdout)
				if exit != 2 || stderr != "" || envelope.Error == nil || envelope.Error.Code != contract.Usage || backend.calls != 0 {
					t.Fatalf("exit=%d stdout=%s stderr=%s calls=%d", exit, stdout, stderr, backend.calls)
				}
				return
			}
			if exit != 0 || stderr != "" {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			envelope, eventOps := decodeNDJSONStream(t, stdout)
			if envelope.Command != test.path || !envelope.OK {
				t.Fatalf("envelope=%+v", envelope)
			}
			if len(eventOps) != 0 || len(backend.submitted) != 0 || backend.calls == 0 {
				t.Fatalf("dry run events=%v submitted=%v calls=%d", eventOps, backend.submitted, backend.calls)
			}
		})
	}
}

func TestGuidedCreateNDJSONIdempotentReplay(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	replay := guidedOperation(ndjsonReplayID, platform.KindAppCreate, platform.PlatformTarget().String(), contract.Succeeded, 2, at)
	backend := newNDJSONBackend()
	backend.replay = &replay
	backend.operations[replay.ID] = replay
	backend.events[replay.ID] = []contract.Event{
		guidedStateEvent(replay, 1, contract.Queued, at.Add(-time.Second)),
		guidedStateEvent(replay, 2, contract.Succeeded, at),
	}
	args := []string{
		"app", "create", "--name", "api", "--node", "main",
		"--git-url", "https://github.com/acme/api", "--idempotency-key", "stable-replay-key", "--output", "ndjson",
	}
	exit, stdout, stderr := runWithBackend(t, context.Background(), backend, args)
	if exit != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	envelope, eventOps := decodeNDJSONStream(t, stdout)
	if envelope.Command != "eregion app create" || !envelope.OK {
		t.Fatalf("envelope=%+v", envelope)
	}
	if eventOps[ndjsonReplayID] == 0 {
		t.Fatalf("replayed operation did not stream: %v", eventOps)
	}
	if len(backend.submitted) != 1 || backend.dryRuns != 1 {
		t.Fatalf("replay submitted=%v dryRuns=%d", backend.submitted, backend.dryRuns)
	}
	data, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), string(ndjsonReplayID)) {
		t.Fatalf("replayed operation is missing from the result: %s", data)
	}
}

// decodeNDJSONStream reads one event-and-result stream. It requires exactly one
// terminal result, because a command request has one result even when a guided
// create follows two operations.
func decodeNDJSONStream(t *testing.T, raw string) (contract.Envelope, map[contract.OperationID]int) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	eventOps := map[contract.OperationID]int{}
	var result contract.Envelope
	results := 0
	for {
		var record struct {
			Type   string             `json:"type"`
			Event  *contract.Event    `json:"event"`
			Result *contract.Envelope `json:"result"`
		}
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("invalid ndjson: %v\n%s", err, raw)
		}
		switch record.Type {
		case "event":
			if record.Event == nil {
				t.Fatalf("event record without event: %s", raw)
			}
			eventOps[record.Event.OperationID]++
		case "result":
			if record.Result == nil {
				t.Fatalf("result record without result: %s", raw)
			}
			results++
			result = *record.Result
		default:
			t.Fatalf("unknown record type %q: %s", record.Type, raw)
		}
	}
	if results != 1 {
		t.Fatalf("got %d result records: %s", results, raw)
	}
	return result, eventOps
}

func decodeNDJSONResult(t *testing.T, raw string) contract.Envelope {
	t.Helper()
	result, _ := decodeNDJSONStream(t, raw)
	return result
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
