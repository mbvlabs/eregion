package cli

import (
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestLogsOutputModes(t *testing.T) {
	for _, test := range []struct {
		name   string
		config *followIO
		follow bool
		stream bool
		code   contract.Code
	}{
		{name: "plain", config: &followIO{mode: "plain"}},
		{name: "plain follow", config: &followIO{mode: "plain"}, follow: true},
		{name: "json", config: &followIO{mode: "json"}},
		{name: "json follow is refused", config: &followIO{mode: "json"}, follow: true, code: contract.Usage},
		{name: "ndjson", config: &followIO{mode: "ndjson"}, stream: true},
		{name: "ndjson follow", config: &followIO{mode: "ndjson"}, follow: true, stream: true},
		{name: "unconfigured", stream: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream, err := logsOutputMode(test.config, test.follow)
			if test.code != "" {
				if contract.Classify(err).Code != test.code {
					t.Fatalf("error = %v, want %s", err, test.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if stream != test.stream {
				t.Fatalf("stream = %t, want %t", stream, test.stream)
			}
		})
	}
}

func TestDomainAddPreviewIsAJSONObject(t *testing.T) {
	raw := json.RawMessage(`{"kind":"platform.domain.add","target":"env/hello/production","dry_run":true,"summary":"bind hello.example.com"}`)
	data, err := domainAddPreview(raw, []domainRecord{{Type: "A", Name: "hello.example.com", Value: "203.0.113.10"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Preview map[string]any `json:"preview"`
			Records []domainRecord `json:"dns_records"`
		} `json:"data"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("data is not an object: %v\n%s", err, encoded)
	}
	if envelope.Data.Preview["kind"] != "platform.domain.add" {
		t.Fatalf("preview = %+v", envelope.Data.Preview)
	}
	if len(envelope.Data.Records) != 1 || envelope.Data.Records[0].Value != "203.0.113.10" {
		t.Fatalf("dns records = %+v", envelope.Data.Records)
	}
}
