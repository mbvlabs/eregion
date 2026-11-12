package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/rpc"
)

type definitionRPCFake struct {
	plan    api.DefinitionPlanInput
	apply   api.DefinitionApplyInput
	applies int
}

func TestDefinitionRPCRequestSizeBoundary(t *testing.T) {
	// NUL encodes as six JSON bytes, the worst possible expansion of one
	// source byte. This deliberately tests framing, not TOML validation.
	definition := strings.Repeat("\x00", appdef.MaxDefinitionBytes)
	name := strings.Repeat("a", 40)
	plan := api.DefinitionPlanInput{App: name, Environment: name, Definition: definition, PlanNonce: strings.Repeat("a", 64)}
	apply := api.DefinitionApplyInput{Plan: plan, ApprovedDigest: strings.Repeat("b", 64), IdempotencyKey: strings.Repeat("k", 128), Confirm: name + "/" + name}
	for _, test := range []struct {
		method string
		input  any
	}{
		{methodDefinitionPlan, plan},
		{methodDefinitionApply, apply},
	} {
		t.Run(test.method, func(t *testing.T) {
			params, err := json.Marshal(test.input)
			if err != nil {
				t.Fatal(err)
			}
			request := rpc.Request{Schema: rpc.Schema, RequestID: rpc.NewRequestID(), Method: test.method, Params: params, Operator: "admin", ClientVersion: "v1.0.0-beta.4", ClientCommit: strings.Repeat("c", 40)}
			wire, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			wire = append(wire, '\n')
			t.Logf("encoded request: %d bytes, limit: %d bytes", len(wire), rpc.MaxRequestBytes)
			if len(wire) > rpc.MaxRequestBytes {
				t.Fatalf("encoded request is %d bytes, limit %d", len(wire), rpc.MaxRequestBytes)
			}
			decoded, _, err := rpc.DecodeRequest(bytes.NewReader(wire))
			if err != nil {
				t.Fatalf("decode %d-byte request: %v", len(wire), err)
			}
			if decoded.Method != test.method || !bytes.Equal(decoded.Params, params) {
				t.Fatal("request parameters changed or were truncated")
			}
		})
	}
}

func (f *definitionRPCFake) PlanDefinition(_ context.Context, _ contract.Actor, input api.DefinitionPlanInput) (api.DefinitionPlan, error) {
	f.plan = input
	return api.DefinitionPlan{Digest: "digest", PlanNonce: "nonce", CanApply: true}, nil
}

func (f *definitionRPCFake) ApplyDefinition(_ context.Context, _ contract.Actor, input api.DefinitionApplyInput) (operations.SubmissionResult, error) {
	f.apply = input
	f.applies++
	return operations.SubmissionResult{Replay: true}, nil
}

func TestDefinitionRPCDispatch(t *testing.T) {
	fake := &definitionRPCFake{}
	service := api.AppService{DefinitionPlanner: fake}
	input := api.DefinitionPlanInput{App: "app", Environment: "prod", Definition: "definition"}
	raw, _ := json.Marshal(input)
	request := rpc.Request{Operator: "admin", Method: methodDefinitionPlan, Params: raw}
	result, err := dispatchNodeMethod(context.Background(), service, request)
	if err != nil || fake.plan != input || result.(api.DefinitionPlan).PlanNonce != "nonce" {
		t.Fatalf("plan: result=%+v input=%+v err=%v", result, fake.plan, err)
	}
	apply := api.DefinitionApplyInput{Plan: input, ApprovedDigest: "digest", IdempotencyKey: "key", Confirm: "app/prod"}
	request.Params, _ = json.Marshal(apply)
	request.Method = methodDefinitionApply
	request.DryRun = true
	if _, err := dispatchNodeMethod(context.Background(), service, request); err == nil || fake.applies != 0 {
		t.Fatalf("dry run admitted: err=%v calls=%d", err, fake.applies)
	}
	request.DryRun = false
	result, err = dispatchNodeMethod(context.Background(), service, request)
	if err != nil || fake.apply != apply || fake.applies != 1 || !result.(operations.SubmissionResult).Replay {
		t.Fatalf("apply: result=%+v input=%+v calls=%d err=%v", result, fake.apply, fake.applies, err)
	}
}
