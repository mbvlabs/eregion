package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// humanAtTerminal reports whether this invocation may ask a person for input.
// Structured output and a non-terminal stdin never prompt, so an agent cannot
// supply a human factor.
func humanAtTerminal(cmd *cobra.Command) bool {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	return !jsonMode && outputMode != "json" &&
		term.IsTerminal(int(os.Stdin.Fd())) && //nolint:gosec // G115: file descriptors fit in int.
		term.IsTerminal(int(os.Stderr.Fd())) //nolint:gosec // G115: file descriptors fit in int.
}

// requireHuman refuses when a command needs a person at a terminal. --yes
// never substitutes: the factor is the code, not an acknowledgement.
func requireHuman(cmd *cobra.Command, action string) error {
	if flag := cmd.Flags().Lookup("yes"); flag != nil {
		if yes, _ := cmd.Flags().GetBool("yes"); yes {
			return contract.NewFault(contract.InputRequired, action+" cannot be accepted with --yes",
				"The human factor is a code from the authenticator app typed at a terminal. There is no flag, environment variable, or acknowledgement that replaces it.")
		}
	}
	if humanAtTerminal(cmd) {
		return nil
	}
	return contract.NewFault(contract.InputRequired, action+" needs a human at an interactive terminal",
		"Run it yourself in a terminal, without --json, --output json, --yes, or a pipe. There is no flag or environment variable for the authenticator code or the exported secrets.")
}

// readSecretTTY reads a value from the terminal without echo.
func readSecretTTY(cmd *cobra.Command, label string) (string, error) {
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), label+": "); err != nil {
		return "", err
	}
	raw, err := term.ReadPassword(int(os.Stdin.Fd())) //nolint:gosec // G115: file descriptors fit in int.
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", contract.NewFault(contract.InputRequired, "could not read the terminal without echo", "Run the command in a real terminal.")
	}
	return strings.TrimSpace(string(raw)), nil
}
