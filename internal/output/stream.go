package output

import (
	"fmt"
	"io"
	"sync"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Stream emits NDJSON for a future event-following adapter. Reads/submissions use
// one ordinary envelope, not this stream. A stream result terminates the viewer
// request; the operation itself may still be running when a viewer detaches.
type Stream struct {
	mu      sync.Mutex
	out     io.Writer
	secrets *secrets.Registry
	closed  bool
	id      contract.OperationID
	last    uint64
}

type Record struct {
	Schema string             `json:"schema"`
	Type   string             `json:"type"`
	Event  *contract.Event    `json:"event,omitempty"`
	Result *contract.Envelope `json:"result,omitempty"`
}

func NewStream(out io.Writer, registry *secrets.Registry) *Stream {
	return &Stream{out: out, secrets: registry}
}

func (s *Stream) Event(event contract.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("event stream is closed")
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if s.id != "" && (s.id != event.OperationID || event.Sequence <= s.last) {
		return fmt.Errorf("event stream requires one operation and increasing sequences")
	}
	event.Message = s.secrets.Redact(event.Message)
	event.Stage = s.secrets.Redact(event.Stage)
	if event.Progress != nil {
		progress := *event.Progress
		progress.Unit = s.secrets.Redact(progress.Unit)
		event.Progress = &progress
	}
	if err := writeJSON(s.out, Record{Schema: contract.Schema, Type: "event", Event: &event}); err != nil {
		s.closed = true // A failed write may have emitted a partial record. Never append.
		return err
	}
	s.id, s.last = event.OperationID, event.Sequence
	return nil
}

func (s *Stream) Finish(result contract.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("event stream is closed")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	clean, err := redactEnvelope(s.secrets, result)
	if err != nil {
		return err
	}
	s.closed = true
	return writeJSON(s.out, Record{Schema: contract.Schema, Type: "result", Result: &clean})
}
