package sshfixture

import (
	"net"
	"sync"
	"time"
)

// UDPRelay forwards WireGuard traffic to a node and can black-hole it in both
// directions. A black hole is a real tunnel loss: the WireGuard session goes
// quiet, and nothing in TCP or SSH sees a reset.
type UDPRelay struct {
	listener *net.UDPConn
	target   *net.UDPAddr

	mu        sync.Mutex
	closed    bool
	blackhole bool
	dropped   int
	upstreams map[string]*net.UDPConn
}

// NewUDPRelay starts a relay in front of target, a UDP host:port.
func NewUDPRelay(target string) (*UDPRelay, error) {
	targetAddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	relay := &UDPRelay{listener: listener, target: targetAddr, upstreams: map[string]*net.UDPConn{}}
	go relay.forward()
	return relay, nil
}

// Address is the endpoint a WireGuard peer sends to.
func (r *UDPRelay) Address() string { return r.listener.LocalAddr().String() }

// SetBlackhole drops every packet in both directions while on.
func (r *UDPRelay) SetBlackhole(on bool) {
	r.mu.Lock()
	r.blackhole = on
	r.mu.Unlock()
}

// Dropped reports how many packets the black hole dropped.
func (r *UDPRelay) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Close stops the relay.
func (r *UDPRelay) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	upstreams := r.upstreams
	r.upstreams = map[string]*net.UDPConn{}
	r.mu.Unlock()
	for _, upstream := range upstreams {
		_ = upstream.Close()
	}
	return r.listener.Close()
}

// drop reports whether a packet is dropped, and counts it.
func (r *UDPRelay) drop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blackhole {
		r.dropped++
	}
	return r.blackhole
}

func (r *UDPRelay) forward() {
	buffer := make([]byte, 65535)
	for {
		count, client, err := r.listener.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if r.drop() {
			continue
		}
		upstream, err := r.upstream(client)
		if err != nil {
			continue
		}
		_, _ = upstream.Write(buffer[:count])
	}
}

// upstream returns the socket that carries one client's packets, so the node
// sees one stable source per client.
func (r *UDPRelay) upstream(client *net.UDPAddr) (*net.UDPConn, error) {
	key := client.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if upstream, ok := r.upstreams[key]; ok {
		return upstream, nil
	}
	if r.closed {
		return nil, net.ErrClosed
	}
	upstream, err := net.DialUDP("udp", nil, r.target)
	if err != nil {
		return nil, err
	}
	r.upstreams[key] = upstream
	go r.backward(upstream, client)
	return upstream, nil
}

func (r *UDPRelay) backward(upstream *net.UDPConn, client *net.UDPAddr) {
	buffer := make([]byte, 65535)
	for {
		_ = upstream.SetReadDeadline(time.Now().Add(10 * time.Minute))
		count, err := upstream.Read(buffer)
		if err != nil {
			return
		}
		if r.drop() {
			continue
		}
		_, _ = r.listener.WriteToUDP(buffer[:count], client)
	}
}
