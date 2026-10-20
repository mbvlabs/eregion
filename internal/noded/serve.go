package noded

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Serve answers node requests until the stream closes. It is the entry point
// the control plane reaches over SSH: the request arrives on the channel's
// standard input, never in a command line.
func Serve(config Config, stdin io.Reader, stdout io.Writer) error {
	steps := node.DefaultRegistry()
	// The journal is an index. It is compacted when the node starts so it does
	// not grow without bound.
	if removed, err := engine.Compact(context.Background(), config.StateDir, engine.DefaultRetention()); err != nil {
		fmt.Fprintf(os.Stderr, "eregion-node: journal compaction failed: %v\n", err)
	} else if removed > 0 {
		fmt.Fprintf(os.Stderr, "eregion-node: compacted %d journal tasks\n", removed)
	}
	reader := bufio.NewReader(stdin)
	writer := bufio.NewWriter(stdout)
	for {
		frame, err := node.ReadFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch frame.Type {
		case node.FrameHello:
			if frame.Version != node.ProtocolVersion {
				// Fail closed: the handshake is refused before any request is
				// read, and the node never guesses a protocol version.
				_ = node.WriteFrame(writer, node.Frame{Type: node.FrameError, Error: "unsupported protocol version"})
				_ = writer.Flush()
				return fmt.Errorf("unsupported protocol version %d", frame.Version)
			}
			if err := node.WriteFrame(writer, node.Frame{Type: node.FrameHello, Version: node.ProtocolVersion, Node: config.Node}); err != nil {
				return err
			}
			if err := writer.Flush(); err != nil {
				return err
			}
		case node.FrameRequest:
			if frame.Request == nil {
				if err := writeError(writer, "request frame has no request"); err != nil {
					return err
				}
				continue
			}
			response := Handle(config, steps, *frame.Request)
			if err := node.WriteFrame(writer, node.Frame{Type: node.FrameResponse, Response: &response}); err != nil {
				return err
			}
			if err := writer.Flush(); err != nil {
				return err
			}
		default:
			if err := writeError(writer, "unsupported frame type "+frame.Type); err != nil {
				return err
			}
		}
	}
}

func writeError(writer *bufio.Writer, reason string) error {
	if err := node.WriteFrame(writer, node.Frame{Type: node.FrameError, Error: reason}); err != nil {
		return err
	}
	return writer.Flush()
}

// Handle answers one request. A run request never executes the step in the
// serving process: the step runs detached so it outlives the channel.
func Handle(config Config, steps *node.StepRegistry, request node.Request) node.Response {
	// Register the request secrets before any detail, log, or event is written.
	registry := &secrets.Registry{}
	for _, value := range request.Secrets {
		registry.Add(value)
	}
	response := handle(config, steps, request)
	response.Detail = registry.Redact(response.Detail)
	return response
}

func handle(config Config, steps *node.StepRegistry, request node.Request) node.Response {
	if err := request.Execution.Validate(); err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.ValidationFailed), Detail: err.Error()}
	}
	switch request.Method {
	case node.MethodStatus:
		return statusResponse(config, request)
	case node.MethodRun:
		return runResponse(config, steps, request)
	default:
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.ValidationFailed), Detail: "unknown node method " + string(request.Method)}
	}
}

func statusResponse(config Config, request node.Request) node.Response {
	nodeEngine, err := engine.New(config.StateDir, config.Node, &secrets.Registry{})
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.Internal), Detail: err.Error()}
	}
	status, err := nodeEngine.Status(context.Background(), request.Execution)
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.PreconditionFailed), Detail: err.Error()}
	}
	return node.ResponseFromStatus(status)
}

// runResponse starts a detached node task and answers "running". It refuses
// to start a second live effect: the effect lock on the node decides that.
func runResponse(config Config, steps *node.StepRegistry, request node.Request) node.Response {
	step, err := steps.Build(request.Execution, request.Payload)
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.PreconditionFailed), Detail: err.Error()}
	}
	inputs, err := step.Inputs()
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.Internal), Detail: err.Error()}
	}
	inputsHash := engine.InputsHash(step.ID(), inputs)
	nodeEngine, err := engine.New(config.StateDir, config.Node, &secrets.Registry{})
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.Internal), Detail: err.Error()}
	}
	status, err := nodeEngine.Status(context.Background(), request.Execution)
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.PreconditionFailed), Detail: err.Error()}
	}
	if status.InputsHash != "" && status.InputsHash != inputsHash {
		return node.Response{
			Status:         engine.StatusFailed,
			ExecutionID:    request.Execution.ExecutionID(),
			InputsHash:     status.InputsHash,
			InputsMismatch: true,
			ErrorCode:      string(contract.PreconditionFailed),
			Detail:         "the node effect was recorded with different inputs",
		}
	}
	switch status.Status {
	case engine.StatusCompleted:
		// The effect is already durable. Report it; never apply it twice.
		return node.ResponseFromStatus(status)
	case engine.StatusRunning:
		return node.Response{
			Status:      engine.StatusRunning,
			ExecutionID: request.Execution.ExecutionID(),
			InputsHash:  inputsHash,
			Retryable:   true,
			Detail:      "another node task owns this effect",
		}
	}
	if err := SpawnTask(config, request); err != nil {
		return node.Response{Status: engine.StatusFailed, ExecutionID: request.Execution.ExecutionID(), ErrorCode: string(contract.Internal), Detail: err.Error()}
	}
	return node.Response{
		Status:      engine.StatusRunning,
		ExecutionID: request.Execution.ExecutionID(),
		InputsHash:  inputsHash,
		Retryable:   true,
		Detail:      "node task started",
	}
}

// SpawnTask starts the step in a detached child process. The child runs in
// its own session without inherited standard streams, so closing the SSH
// channel neither signals it nor writes into the channel. The request reaches
// the child through an inherited pipe, so a transient secret never touches
// the node's disk.
func SpawnTask(config Config, request node.Request) error {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return err
	}
	logPath, err := taskLogPath(config, request)
	if err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: the node's own log directory.
	if err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return err
	}
	defer func() { _ = logFile.Close() }()
	raw, err := json.Marshal(request)
	if err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return err
	}
	taskCommand := config.taskCommand()
	command := exec.Command(taskCommand[0], taskCommand[1:]...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.ExtraFiles = []*os.File{readEnd}
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return err
	}
	go func() {
		defer func() { _ = writeEnd.Close() }()
		_, _ = writeEnd.Write(raw)
	}()
	go func() { _ = command.Wait() }()
	return nil
}

// taskLogPath is the bounded log of one node task.
func taskLogPath(config Config, request node.Request) (string, error) {
	dir := filepath.Join(config.StateDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := strings.ReplaceAll(request.Execution.ExecutionID(), ":", "_")
	return filepath.Join(dir, name+".log"), nil
}
