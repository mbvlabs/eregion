package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/secrets"
	"golang.org/x/crypto/ssh"
)

// NodeCommand is the fixed command the control plane runs on a node. It
// carries no request value: the request travels over the channel's standard
// input, so nothing an operation supplies is ever interpreted by a shell.
const NodeCommand = "/usr/local/bin/eregion-node serve"

// DefaultTimeout bounds one connection and handshake.
const DefaultTimeout = 30 * time.Second

// DefaultPollInterval is how often a detached node task is asked for its
// status.
const DefaultPollInterval = 200 * time.Millisecond

// DefaultMaxWait bounds how long one call waits for a detached node task.
// The node task itself keeps running; only the control plane's wait ends.
const DefaultMaxWait = 10 * time.Minute

// userPattern accepts a POSIX user name. The user is not interpreted by a
// shell, but a strict name keeps configuration typos visible.
var userPattern = regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`)

// commandPattern accepts a remote command built from ordinary path and
// option characters. Shell metacharacters are rejected: the command is run
// by the node's login shell, and no untrusted value may reach it.
var commandPattern = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+( [A-Za-z0-9_@%+=:,./-]+)*$`)

// SSHConfig describes one node the control plane dispatches to.
type SSHConfig struct {
	// Address is the node's host:port.
	Address string
	// User is the node account. It must be unprivileged.
	User string
	// IdentityFile is the private key the control plane authenticates with.
	IdentityFile string
	// HostKeys holds the pinned host keys. A node without a pinned key is
	// refused.
	HostKeys *HostKeys
	// Command is the remote command. It defaults to NodeCommand.
	Command string
	// Timeout bounds the connection and the handshake.
	Timeout time.Duration
	// Dialer opens the byte stream. It defaults to a plain TCP dialer; a
	// userspace WireGuard dialer replaces it in the next increment.
	Dialer Dialer
	// ExpectedNode is the node name the handshake must report. Empty accepts
	// any name, but the host key still pins the host.
	ExpectedNode string
	// PollInterval is how often a detached task is asked for its status.
	PollInterval time.Duration
	// MaxWait bounds how long one call waits for a detached node task. The
	// node task keeps running when the wait ends; the outcome is unknown.
	MaxWait time.Duration
}

// SSHTransport dispatches node requests over pinned SSH.
type SSHTransport struct {
	config SSHConfig
	signer ssh.Signer
	dialer Dialer
}

var _ node.Transport = (*SSHTransport)(nil)

