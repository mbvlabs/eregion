package transport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"golang.org/x/crypto/ssh"
)

func testExecution(t *testing.T, step string) engine.Execution {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return engine.Execution{OperationID: id, Attempt: 1, Step: step, RequestID: "request"}
}

func runRequest(t *testing.T, exec engine.Execution, delay int) node.Request {
	t.Helper()
	raw, err := json.Marshal(node.DiagnosticPayload{Index: 1, Total: 1, StepDelayMS: delay})
	if err != nil {
		t.Fatal(err)
	}
	return node.Request{Method: node.MethodRun, RequestID: "request", Execution: exec, Payload: raw}
}

func statusRequest(exec engine.Execution) node.Request {
	return node.Request{Method: node.MethodStatus, RequestID: "status", Execution: exec}
}

func markerPath(stateDir string, exec engine.Execution) string {
	return filepath.Join(stateDir, "effects", string(exec.OperationID), exec.Step+".applied")
}

func TestSSHTransportRunsAndReportsStatus(t *testing.T) {
	server := newTestServer(t)
	client := server.transport(t, nil)
	exec := testExecution(t, node.DiagnosticStepID(1))
	response, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted {
		t.Fatalf("run response: %+v", response)
	}
	status, err := client.Call(context.Background(), statusRequest(exec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != engine.StatusCompleted || status.InputsHash == "" {
		t.Fatalf("status response: %+v", status)
	}
	if _, err := os.Stat(markerPath(server.config.StateDir, exec)); err != nil {
		t.Fatalf("the node did not write the effect: %v", err)
	}
}

func TestSSHTransportRefusesAnUnpinnedHostKey(t *testing.T) {
	server := newTestServer(t)
	other, err := ssh.NewSignerFromKey(mustGenerateKey(t))
	if err != nil {
		t.Fatal(err)
	}
	keys := NewHostKeys()
	keys.Pin(server.address(), other.PublicKey())
	client := server.transport(t, keys)
	exec := testExecution(t, node.DiagnosticStepID(1))
	_, err = client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err == nil {
		t.Fatal("a host key mismatch was accepted")
	}
	if !IsPermanentTransport(err) {
		t.Fatalf("a host key mismatch is not permanent: %v", err)
	}
	var mismatch *HostKeyMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(markerPath(server.config.StateDir, exec)); !os.IsNotExist(statErr) {
		t.Fatal("the node ran a request before the host key was verified")
	}
}

func TestSSHTransportFailsClosedOnAVersionMismatch(t *testing.T) {
	server := newTestServer(t)
	server.version = node.ProtocolVersion + 1
	client := server.transport(t, nil)
	exec := testExecution(t, node.DiagnosticStepID(1))
	_, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err == nil {
		t.Fatal("a version mismatch was accepted")
	}
	var mismatch *VersionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !IsPermanentTransport(err) {
		t.Fatal("a version mismatch is not permanent")
	}
	if _, statErr := os.Stat(markerPath(server.config.StateDir, exec)); !os.IsNotExist(statErr) {
		t.Fatal("the node ran a request before the version was accepted")
	}
}

func TestSSHTransportReportsALostStreamWithoutStoppingTheNode(t *testing.T) {
	server := newTestServer(t)
	server.dropResponse = true
	client := server.transport(t, nil)
	exec := testExecution(t, node.DiagnosticStepID(1))
	_, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err == nil {
		t.Fatal("a lost stream was reported as success")
	}
	if !node.IsStreamLost(err) {
		t.Fatalf("a lost stream is not explicit: %v", err)
	}
	// The node task outlived the caller: the effect is still applied.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(markerPath(server.config.StateDir, exec)); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node task did not survive the lost stream")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The retry path asks for status before it runs anything, and the status
	// reports the completed effect.
	status, err := client.Call(context.Background(), statusRequest(exec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != engine.StatusCompleted {
		t.Fatalf("status after a lost stream: %+v", status)
	}
}

func TestSSHTransportDuplicateRequestsProduceOneEffect(t *testing.T) {
	server := newTestServer(t)
	client := server.transport(t, nil)
	exec := testExecution(t, node.DiagnosticStepID(1))
	var wait sync.WaitGroup
	results := make(chan node.Response, 2)
	for range 2 {
		wait.Go(func() {
			response, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
			if err != nil {
				t.Error(err)
				return
			}
			results <- response
		})
	}
	wait.Wait()
	close(results)
	for response := range results {
		if response.Status != engine.StatusCompleted {
			t.Fatalf("duplicate response: %+v", response)
		}
	}
	entries, err := os.ReadDir(filepath.Join(server.config.StateDir, "effects", string(exec.OperationID)))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".applied") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate requests produced %d effects", count)
	}
}

func TestSSHTransportCancellationDoesNotKillTheNodeTask(t *testing.T) {
	server := newTestServer(t)
	client := server.transport(t, nil)
	exec := testExecution(t, node.DiagnosticStepID(1))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := client.Call(ctx, runRequest(t, exec, 400), nil)
	if err == nil {
		t.Fatal("a cancelled call reported success")
	}
	if !node.IsStreamLost(err) {
		t.Fatalf("a cancelled call is not an explicit unknown outcome: %v", err)
	}
	// The detached node task is not signalled: the effect is still applied.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(markerPath(server.config.StateDir, exec)); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node task was killed by the cancellation")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSSHTransportRequiresAPinnedHostKey(t *testing.T) {
	server := newTestServer(t)
	_, err := NewSSHTransport(SSHConfig{
		Address:      server.address(),
		User:         "eregion",
		IdentityFile: server.identity,
		HostKeys:     NewHostKeys(),
	})
	if err == nil {
		t.Fatal("an unpinned node was accepted")
	}
}

func TestSSHTransportRejectsACommandWithShellMetacharacters(t *testing.T) {
	server := newTestServer(t)
	_, err := NewSSHTransport(SSHConfig{
		Address:      server.address(),
		User:         "eregion",
		IdentityFile: server.identity,
		HostKeys:     NewHostKeys(),
		Command:      "eregion-node serve; rm -rf /",
	})
	if err == nil {
		t.Fatal("a command with shell metacharacters was accepted")
	}
}

func TestSSHTransportRejectsAWorldReadableIdentity(t *testing.T) {
	server := newTestServer(t)
	loose := filepath.Join(t.TempDir(), "id_ed25519")
	raw, err := os.ReadFile(server.identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	keys := NewHostKeys()
	keys.Pin(server.address(), server.hostKey)
	_, err = NewSSHTransport(SSHConfig{
		Address:      server.address(),
		User:         "eregion",
		IdentityFile: loose,
		HostKeys:     keys,
	})
	if err == nil {
		t.Fatal("a world-readable identity was accepted")
	}
}

func TestSSHTransportRedactsSecretsInTheStderrTail(t *testing.T) {
	const secret = "leaked-secret-value"
	server := newTestServer(t)
	server.stderr = "the node wrote " + secret + " to standard error\n"
	server.dropResponse = true
	client := server.transport(t, nil)
	exec := testExecution(t, node.DiagnosticStepID(1))
	request := runRequest(t, exec, 0)
	request.Secrets = map[string]string{"token": secret}
	_, err := client.Call(context.Background(), request, nil)
	if err == nil {
		t.Fatal("a lost stream was reported as success")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the stderr tail leaked a secret: %v", err)
	}
}

func TestSSHTransportBoundsTheStderrTail(t *testing.T) {
	server := newTestServer(t)
	server.stderr = strings.Repeat("noise ", 40000)
	server.dropResponse = true
	client := server.transport(t, nil)
	exec := testExecution(t, node.DiagnosticStepID(1))
	_, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err == nil {
		t.Fatal("a lost stream was reported as success")
	}
	if len(err.Error()) > StderrTailBytes+512 {
		t.Fatalf("the stderr tail is not bounded: %d bytes", len(err.Error()))
	}
}

func TestHostKeysLoadKnownHosts(t *testing.T) {
	server := newTestServer(t)
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(server.hostKey)))
	if err := os.WriteFile(path, []byte(HostID(server.address())+" "+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadHostKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if !keys.Pinned(server.address()) {
		t.Fatal("the known_hosts entry was not loaded")
	}
	callback := keys.Callback(server.address())
	if err := callback("", nil, server.hostKey); err != nil {
		t.Fatalf("the pinned key was rejected: %v", err)
	}
	other, err := ssh.NewSignerFromKey(mustGenerateKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("", nil, other.PublicKey()); err == nil {
		t.Fatal("an unpinned key was accepted")
	}
}
