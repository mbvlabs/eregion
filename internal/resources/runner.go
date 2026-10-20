package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Command is one program invocation. Args is an argument list; it is never
// joined into a shell string. Secrets must be passed through protected files,
// not through Args or Env. The service itself only passes paths and identifiers.
type Command struct {
	Name   string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// CommandResult is the outcome of a started command. ExitCode is meaningful
// whenever error is nil; a non-zero exit is a completed command, not a failure
// to start it.
type CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Runner executes one command with time-bounded context handling. Implementations
// must not log the command, its arguments, or its environment. NodeIdentity is
// part of the execution boundary: a service only acts when the runner proves it
// is the configured node. A local runner must reject a nonlocal host instead of
// implying that a Host string selects a remote machine.
type Runner interface {
	Run(context.Context, Command) (CommandResult, error)
	NodeIdentity(context.Context) (string, error)
}

// ExecRunner runs commands directly with exec.CommandContext. It never uses a
// shell. Output not redirected by Command is captured up to outputLimit bytes so
// a noisy database command cannot exhaust memory. It is a local-only runner: the
// verified node identity is the local Docker daemon name.
type ExecRunner struct{}

// NodeIdentity returns the Docker daemon name that this process actually
// controls. It is the code-boundary proof used to reject a Host that names a
// different machine.
func (ExecRunner) NodeIdentity(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Name}}") //nolint:gosec // G204: fixed program and argument list; no shell.
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read local Docker node identity: %w", err)
	}
	name := strings.TrimSpace(string(output))
	if name == "" {
		return "", errors.New("local Docker node identity is empty")
	}
	return name, nil
}

const outputLimit = 64 * 1024

func (ExecRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	if command.Name == "" {
		return CommandResult{}, errors.New("command name is required")
	}
	cmd := exec.CommandContext(ctx, command.Name, command.Args...) //nolint:gosec // G204: fixed program plus an argument list; no shell.
	cmd.Dir = command.Dir
	if len(command.Env) > 0 {
		cmd.Env = append(os.Environ(), command.Env...)
	}
	cmd.Stdin = command.Stdin

	stdout := newCapture(command.Stdout)
	stderr := newCapture(command.Stderr)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	result := CommandResult{ExitCode: cmd.ProcessState.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero exit is a completed command. Callers decide whether that is
		// a health observation or a failed effect.
		return result, nil
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("command %s did not finish: %w", command.Name, ctx.Err())
	}
	return result, fmt.Errorf("start command %s: %w", command.Name, err)
}

// capture writes to the caller's writer and/or keeps a bounded copy. It is safe
// for the separate stdout and stderr pipes exec starts.
type capture struct {
	mu      sync.Mutex
	writer  io.Writer
	buffer  bytes.Buffer
	limited int
}

func newCapture(writer io.Writer) *capture {
	return &capture{writer: writer, limited: outputLimit}
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writer != nil {
		if _, err := c.writer.Write(p); err != nil {
			return 0, err
		}
	}
	remaining := c.limited - c.buffer.Len()
	if remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = c.buffer.Write(p[:remaining])
	}
	return len(p), nil
}

func (c *capture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.buffer.Len())
	copy(out, c.buffer.Bytes())
	return out
}
