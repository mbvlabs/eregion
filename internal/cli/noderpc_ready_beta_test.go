package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/rpc"
)

func TestStartedNodeReadyUsesStartupCheckOnce(t *testing.T) {
	calls := 0
	app := api.AppService{ReadyCheck: func(context.Context) error { calls++; return nil }}
	req := rpc.Request{Method: methodReady, Operator: "admin"}
	// Model the successful startup check performed by openNodeApp.
	if err := app.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := dispatchStartedNodeMethod(context.Background(), app, req)
	if err != nil || !got.(map[string]bool)["ready"] || calls != 1 {
		t.Fatalf("ready = %v, %v; checks = %d", got, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dispatchStartedNodeMethod(ctx, app, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request = %v", err)
	}
	req.Operator = ""
	if _, err := dispatchStartedNodeMethod(context.Background(), app, req); err == nil {
		t.Fatal("missing operator accepted")
	}
}
