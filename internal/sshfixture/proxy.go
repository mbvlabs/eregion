package sshfixture

import (
	"io"
	"net"
	"sync"
	"time"
)

// CutProxy forwards TCP traffic to a node and cuts every active connection on
// demand. A test cuts after the node has started the task, so the cut lands
// after the request was dispatched and before its outcome reached the caller:
// a lost response at the transport boundary.
//
// It deliberately has no timer: elapsed time cannot distinguish an SSH
// handshake from an idle request, and a slow handshake used to be cut as if it
// were a lost response.
type CutProxy struct {
	listener net.Listener
	target   string

	mu          sync.Mutex
	closed      bool
	cutCount    int
	connections map[*proxyConnection]struct{}
}

type proxyConnection struct {
	client net.Conn
	server net.Conn
}

// NewCutProxy starts a proxy in front of target.
func NewCutProxy(target string) (*CutProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := &CutProxy{
		listener:    listener,
		target:      target,
		connections: map[*proxyConnection]struct{}{},
	}
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

// Cut closes every connection that is open now and reports how many it cut.
func (p *CutProxy) Cut() int {
	p.mu.Lock()
	connections := make([]*proxyConnection, 0, len(p.connections))
	for connection := range p.connections {
		connections = append(connections, connection)
	}
	p.cutCount += len(connections)
	p.mu.Unlock()
	for _, connection := range connections {
		_ = connection.client.Close()
		_ = connection.server.Close()
	}
	return len(connections)
}

// Close stops the proxy and any connection it still forwards.
func (p *CutProxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	connections := make([]*proxyConnection, 0, len(p.connections))
	for connection := range p.connections {
		connections = append(connections, connection)
	}
	p.mu.Unlock()
	err := p.listener.Close()
	for _, connection := range connections {
		_ = connection.client.Close()
		_ = connection.server.Close()
	}
	return err
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
	connection := &proxyConnection{client: client, server: server}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	p.connections[connection] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.connections, connection)
		p.mu.Unlock()
	}()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(client, server)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = server.Close()
	<-done
}