// NewSSHTransport validates the configuration and loads the node identity.
func NewSSHTransport(config SSHConfig) (*SSHTransport, error) {
	if config.Address == "" {
		return nil, errors.New("the node address is required")
	}
	if _, _, err := net.SplitHostPort(config.Address); err != nil {
		return nil, fmt.Errorf("the node address %q must be host:port", config.Address)
	}
	if !userPattern.MatchString(config.User) {
		return nil, fmt.Errorf("the node user %q is not a plain account name", config.User)
	}
	if config.IdentityFile == "" {
		return nil, errors.New("the node identity file is required")
	}
	info, err := os.Stat(config.IdentityFile)
	if err != nil {
		return nil, fmt.Errorf("read the node identity: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("the node identity %s must not be readable by group or others", config.IdentityFile)
	}
	raw, err := os.ReadFile(config.IdentityFile) //nolint:gosec // G304: the operator's own identity file.
	if err != nil {
		return nil, fmt.Errorf("read the node identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("the node identity %s is encrypted; use an unencrypted dedicated key", config.IdentityFile)
		}
		return nil, fmt.Errorf("parse the node identity: %w", err)
	}
	if config.HostKeys == nil {
		return nil, errors.New("the node host keys are required; a node without a pinned key is refused")
	}
	if !config.HostKeys.Pinned(config.Address) {
		return nil, fmt.Errorf("no host key is pinned for %s", config.Address)
	}
	command := config.Command
	if command == "" {
		command = NodeCommand
	}
	if !commandPattern.MatchString(command) {
		return nil, fmt.Errorf("the node command %q contains characters that are not allowed", command)
	}
	config.Command = command
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.PollInterval <= 0 {
		config.PollInterval = DefaultPollInterval
	}
	if config.MaxWait <= 0 {
		config.MaxWait = DefaultMaxWait
	}
	dialer := config.Dialer
	if dialer == nil {
		dialer = NetDialer{Timeout: config.Timeout}
	}
	return &SSHTransport{config: config, signer: signer, dialer: dialer}, nil
}

// Name identifies the transport in logs and diagnostics.
func (t *SSHTransport) Name() string { return "ssh" }

// Call dispatches one request to the node.
//
// A run request that the node accepts starts a detached node task. The
// transport then asks for the task's status until it is terminal. A lost
// stream is an explicit unknown outcome: it never means the node stopped, and
// the retry path asks for status before it runs anything.
func (t *SSHTransport) Call(ctx context.Context, request node.Request, onEvent func(node.Event)) (node.Response, error) {
	// Register the request secrets before any event, log, or error is written.
	registry := &secrets.Registry{}
	for _, value := range request.Secrets {
		registry.Add(value)
	}
	client, err := t.connect(ctx)
	if err != nil {
		return node.Response{}, err
	}
	defer func() { _ = client.Close() }()
	response, err := t.exchange(ctx, client, request, onEvent, registry)
	if err != nil {
		return node.Response{}, err
	}
	if request.Method != node.MethodRun || response.Status != engine.StatusRunning {
		return response, nil
	}
	return t.awaitTerminal(ctx, client, request, registry)
}

// connect opens the connection and verifies the pinned host key.
func (t *SSHTransport) connect(ctx context.Context) (*ssh.Client, error) {
	raw, err := t.dialer.DialContext(ctx, "tcp", t.config.Address)
	if err != nil {
		return nil, fmt.Errorf("dial the node %s: %w", t.config.Address, err)
	}
	callback := t.config.HostKeys.Callback(t.config.Address)
	var pinFailure *HostKeyError
	clientConfig := &ssh.ClientConfig{
		User:              t.config.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(t.signer)},
		HostKeyCallback:   callback,
		HostKeyAlgorithms: t.config.HostKeys.Algorithms(t.config.Address),
		Timeout:           t.config.Timeout,
	}
	conn, channels, requests, err := handshake(ctx, raw, t.config.Address, clientConfig, t.config.Timeout)
	if err != nil {
		_ = raw.Close()
		if errors.As(err, &pinFailure) {
			return nil, &HostKeyMismatchError{
				Address: t.config.Address,
				Got:     pinFailure.Got,
				Want:    pinFailure.Want,
				Unknown: pinFailure.Unknown,
			}
		}
		return nil, fmt.Errorf("the SSH handshake with the node %s failed: %w", t.config.Address, err)
	}
	return ssh.NewClient(conn, channels, requests), nil
}

// handshake performs the SSH handshake under a deadline that also honours the
// caller's context.
func handshake(ctx context.Context, conn net.Conn, address string, config *ssh.ClientConfig, timeout time.Duration) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-finished:
		}
	}()
	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	close(finished)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return sshConn, channels, requests, nil
}

// exchange runs one request on a fresh session of the connection.
func (t *SSHTransport) exchange(ctx context.Context, client *ssh.Client, request node.Request, onEvent func(node.Event), registry *secrets.Registry) (node.Response, error) {
	type outcome struct {
		response node.Response
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := t.runSession(client, request, onEvent, registry)
		done <- outcome{response: response, err: err}
	}()
	select {
	case result := <-done:
		return result.response, result.err
	case <-ctx.Done():
		// Closing the connection aborts the read. It never signals the remote
		// task: a node task is detached and keeps running.
		_ = client.Close()
		<-done
		return node.Response{}, fmt.Errorf("%w: the request ended before the node reported an outcome: %w", node.ErrStreamLost, ctx.Err())
	}
}

