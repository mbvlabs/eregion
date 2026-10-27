package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/sshfixture"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

var (
	testControlAddress = netip.MustParseAddr("10.77.0.1")
	testNodeAddress    = netip.MustParseAddr("10.77.0.2")
	testNodeSSH        = "10.77.0.2:22"
)

// wgTestNode is an in-process WireGuard node: a second userspace device whose
// netstack serves the in-process SSH node on its tunnel address. It needs no
// Docker and no privilege, so the tunnel behaviour is tested everywhere.
type wgTestNode struct {
	server    *testServer
	endpoint  string
	publicKey WireGuardPublicKey
	// controlKey is the control plane's private key that the node accepts.
	controlKey WireGuardKey
}

func newWGTestNode(t *testing.T) *wgTestNode {
	t.Helper()
	server := newTestServer(t)
	nodeKey, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	controlKey, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	tunDevice, tunNet, err := netstack.CreateNetTUN([]netip.Addr{testNodeAddress}, nil, TunnelMTU)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(tunDevice, conn.NewDefaultBind(), &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf})
	public := controlKey.PublicKey()
	config := fmt.Sprintf("private_key=%s\nlisten_port=0\npublic_key=%s\nallowed_ip=%s/32\n", nodeKey.uapiHex(), hex.EncodeToString(public[:]), testControlAddress)
	if err := dev.IpcSet(config); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dev.Close)
	port := 0
	state, err := dev.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(state, "\n") {
		if value, ok := strings.CutPrefix(line, "listen_port="); ok {
			port, _ = strconv.Atoi(value)
		}
	}
	if port == 0 {
		t.Fatal("the in-process WireGuard node has no listen port")
	}
	listener, err := tunNet.ListenTCPAddrPort(netip.MustParseAddrPort(testNodeSSH))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.handle(conn)
		}
	}()
	return &wgTestNode{server: server, endpoint: "127.0.0.1:" + strconv.Itoa(port), publicKey: nodeKey.PublicKey(), controlKey: controlKey}
}

// dialer builds a WireGuard dialer to the node with short test timings.
func (n *wgTestNode) dialer(t *testing.T, endpoint string, adjust func(*WireGuardConfig)) *WireGuardDialer {
	t.Helper()
	config := WireGuardConfig{
		PrivateKey:       n.controlKey,
		Address:          testControlAddress,
		NodeAddress:      testNodeAddress,
		PeerPublicKey:    n.publicKey,
		Endpoint:         endpoint,
		Keepalive:        time.Second,
		HandshakeTimeout: 2 * time.Second,
		ProbeInterval:    100 * time.Millisecond,
		LossAfter:        600 * time.Millisecond,
	}
	if adjust != nil {
		adjust(&config)
	}
	dialer, err := NewWireGuardDialer(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close() })
	return dialer
}

