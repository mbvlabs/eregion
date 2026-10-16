package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestRedactionPreservesPublicProtocolStructure(t *testing.T) {
	registry := &secrets.Registry{}
	registry.Add("schema", "v1", "1", "state", "usage_error", "type", "sequence")
	var out bytes.Buffer
	writer := Writer{Out: &out, Mode: JSON, Secrets: registry}
	fault := &contract.Fault{Code: contract.Usage, Message: "schema v1", Details: map[string]any{"schema": "state"}}
	if err := writer.Write(contract.Failure("eregion", fault), ""); err != nil {
		t.Fatal(err)
	}
	var envelope contract.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.Schema != contract.Schema || envelope.Error.Code != contract.Usage {
		t.Fatalf("damaged protocol: %s %v", out.String(), err)
	}
	if envelope.Error.Message != "[REDACTED] [REDACTED]" || envelope.Error.Details["[REDACTED]"] != "[REDACTED]" {
		t.Fatalf("dynamic content leaked: %s", out.String())
	}
	out.Reset()
	stream := NewStream(&out, registry)
	e := event(1)
	e.Message = "schema v1 state sequence"
	if err := stream.Event(e); err != nil {
		t.Fatal(err)
	}
	var record Record
	if err := json.Unmarshal(out.Bytes(), &record); err != nil || record.Type != "event" || record.Schema != contract.Schema || record.Event.Sequence != 1 {
		t.Fatalf("damaged stream: %s %v", out.String(), err)
	}
	if strings.Contains(record.Event.Message, "schema") {
		t.Fatal("dynamic event content leaked")
	}
}
