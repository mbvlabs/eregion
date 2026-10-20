package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("noninteractive command read stdin") }

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, panicReader{}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func decodeOne(t *testing.T, raw string) contract.Envelope {
	t.Helper()
	if strings.ContainsRune(raw, '\x1b') {
		t.Fatal("output contains terminal escapes")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var envelope contract.Envelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("invalid envelope: %v: %q", err, raw)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("expected exactly one envelope, got trailing data: %v", err)
	}
	if envelope.Schema != contract.Schema || envelope.Command == "" {
		t.Fatalf("missing contract identity: %+v", envelope)
	}
	return envelope
}

func TestMachineSuccess(t *testing.T) {
	for _, args := range [][]string{
		{"--json"}, {"version", "--json"}, {"--output", "json", "version"},
		{"commands", "--json"}, {"commands", "--path", "version", "--json"},
		{"commands", "--path=eregion version", "--json"},
		{"help", "--json"}, {"help", "version", "--json"},
		{"--help", "--json"}, {"version", "--help", "--json"},
		{"--json", "help", "commands"}, {"commands", "-h", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := invoke(t, args...)
			envelope := decodeOne(t, stdout)
			if code != 0 || !envelope.OK || envelope.Error != nil || stderr != "" {
				t.Fatalf("code=%d stderr=%q envelope=%+v", code, stderr, envelope)
			}
		})
	}
}

func TestMachineErrors(t *testing.T) {
	for _, test := range []struct {
		args []string
		code contract.Code
	}{
		{[]string{"deploy", "--json"}, contract.Usage},
		{[]string{"version", "extra", "--json"}, contract.Usage},
		{[]string{"version", "--unknown", "--json"}, contract.Usage},
		{[]string{"--unknown", "version", "--json"}, contract.Usage},
		{[]string{"commands", "--path", "deploy", "--json"}, contract.Usage},
		{[]string{"help", "version", "extra", "--json"}, contract.Usage},
		{[]string{"completion", "bash", "--json"}, contract.Usage},
		{[]string{"commands", "--path=", "--json"}, contract.InputRequired},
		{[]string{"--json", "commands", "--path"}, contract.InputRequired},
		{[]string{"--json", "--output"}, contract.InputRequired},
		{[]string{"version", "--output", "invalid", "--json"}, contract.InvalidOutputMode},
		{[]string{"version", "--output=plain", "--json"}, contract.InvalidOutputMode},
		{[]string{"version", "--json=invalid", "--output=json"}, contract.Usage},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			code, stdout, stderr := invoke(t, test.args...)
			envelope := decodeOne(t, stdout)
			if code != 2 || envelope.OK || envelope.Error == nil || envelope.Error.Code != test.code || stderr != "" {
				t.Fatalf("code=%d stderr=%q envelope=%+v", code, stderr, envelope)
			}
		})
	}
}

func TestPlainOutputAndDelimiters(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"help"}, {"--help"}, {"commands"}, {"version", "--json=false"}} {
		code, stdout, stderr := invoke(t, args...)
		if code != 0 || stdout == "" || strings.ContainsRune(stdout, '\x1b') || stderr != "" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
	for _, args := range [][]string{{"version", "--", "--json"}, {"commands", "--path", "--json"}} {
		code, stdout, stderr := invoke(t, args...)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "error [usage_error]") {
			t.Fatalf("flag-looking argument must not select JSON: %v: %d %q %q", args, code, stdout, stderr)
		}
	}
}

func TestCatalogIsCompleteAndOnlyImplemented(t *testing.T) {
	code, stdout, _ := invoke(t, "commands", "--json")
	if code != 0 {
		t.Fatal(stdout)
	}
	envelope := decodeOne(t, stdout)
	raw, _ := json.Marshal(envelope.Data)
	var data struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	implemented := newApp()
	if len(data.Commands) != len(walk(implemented.root)) {
		t.Fatalf("catalog does not match implemented tree: %d commands", len(data.Commands))
	}
	seen := map[string]bool{}
	for _, command := range data.Commands {
		if seen[command.Path] {
			t.Fatalf("duplicate catalog command: %s", command.Path)
		}
		seen[command.Path] = true
		events := command.Path == "eregion operations follow" && command.Output == "events"
		if command.Prompts || (command.Output != "result" && !events) {
			t.Fatalf("advertised unimplemented behavior: %+v", command)
		}
		if command.Mutating && !command.DryRun && command.DryRunReason == "" {
			t.Fatalf("mutation lacks preview/exemption: %+v", command)
		}
		flags := map[string]bool{}
		for _, flag := range command.Flags {
			flags[flag.Name] = true
		}
		if !flags["json"] || !flags["output"] || !flags["help"] {
			t.Fatalf("missing inherited/help flags: %+v", command)
		}
	}
	for _, path := range []string{"eregion operations follow", "eregion platform submit", "eregion platform fleet", "eregion ui", "eregion secret list", "eregion upgrade check"} {
		if !seen[path] {
			t.Fatalf("missing implemented product command: %s", path)
		}
	}
}

func TestInvocationIsolation(t *testing.T) {
	for range 12 {
		t.Run("parallel", func(t *testing.T) {
			t.Parallel()
			code, stdout, _ := invoke(t, "version", "--json")
			if code != 0 || !decodeOne(t, stdout).OK {
				t.Fatal(stdout)
			}
		})
	}
}

func TestRedactionOnParseAndHelpPaths(t *testing.T) {
	for _, args := range [][]string{{"secret-token-value", "--json"}, {"secret-token-value"}, {"version", "--json"}, {"help", "--json"}} {
		var stdout, stderr bytes.Buffer
		registry := &secrets.Registry{}
		registry.Add("secret-token-value", "binary")
		run(context.Background(), args, panicReader{}, &stdout, &stderr, registry)
		if strings.Contains(stdout.String()+stderr.String(), "secret-token-value") || strings.Contains(stdout.String()+stderr.String(), "binary") {
			t.Fatalf("redaction bypass: %q %q", stdout.String(), stderr.String())
		}
	}
}

func TestRedactionOfQuotedParseErrors(t *testing.T) {
	secret := "quoted\"credential\nvalue"
	for _, suffix := range [][]string{nil, {"--json"}} {
		args := append([]string{secret}, suffix...)
		var stdout, stderr bytes.Buffer
		registry := &secrets.Registry{}
		registry.Add(secret)
		run(context.Background(), args, panicReader{}, &stdout, &stderr, registry)
		if strings.Contains(stdout.String()+stderr.String(), "credential") {
			t.Fatalf("quoted parse error leaked a secret: %q %q", stdout.String(), stderr.String())
		}
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"version", "--json"}, panicReader{}, &stdout, &stderr)
	envelope := decodeOne(t, stdout.String())
	if code != 130 || envelope.Error.Code != contract.Cancelled {
		t.Fatalf("code=%d envelope=%+v", code, envelope)
	}
}

type brokenWriter struct{ calls int }

func (w *brokenWriter) Write([]byte) (int, error) { w.calls++; return 0, io.ErrClosedPipe }

func TestOutputFailureDoesNotAppendAnotherEnvelope(t *testing.T) {
	var stderr bytes.Buffer
	out := &brokenWriter{}
	if code := Run(context.Background(), []string{"version", "--json"}, panicReader{}, out, &stderr); code != 70 || out.calls != 1 {
		t.Fatalf("code=%d writes=%d", code, out.calls)
	}
	if strings.Contains(stderr.String(), io.ErrClosedPipe.Error()) {
		t.Fatal("leaked arbitrary writer error")
	}
}
