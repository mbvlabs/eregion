package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

// guidedCreateBackend serves operation snapshots and event pages for the two
// follows a guided create performs. The embedded Backend keeps the test fake
// limited to the reads the viewer uses.
type guidedCreateBackend struct {
	Backend
	operations map[contract.OperationID]contract.Operation
	events     map[contract.OperationID][]contract.Event
	gets       []contract.OperationID
	cancelOn   contract.OperationID
	cancel     context.CancelFunc
	inventory  map[string]json.RawMessage
}

func (f *guidedCreateBackend) Get(_ context.Context, id contract.OperationID) (json.RawMessage, error) {
	f.gets = append(f.gets, id)
	op, ok := f.operations[id]
	if !ok {
		return nil, contract.NewFault(contract.NotFound, "operation not found", "Inspect the operation list.")
	}
	raw, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	if id == f.cancelOn && f.cancel != nil {
		f.cancel()
	}
	return raw, nil
}

func (f *guidedCreateBackend) Events(_ context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
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
	raw, err := json.Marshal(operations.EventPage{
		Events: page,
		Cursor: contract.Cursor{OperationID: cursor.OperationID, After: after},
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (f *guidedCreateBackend) PlatformInventory(_ context.Context, collection string, _ url.Values) (json.RawMessage, error) {
	if raw, ok := f.inventory[collection]; ok {
		return raw, nil
	}
	return json.RawMessage(`{"items":[]}`), nil
}

func guidedOperation(id contract.OperationID, kind, target string, state contract.State, sequence uint64, updated time.Time) contract.Operation {
	return contract.Operation{
		ID: id, Kind: kind, Target: target,
		Actor: contract.Actor{Kind: "operator", Subject: "alice"},
		State: state, Attempt: 1, LastSequence: sequence,
		CreatedAt: updated.Add(-time.Minute), UpdatedAt: updated,
	}
}

func guidedStateEvent(op contract.Operation, sequence uint64, state contract.State, at time.Time) contract.Event {
	return contract.Event{
		OperationID: op.ID, Sequence: sequence, At: at, Kind: "state", Attempt: 1, State: state,
	}
}

func guidedCreateCommand(t *testing.T) (*cobra.Command, *strings.Builder, *strings.Builder) {
	t.Helper()
	cmd := &cobra.Command{Use: "create"}
	out, errOut := &strings.Builder{}, &strings.Builder{}
	cmd.SetContext(context.WithValue(context.Background(), followIOKey{}, &followIO{
		in: strings.NewReader(""), out: out, err: errOut, mode: "plain",
	}))
	return cmd, out, errOut
}

func TestGuidedCreateFollowsTheFirstDeployAfterTheCreate(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	createID := contract.OperationID("op_" + strings.Repeat("a", 32))
	deployID := contract.OperationID("op_" + strings.Repeat("b", 32))
	create := guidedOperation(createID, platform.KindAppCreate, "platform", contract.Succeeded, 2, at.Add(2*time.Second))
	create.FirstDeployOperationID = deployID
	deploy := guidedOperation(deployID, platform.KindDeploy, "env/api/production", contract.Succeeded, 2, at.Add(4*time.Second))
	backend := &guidedCreateBackend{
		operations: map[contract.OperationID]contract.Operation{createID: create, deployID: deploy},
		events: map[contract.OperationID][]contract.Event{
			createID: {
				guidedStateEvent(create, 1, contract.Queued, at),
				guidedStateEvent(create, 2, contract.Succeeded, at.Add(2*time.Second)),
			},
			deployID: {
				guidedStateEvent(deploy, 1, contract.Queued, at.Add(2*time.Second)),
				guidedStateEvent(deploy, 2, contract.Succeeded, at.Add(4*time.Second)),
			},
		},
	}
	cmd, out, _ := guidedCreateCommand(t)
	a := &app{secrets: &secrets.Registry{}}
	var outcomeID contract.OperationID
	res, err := a.followGuidedCreate(cmd, backend, create, "env/api/production", func(_ context.Context, _ Backend, op contract.Operation) []string {
		outcomeID = op.ID
		return []string{"outcome " + string(op.ID)}
	})
	if err != nil {
		t.Fatal(err)
	}
	followed, ok := res.data.(followResult)
	if !ok || followed.Operation.ID != deployID || followed.State != contract.Succeeded {
		t.Fatalf("guided create result = %+v", res.data)
	}
	if outcomeID != deployID {
		t.Fatalf("outcome ran for %q, want the first deploy", outcomeID)
	}
	if len(backend.gets) < 2 || backend.gets[0] != createID || backend.gets[1] != deployID {
		t.Fatalf("follow order = %v, want create then first deploy", backend.gets)
	}
	if !strings.Contains(out.String(), "outcome "+string(deployID)) {
		t.Fatalf("deploy outcome is missing from output:\n%s", out.String())
	}
}

func TestGuidedCreateStopsAfterAFailedCreate(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	createID := contract.OperationID("op_" + strings.Repeat("c", 32))
	deployID := contract.OperationID("op_" + strings.Repeat("d", 32))
	create := guidedOperation(createID, platform.KindAppCreate, "platform", contract.Failed, 2, at.Add(2*time.Second))
	create.FirstDeployOperationID = deployID
	create.ErrorCode = contract.PreconditionFailed
	backend := &guidedCreateBackend{
		operations: map[contract.OperationID]contract.Operation{createID: create},
		events: map[contract.OperationID][]contract.Event{
			createID: {
				guidedStateEvent(create, 1, contract.Queued, at),
				guidedStateEvent(create, 2, contract.Failed, at.Add(2*time.Second)),
			},
		},
	}
	cmd, _, _ := guidedCreateCommand(t)
	a := &app{secrets: &secrets.Registry{}}
	res, err := a.followGuidedCreate(cmd, backend, create, "env/api/production", func(context.Context, Backend, contract.Operation) []string {
		t.Fatal("the first deploy was followed after a failed create")
		return nil
	})
	if contract.ExitCode(err) != 5 {
		t.Fatalf("failed create exit = %d, error = %v", contract.ExitCode(err), err)
	}
	followed, ok := res.data.(followResult)
	if !ok || followed.State != contract.Failed {
		t.Fatalf("failed create result = %+v", res.data)
	}
	for _, id := range backend.gets {
		if id == deployID {
			t.Fatal("the first deploy was read after a failed create")
		}
	}
}

func TestGuidedCreateDetachPrintsTheCreateResumeCommand(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	createID := contract.OperationID("op_" + strings.Repeat("e", 32))
	deployID := contract.OperationID("op_" + strings.Repeat("f", 32))
	create := guidedOperation(createID, platform.KindAppCreate, "platform", contract.Running, 1, at)
	create.FirstDeployOperationID = deployID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{Use: "create"}
	out, errOut := &strings.Builder{}, &strings.Builder{}
	cmd.SetContext(context.WithValue(ctx, followIOKey{}, &followIO{in: strings.NewReader(""), out: out, err: errOut, mode: "plain"}))
	backend := &guidedCreateBackend{
		operations: map[contract.OperationID]contract.Operation{createID: create},
		events: map[contract.OperationID][]contract.Event{
			createID: {guidedStateEvent(create, 1, contract.Running, at)},
		},
		cancelOn: createID, cancel: cancel,
	}
	a := &app{secrets: &secrets.Registry{}}
	_, err := a.followGuidedCreate(cmd, backend, create, "env/api/production", func(context.Context, Backend, contract.Operation) []string {
		t.Fatal("the first deploy was followed after a detach")
		return nil
	})
	if contract.Classify(err).Code != contract.Cancelled {
		t.Fatalf("detach error = %v, want cancelled", err)
	}
	want := fmt.Sprintf("resume: eregion operations follow %s", createID)
	if !strings.Contains(out.String(), want) {
		t.Fatalf("resume command %q is missing from output:\n%s", want, out.String())
	}
	wantDeploy := fmt.Sprintf("first deploy: eregion operations follow %s", deployID)
	if !strings.Contains(out.String(), wantDeploy) {
		t.Fatalf("first deploy follow command %q is missing from output:\n%s", wantDeploy, out.String())
	}
	for _, id := range backend.gets {
		if id == deployID {
			t.Fatal("the first deploy was read after a detach")
		}
	}
}

func TestGuidedCreateEndsWithAFailedFirstDeployAndNoServingClaim(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	createID := contract.OperationID("op_" + strings.Repeat("1", 32))
	deployID := contract.OperationID("op_" + strings.Repeat("2", 32))
	create := guidedOperation(createID, platform.KindAppCreate, "platform", contract.Succeeded, 2, at.Add(2*time.Second))
	create.FirstDeployOperationID = deployID
	deploy := guidedOperation(deployID, platform.KindDeploy, "env/api/production", contract.Failed, 2, at.Add(4*time.Second))
	deploy.ErrorCode = contract.PreconditionFailed
	healthFailure := guidedStateEvent(deploy, 2, contract.Failed, at.Add(4*time.Second))
	healthFailure.Message = "health check failed: 503"
	inventory, err := json.Marshal(map[string]any{"items": []platform.Environment{{App: "api", Name: "production"}}})
	if err != nil {
		t.Fatal(err)
	}
	backend := &guidedCreateBackend{
		operations: map[contract.OperationID]contract.Operation{createID: create, deployID: deploy},
		events: map[contract.OperationID][]contract.Event{
			createID: {
				guidedStateEvent(create, 1, contract.Queued, at),
				guidedStateEvent(create, 2, contract.Succeeded, at.Add(2*time.Second)),
			},
			deployID: {
				guidedStateEvent(deploy, 1, contract.Queued, at.Add(2*time.Second)),
				healthFailure,
			},
		},
		inventory: map[string]json.RawMessage{"environments": inventory},
	}
	cmd, out, _ := guidedCreateCommand(t)
	a := &app{secrets: &secrets.Registry{}}
	res, err := a.followGuidedCreate(cmd, backend, create, "env/api/production", a.deployOutcome("api", "production"))
	if contract.ExitCode(err) != 5 {
		t.Fatalf("failed first deploy exit = %d, error = %v", contract.ExitCode(err), err)
	}
	followed, ok := res.data.(followResult)
	if !ok || followed.Operation.ID != deployID || followed.State != contract.Failed {
		t.Fatalf("failed deploy result = %+v", res.data)
	}
	text := out.String()
	if !strings.Contains(text, "health check failed: 503") {
		t.Fatalf("failed step error is missing from output:\n%s", text)
	}
	if !strings.Contains(text, "serving release: none") || !strings.Contains(text, "public url: no domain binding") {
		t.Fatalf("failed deploy outcome has no explicit serving state:\n%s", text)
	}
	if strings.Contains(text, "public url: https://") {
		t.Fatalf("failed deploy claimed a serving URL:\n%s", text)
	}
}
