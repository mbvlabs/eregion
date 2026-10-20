package noded

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
)

// testConfig writes a node configuration for a test and returns it.
func testConfig(t *testing.T, taskCommand []string) Config {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "node.json")
	raw, err := json.Marshal(map[string]any{
		"node":         "test-node",
		"state_dir":    filepath.Join(directory, "state"),
		"task_command": taskCommand,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		Node:        "test-node",
		StateDir:    filepath.Join(directory, "state"),
		Path:        path,
		TaskCommand: taskCommand,
	}
}

// testExecution returns an execution identity for a test.
func testExecution(t *testing.T) engine.Execution {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return engine.Execution{OperationID: id, Attempt: 1, Step: node.DiagnosticStepID(1), RequestID: "request"}
}

// executionFromID rebuilds an execution identity from its execution ID.
func executionFromID(t *testing.T, id string) engine.Execution {
	t.Helper()
	parts := strings.Split(id, ":")
	if len(parts) != 3 {
		t.Fatalf("invalid execution ID %q", id)
	}
	attempt, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return engine.Execution{
		OperationID: contract.OperationID(parts[0]),
		Attempt:     uint32(attempt),
		Step:        parts[2],
		RequestID:   "request",
	}
}

// TestSpawnTaskWritesTheRequestBeforeTheProcessExits proves that the request
// reaches the detached task even when the serving process exits as soon as
// the channel closes. The request is larger than the request pipe, so a write
// that is not finished before the process exits is lost.
func TestSpawnTaskWritesTheRequestBeforeTheProcessExits(t *testing.T) {
	config := testConfig(t, []string{os.Args[0], "-test.run=^TestNodeTaskRunner$"})
	t.Setenv("EREGION_NODE_CONFIG", config.Path)
	t.Setenv("EREGION_NODE_TASK_RUNNER", "1")
	t.Setenv("EREGION_NODE_SPAWN_HELPER", "1")
	execution := testExecution(t)
	command := exec.Command(os.Args[0], "-test.run=^TestNodeTaskSpawnHelper$")
	command.Env = append(os.Environ(),
		"EREGION_NODE_CONFIG="+config.Path,
		"EREGION_NODE_TASK_RUNNER=1",
		"EREGION_NODE_SPAWN_HELPER=1",
		"EREGION_NODE_EXECUTION="+execution.ExecutionID(),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the spawn helper failed: %v: %s", err, output)
	}
	// The detached task outlived the serving process: the effect is applied.
	marker := node.MarkerPath(config.StateDir, execution)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached task never received the request")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestNodeTaskSpawnHelper starts one detached node task and exits at once,
// the way a serving process does when the channel closes.
func TestNodeTaskSpawnHelper(t *testing.T) {
	if os.Getenv("EREGION_NODE_SPAWN_HELPER") != "1" {
		t.Skip("node task spawn helper")
	}
	config, err := LoadConfig(os.Getenv("EREGION_NODE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	execution := executionFromID(t, os.Getenv("EREGION_NODE_EXECUTION"))
	raw, err := json.Marshal(node.DiagnosticPayload{Index: 1, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	request := node.Request{
		Method:    node.MethodRun,
		RequestID: "request",
		Execution: execution,
		Payload:   raw,
		// The request is larger than the request pipe, so the write cannot
		// finish before the child reads it.
		Secrets: map[string]string{"blob": strings.Repeat("x", 256*1024)},
	}
	if err := SpawnTask(config, request); err != nil {
		t.Fatal(err)
	}
	// The serving process exits as soon as the channel closes.
	os.Exit(0)
}

// TestNodeTaskRunner runs one detached node task. The spawn helper starts it
// as a child process with the request on an inherited pipe.
func TestNodeTaskRunner(t *testing.T) {
	if os.Getenv("EREGION_NODE_TASK_RUNNER") != "1" {
		t.Skip("node task runner")
	}
	config, err := LoadConfig(os.Getenv("EREGION_NODE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RunTask(config); err != nil {
		t.Fatal(err)
	}
}

// TestTaskCommandUsesTheExecutablePath proves that the detached task starts
// the running binary, not whatever the argument vector happens to name.
func TestTaskCommandUsesTheExecutablePath(t *testing.T) {
	original := os.Args[0]
	os.Args[0] = "eregion-node"
	t.Cleanup(func() { os.Args[0] = original })
	config := Config{Node: "test-node", StateDir: t.TempDir(), Path: "/etc/eregion/node.json"}
	command := config.taskCommand()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if command[0] != executable {
		t.Fatalf("the task command does not start the running binary: %v", command)
	}
	if len(command) != 4 || command[1] != "task" || command[2] != "--config" || command[3] != config.Path {
		t.Fatalf("the task command does not pass the configuration: %v", command)
	}
}

// TestTaskCommandKeepsAConfiguredCommand proves that a configured task command
// is used verbatim.
func TestTaskCommandKeepsAConfiguredCommand(t *testing.T) {
	config := Config{Node: "test-node", StateDir: t.TempDir(), Path: "/etc/eregion/node.json", TaskCommand: []string{"/bin/true"}}
	command := config.taskCommand()
	if len(command) != 1 || command[0] != "/bin/true" {
		t.Fatalf("the configured task command was replaced: %v", command)
	}
}

// TestConfigRejectsAnUnsafeTaskCommand proves that a task command is a local
// program and never something the search path resolves.
func TestConfigRejectsAnUnsafeTaskCommand(t *testing.T) {
	for _, command := range [][]string{
		{"eregion-node"},
		{"", "task"},
		{"/bin/true", ""},
		{"/bin/true", "bad\nargument"},
	} {
		config := Config{Node: "test-node", StateDir: "/var/lib/eregion/node", TaskCommand: command}
		if err := config.Validate(); err == nil {
			t.Fatalf("an unsafe task command was accepted: %v", command)
		}
	}
}
