package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestIntegrationActionOutputKeepsTrustedIdentityAndExtensionSchema(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	run.Service.redaction = &secrets.Registry{}
	run.Service.redaction.Add("a", "stdout", "rawsecret")
	submit := func(key string) contract.Operation {
		t.Helper()
		op, err := run.Service.Submit(ctx, operations.Submission{
			Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
			IdempotencyKey: key, Input: []byte(`{}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		return op
	}
	op := submit("trusted-action-output")
	// Typed callers are responsible for redacting free text at its source.
	snapshot := struct {
		Domain string `json:"domain"`
		Stdout string `json:"stdout"`
	}{Domain: "app.example", Stdout: run.Service.redact("rawsecret")}
	_, owned, err := run.Service.mutateOwned(ctx, op.ID, 1, func(tx pgx.Tx, current *contract.Operation) error {
		return run.Service.appendActionOutputTx(ctx, tx, current, snapshot)
	})
	if err != nil || !owned {
		t.Fatalf("append: %v %v", owned, err)
	}
	if err := run.Service.finishPlatformOperation(ctx, op.ID, 1, func(pgx.Tx, *contract.Operation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || got.LatestResult == nil {
		t.Fatalf("result: %+v %v", got, err)
	}
	var output map[string]json.RawMessage
	if err := json.Unmarshal(got.LatestResult.Data, &output); err != nil {
		t.Fatal(err)
	}
	if string(output["domain"]) != `"app.example"` || len(output["stdout"]) == 0 ||
		strings.Contains(string(got.LatestResult.Data), "rawsecret") {
		t.Fatalf("typed output changed identity/schema or leaked text: %s", got.LatestResult.Data)
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if strings.Contains(string(event.ActionOutput), "rawsecret") {
			t.Fatalf("plaintext persisted: %s", event.ActionOutput)
		}
	}

	extension := submit("extension-redaction-structure")
	executor := platform.ExecutorFunc(func(context.Context, platform.Execution) (platform.Result, error) {
		return platform.Result{Data: map[string]any{"stdout": "rawsecret", "counter": json.Number("9007199254740993")}}, nil
	})
	if err := (&platformWorker{service: run.Service}).runExtension(ctx, extension.ID, 1,
		platform.Normalized{Kind: extension.Kind, Target: platform.PlatformTarget(), Summary: "extension"}, executor); err != nil {
		t.Fatal(err)
	}
	got, err = run.Service.Get(ctx, testActor, extension.ID)
	if err != nil || got.LatestResult == nil {
		t.Fatalf("extension: %+v %v", got, err)
	}
	output = nil
	if err := json.Unmarshal(got.LatestResult.Data, &output); err != nil {
		t.Fatal(err)
	}
	if len(output["stdout"]) == 0 || string(output["counter"]) != "9007199254740993" ||
		strings.Contains(string(got.LatestResult.Data), "rawsecret") {
		t.Fatalf("extension output changed schema, precision or leaked text: %s", got.LatestResult.Data)
	}
}
