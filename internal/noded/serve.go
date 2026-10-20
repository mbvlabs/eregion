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
//
// The journal is not compacted here. Every status poll starts a new serving
// process, and a compaction on every poll would take the journal lock away
// from a running step. The journal is compacted after each task and by the
// explicit "compact" command.
func Serve(config Config, stdin io.Reader, stdout io.Writer) error {
	steps := node.DefaultRegistry()
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

// TaskLogTailBytes bounds the node task log tail the node returns with a
// status response. The tail explains a task that died, never started, or
// failed; it is bounded so a chatty task cannot exhaust the control plane,
// and it stays far below the protocol's frame limit.
const TaskLogTailBytes = 8 << 10

func statusResponse(config Config, request node.Request) node.Response {
	nodeEngine, err := engine.New(config.StateDir, config.Node, &secrets.Registry{})
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.Internal), Detail: err.Error()}
	}
	status, err := nodeEngine.Status(context.Background(), request.Execution)
	if err != nil {
		return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.PreconditionFailed), Detail: err.Error()}
	}
	response := node.ResponseFromStatus(status)
	if !explainsTaskOutcome(status.Status) {
		return response
	}
	tail, err := taskLogTail(config, request, len(response.Detail))
	if err != nil || tail == "" {
		return response
	}
	response.Detail = joinDetail(response.Detail, tail)
	return response
}

// explainsTaskOutcome reports whether a status is one where the node task log
// explains what happened: a task that died, a task that never started, or a
// task that failed. A running or completed task needs no log.
func explainsTaskOutcome(status engine.Status) bool {
	switch status {
	case engine.StatusStale, engine.StatusAbsent, engine.StatusFailed:
		return true
	default:
		return false
	}
}

// taskLogTail returns the bounded tail of the node task log for one
// execution. A missing log is not an error: the task may never have started.
//
// The node redacts the tail with the secrets of the request it answers. A
// status request carries none, so the transport redacts the tail again with
// the secrets of the run request before it becomes an error detail.
func taskLogTail(config Config, request node.Request, detail int) (string, error) {
	limit := TaskLogTailBytes
	// The tail travels in the same frame as the rest of the response, so the
	// frame never exceeds the protocol's limit.
	if room := node.MaxFrameBytes - detail - 1024; room < limit {
		limit = room
	}
	if limit <= 0 {
		return "", nil
	}
	file, err := os.Open(taskLogPath(config, request)) //nolint:gosec // G304: the node's own log path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	// Only the tail of the log is read: a chatty task must not be able to
	// exhaust the node's memory.
	if info.Size() > int64(limit) {
		if _, err := file.Seek(info.Size()-int64(limit), io.SeekStart); err != nil {
			return "", err
		}
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// joinDetail appends the node task log to a status detail.
func joinDetail(detail, tail string) string {
	if detail == "" {
		return "node task log:\n" + tail
	}
	return detail + "\nnode task log:\n" + tail
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
//
// The request is written before this function returns, so a serving process
// that exits as soon as the channel closes never drops it. A failed write is
// an explicit error: the child is stopped instead of running without a
// request.
func SpawnTask(config Config, request node.Request) error {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return err
	}
	if err := taskLogDir(config); err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return err
	}
	logFile, err := os.OpenFile(taskLogPath(config, request), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: the node's own log directory.
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
	// The child owns its own copy of the read end; this process does not need
	// it and must not hold the request pipe open.
	_ = readEnd.Close()
	// The request is written synchronously. The child reads it immediately,
	// and a serving process that exits when the channel closes must never
	// drop it. The request is bounded by the protocol's frame limit.
	if _, err := writeEnd.Write(raw); err != nil {
		_ = writeEnd.Close()
		// The child never received the request, so it cannot do anything
		// useful. It is stopped instead of being left behind.
		_ = command.Process.Kill()
		go func() { _ = command.Wait() }()
		return fmt.Errorf("write the node task request: %w", err)
	}
	// The write end is closed so the child sees the end of the request.
	_ = writeEnd.Close()
	go func() { _ = command.Wait() }()
	return nil
}

// taskLogPath is the bounded log of one node task.
func taskLogPath(config Config, request node.Request) string {
	name := strings.ReplaceAll(request.Execution.ExecutionID(), ":", "_")
	return filepath.Join(config.StateDir, "logs", name+".log")
}

// taskLogDir creates the node's private log directory.
func taskLogDir(config Config) error {
	return os.MkdirAll(filepath.Join(config.StateDir, "logs"), 0o700)
}
