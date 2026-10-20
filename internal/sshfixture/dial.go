package sshfixture

import (
	"net"
	"time"
)

// netDialTimeout opens a TCP connection with a timeout.
func netDialTimeout(address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", address, timeout)
}