// transport builds the pinned SSH transport over a dialer.
func (n *wgTestNode) transport(t *testing.T, dialer Dialer) *SSHTransport {
	t.Helper()
	keys := NewHostKeys()
	keys.Pin(testNodeSSH, n.server.hostKey)
	client, err := NewSSHTransport(SSHConfig{
		Address:      testNodeSSH,
		User:         "eregion",
		IdentityFile: n.server.identity,
		HostKeys:     keys,
		Command:      "eregion node-agent serve",
		ExpectedNode: n.server.config.Node,
		Timeout:      10 * time.Second,
		PollInterval: 50 * time.Millisecond,
		Dialer:       dialer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func awaitCondition(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWireGuardTransportRunsOverOneSharedTunnel(t *testing.T) {
	wg := newWGTestNode(t)
	dialer := wg.dialer(t, wg.endpoint, nil)
	client := wg.transport(t, dialer)
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
	if status.Status != engine.StatusCompleted {
		t.Fatalf("status response: %+v", status)
	}
	if dialer.current == nil || dialer.current != first {
		t.Fatal("the second call did not reuse the tunnel")
	}
	if got := countEffects(t, wg.server.config.StateDir, exec); got != 1 {
		t.Fatalf("the node wrote %d effects", got)
	}
}

func TestWireGuardHandshakeFailureDispatchesNoWork(t *testing.T) {
	wg := newWGTestNode(t)
	stranger, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	// The node does not know this key: it stays silent, exactly as a node
	// that is down.
	dialer := wg.dialer(t, wg.endpoint, func(config *WireGuardConfig) { config.PrivateKey = stranger })
	client := wg.transport(t, dialer)
	exec := testExecution(t, node.DiagnosticStepID(1))
	started := time.Now()
	_, err = client.Call(context.Background(), runRequest(t, exec, 0), nil)
	if err == nil {
		t.Fatal("a tunnel without a handshake was reported as success")
	}
	if !IsTunnelDown(err) {
		t.Fatalf("a handshake failure is not a tunnel-down error: %v", err)
	}
	if node.IsStreamLost(err) {
		t.Fatalf("a handshake failure is reported as a lost stream: %v", err)
	}
	if IsPermanentTransport(err) {
		t.Fatalf("a handshake failure is permanent: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the handshake failure took %s, beyond its bound", elapsed)
	}
	if !strings.Contains(err.Error(), wg.publicKey.String()) || !strings.Contains(err.Error(), "no request was sent") {
		t.Fatalf("the handshake failure does not say what to check: %v", err)
	}
	if got := countEffects(t, wg.server.config.StateDir, exec); got != 0 {
		t.Fatalf("a handshake failure dispatched %d effects", got)
	}
	if _, err := os.Stat(filepath.Join(wg.server.config.StateDir, "journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a handshake failure reached the node journal: %v", err)
	}
}

func TestWireGuardTunnelGoesDownAndReconnects(t *testing.T) {
	wg := newWGTestNode(t)
	relay, err := sshfixture.NewUDPRelay(wg.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	dialer := wg.dialer(t, relay.Address(), nil)
	client := wg.transport(t, dialer)
	first := testExecution(t, node.DiagnosticStepID(1))
	if response, err := client.Call(context.Background(), runRequest(t, first, 0), nil); err != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("run over the tunnel: %+v %v", response, err)
	}
	firstTunnel := dialer.current

	relay.SetBlackhole(true)
	awaitCondition(t, 10*time.Second, "the tunnel loss", func() bool { return !dialer.Up() })
	second := testExecution(t, node.DiagnosticStepID(1))
	_, err = client.Call(context.Background(), runRequest(t, second, 0), nil)
	if !IsTunnelDown(err) {
		t.Fatalf("a call through a down tunnel is not a tunnel-down error: %v", err)
	}
	if got := countEffects(t, wg.server.config.StateDir, second); got != 0 {
		t.Fatalf("a down tunnel dispatched %d effects", got)
	}
	if relay.Dropped() == 0 {
		t.Fatal("the black hole dropped nothing")
	}

	relay.SetBlackhole(false)
	response, err := client.Call(context.Background(), runRequest(t, second, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted {
		t.Fatalf("run after the reconnect: %+v", response)
	}
	if dialer.current == nil || dialer.current == firstTunnel {
		t.Fatal("the reconnect reused the lost tunnel")
	}
	if got := countEffects(t, wg.server.config.StateDir, second); got != 1 {
		t.Fatalf("the reconnect wrote %d effects", got)
	}
	wg.server.waitTaskExit(t, first)
	wg.server.waitTaskExit(t, second)
}

func TestWireGuardLostTunnelAfterDispatchRecoversThroughStatus(t *testing.T) {
	wg := newWGTestNode(t)
	relay, err := sshfixture.NewUDPRelay(wg.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	dialer := wg.dialer(t, relay.Address(), nil)
	client := wg.transport(t, dialer)
	exec := testExecution(t, node.DiagnosticStepID(1))
	type result struct {
		err error
	}
	results := make(chan result, 1)
	go func() {
		_, err := client.Call(context.Background(), runRequest(t, exec, 1000), nil)
		results <- result{err: err}
	}()
	// The node task is dispatched and holds the effect: now the tunnel dies.
	awaitCondition(t, 10*time.Second, "the node task", func() bool { return journalHasRunningTask(wg.server.config.StateDir) })
	relay.SetBlackhole(true)
	var got result
	select {
	case got = <-results:
	case <-time.After(20 * time.Second):
		t.Fatal("the lost tunnel was not reported")
	}
	if !node.IsStreamLost(got.err) {
		t.Fatalf("a tunnel lost after dispatch is not a lost stream: %v", got.err)
	}
	if IsTunnelDown(got.err) {
		t.Fatalf("a tunnel lost after dispatch is reported as a handshake failure: %v", got.err)
	}
	// The node task outlived the tunnel.
	awaitCondition(t, 10*time.Second, "the detached effect", func() bool { return countEffects(t, wg.server.config.StateDir, exec) == 1 })

	relay.SetBlackhole(false)
	// The retry path asks for status first, then retries the run: neither
	// applies the effect again.
	status, err := client.Call(context.Background(), statusRequest(exec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != engine.StatusCompleted {
		t.Fatalf("status after the reconnect: %+v", status)
	}
	again, err := client.Call(context.Background(), runRequest(t, exec, 1000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != engine.StatusCompleted {
		t.Fatalf("the retried run: %+v", again)
	}
	if got := countEffects(t, wg.server.config.StateDir, exec); got != 1 {
		t.Fatalf("the reconnect duplicated the effect: %d effects", got)
	}
}

func TestWireGuardStillPinsTheSSHHostKey(t *testing.T) {
	wg := newWGTestNode(t)
	dialer := wg.dialer(t, wg.endpoint, nil)
	keys := NewHostKeys()
	other, err := sshSignerForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	keys.Pin(testNodeSSH, other)
	client, err := NewSSHTransport(SSHConfig{
		Address: testNodeSSH, User: "eregion", IdentityFile: wg.server.identity,
		HostKeys: keys, Command: "eregion node-agent serve", Timeout: 10 * time.Second, Dialer: dialer,
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := testExecution(t, node.DiagnosticStepID(1))
	if _, err := client.Call(context.Background(), runRequest(t, exec, 0), nil); !IsPermanentTransport(err) {
		t.Fatalf("a host key mismatch inside the tunnel is not permanent: %v", err)
	}
	if got := countEffects(t, wg.server.config.StateDir, exec); got != 0 {
		t.Fatalf("a host key mismatch dispatched %d effects", got)
	}
}

func TestWireGuardCloseEndsConnectionsAndDials(t *testing.T) {
	wg := newWGTestNode(t)
	dialer := wg.dialer(t, wg.endpoint, nil)
	raw, err := dialer.DialContext(context.Background(), "tcp", testNodeSSH)
	if err != nil {
		t.Fatal(err)
	}
	if err := dialer.Close(); err != nil {
		t.Fatal(err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := raw.Read(make([]byte, 1)); !errors.Is(err, ErrDialerClosed) {
		t.Fatalf("an open connection survived the shutdown: %v", err)
	}
	if _, err := dialer.DialContext(context.Background(), "tcp", testNodeSSH); !errors.Is(err, ErrDialerClosed) {
		t.Fatalf("a closed dialer dialed: %v", err)
	}
}

func TestWireGuardRefusesAnAddressOutsideTheAllowedIPs(t *testing.T) {
	wg := newWGTestNode(t)
	dialer := wg.dialer(t, wg.endpoint, nil)
	started := time.Now()
	if _, err := dialer.DialContext(context.Background(), "tcp", "10.77.0.3:22"); err == nil || !strings.Contains(err.Error(), "outside the WireGuard allowed IPs") {
		t.Fatalf("an address outside the allowed IPs was dialed: %v", err)
	}
	if time.Since(started) > time.Second || dialer.Up() {
		t.Fatal("refusing an address outside the allowed IPs touched the tunnel")
	}
}

func TestNewWireGuardDialerValidatesTheConfiguration(t *testing.T) {
	key, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	valid := func() WireGuardConfig {
		return WireGuardConfig{PrivateKey: key, Address: testControlAddress, NodeAddress: testNodeAddress, PeerPublicKey: peer.PublicKey(), Endpoint: "node.example:51820"}
	}
	if _, err := NewWireGuardDialer(valid()); err != nil {
		t.Fatalf("a valid configuration was refused: %v", err)
	}
	cases := map[string]func(*WireGuardConfig){
		"no private key":        func(c *WireGuardConfig) { c.PrivateKey = WireGuardKey{} },
		"no peer key":           func(c *WireGuardConfig) { c.PeerPublicKey = WireGuardPublicKey{} },
		"own key as peer":       func(c *WireGuardConfig) { c.PeerPublicKey = key.PublicKey() },
		"no address":            func(c *WireGuardConfig) { c.Address = netip.Addr{} },
		"shared address":        func(c *WireGuardConfig) { c.Address = testNodeAddress },
		"endpoint without port": func(c *WireGuardConfig) { c.Endpoint = "node.example" },
		"endpoint bad port":     func(c *WireGuardConfig) { c.Endpoint = "node.example:70000" },
		"default route":         func(c *WireGuardConfig) { c.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")} },
		"broad IPv4 prefix":     func(c *WireGuardConfig) { c.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} },
		"broad IPv6 prefix":     func(c *WireGuardConfig) { c.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("fd00::/8")} },
		"node outside":          func(c *WireGuardConfig) { c.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("10.78.0.0/24")} },
		"keepalive too short":   func(c *WireGuardConfig) { c.Keepalive = 100 * time.Millisecond },
		"loss before probes":    func(c *WireGuardConfig) { c.ProbeInterval, c.LossAfter = time.Second, time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := valid()
			mutate(&config)
			if _, err := NewWireGuardDialer(config); err == nil {
				t.Fatal("the configuration was accepted")
			}
		})
	}
	config := valid()
	config.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24")}
	if _, err := NewWireGuardDialer(config); err != nil {
		t.Fatalf("the node subnet was refused: %v", err)
	}
}

func TestReadWireGuardKeyEnforcesTheFileMode(t *testing.T) {
	key, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "wg.key")
	encoded := base64.StdEncoding.EncodeToString(key.raw[:])
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := ReadWireGuardKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.PublicKey() != key.PublicKey() {
		t.Fatal("the key file was read wrongly")
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWireGuardKey(path); err == nil {
		t.Fatal("a group-readable key file was accepted")
	}
	if _, err := ReadWireGuardKey(directory); err == nil {
		t.Fatal("a directory was accepted as a key file")
	}
	garbage := filepath.Join(directory, "garbage.key")
	if err := os.WriteFile(garbage, []byte("not-a-key-but-secret-text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWireGuardKey(garbage); err == nil || strings.Contains(err.Error(), "secret-text") {
		t.Fatalf("a malformed key file was accepted or echoed: %v", err)
	}
}

func TestWireGuardKeyMaterialIsRedacted(t *testing.T) {
	key, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(key.raw[:])
	hexed := hex.EncodeToString(key.raw[:])
	config := WireGuardConfig{PrivateKey: key, Address: testControlAddress, NodeAddress: testNodeAddress, Endpoint: "node.example:51820"}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("tunnel", "key", key, "config", config)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("tunnel", "key", key)
	rendered := []string{
		fmt.Sprintf("%v", key), fmt.Sprintf("%+v", key), fmt.Sprintf("%#v", key), fmt.Sprintf("%s", key),
		fmt.Sprintf("%v", config), fmt.Sprintf("%+v", config), string(raw), logged.String(),
	}
	for _, text := range rendered {
		if strings.Contains(text, encoded) || strings.Contains(text, hexed) {
			t.Fatalf("the private key leaked: %s", text)
		}
	}
	registry := &secrets.Registry{}
	key.Register(registry)
	for _, form := range []string{encoded, hexed} {
		if got := registry.Redact("key " + form); strings.Contains(got, form) {
			t.Fatalf("the registry does not redact the key: %s", got)
		}
	}
}

// TestWireGuardDialerOnlyDialsTCP guards the seam: the dialer carries the SSH
// stream and nothing else.
func TestWireGuardDialerOnlyDialsTCP(t *testing.T) {
	wg := newWGTestNode(t)
	dialer := wg.dialer(t, wg.endpoint, nil)
	if _, err := dialer.DialContext(context.Background(), "udp", testNodeSSH); err == nil {
		t.Fatal("the WireGuard dialer dialed UDP")
	}
}

// TestWireGuardKeyLockRefusesASecondUser proves that two dialers on one host
// cannot share a key: the node knows one endpoint per peer, so they would
// steal it from each other.
func TestWireGuardKeyLockRefusesASecondUser(t *testing.T) {
	wg := newWGTestNode(t)
	directory := t.TempDir()
	first := wg.dialer(t, wg.endpoint, func(config *WireGuardConfig) { config.LockDir = directory })
	config := WireGuardConfig{
		PrivateKey: wg.controlKey, Address: testControlAddress, NodeAddress: testNodeAddress,
		PeerPublicKey: wg.publicKey, Endpoint: wg.endpoint, LockDir: directory,
	}
	if _, err := NewWireGuardDialer(config); !errors.Is(err, ErrKeyInUse) {
		t.Fatalf("a second dialer with the same key was accepted: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewWireGuardDialer(config)
	if err != nil {
		t.Fatalf("the key lock was not released by Close: %v", err)
	}
	_ = second.Close()
}
