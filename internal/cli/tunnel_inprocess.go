package cli

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
)

// inProcessWireGuardDial is the fallback when the shared daemon is not used:
// one userspace tunnel in this process, shared by routes with the same key and
// node. EREGION_TUNNEL=inprocess selects it on unix; other platforms always
// use it.
var (
	inProcessMu      sync.Mutex
	inProcessDialers = map[string]*transport.WireGuardDialer{}
)

func inProcessWireGuardDial(wg *host.ContextWireGuard) (func(context.Context, string, string) (net.Conn, error), error) {
	key, err := transport.ReadWireGuardKey(wg.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	id := key.PublicKey().String() + "|" + wg.NodePublicKey + "|" + wg.Endpoint + "|" + wg.Address + "|" + wg.NodeAddress
	inProcessMu.Lock()
	defer inProcessMu.Unlock()
	if dialer, ok := inProcessDialers[id]; ok {
		return dialer.DialContext, nil
	}
	peer, err := transport.ParseWireGuardPublicKey(wg.NodePublicKey)
	if err != nil {
		return nil, err
	}
	self, err := netip.ParseAddr(wg.Address)
	if err != nil {
		return nil, err
	}
	nodeAddr, err := netip.ParseAddr(wg.NodeAddress)
	if err != nil {
		return nil, err
	}
	dialer, err := transport.NewWireGuardDialer(transport.WireGuardConfig{PrivateKey: key, Address: self, NodeAddress: nodeAddr, PeerPublicKey: peer, Endpoint: wg.Endpoint})
	if err != nil {
		return nil, err
	}
	// A different tunnel for the same key (a rebuilt node) replaces the old one.
	for other, d := range inProcessDialers {
		if strings.HasPrefix(other, key.PublicKey().String()+"|") {
			_ = d.Close()
			delete(inProcessDialers, other)
		}
	}
	inProcessDialers[id] = dialer
	return dialer.DialContext, nil
}

func closeInProcessWireGuardRoute(route *host.ContextSSH) {
	if route == nil || route.WireGuard == nil {
		return
	}
	key, err := transport.ReadWireGuardKey(route.WireGuard.PrivateKeyFile)
	if err != nil {
		return
	}
	prefix := key.PublicKey().String() + "|"
	inProcessMu.Lock()
	defer inProcessMu.Unlock()
	for id, dialer := range inProcessDialers {
		if strings.HasPrefix(id, prefix) {
			_ = dialer.Close()
			delete(inProcessDialers, id)
		}
	}
}
