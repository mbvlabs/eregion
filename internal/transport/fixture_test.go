package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/sshfixture"
	"golang.org/x/crypto/ssh"
)

// startFixture starts the disposable SSH node. The test skips when Docker or
// the fixture image is not available.
func startFixture(t *testing.T) *sshfixture.Fixture {
	t.Helper()
	return startFixtureWith(t, sshfixture.Options{})
}

// startFixtureWith starts the disposable SSH node with explicit options.
func startFixtureWith(t *testing.T, options sshfixture.Options) *sshfixture.Fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fixture, err := sshfixture.Start(ctx, options)
	if err != nil {
		if errors.Is(err, sshfixture.ErrUnavailable) {
			t.Skipf("the SSH fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close(context.Background()) })
	return fixture
}

// fixtureTransport builds a client for the disposable node.
func fixtureTransport(t *testing.T, fixture *sshfixture.Fixture) *SSHTransport {
	t.Helper()
	keys, err := LoadHostKeys(fixture.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewSSHTransport(SSHConfig{
		Address:      fixture.Address,
		User:         "eregion",
		IdentityFile: fixture.IdentityFile,
		HostKeys:     keys,
		ExpectedNode: "fixture-node",
		Timeout:      30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSSHFixtureRunsAndReportsStatus(t *testing.T) {
	fixture := startFixture(t)
	client := fixtureTransport(t, fixture)
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
	count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("the node wrote %d effects", count)
	}
}

func TestSSHFixtureRefusesAnUnpinnedHostKeyBeforeExecution(t *testing.T) {
	fixture := startFixture(t)
	keys := NewHostKeys()
	other, err := sshSignerForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	keys.Pin(fixture.Address, other)
	client, err := NewSSHTransport(SSHConfig{
		Address:      fixture.Address,
		User:         "eregion",
		IdentityFile: fixture.IdentityFile,
		HostKeys:     keys,
		Timeout:      30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := testExecution(t, node.DiagnosticStepID(1))
	if _, err := client.Call(context.Background(), runRequest(t, exec, 0), nil); err == nil {
		t.Fatal("a host key mismatch was accepted")
	} else if !IsPermanentTransport(err) {
		t.Fatalf("a host key mismatch is not permanent: %v", err)
	}
	count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("the node ran %d effects before the host key was verified", count)
	}
}

func TestSSHFixtureDuplicateRequestsProduceOneEffect(t *testing.T) {
	fixture := startFixture(t)
	client := fixtureTransport(t, fixture)
	exec := testExecution(t, node.DiagnosticStepID(1))
	for range 2 {
		response, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.Status != engine.StatusCompleted {
			t.Fatalf("duplicate response: %+v", response)
		}
	}
	count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate requests produced %d effects", count)
	}
}

func TestSSHFixtureCancellationDoesNotKillTheNodeTask(t *testing.T) {
	fixture := startFixture(t)
	client := fixtureTransport(t, fixture)
	exec := testExecution(t, node.DiagnosticStepID(1))
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		response node.Response
		err      error
	}
	results := make(chan result, 1)
	go func() {
		response, callErr := client.Call(ctx, runRequest(t, exec, 1000), nil)
		results <- result{response: response, err: callErr}
	}()
	// Cancel only after the node journal records the running task. A cancel
	// during the SSH handshake never dispatched the request, so it is not this
	// scenario's unknown outcome.
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case got := <-results:
			t.Fatalf("the call ended before the node task started: %v", got.err)
		default:
		}
		pid, err := fixture.TaskOwnerPID(context.Background(), string(exec.OperationID))
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node task did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	var got result
	select {
	case got = <-results:
	case <-time.After(60 * time.Second):
		t.Fatal("the cancelled call did not return")
	}
	if got.err == nil {
		t.Fatal("a cancelled call reported success")
	}
	if !node.IsStreamLost(got.err) {
		t.Fatalf("a cancelled call is not an explicit unknown outcome: %v", got.err)
	}
	effectDeadline := time.Now().Add(20 * time.Second)
	for {
		count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			return
		}
		if time.Now().After(effectDeadline) {
			t.Fatal("the node task was killed by the cancellation")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSSHFixtureRecoversThroughStatusAfterALostResponse(t *testing.T) {
	fixture := startFixture(t)
	proxy, err := sshfixture.NewCutProxy(fixture.Address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	// The proxy forwards to the node, so it presents the node's host key. The
	// pin follows the address the client dials.
	keys := NewHostKeys()
	keys.Pin(proxy.Address(), fixture.HostKey)

	client, err := NewSSHTransport(SSHConfig{
		Address:      proxy.Address(),
		User:         "eregion",
		IdentityFile: fixture.IdentityFile,
		HostKeys:     keys,
		ExpectedNode: "fixture-node",
		Timeout:      30 * time.Second,
		PollInterval: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := testExecution(t, node.DiagnosticStepID(1))
	request := runRequest(t, exec, 1000)
	type result struct {
		response node.Response
		err      error
	}
	results := make(chan result, 1)
	go func() {
		response, callErr := client.Call(context.Background(), request, nil)
		results <- result{response: response, err: callErr}
	}()
	// Cut only after the node journal records the running task. A time-based
	// cut can land in the SSH handshake on a loaded runner and is then an
	// ordinary connect failure, not a lost response.
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case got := <-results:
			t.Fatalf("the call ended before the node task started: %v", got.err)
		default:
		}
		pid, err := fixture.TaskOwnerPID(context.Background(), string(exec.OperationID))
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node task did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if proxy.Cut() == 0 {
		t.Fatal("the proxy cut no connection")
	}
	var got result
	select {
	case got = <-results:
	case <-time.After(60 * time.Second):
		t.Fatal("the cut call did not return")
	}
	if got.err == nil {
		t.Fatal("the cut connection was reported as success")
	}
	if !node.IsStreamLost(got.err) {
		t.Fatalf("the lost response is not explicit: %v", got.err)
	}
	if proxy.Cuts() == 0 {
		t.Fatal("the proxy did not cut the connection")
	}
	// The node task outlived the caller: the effect is applied.
	effectDeadline := time.Now().Add(20 * time.Second)
	for {
		count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(effectDeadline) {
			t.Fatal("the node task did not survive the lost response")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The retry path asks for status before it runs anything.
	status, err := client.Call(context.Background(), statusRequest(exec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != engine.StatusCompleted {
		t.Fatalf("status after a lost response: %+v", status)
	}
	count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("the retry applied the effect again: %d effects", count)
	}
}

func TestSSHFixtureReportsAProtocolRefusal(t *testing.T) {
	fixture := startFixture(t)
	client := fixtureTransport(t, fixture)
	exec := testExecution(t, node.DiagnosticStepID(1))
	// An unknown step is refused by the node; the transport reports it as a
	// failed response, not as a lost stream.
	request := node.Request{Method: node.MethodRun, RequestID: "unknown", Execution: exec, Payload: json.RawMessage(`{}`)}
	response, err := client.Call(context.Background(), request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusFailed {
		t.Fatalf("an unknown step did not fail closed: %+v", response)
	}
	if response.ErrorCode == "" {
		t.Fatal("the node did not classify the failure")
	}
	count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a refused request wrote %d effects", count)
	}
}

// TestSSHFixtureReportsADeadNodeTaskPromptly proves that a node task that
// died mid-step is reported right away, with the node task log, instead of
// being polled until the caller's deadline. The next attempt reconciles the
// effect and applies it exactly once.
func TestSSHFixtureReportsADeadNodeTaskPromptly(t *testing.T) {
	fixture := startFixture(t)
	client := fixtureTransport(t, fixture)
	exec := testExecution(t, node.DiagnosticStepID(1))
	type result struct {
		response node.Response
		err      error
	}
	results := make(chan result, 1)
	go func() {
		response, err := client.Call(context.Background(), runRequest(t, exec, 1000), nil)
		results <- result{response: response, err: err}
	}()
	// Kill the node task while it holds the effect lock and waits for its
	// step delay.
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		pid, err := fixture.TaskOwnerPID(ctx, string(exec.OperationID))
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			if err := fixture.KillTask(ctx, string(exec.OperationID)); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node task did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var got result
	select {
	case got = <-results:
	case <-time.After(60 * time.Second):
		t.Fatal("the dead node task was not reported")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.response.Status != engine.StatusFailed || !got.response.Retryable {
		t.Fatalf("a dead node task is not a retryable failure: %+v", got.response)
	}
	if !strings.Contains(got.response.Detail, "diagnostic marker is absent") {
		t.Fatalf("the node task log is missing from the detail: %+v", got.response)
	}
	// The next attempt reconciles the effect: it is applied exactly once.
	recovered, err := client.Call(context.Background(), runRequest(t, exec, 1000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != engine.StatusCompleted {
		t.Fatalf("the recovery did not complete the step: %+v", recovered)
	}
	count, err := fixture.EffectCount(ctx, string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("the recovery applied %d effects", count)
	}
}

// TestSSHFixtureReportsANodeTaskThatNeverStarted proves that a node task that
// never reaches the node journal is reported as a failed start, with the node
// task log, instead of being polled until the caller's deadline.
func TestSSHFixtureReportsANodeTaskThatNeverStarted(t *testing.T) {
	fixture := startFixtureWith(t, sshfixture.Options{TaskCommand: []string{
		"/bin/sh", "-c", "cat <&3 >/dev/null; echo 'the node task could not start' >&2; exit 1",
	}})
	client := fixtureTransport(t, fixture)
	exec := testExecution(t, node.DiagnosticStepID(1))
	started := time.Now()
	response, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("a node task that never started was not reported promptly: %s", elapsed)
	}
	if response.Status != engine.StatusFailed || !response.Retryable {
		t.Fatalf("a node task that never started is not a retryable failure: %+v", response)
	}
	if !strings.Contains(response.Detail, "the node task could not start") {
		t.Fatalf("the node task log is missing from the detail: %+v", response)
	}
	count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a node task that never started applied %d effects", count)
	}
}

// sshSignerForTest returns a host key that is not the node's.
func sshSignerForTest(t *testing.T) (ssh.PublicKey, error) {
	signer, err := ssh.NewSignerFromKey(mustGenerateKey(t))
	if err != nil {
		return nil, err
	}
	return signer.PublicKey(), nil
}
