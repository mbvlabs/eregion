package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/rpc"
)

func TestIntegrationBuildSlotsRPC(t *testing.T) {
	pool := beta8TestPool(t)
	run := beta8TestRuntime(t, pool)
	app := api.NewAppService(run.Service, nil, nil)
	ctx := context.Background()
	request := rpc.Request{Method: methodBuildSlotsList, Operator: "operator", Params: json.RawMessage(`{}`)}
	data, err := dispatchNodeMethod(ctx, app, request)
	slots, ok := data.([]api.BuildSlot)
	if err != nil || !ok || slots == nil || len(slots) != 0 {
		t.Fatalf("list dispatch: %#v %v", data, err)
	}
	request.DryRun = true
	if _, err := dispatchNodeMethod(ctx, app, request); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("list dry-run: %v", err)
	}
	request.Method = methodBuildSlotsClear
	request.Params = json.RawMessage(`{"operation_id":"op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if _, err := dispatchNodeMethod(ctx, app, request); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("missing clear preview: %v", err)
	}
	request.Params = json.RawMessage(`{"operation_id":"op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","force":true}`)
	if _, err := dispatchNodeMethod(ctx, app, request); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("force accepted: %v", err)
	}
}
