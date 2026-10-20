package noded

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// RunTask runs one detached node task. It reads its request from the
// inherited pipe, never from a command line or an environment variable, and
// it runs to completion regardless of what the control plane does with the
// connection that started it.
func RunTask(config Config) error {
	file := os.NewFile(TaskRequestFD, "node-request")
	if file == nil {
		return errors.New("the node task request is missing")
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, node.MaxFrameBytes))
	if err != nil {
		return err
	}
	var request node.Request
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("invalid node task request: %w", err)
	}
	// Register the request secrets before any event, log, error, or journal
	// write happens.
	registry := &secrets.Registry{}
	for _, value := range request.Secrets {
		registry.Add(value)
	}
	nodeEngine, err := engine.New(config.StateDir, config.Node, registry)
	if err != nil {
		return err
	}
	nodeEngine.SecretValues = request.Secrets
	step, err := node.DefaultRegistry().Build(request.Execution, request.Payload)
	if err != nil {
		return err
	}
	report := func(event engine.Event) {
		fmt.Fprintf(os.Stdout, "%s %s %s\n", event.Kind, event.Step, registry.Redact(event.Message))
	}
	status, err := nodeEngine.Run(context.Background(), step, request.Execution, report)
	// The journal is compacted after every task so it stays bounded on a node
	// that never restarts.
	if _, compactErr := engine.Compact(context.Background(), config.StateDir, engine.DefaultRetention()); compactErr != nil {
		fmt.Fprintf(os.Stderr, "eregion-node: journal compaction failed: %v\n", compactErr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "node task failed: %s\n", registry.Redact(err.Error()))
		return err
	}
	fmt.Fprintf(os.Stdout, "completed %s applied_attempt=%d\n", status.ExecutionID, status.AppliedAttempt)
	return nil
}

// Compact bounds the node journal.
func Compact(config Config) (int, error) {
	return engine.Compact(context.Background(), config.StateDir, engine.DefaultRetention())
}
