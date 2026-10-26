package transport

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/noded"
	"golang.org/x/crypto/ssh"
)

// testServer is an in-process SSH node. It speaks the node protocol over the
// SSH channel and runs the real node implementation, so a test can drive the
// deterministic protocol failures that a container cannot produce on demand.
type testServer struct {
	listener net.Listener
	hostKey  ssh.PublicKey
	signer   ssh.Signer
	client   ssh.PublicKey
	identity string
	config   noded.Config

	// version is the protocol version the server reports. Zero reports the
	// current version.
	version int
	// dropResponse closes the connection after the node accepted a request
	// but before it reported the outcome.
	dropResponse bool
	// refuseRequest closes the connection before the request is handled.
	refuseRequest bool
	// stderr is written to the channel's standard error before the request is
	// handled.
	stderr string
	// transportConfig adjusts the client configuration of the test.
	transportConfig func(*SSHConfig)

	mu      sync.Mutex
	closed  bool
	stopped chan struct{}
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	identity := filepath.Join(directory, "id_ed25519")
	block, err := ssh.MarshalPrivateKey(clientKey, "eregion-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testServer{
		listener: listener,
		hostKey:  hostSigner.PublicKey(),
		signer:   hostSigner,
		client:   clientSigner.PublicKey(),
		identity: identity,
		stopped:  make(chan struct{}),
	}
	server.config = noded.Config{
		Node:        "test-node",
		StateDir:    t.TempDir(),
		Path:        filepath.Join(directory, "node.json"),
		TaskCommand: []string{os.Args[0], "-test.run=^TestNodeTaskHelper$"},
	}
	raw, err := json.Marshal(map[string]string{"node": server.config.Node, "state_dir": server.config.StateDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.config.Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The detached task helper inherits the configuration through the
	// environment, the same way the node binary finds it.
	t.Setenv("EREGION_NODE_TASK_HELPER", "1")
	t.Setenv("EREGION_NODE_CONFIG", server.config.Path)
	go server.accept()
	t.Cleanup(server.Close)
	return server
}

func (s *testServer) address() string { return s.listener.Addr().String() }

// An effect marker (and even completed status) precedes the detached helper's
// final journal compaction. Wait for the owning process to exit before TempDir
// cleanup; cancelling the caller must still leave the task itself untouched.
func (s *testServer) waitTaskExit(t *testing.T, exec engine.Execution) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		journal, err := engine.LoadJournal(engine.JournalPath(s.config.StateDir))
		if err == nil {
			if task, ok := journal.Tasks[exec.EffectKey()]; ok && task.Owner.PID > 0 {
				if errors.Is(syscall.Kill(task.Owner.PID, 0), syscall.ESRCH) {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detached node task did not exit before fixture cleanup")
}

func (s *testServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.listener.Close()
	close(s.stopped)
}

func (s *testServer) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *testServer) handle(conn net.Conn) {
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), s.client.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown client key")
		},
	}
	config.AddHostKey(s.signer)
	sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer func() { _ = sshConn.Close() }()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(channel, channelRequests)
	}
}

func (s *testServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		_ = request.Reply(true, nil)
		if s.stderr != "" {
			_, _ = channel.Stderr().Write([]byte(s.stderr))
		}
		s.serve(channel)
		return
	}
}

// serve speaks the node protocol on one SSH channel.
func (s *testServer) serve(channel ssh.Channel) {
	reader := bufio.NewReader(channel)
	writer := bufio.NewWriter(channel)
	for {
		frame, err := node.ReadFrame(reader)
		if err != nil {
			return
		}
		switch frame.Type {
		case node.FrameHello:
			version := s.version
			if version == 0 {
				version = node.ProtocolVersion
			}
			if err := node.WriteFrame(writer, node.Frame{Type: node.FrameHello, Version: version, Node: s.config.Node}); err != nil {
				return
			}
			if err := writer.Flush(); err != nil {
				return
			}
		case node.FrameRequest:
			if frame.Request == nil {
				continue
			}
			if s.refuseRequest {
				return
			}
			response := noded.Handle(s.config, node.DefaultRegistry(), *frame.Request)
			if s.dropResponse && frame.Request.Method == node.MethodRun {
				// The node accepted the request and started the task; the
				// stream dies before the outcome is reported.
				return
			}
			if err := node.WriteFrame(writer, node.Frame{Type: node.FrameResponse, Response: &response}); err != nil {
				return
			}
			if err := writer.Flush(); err != nil {
				return
			}
		}
	}
}

// transport builds a client for the test server.
func (s *testServer) transport(t *testing.T, keys *HostKeys) *SSHTransport {
	t.Helper()
	if keys == nil {
		keys = NewHostKeys()
		keys.Pin(s.address(), s.hostKey)
	}
	config := SSHConfig{
		Address:      s.address(),
		User:         "eregion",
		IdentityFile: s.identity,
		HostKeys:     keys,
		Command:      "eregion node-agent serve",
		ExpectedNode: s.config.Node,
		Timeout:      10 * time.Second,
	}
	if s.transportConfig != nil {
		s.transportConfig(&config)
	}
	client, err := NewSSHTransport(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestNodeTaskHelper runs one detached node task. The transport test starts
// it as a child process with the request on an inherited pipe.
func TestNodeTaskHelper(t *testing.T) {
	if os.Getenv("EREGION_NODE_TASK_HELPER") != "1" {
		t.Skip("node task helper")
	}
	config, err := noded.LoadConfig(os.Getenv("EREGION_NODE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("EREGION_NODE_TASK_NEVER_STARTS") == "1" {
		// A task command that cannot start: it reads the request and then
		// fails before the node journal ever hears about the task.
		readTaskRequest()
		fmt.Fprintln(os.Stderr, "the node task could not start")
		os.Exit(1)
	}
	if path := os.Getenv("EREGION_NODE_TASK_DIE_FILE"); path != "" {
		dieMidStep(path, config.StateDir)
	}
	if err := noded.RunTask(config); err != nil {
		t.Fatal(err)
	}
}

// readTaskRequest reads the request a detached task receives on its inherited
// pipe. The helper needs it so the parent's write never fails on a closed
// pipe.
func readTaskRequest() {
	file := os.NewFile(noded.TaskRequestFD, "node-request")
	if file == nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(file, node.MaxFrameBytes))
}

// dieMidStep kills the task once the node journal records it, so the node
// observes a task that died mid-step. The marker file keeps every later task
// alive.
func dieMidStep(path, stateDir string) {
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.WriteFile(path, []byte("died\n"), 0o600); err != nil {
		return
	}
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if journalHasRunningTask(stateDir) {
				fmt.Fprintln(os.Stderr, "the node task died mid-step")
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// journalHasRunningTask reports whether the node journal records a task that
// is running.
func journalHasRunningTask(stateDir string) bool {
	journal, err := engine.LoadJournal(engine.JournalPath(stateDir))
	if err != nil {
		return false
	}
	for _, task := range journal.Tasks {
		if task.Status == engine.TaskRunning {
			return true
		}
	}
	return false
}
