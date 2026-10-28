package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

// pipeStdin makes the process stdin a pipe, so a command that checks for a
// human terminal cannot pass by borrowing the test runner's terminal.
func pipeStdin(t *testing.T) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = previous
		_ = reader.Close()
		_ = writer.Close()
	})
}

func TestDeleteRefusesWithoutAHumanTerminal(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	pipeStdin(t)
	for _, test := range []struct {
		name    string
		args    []string
		message string
		plain   bool
	}{
		{
			name: "json",
			args: []string{"app", "delete", "api", "--confirm", "api", "--json"},
		},
		{
			name:    "yes",
			args:    []string{"app", "delete", "api", "--confirm", "api", "--yes", "--json"},
			message: "--yes",
		},
		{
			name:    "pipe",
			args:    []string{"app", "delete", "api", "--confirm", "api"},
			message: "interactive terminal",
			plain:   true,
		},
		{
			name: "environment",
			args: []string{"env", "delete", "api", "production", "--confirm", "api/production", "--json"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := invoke(t, test.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (input_required); stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !test.plain {
				envelope := decodeOne(t, stdout)
				if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
					t.Fatalf("delete refusal = %+v, want input_required", envelope.Error)
				}
				if test.message != "" && !strings.Contains(envelope.Error.Message, test.message) {
					t.Fatalf("refusal %q does not name %q", envelope.Error.Message, test.message)
				}
				return
			}
			if !strings.Contains(stderr, "input_required") || !strings.Contains(stderr, test.message) {
				t.Fatalf("plain refusal stderr = %q", stderr)
			}
		})
	}
}

func TestDeleteDryRunNeedsNoFactorAndNoTerminal(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	pipeStdin(t)
	code, stdout, stderr := invoke(t, "app", "delete", "api", "--confirm", "api", "--dry-run", "--json")
	envelope := decodeOne(t, stdout)
	if code != 3 || envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.NotFound {
		t.Fatalf("dry run = code %d error %+v stderr %q; it must pass the factor and reach context resolution", code, envelope.Error, stderr)
	}
}