// runSession performs the handshake, sends the request, and reads the answer.
func (t *SSHTransport) runSession(client *ssh.Client, request node.Request, onEvent func(node.Event), registry *secrets.Registry) (node.Response, error) {
	session, err := client.NewSession()
	if err != nil {
		return node.Response{}, fmt.Errorf("%w: the node connection is gone: %w", node.ErrStreamLost, err)
	}
	defer func() { _ = session.Close() }()
	stdin, err := session.StdinPipe()
	if err != nil {
		return node.Response{}, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return node.Response{}, err
	}
	stderr := &tailBuffer{limit: StderrTailBytes}
	session.Stderr = stderr
	if err := session.Start(t.config.Command); err != nil {
		return node.Response{}, fmt.Errorf("start the node command: %w", err)
	}
	writer := bufio.NewWriter(stdin)
	reader := bufio.NewReader(stdout)

	if err := node.WriteFrame(writer, node.Frame{Type: node.FrameHello, Version: node.ProtocolVersion}); err != nil {
		return node.Response{}, t.streamFailure(err, stderr, registry)
	}
	if err := writer.Flush(); err != nil {
		return node.Response{}, t.streamFailure(err, stderr, registry)
	}
	hello, err := node.ReadFrame(reader)
	if err != nil {
		return node.Response{}, t.streamFailure(err, stderr, registry)
	}
	switch {
	case hello.Type == node.FrameError:
		return node.Response{}, &HandshakeError{Address: t.config.Address, Reason: hello.Error}
	case hello.Type != node.FrameHello:
		return node.Response{}, &HandshakeError{Address: t.config.Address, Reason: "unexpected frame " + hello.Type}
	case hello.Version != node.ProtocolVersion:
		return node.Response{}, &VersionMismatchError{Address: t.config.Address, Got: hello.Version, Want: node.ProtocolVersion}
	case t.config.ExpectedNode != "" && hello.Node != t.config.ExpectedNode:
		return node.Response{}, &HandshakeError{Address: t.config.Address, Reason: fmt.Sprintf("the node reports the name %q, expected %q", hello.Node, t.config.ExpectedNode)}
	}

	frame := node.Frame{Type: node.FrameRequest, Version: node.ProtocolVersion, Request: &request}
	if err := node.WriteFrame(writer, frame); err != nil {
		return node.Response{}, t.streamFailure(err, stderr, registry)
	}
	if err := writer.Flush(); err != nil {
		return node.Response{}, t.streamFailure(err, stderr, registry)
	}
	for {
		next, err := node.ReadFrame(reader)
		if err != nil {
			return node.Response{}, t.streamFailure(err, stderr, registry)
		}
		switch next.Type {
		case node.FrameEvent:
			if onEvent != nil && next.Event != nil {
				onEvent(node.Event{
					Kind:        next.Event.Kind,
					Step:        next.Event.Step,
					ExecutionID: next.Event.ExecutionID,
					Message:     registry.Redact(next.Event.Message),
				})
			}
		case node.FrameResponse:
			if next.Response == nil {
				return node.Response{}, errors.New("the node sent an empty response")
			}
			// The node redacts its own detail; the transport redacts again
			// before the detail reaches a log, an event, or an error.
			response := *next.Response
			response.Detail = registry.Redact(response.Detail)
			return response, nil
		case node.FrameError:
			return node.Response{}, &HandshakeError{Address: t.config.Address, Reason: next.Error}
		default:
			return node.Response{}, fmt.Errorf("the node sent an unexpected frame %q", next.Type)
		}
	}
}

// awaitTerminal asks for the status of a detached node task until it is
// terminal. A lost stream stays an explicit unknown outcome.
func (t *SSHTransport) awaitTerminal(ctx context.Context, client *ssh.Client, request node.Request, registry *secrets.Registry) (node.Response, error) {
	interval := wait(t.config.PollInterval)
	deadline := time.Now().Add(t.config.MaxWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	for {
		if !time.Now().Before(deadline) {
			return node.Response{}, fmt.Errorf("%w: the node task is still running after %s; the node may still finish it", node.ErrStreamLost, t.config.MaxWait)
		}
		waitFor := interval
		if remaining := time.Until(deadline); remaining < waitFor {
			waitFor = remaining
		}
		select {
		case <-ctx.Done():
			return node.Response{}, fmt.Errorf("%w: the node task is still running and the request ended: %w", node.ErrStreamLost, ctx.Err())
		case <-time.After(waitFor):
		}
		status := node.Request{Method: node.MethodStatus, RequestID: request.RequestID, Execution: request.Execution}
		response, err := t.exchange(ctx, client, status, nil, registry)
		if err != nil {
			return node.Response{}, err
		}
		switch response.Status {
		case engine.StatusCompleted, engine.StatusFailed:
			return response, nil
		}
	}
}

// streamFailure classifies a failed exchange. A broken stream is an explicit
// lost-stream outcome; a malformed frame is a protocol error.
func (t *SSHTransport) streamFailure(err error, stderr *tailBuffer, registry *secrets.Registry) error {
	if errors.Is(err, node.ErrProtocol) {
		return fmt.Errorf("the node sent an invalid frame: %w%s", err, stderr.detail(registry.Redact))
	}
	return fmt.Errorf("%w: the node stream ended before the node reported an outcome: %w%s", node.ErrStreamLost, err, stderr.detail(registry.Redact))
}
