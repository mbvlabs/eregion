package transport

import (
	"context"
	"encoding/json"
	"errors"
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fixture, err := sshfixture.Start(ctx, sshfixture.Options{})
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
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if _, err := client.Call(ctx, runRequest(t, exec, 400), nil); err == nil {
		t.Fatal("a cancelled call reported success")
	} else if !node.IsStreamLost(err) {
		t.Fatalf("a cancelled call is not an explicit unknown outcome: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the node task was killed by the cancellation")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSSHFixtureRecoversThroughStatusAfterALostResponse(t *testing.T) {
	fixture := startFixture(t)
	proxy, err := sshfixture.NewCutProxy(fixture.Address, 50*time.Millisecond, 150*time.Millisecond)
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
	_, err = client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err == nil {
		t.Fatal("the cut connection was reported as success")
	}
	if !node.IsStreamLost(err) {
		t.Fatalf("the lost response is not explicit: %v", err)
	}
	if proxy.Cuts() == 0 {
		t.Fatal("the proxy did not cut the connection")
	}
	// The node task outlived the caller: the effect is applied.
	deadline := time.Now().Add(20 * time.Second)
	for {
		count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
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

// sshSignerForTest returns a host key that is not the node's.
func sshSignerForTest(t *testing.T) (ssh.PublicKey, error) {
	signer, err := ssh.NewSignerFromKey(mustGenerateKey(t))
	if err != nil {
		return nil, err
	}
	return signer.PublicKey(), nil
}
