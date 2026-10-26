package transport

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/node"
)

// StderrTailBytes bounds the remote command's standard error. Only the last
// bytes are kept, so a chatty node cannot exhaust the control plane.
const StderrTailBytes = 8 << 10

// HostKeyMismatchError reports a node whose host key is not the pinned one.
// It is permanent: retrying cannot make a wrong key right.
type HostKeyMismatchError struct {
	Address string
	Got     string
	Want    []string
	Unknown bool
}

func (e *HostKeyMismatchError) Error() string {
	if e.Unknown {
		return fmt.Sprintf("no host key is pinned for %s", e.Address)
	}
	return fmt.Sprintf("the host key of %s (%s) does not match the pinned key", e.Address, e.Got)
}

// Unwrap exposes the underlying pin failure.
func (e *HostKeyMismatchError) Unwrap() error { return ErrPermanentTransport }

// HostKeyRevokedError reports a node whose host key the known_hosts file
// revokes. A revoked key is refused even when another line pins it, and the
// refusal is permanent: a revoked key is never accepted.
type HostKeyRevokedError struct {
	Address string
	Got     string
}

func (e *HostKeyRevokedError) Error() string {
	return fmt.Sprintf("the host key of %s (%s) is revoked by the known_hosts file", e.Address, e.Got)
}

// Unwrap exposes the permanent classification.
func (e *HostKeyRevokedError) Unwrap() error { return ErrPermanentTransport }

// VersionMismatchError reports a node that does not speak the node protocol
// version this control plane implements. It is permanent.
type VersionMismatchError struct {
	Address string
	Got     int
	Want    int
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("the node at %s speaks node protocol version %d, this control plane speaks %d", e.Address, e.Got, e.Want)
}

// Unwrap exposes the permanent classification.
func (e *VersionMismatchError) Unwrap() error { return ErrPermanentTransport }

// HandshakeError reports a node that refused the protocol handshake.
type HandshakeError struct {
	Address string
	Reason  string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("the node at %s refused the handshake: %s", e.Address, e.Reason)
}

// Unwrap exposes the permanent classification.
func (e *HandshakeError) Unwrap() error { return ErrPermanentTransport }

// ErrPermanentTransport marks a transport failure that a retry cannot fix: a
// host-key mismatch, a protocol version mismatch, or a refused handshake. The
// control plane reports it as a precondition failure instead of retrying it
// forever. It is the node package's sentinel, so the control plane does not
// have to know which transport produced it.
var ErrPermanentTransport = node.ErrPermanentTransport

// IsPermanentTransport reports whether a transport error is permanent.
func IsPermanentTransport(err error) bool { return node.IsPermanentTransport(err) }

// tailBuffer keeps the last bytes written to it. The SSH session copies
// standard error from its own goroutine while a failed call reads the tail, so
// access is serialized.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

// Write keeps the tail of the stream. It never fails: a full buffer drops the
// oldest bytes instead of the newest.
func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.limit {
		t.data = t.data[len(t.data)-t.limit:]
	}
	return len(p), nil
}

// String returns the bounded tail.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

// detail renders the tail for an error message. It is empty when the node
// wrote nothing to standard error.
func (t *tailBuffer) detail(redact func(string) string) string {
	text := strings.TrimSpace(t.String())
	if text == "" {
		return ""
	}
	return ": " + redact(text)
}

// wait returns how long to wait before the next status poll.
func wait(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 200 * time.Millisecond
	}
	return interval
}
