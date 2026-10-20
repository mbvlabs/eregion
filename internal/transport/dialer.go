// Package transport moves node requests to a node. The control plane depends
// only on the node transport interface, so a transport can be local, pinned
// SSH, or something else without changing the worker.
package transport

import (
	"context"
	"net"
	"time"
)

// Dialer opens the byte stream the SSH client runs over. A userspace
// WireGuard dialer implements this interface and becomes the only thing the
// next increment has to add: the SSH transport never dials a socket itself.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// NetDialer is the default dialer. It opens a plain TCP connection.
type NetDialer struct {
	// Timeout bounds the dial. Zero means no explicit bound.
	Timeout time.Duration
}

// DialContext opens a TCP connection to address.
func (d NetDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: d.Timeout}
	return dialer.DialContext(ctx, network, address)
}
