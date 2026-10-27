package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/protect"
	"github.com/mbvlabs/eregion/internal/totp"
	"github.com/spf13/cobra"
)

// protectMeta is the shared metadata for the node's human-factor commands.
func protectMeta(summary, example string) CommandMeta {
	meta := operationMeta(summary, example)
	meta.WhenToUse = []string{summary + ". Runs on the node through SSH inside WireGuard; the TOTP secret stays root-only on the node and is shown once at a terminal."}
	return meta
}

// protectSpecs is included in the command tree by tree.go.
func (a *app) protectSpecs() []spec {
	initMeta := protectMeta("Set up the node's TOTP human factor", "eregion protect init")
	initMeta.Mutating = true
	initMeta.DryRunReason = "Two-factor setup is an interactive one-time exchange: a dry run cannot show a usable secret or confirm a code."
	initMeta.Prompts = false
	statusMeta := protectMeta("Show whether the node's TOTP human factor is set up", "eregion protect status --json")
	statusMeta.Next = []string{"eregion protect init"}
	return []spec{{
		use:  "protect",
		meta: protectMeta("Manage the node's TOTP human factor", "eregion protect status --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "init", meta: initMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().Bool("rotate", false, "Replace the existing secret; asks for a current code first")
				cmd.Flags().Bool("yes", false, "Refused: the authenticator code cannot be replaced by an acknowledgement")
			}, run: a.protectInit},
			{use: "status", meta: statusMeta, run: a.protectStatus},
		},
	}}
}

func (a *app) protectInit(cmd *cobra.Command, _ []string) (result, error) {
	if err := requireHuman(cmd, "eregion protect init"); err != nil {
		return result{}, err
	}
	rotate, _ := cmd.Flags().GetBool("rotate")
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	var current string
	if rotate {
		current, err = readProtectCode(cmd, "Current code from the existing eregion entry")
		if err != nil {
			return result{}, err
		}
	}
	setup, err := client.ProtectInit(cmd.Context(), rotate, currentContextName(), current)
	if err != nil {
		return result{}, err
	}
	a.secrets.Add(setup.Secret)
	if err := showProtectSetup(cmd, setup); err != nil {
		return result{}, err
	}
	var status protect.Status
	for attempt := 1; ; attempt++ {
		code, err := readProtectCode(cmd, "Code the authenticator app now shows for eregion")
		if err != nil {
			return result{}, err
		}
		status, err = client.ProtectConfirm(cmd.Context(), code)
		if err == nil {
			break
		}
		if attempt >= 3 || !protect.Retryable(err) {
			return result{}, err
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "That code was not accepted ("+contract.Classify(err).Message+"). Check the device clock and try the next code.")
	}
	message := "two-factor protection is set up: minting a token and exporting secrets now ask for a code from the authenticator app"
	if rotate {
		message = "the two-factor secret was rotated: remove the old eregion entry from the authenticator app and keep the new one"
	}
	return result{
		data:  map[string]any{"configured": true, "rotated": rotate, "confirmed_at": status.ConfirmedAt},
		plain: message + "\n",
	}, nil
}

func (a *app) protectStatus(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	status, err := client.ProtectStatus(cmd.Context())
	if err != nil {
		return result{}, err
	}
	var lines []string
	switch {
	case status.Configured && status.ConfirmedAt != "":
		lines = append(lines, "Two-factor protection is set up (since "+status.ConfirmedAt+").")
	case status.Configured:
		lines = append(lines, "Two-factor protection is set up.")
	default:
		lines = append(lines, "Two-factor protection is not set up.")
	}
	if status.Pending {
		lines = append(lines, "A new secret from eregion protect init is waiting for its first code.")
	}
	if status.LockedUntil != "" {
		lines = append(lines, "Codes are locked out until "+status.LockedUntil+" after too many wrong codes.")
	} else if status.Failures > 0 {
		lines = append(lines, fmt.Sprintf("%d wrong code(s) since the last accepted one.", status.Failures))
	}
	var next []string
	if !status.Configured {
		next = []string{"eregion protect init"}
	}
	return result{data: status, plain: strings.Join(lines, "\n") + "\n", next: next}, nil
}

// showProtectSetup prints the otpauth URI as a QR code and the secret for
// typing in by hand, on stderr, outside the result envelope.
func showProtectSetup(cmd *cobra.Command, setup protect.Setup) error {
	code, err := ui.QR(setup.URI)
	if err != nil {
		return err
	}
	resume := ui.Suspend()
	defer resume()
	_, err = fmt.Fprint(cmd.ErrOrStderr(),
		"Scan this QR code with an authenticator app (1Password, Aegis, Google Authenticator, ...):\n\n"+code+"\n"+
			"Or add it by hand: "+groupSecret(setup.Secret)+" (time-based, SHA1, 6 digits, 30 seconds)\n"+
			"URI: "+setup.URI+"\n"+
			"The node keeps the secret root-only and does not show it again. Keep a backup of the authenticator entry:\n"+
			"without it, only root on the node can mint tokens or export the handoff.\n\n")
	return err
}

func groupSecret(secret string) string {
	var parts []string
	for len(secret) > 4 {
		parts = append(parts, secret[:4])
		secret = secret[4:]
	}
	return strings.Join(append(parts, secret), " ")
}

func readProtectCode(cmd *cobra.Command, label string) (string, error) {
	for attempt := 1; ; attempt++ {
		// A one-time code expires in seconds; it is shown as typed.
		code, err := ui.Input(label, "", "")
		if err != nil {
			return "", err
		}
		if code = totp.NormalizeCode(code); totp.WellFormed(code) {
			return code, nil
		}
		if attempt >= 3 {
			return "", contract.NewFault(contract.ValidationFailed, "the code must be 6 digits", "Type the 6-digit code the authenticator app shows.")
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "The code is 6 digits.")
	}
}

// currentContextName is a best-effort label for the authenticator entry.
func currentContextName() string {
	store, err := host.LoadContextStore("")
	if err != nil {
		return ""
	}
	current, err := store.Get("")
	if err != nil {
		return ""
	}
	return current.Name
}
