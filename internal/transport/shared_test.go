//go:build unix

package transport

import (
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestReadLineLeavesTheRest(t *testing.T) {
	a, b := net.Pipe()
	go func() { _, _ = a.Write([]byte("{\"ok\":true}\nRAW")); _ = a.Close() }()
	line, err := readLine(b)
	if err != nil || string(line) != `{"ok":true}` {
		t.Fatalf("line = %q, %v", line, err)
	}
	rest, _ := io.ReadAll(b)
	if string(rest) != "RAW" {
		t.Fatalf("rest = %q, want RAW", rest)
	}
}

func TestTunnelFingerprint(t *testing.T) {
	priv, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	peerKey, err := GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := WireGuardConfig{
		PrivateKey:    priv,
		Address:       netip.MustParseAddr("10.42.255.1"),
		NodeAddress:   netip.MustParseAddr("10.42.0.1"),
		PeerPublicKey: peerKey.PublicKey(),
		Endpoint:      "203.0.113.5:51820",
		AllowedIPs:    []netip.Prefix{netip.MustParsePrefix("10.42.0.0/24")},
	}
	a := TunnelFingerprint(cfg)
	if a != TunnelFingerprint(cfg) {
		t.Fatal("fingerprint is not stable")
	}
	if strings.Contains(a, priv.Encode()) {
		t.Fatal("fingerprint leaks the private key")
	}
	cfg.Endpoint = "203.0.113.6:51820"
	if a == TunnelFingerprint(cfg) {
		t.Fatal("a new endpoint should change the fingerprint")
	}
}

func TestSharedDialBudget(t *testing.T) {
	if sharedDialBudget <= SharedDialTimeout {
		t.Fatalf("client budget %s must exceed the daemon's %s", sharedDialBudget, SharedDialTimeout)
	}
	var d Dialer = NewSharedDialer(SharedDialerConfig{Socket: "x"})
	if b, ok := d.(interface{ DialBudget() time.Duration }); !ok || b.DialBudget() != sharedDialBudget {
		t.Fatalf("SharedDialer budget = %v, %v", b, ok)
	}
	if _, ok := any(NetDialer{}).(interface{ DialBudget() time.Duration }); ok {
		t.Fatal("NetDialer must not declare a dial budget")
	}
}

func TestTunnelDialErrorText(t *testing.T) {
	err := &TunnelDialError{Addr: "10.42.0.1:22", Message: "i/o timeout"}
	if !strings.Contains(err.Error(), "10.42.0.1:22") || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("text = %q", err.Error())
	}
}
