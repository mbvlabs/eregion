package node

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion is the node wire protocol version. The control plane and
// the node binary exchange it in the handshake before any request is accepted.
// A change to the request, event, or response shape is a new version; both
// sides must be upgraded together. There is no negotiation and no downgrade.
const ProtocolVersion = 1

// Frame types exchanged over the node protocol.
const (
	// FrameHello is the handshake. It is exchanged before any request.
	FrameHello = "hello"
	// FrameRequest carries one node request.
	FrameRequest = "request"
	// FrameEvent carries one redacted node observation.
	FrameEvent = "event"
	// FrameResponse carries the final node result.
	FrameResponse = "response"
	// FrameError reports a protocol-level refusal.
	FrameError = "error"
)

// MaxFrameBytes bounds one protocol line. A node request is small; a larger
// line is a protocol violation, not a large payload.
const MaxFrameBytes = 1 << 20

// Frame is one line of the node protocol.
type Frame struct {
	Type     string    `json:"type"`
	Version  int       `json:"version,omitempty"`
	Node     string    `json:"node,omitempty"`
	Request  *Request  `json:"request,omitempty"`
	Event    *Event    `json:"event,omitempty"`
	Response *Response `json:"response,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// ErrStreamLost means the transport lost the stream before the node reported
// an outcome. It never means the node stopped: the node task may still be
// running, and the retry path must ask for status before it runs anything.
var ErrStreamLost = errors.New("node stream lost")

// ErrProtocol marks a malformed or unsupported node frame. It is not a lost
// stream: the node answered, but not with something this side understands.
var ErrProtocol = errors.New("invalid node protocol")

// IsStreamLost reports whether err is an explicit lost-stream outcome.
func IsStreamLost(err error) bool { return errors.Is(err, ErrStreamLost) }

// WriteFrame writes one protocol frame as a single line.
func WriteFrame(w io.Writer, frame Frame) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(raw) > MaxFrameBytes {
		return fmt.Errorf("node frame is too large: %d bytes", len(raw))
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one protocol frame. It returns io.EOF at the end of the
// stream and an explicit error for an oversized or malformed line.
func ReadFrame(r *bufio.Reader) (Frame, error) {
	line, err := readLine(r)
	if err != nil {
		return Frame{}, err
	}
	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		return Frame{}, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if frame.Type == "" {
		return Frame{}, fmt.Errorf("%w: the frame has no type", ErrProtocol)
	}
	return frame, nil
}

// readLine reads one bounded line without the trailing newline.
func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if err == io.EOF && len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > MaxFrameBytes {
			return nil, fmt.Errorf("node frame exceeds %d bytes", MaxFrameBytes)
		}
		if !isPrefix {
			return out, nil
		}
	}
}
