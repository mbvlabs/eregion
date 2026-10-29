package platform

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestBuildStopWritesCancellationWhileBuildHoldsHostLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	root := t.TempDir()
	eng, err := engine.New(root, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	release, err := engine.Acquire(ctx, filepath.Join(root, "host.lock"), engine.LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	target := "op_" + strings.Repeat("a", 32)
	execution := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("b", 32)), Attempt: 1, Step: "platform-build-stop-01-v1"}
	raw, _ := json.Marshal(BuildStopPayload{Step: execution.Step, OperationID: target, PackStep: "platform-pack-01-v1"})
	step, err := buildBuildStopStep(execution, raw)
	if err != nil {
		t.Fatal(err)
	}
	status, err := eng.Run(ctx, step, execution, nil)
	if err != nil || status.Status != engine.StatusCompleted {
		t.Fatalf("stop did not run while pack holds lock: %+v %v", status, err)
	}
	if _, err := os.Stat(buildCancelPath(root, target, "platform-pack-01-v1")); err != nil {
		t.Fatalf("cancellation marker missing: %v", err)
	}
}
