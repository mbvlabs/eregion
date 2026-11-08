package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestWorkspaceRejectsMachineOutputAndNonTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code contract.Code
	}{
		{name: "json flag", args: []string{"ui", "--json"}, code: contract.InvalidOutputMode},
		{name: "json before command", args: []string{"--json", "ui"}, code: contract.InvalidOutputMode},
		{name: "output json", args: []string{"ui", "--output", "json"}, code: contract.InvalidOutputMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit, stdout, stderr := invoke(t, tc.args...)
			if exit != 2 || stderr != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
			}
			envelope := decodeOne(t, stdout)
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != tc.code {
				t.Fatalf("envelope = %+v", envelope)
			}
			if !strings.Contains(envelope.Error.Message, "cannot produce machine output") {
				t.Fatalf("message = %q", envelope.Error.Message)
			}
		})
	}

	exit, stdout, stderr := invoke(t, "ui")
	if exit != 2 || stdout != "" || !strings.HasPrefix(stderr, "error [usage_error]") ||
		!strings.Contains(stderr, "requires a terminal") {
		t.Fatalf("nonterminal ui: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

// TestWorkspaceHelpNamesOnlyExistingFlags keeps the workspace's guidance in
// step with its actual flags; an earlier version required a nonexistent
// --interactive flag.
func TestWorkspaceHelpNamesOnlyExistingFlags(t *testing.T) {
	a := newApp()
	cmd, _, err := a.root.Find([]string{"ui"})
	if err != nil {
		t.Fatal(err)
	}
	meta := a.meta[cmd]
	if flagFor(cmd, "interactive") != nil {
		t.Fatal("ui unexpectedly has an --interactive flag")
	}
	for _, line := range meta.WhenToUse {
		if strings.Contains(line, "--interactive") {
			t.Fatalf("ui help names a flag that does not exist: %q", line)
		}
	}
}
