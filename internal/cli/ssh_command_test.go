package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func TestSSHUsesPTYForACommandOnlyAtATerminal(t *testing.T) {
	for _, test := range []struct {
		name       string
		command    string
		structured bool
		stdinTTY   bool
		stdoutTTY  bool
		pty        bool
	}{
		{name: "shell", pty: true},
		{name: "command at a terminal", command: "uname -m", stdinTTY: true, stdoutTTY: true, pty: true},
		{name: "command with piped stdout", command: "uname -m", stdinTTY: true, pty: false},
		{name: "command with piped stdin", command: "uname -m", stdoutTTY: true, pty: false},
		{name: "command under json", command: "uname -m", structured: true, stdinTTY: true, stdoutTTY: true, pty: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sshUsesPTY(test.command, test.structured, test.stdinTTY, test.stdoutTTY); got != test.pty {
				t.Fatalf("sshUsesPTY = %t, want %t", got, test.pty)
			}
		})
	}
}

func TestSudoPasswordWatchDetectsNoTerminalMessages(t *testing.T) {
	for _, test := range []struct {
		name  string
		parts []string
		seen  bool
	}{
		{name: "classic terminal required", parts: []string{"sudo: a terminal is required to read the password; either use the -S option\n"}, seen: true},
		{name: "classic password required", parts: []string{"sudo: a password is required\n"}, seen: true},
		{name: "sudo-rs terminal required", parts: []string{"sudo: A terminal is required to authenticate\n"}, seen: true},
		{name: "sudo-rs interactive authentication", parts: []string{"sudo: interactive authentication is required\n"}, seen: true},
		{name: "classic mixed case", parts: []string{"SuDo: A PaSsWoRd Is ReQuIrEd\n"}, seen: true},
		{name: "sudo-rs mixed case", parts: []string{"SUDO: A TERMINAL IS REQUIRED TO AUTHENTICATE\n"}, seen: true},
		{name: "classic split across writes", parts: []string{"sudo: a ", "password is re", "quired"}, seen: true},
		{name: "sudo-rs split across writes", parts: []string{"sudo: interactive authentica", "tion is required\n"}, seen: true},
		{name: "sudo-rs split after a long write", parts: []string{strings.Repeat("x", 100) + "sudo: interactive authentica", "tion is required\n"}, seen: true},
		{name: "ordinary stderr", parts: []string{"starting\n", "sudo: unable to resolve host node\n"}, seen: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			watch := &sudoPasswordWatch{w: &out}
			var want strings.Builder
			for _, part := range test.parts {
				if _, err := watch.Write([]byte(part)); err != nil {
					t.Fatal(err)
				}
				want.WriteString(part)
			}
			if watch.seen != test.seen {
				t.Fatalf("seen = %t, want %t", watch.seen, test.seen)
			}
			if out.String() != want.String() {
				t.Fatalf("stderr was not passed through: %q", out.String())
			}
		})
	}
}

func TestSSHSudoFaultCarriesTheFix(t *testing.T) {
	fault := contract.Classify(sshSudoFault(1))
	if fault.Code != contract.InputRequired {
		t.Fatalf("sudo refusal code = %s", fault.Code)
	}
	if !strings.Contains(fault.Hint, "--command") || !strings.Contains(fault.Message, "sudo") {
		t.Fatalf("sudo refusal is not actionable: %+v", fault)
	}
	if status, _ := fault.Details["exit_status"].(int); status != 1 {
		t.Fatalf("exit_status = %v", fault.Details["exit_status"])
	}
}

func streamedFaultApp() *app {
	a := newApp()
	a.attach(a.root, spec{
		use:  "test-streamed-fault",
		meta: readMeta("Test a streamed command failure", []string{"Exercise runner presentation."}, []string{"eregion test-streamed-fault"}),
		run: func(*cobra.Command, []string) (result, error) {
			fault := sshSudoFault(1)
			return result{streamed: true, streamFault: fault}, fault
		},
	})
	return a
}

func TestRunnerShowsAStreamedFaultInJSON(t *testing.T) {
	a := streamedFaultApp()
	var stdout, stderr bytes.Buffer
	code := runApp(context.Background(), []string{"test-streamed-fault", "--json"}, strings.NewReader(""), &stdout, &stderr, &secrets.Registry{}, a)
	if code != 2 {
		t.Fatalf("exit code = %d, want input_required's 2; stderr=%q", code, stderr.String())
	}
	envelope := decodeOne(t, stdout.String())
	if envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
		t.Fatalf("streamed failure envelope = %+v", envelope)
	}
	if !strings.Contains(envelope.Error.Hint, "--command") || !strings.Contains(envelope.Error.Hint, "sudo can prompt") {
		t.Fatalf("streamed failure hint = %q", envelope.Error.Hint)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON refusal wrote stderr: %q", stderr.String())
	}
}

func TestRunnerShowsAStreamedFaultInPlain(t *testing.T) {
	a := streamedFaultApp()
	var stdout, stderr bytes.Buffer
	code := runApp(context.Background(), []string{"test-streamed-fault"}, strings.NewReader(""), &stdout, &stderr, &secrets.Registry{}, a)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("plain refusal wrote stdout: %q", stdout.String())
	}
	for _, want := range []string{"error [input_required]:", "hint:", "sudo on the node needs a password", "eregion ssh --command"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("plain refusal lacks %q: %q", want, stderr.String())
		}
	}
}
