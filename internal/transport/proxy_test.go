package transport

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// cutProxy forwards TCP traffic to a node and cuts the connection when the
// client has been idle. The client is idle exactly while it waits for a
// response, so the cut lands after the request was dispatched and before the
// outcome reached the caller: a lost response at the transport boundary.
type cutProxy struct {
	listener net.Listener
	target   string
	idle     time.Duration

	mu       sync.Mutex
	closed   bool
	cutCount int
}

func newCutProxy(t *testing.T, target string, idle time.Duration) *cutProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &cutProxy{listener: listener, target: target, idle: idle}
	go proxy.accept()
	t.Cleanup(proxy.Close)
	return proxy
}

// Address is the address the client connects to.
func (p *cutProxy) Address() string { return p.listener.Addr().String() }

// Cuts reports how many connections the proxy cut.
func (p *cutProxy) Cuts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cutCount
}

// Close stops the proxy.
func (p *cutProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	_ = p.listener.Close()
}

func (p *cutProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(client)
	}
}

func (p *cutProxy) forward(client net.Conn) {
	server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
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
				p.mu.Lock()
				p.cutCount++
				p.mu.Unlock()
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
