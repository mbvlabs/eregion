package controlplane

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationPlatformPreviewReplaysAnAcceptedCreate reproduces the create
// retry: on the old preview path the second dry run refused because the app
// already existed, so the CLI stopped before its idempotent submit could return
// the original operation.
func TestIntegrationPlatformPreviewReplaysAnAcceptedCreate(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	service := api.NewAppService(run.Service, nil, nil)
	raw, _ := json.Marshal(map[string]any{"name": "replay"})
	normalized, err := platform.Default().Normalize(platform.KindAppCreate, platform.PlatformTarget().String(), raw)
	if err != nil {
		t.Fatal(err)
	}
	const key = "cli:replay:create"
	if _, err := service.SubmitNormalized(ctx, testActor, normalized, key, false); err != nil {
		t.Fatalf("first create: %v", err)
	}

	replayed, err := service.SubmitNormalized(ctx, testActor, normalized, key, true)
	if err != nil {
		t.Fatalf("preview of the same key: %v", err)
	}
	preview, ok := replayed.(platform.Preview)
	if !ok || !preview.Replay || preview.Operation == nil || preview.Operation.ID == "" {
		t.Fatalf("preview did not replay the accepted operation: %+v", replayed)
	}
	if preview.Operation.Kind != platform.KindAppCreate {
		t.Fatalf("replayed operation kind = %s", preview.Operation.Kind)
	}

	// The preview must skip the conflict checks that would refuse a second
	// create, and the submit must return that same operation.
	submitted, err := service.SubmitNormalized(ctx, testActor, normalized, key, false)
	if err != nil {
		t.Fatalf("submit of the same key: %v", err)
	}
	op, ok := submitted.(contract.Operation)
	if !ok || op.ID != preview.Operation.ID {
		t.Fatalf("replayed submit = %+v, want %s", submitted, preview.Operation.ID)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM eregion_operations WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5",
		testActor.Kind, testActor.Subject, platform.KindAppCreate, platform.PlatformTarget().String(), key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("operations with the key = %d, want 1", count)
	}

	// Without the operator's key, the preview keeps refusing the existing app.
	if _, err := service.SubmitNormalized(ctx, testActor, normalized, "", true); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("preview without a key = %v, want conflict", err)
	}

	// The same key with different inputs is still the existing conflict.
	other, _ := json.Marshal(map[string]any{"name": "replay-other"})
	otherNormalized, err := platform.Default().Normalize(platform.KindAppCreate, platform.PlatformTarget().String(), other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SubmitNormalized(ctx, testActor, otherNormalized, key, false); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("same key with different inputs = %v, want conflict", err)
	}
}
