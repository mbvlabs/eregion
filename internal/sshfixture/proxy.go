package sshfixture

import (
	"io"
	"net"
	"sync"
	"time"
)

// CutProxy forwards TCP traffic to a node and cuts a connection that stayed
// open while the client waited. The client is idle exactly while it waits for
// a response, so the cut lands after the request was dispatched and before the
// outcome reached the caller: a lost response at the transport boundary.
//
// A connection is only cut when it is older than MinAge, so the SSH handshake
// is never cut halfway.
type CutProxy struct {
	listener net.Listener
	target   string
	idle     time.Duration
	minAge   time.Duration

	mu       sync.Mutex
	closed   bool
	cutCount int
}

// NewCutProxy starts a proxy in front of target.
func NewCutProxy(target string, idle, minAge time.Duration) (*CutProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := &CutProxy{listener: listener, target: target, idle: idle, minAge: minAge}
	go proxy.accept()
	return proxy, nil
}

// Address is the address a client connects to.
func (p *CutProxy) Address() string { return p.listener.Addr().String() }

// Cuts reports how many connections the proxy cut.
func (p *CutProxy) Cuts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cutCount
}

// Close stops the proxy.
func (p *CutProxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	return p.listener.Close()
}

func (p *CutProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(client)
	}
}

func (p *CutProxy) forward(client net.Conn) {
	server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	started := time.Now()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	buffer := make([]byte, 32*1024)
	for {
		_ = client.SetReadDeadline(time.Now().Add(p.idle))
		count, err := client.Read(buffer)
		if count > 0 {
			if _, writeErr := server.Write(buffer[:count]); writeErr != nil {
				break
			}
		}
		if err != nil {
			var netErr net.Error
			if ok := asNetError(err, &netErr); ok && netErr.Timeout() {
				if time.Since(started) > p.minAge {
					p.mu.Lock()
					p.cutCount++
					p.mu.Unlock()
					break
				}
				// The connection is still young: keep it open and wait for the
				// next client write.
				continue
			}
			break
		}
	}
	_ = client.Close()
	_ = server.Close()
}

// asNetError reports whether err is a network error.
func asNetError(err error, target *net.Error) bool {
	netErr, ok := err.(net.Error)
	if ok {
		*target = netErr
	}
	return ok
}
