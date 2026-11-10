package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func TestPromptInterruptionUsesCancellationContract(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "direct", err: ui.ErrAborted},
		{name: "wrapped", err: fmt.Errorf("prompt input: %w", ui.ErrAborted)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !errors.Is(test.err, ui.ErrAborted) {
				t.Fatalf("prompt interruption lost its typed identity: %v", test.err)
			}
			fault := contract.Classify(test.err)
			if fault.Code != contract.Cancelled {
				t.Fatalf("prompt interruption code = %s, want %s", fault.Code, contract.Cancelled)
			}
			if exit := contract.ExitCode(test.err); exit != 130 {
				t.Fatalf("prompt interruption exit = %d, want 130", exit)
			}
		})
	}
}

func TestPromptErrorsPreserveNonCancellationClasses(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code contract.Code
		exit int
	}{
		{name: "input required", err: contract.NewFault(contract.InputRequired, "answer required", "Run again."), code: contract.InputRequired, exit: 2},
		{name: "validation", err: contract.NewFault(contract.ValidationFailed, "invalid value", "Fix it."), code: contract.ValidationFailed, exit: 2},
		{name: "generic internal", err: errors.New("canary implementation detail"), code: contract.Internal, exit: 70},
	} {
		t.Run(test.name, func(t *testing.T) {
			fault := contract.Classify(test.err)
			if fault.Code != test.code || contract.ExitCode(test.err) != test.exit {
				t.Fatalf("classification = %s/%d, want %s/%d", fault.Code, contract.ExitCode(test.err), test.code, test.exit)
			}
			if errors.Is(test.err, ui.ErrAborted) || fault.Code == contract.Cancelled {
				t.Fatalf("non-interrupt error was rewritten as cancellation: %v", test.err)
			}
		})
	}
}

func promptAbortApp() *app {
	a := newApp()
	a.attach(a.root, spec{
		use:  "test-prompt-abort",
		meta: readMeta("Test prompt interruption presentation", []string{"Exercise runner presentation."}, []string{"eregion test-prompt-abort"}),
		run: func(*cobra.Command, []string) (result, error) {
			return result{}, ui.ErrAborted
		},
	})
	return a
}

func TestRunnerReportsPromptInterruptionAsCancelled(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		a := promptAbortApp()
		var stdout, stderr bytes.Buffer
		code := runApp(context.Background(), []string{"test-prompt-abort", "--json"}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
		if code != 130 {
			t.Fatalf("exit = %d, want 130; stderr=%q", code, stderr.String())
		}
		envelope := decodeOne(t, stdout.String())
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.Cancelled {
			t.Fatalf("prompt interruption envelope = %+v", envelope)
		}
		if stderr.Len() != 0 {
			t.Fatalf("JSON cancellation wrote stderr: %q", stderr.String())
		}
	})

	t.Run("plain", func(t *testing.T) {
		a := promptAbortApp()
		var stdout, stderr bytes.Buffer
		code := runApp(context.Background(), []string{"test-prompt-abort"}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
		if code != 130 {
			t.Fatalf("exit = %d, want 130", code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("plain cancellation wrote stdout: %q", stdout.String())
		}
		for _, want := range []string{"error [cancelled]:", "request interrupted"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("plain cancellation lacks %q: %q", want, stderr.String())
			}
		}
		if strings.Contains(stderr.String(), "internal_error") {
			t.Fatalf("prompt interruption was reported as internal: %q", stderr.String())
		}
	})
}
