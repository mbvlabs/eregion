package noded

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/node"
)

// TestStatusResponseBoundsTheTaskLog proves that the node answers a status
// request with a bounded tail of the node task log. A chatty task must not be
// able to fill the frame, and the tail must still say what happened.
func TestStatusResponseBoundsTheTaskLog(t *testing.T) {
	directory := t.TempDir()
	stateDir := filepath.Join(directory, "state")
	config := Config{Node: "test-node", StateDir: stateDir, Path: filepath.Join(directory, "node.json")}
	execution := testExecution(t)
	if err := os.MkdirAll(filepath.Join(stateDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	name := strings.ReplaceAll(execution.ExecutionID(), ":", "_") + ".log"
	if err := os.WriteFile(filepath.Join(stateDir, "logs", name), bytes.Repeat([]byte("noise "), 200000), 0o600); err != nil {
		t.Fatal(err)
	}
	response := statusResponse(config, node.Request{Method: node.MethodStatus, Execution: execution})
	if !strings.Contains(response.Detail, "node task log:") {
		t.Fatalf("the node task log is missing from the detail: %q", response.Detail)
	}
	if len(response.Detail) > TaskLogTailBytes+1024 {
		t.Fatalf("the node task log tail is not bounded: %d bytes", len(response.Detail))
	}
}
