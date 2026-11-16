package sshfixture

import (
	"context"
	"net"
	"time"
)

// netDialTimeout opens a TCP connection with a timeout.
func netDialTimeout(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
}
