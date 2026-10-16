package output

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestOutputSelection(t *testing.T) {
	for _, test := range []struct {
		args []string
		mode Mode
		err  bool
	}{
		{nil, Plain, false},
		{[]string{"--unknown", "--json"}, JSON, false},
		{[]string{"--output=json"}, JSON, false},
		{[]string{"--json=false"}, Plain, false},
		{[]string{"--json", "--output=plain"}, JSON, true},
		{[]string{"--json", "--output=ndjson"}, JSON, true},
		{[]string{"--output=ndjson"}, Plain, true},
		{[]string{"--output", "--json"}, JSON, true},
		{[]string{"--output=json", "--json=wat"}, JSON, true},
		{[]string{"--", "--json"}, Plain, false},
		{[]string{"--path", "--json"}, Plain, false},
		{[]string{"--path=--json"}, Plain, false},
	} {
		mode, err := OptionsFromArgs(test.args, "path")
		if mode != test.mode || (err != nil) != test.err {
			t.Fatalf("%v: %s %v", test.args, mode, err)
		}
	}
}

func TestSafeText(t *testing.T) {
	value := SafeText("\x1b]52;c;clipboard\a\r\u009b\u202e\n\t✓")
	if strings.ContainsAny(value, "\x1b\a\r\u009b\u202e") || !strings.HasSuffix(value, "\n\t✓") {
		t.Fatalf("unsafe/incorrect text: %q", value)
	}
}

func TestWriterRedactsEveryField(t *testing.T) {
	registry := &secrets.Registry{}
	registry.Add("secret-value")
	for _, mode := range []Mode{Plain, JSON} {
		for _, envelope := range []contract.Envelope{
			contract.Success("secret-value", map[string]any{"secret-value": "secret-value"}, []string{"secret-value"}),
			contract.Failure("secret-value", &contract.Fault{Code: contract.ValidationFailed, Message: "secret-value", Hint: "secret-value", Details: map[string]any{"secret-value": "secret-value"}}),
		} {
			var stdout, stderr bytes.Buffer
			writer := Writer{Out: &stdout, Err: &stderr, Mode: mode, Secrets: registry}
			if err := writer.Write(envelope, "secret-value\x1b[2J"); err != nil {
				t.Fatal(err)
			}
			raw := stdout.String() + stderr.String()
			if strings.Contains(raw, "secret-value") || strings.ContainsRune(raw, '\x1b') {
				t.Fatalf("unsafe output: %q", raw)
			}
			if mode == JSON && (!json.Valid(stdout.Bytes()) || stderr.Len() != 0) {
				t.Fatalf("invalid JSON destination: %q %q", stdout.String(), stderr.String())
			}
		}
	}
}

func event(sequence uint64) contract.Event {
	return contract.Event{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Sequence: sequence, At: time.Now().UTC(), Kind: "log", Message: "secret-value"}
}

func TestNDJSONStream(t *testing.T) {
	var out bytes.Buffer
	registry := &secrets.Registry{}
	registry.Add("secret-value")
	stream := NewStream(&out, registry)
	if err := stream.Event(event(1)); err != nil {
		t.Fatal(err)
	}
	if stream.Event(event(1)) == nil {
		t.Fatal("accepted duplicate sequence")
	}
	if err := stream.Event(event(2)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Finish(contract.Success("eregion operations follow", map[string]any{"state": "running"}, nil)); err != nil {
		t.Fatal(err)
	}
	if stream.Event(event(3)) == nil || stream.Finish(contract.Success("again", nil, nil)) == nil {
		t.Fatal("stream accepted output after its final result")
	}
	decoder := json.NewDecoder(&out)
	for i, kind := range []string{"event", "event", "result"} {
		var record Record
		if err := decoder.Decode(&record); err != nil || record.Type != kind || record.Schema != contract.Schema {
			t.Fatalf("record %d: %+v %v", i, record, err)
		}
		if record.Event != nil && record.Event.Message != secrets.Placeholder {
			t.Fatal("event leaked a secret")
		}
	}
	if decoder.Decode(new(any)) != io.EOF {
		t.Fatal("unexpected trailing stream data")
	}
}

type shortWriter struct{ calls int }

func (w *shortWriter) Write(data []byte) (int, error) { w.calls++; return len(data) - 1, nil }

func TestStreamClosesOnWriteFailure(t *testing.T) {
	w := &shortWriter{}
	stream := NewStream(w, &secrets.Registry{})
	if stream.Event(event(1)) != io.ErrShortWrite || stream.Event(event(2)) == nil || w.calls != 1 {
		t.Fatal("stream continued after partial output")
	}
}

func TestStreamRejectsInvalidEventsAndResults(t *testing.T) {
	var out bytes.Buffer
	stream := NewStream(&out, &secrets.Registry{})
	bad := event(0)
	if stream.Event(bad) == nil || out.Len() != 0 {
		t.Fatal("invalid event reached the wire")
	}
	if stream.Finish(contract.Envelope{}) == nil || out.Len() != 0 {
		t.Fatal("invalid result reached the wire")
	}
	if err := stream.Event(event(1)); err != nil {
		t.Fatal(err)
	}
	other := event(2)
	other.OperationID = contract.OperationID("op_" + strings.Repeat("b", 32))
	if stream.Event(other) == nil {
		t.Fatal("mixed operations in a stream")
	}
	if err := stream.Finish(contract.Failure("eregion operations follow", contract.NewFault(contract.Unavailable, "connection lost", "Reconnect from the cursor."))); err != nil {
		t.Fatal(err)
	}
}
