package transport

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/sshfixture"
)

// startWireGuardFixture starts the disposable SSH node behind kernel
// WireGuard. The test skips when Docker, NET_ADMIN, or the wireguard module
// is not available.
func startWireGuardFixture(t *testing.T) *sshfixture.Fixture {
	t.Helper()
	return startFixtureWith(t, sshfixture.Options{WireGuard: true})
}

// fixtureWireGuardDialer builds a dialer to the fixture node through endpoint,
// which is the node's WireGuard port or a relay in front of it.
func fixtureWireGuardDialer(t *testing.T, fixture *sshfixture.Fixture, endpoint string) *WireGuardDialer {
	t.Helper()
	key, err := ReadWireGuardKey(fixture.WireGuard.PrivateKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ParseWireGuardPublicKey(fixture.WireGuard.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := NewWireGuardDialer(WireGuardConfig{
		PrivateKey:       key,
		Address:          netip.MustParseAddr(fixture.WireGuard.ControlAddress),
		NodeAddress:      netip.MustParseAddr(fixture.WireGuard.NodeAddress),
		PeerPublicKey:    peer,
		Endpoint:         endpoint,
		AllowedIPs:       []netip.Prefix{netip.MustParsePrefix(fixture.WireGuard.Subnet)},
		Keepalive:        time.Second,
		HandshakeTimeout: 3 * time.Second,
		ProbeInterval:    200 * time.Millisecond,
		LossAfter:        1500 * time.Millisecond,
		LockDir:          t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close() })
	return dialer
}

// fixtureWireGuardTransport builds the pinned SSH transport over a dialer.
func fixtureWireGuardTransport(t *testing.T, fixture *sshfixture.Fixture, dialer Dialer) *SSHTransport {
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
		PollInterval: 100 * time.Millisecond,
		Dialer:       dialer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fixtureEffects(t *testing.T, fixture *sshfixture.Fixture, exec engine.Execution) int {
	t.Helper()
	count, err := fixture.EffectCount(context.Background(), string(exec.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func TestWireGuardFixtureRunsAndReportsStatus(t *testing.T) {
	fixture := startWireGuardFixture(t)
	dialer := fixtureWireGuardDialer(t, fixture, fixture.WireGuard.Endpoint)
	client := fixtureWireGuardTransport(t, fixture, dialer)
	exec := testExecution(t, node.DiagnosticStepID(1))
	response, err := client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted {
		t.Fatalf("run response: %+v", response)
	}
	first := dialer.current
	status, err := client.Call(context.Background(), statusRequest(exec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != engine.StatusCompleted || status.InputsHash == "" {
		t.Fatalf("status response: %+v", status)
	}
	if dialer.current == nil || dialer.current != first {
		t.Fatal("the second call did not reuse the tunnel")
	}
	if got := fixtureEffects(t, fixture, exec); got != 1 {
		t.Fatalf("the node wrote %d effects", got)
	}
}

func TestWireGuardFixtureHandshakeFailureDispatchesNoWork(t *testing.T) {
	fixture := startWireGuardFixture(t)
	stranger, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	// The node no longer knows the control plane's key and stays silent.
	if err := fixture.SetWireGuardPeer(context.Background(), stranger.PublicKey().String()); err != nil {
		t.Fatal(err)
	}
	client := fixtureWireGuardTransport(t, fixture, fixtureWireGuardDialer(t, fixture, fixture.WireGuard.Endpoint))
	exec := testExecution(t, node.DiagnosticStepID(1))
	started := time.Now()
	_, err = client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if !IsTunnelDown(err) || node.IsStreamLost(err) {
		t.Fatalf("a handshake failure is not a tunnel-down error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the handshake failure took %s", elapsed)
	}
	if got := fixtureEffects(t, fixture, exec); got != 0 {
		t.Fatalf("a handshake failure dispatched %d effects", got)
	}
	journal, err := fixture.Read(context.Background(), "/var/lib/eregion/node/journal.json")
	if err != nil {
		t.Fatal(err)
	}
	if journal != "" {
		t.Fatalf("a handshake failure reached the node journal: %s", journal)
	}
}

func TestWireGuardFixtureTunnelGoesDownAndReconnects(t *testing.T) {
	fixture := startWireGuardFixture(t)
	relay, err := sshfixture.NewUDPRelay(fixture.WireGuard.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	dialer := fixtureWireGuardDialer(t, fixture, relay.Address())
	client := fixtureWireGuardTransport(t, fixture, dialer)
	first := testExecution(t, node.DiagnosticStepID(1))
	if response, err := client.Call(context.Background(), runRequest(t, first, 0), nil); err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("run over the tunnel: %+v %v", response, err)
	}
	relay.SetBlackhole(true)
	awaitCondition(t, 20*time.Second, "the tunnel loss", func() bool { return !dialer.Up() })
	second := testExecution(t, node.DiagnosticStepID(1))
	if _, err := client.Call(context.Background(), runRequest(t, second, 0), nil); !IsTunnelDown(err) {
		t.Fatalf("a call through a down tunnel is not a tunnel-down error: %v", err)
	}
	if got := fixtureEffects(t, fixture, second); got != 0 {
		t.Fatalf("a down tunnel dispatched %d effects", got)
	}
	relay.SetBlackhole(false)
	response, err := client.Call(context.Background(), runRequest(t, second, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted {
		t.Fatalf("run after the reconnect: %+v", response)
	}
	if got := fixtureEffects(t, fixture, second); got != 1 {
		t.Fatalf("the reconnect wrote %d effects", got)
	}
}

func TestWireGuardFixtureLostTunnelAfterDispatchRecoversThroughStatus(t *testing.T) {
	fixture := startWireGuardFixture(t)
	relay, err := sshfixture.NewUDPRelay(fixture.WireGuard.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	dialer := fixtureWireGuardDialer(t, fixture, relay.Address())
	client := fixtureWireGuardTransport(t, fixture, dialer)
	exec := testExecution(t, node.DiagnosticStepID(1))
	results := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), runRequest(t, exec, 1000), nil)
		results <- err
	}()
	ctx := context.Background()
	awaitCondition(t, 30*time.Second, "the node task", func() bool {
		pid, err := fixture.TaskOwnerPID(ctx, string(exec.OperationID))
		return err == nil && pid != 0
	})
	relay.SetBlackhole(true)
	var lost error
	select {
	case lost = <-results:
	case <-time.After(30 * time.Second):
		t.Fatal("the lost tunnel was not reported")
	}
	if !node.IsStreamLost(lost) || IsTunnelDown(lost) {
		t.Fatalf("a tunnel lost after dispatch is not a lost stream: %v", lost)
	}
	// The node task outlived the tunnel.
	awaitCondition(t, 20*time.Second, "the detached effect", func() bool { return fixtureEffects(t, fixture, exec) == 1 })
	relay.SetBlackhole(false)
	status, err := client.Call(ctx, statusRequest(exec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != engine.StatusCompleted {
		t.Fatalf("status after the reconnect: %+v", status)
	}
	again, err := client.Call(ctx, runRequest(t, exec, 1000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != engine.StatusCompleted {
		t.Fatalf("the retried run: %+v", again)
	}
	if got := fixtureEffects(t, fixture, exec); got != 1 {
		t.Fatalf("the reconnect duplicated the effect: %d effects", got)
	}
}

func TestWireGuardFixtureDuplicateRequestsProduceOneEffect(t *testing.T) {
	fixture := startWireGuardFixture(t)
	client := fixtureWireGuardTransport(t, fixture, fixtureWireGuardDialer(t, fixture, fixture.WireGuard.Endpoint))
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
	if got := fixtureEffects(t, fixture, exec); got != 1 {
		t.Fatalf("duplicate requests produced %d effects", got)
	}
}

func TestWireGuardFixtureNodeTaskOutlivesItsCaller(t *testing.T) {
	fixture := startWireGuardFixture(t)
	client := fixtureWireGuardTransport(t, fixture, fixtureWireGuardDialer(t, fixture, fixture.WireGuard.Endpoint))
	exec := testExecution(t, node.DiagnosticStepID(1))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	_, err := client.Call(ctx, runRequest(t, exec, 800), nil)
	if !node.IsStreamLost(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled call is not an explicit unknown outcome: %v", err)
	}
	awaitCondition(t, 20*time.Second, "the detached effect", func() bool { return fixtureEffects(t, fixture, exec) == 1 })
}
