package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/bundle"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// clearScreen clears the screen and, where the terminal supports it, the
// scrollback.
const clearScreen = "\x1b[H\x1b[2J\x1b[3J"

// handoffField is one labelled value of the secrets record.
type handoffField struct{ label, value string }

// handoffGroup is one password-manager entry of the secrets record.
type handoffGroup struct {
	title  string
	fields []handoffField
}

// secretsHandoff is the record server init hands to the operator: every server
// secret in plain text plus an armored age transfer bundle. eregion keeps only
// the operator's SSH and WireGuard keys in the local context, so this is the
// copy that survives losing this machine.
type secretsHandoff struct {
	node       string
	groups     []handoffGroup
	passphrase string
	bundle     []byte
	// path is the --secrets-out record; empty when the operator only sees it
	// on screen.
	path string
}

// newSecretsHandoff encrypts the record into a transfer bundle with a new
// passphrase. Nothing is written here; writeRecord is the explicit
// --secrets-out step.
func newSecretsHandoff(node string, groups []handoffGroup) (*secretsHandoff, error) {
	h := &secretsHandoff{node: node, groups: groups}
	pass, err := bundle.NewPassphrase()
	if err != nil {
		return nil, err
	}
	sealed, err := bundle.Encrypt([]byte(h.body()), pass)
	if err != nil {
		return nil, err
	}
	h.passphrase, h.bundle = pass, sealed
	return h, nil
}

// record is every value, then the transfer bundle and its passphrase.
func (h *secretsHandoff) record() string {
	return h.body() + "\n## Transfer bundle\n" +
		"On another machine run eregion login and paste this block.\n" +
		"Passphrase: " + h.passphrase + "\n" + string(h.bundle)
}

func (h *secretsHandoff) body() string {
	var b strings.Builder
	b.WriteString("eregion secrets handoff for server " + h.node + "\n")
	for _, g := range h.groups {
		b.WriteString("\n## " + g.title + "\n")
		for _, f := range g.fields {
			v := strings.TrimRight(f.value, "\n")
			if v == "" {
				continue
			}
			if strings.Contains(v, "\n") {
				b.WriteString(f.label + ":\n" + strings.TrimLeft(v, "\r\n") + "\n")
				continue
			}
			b.WriteString(f.label + ": " + v + "\n")
		}
	}
	return b.String()
}

// writeRecord writes the whole record (secrets, bundle and passphrase) to an
// explicit owner-only file, atomically. It never overwrites a file the operator
// did not name for this init.
func (h *secretsHandoff) writeRecord(path string, overwrite bool) error {
	path = expandHome(strings.TrimSpace(path))
	if path == "" {
		return contract.NewFault(contract.InputRequired, "--secrets-out needs a path", "Pass a new file path.")
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if !overwrite {
		if _, err := os.Lstat(path); err == nil {
			fault := contract.NewFault(contract.PreconditionFailed, path+" already exists", "Move it away or choose another --secrets-out path.")
			fault.Details = map[string]any{"reason": "secrets_out_exists", "path": path}
			return fault
		} else if !errors.Is(err, os.ErrNotExist) {
			return contract.NewFault(contract.ValidationFailed, "could not inspect the secrets handoff path", err.Error())
		}
	}
	if err := engine.WriteFileAtomic(path, []byte(h.record()), 0o600); err != nil {
		return contract.NewFault(contract.ValidationFailed, "could not write the secrets handoff", err.Error())
	}
	h.path = path
	return nil
}

// show is the handoff gate: copy or show the record, then type 'stored'. It
// never writes a file by itself.
func (h *secretsHandoff) show(w io.Writer) error {
	resume := ui.Suspend()
	defer resume()
	_, _ = fmt.Fprintln(w, "\n"+ui.Header("Secrets handoff", 72))
	_, _ = fmt.Fprintln(w, "The server's record: the operator's keys, the sudo password, the database URL and the vault keyring.")
	_, _ = fmt.Fprintln(w, "eregion doesn't show it again. Store it in your password manager.")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, ui.Dim("Bundle passphrase")+"  "+h.passphrase)
	if h.path != "" {
		_, _ = fmt.Fprintln(w, ui.Note("The full record was written to "+h.path+". Keep the passphrase apart from the file."))
	}
	_, _ = fmt.Fprintln(w, ui.Note("To log in from another machine: eregion login, paste the transfer bundle, then this passphrase."))
	_, _ = fmt.Fprintln(w)
	for {
		choice, err := ui.Choose("Secrets handoff", "", [][2]string{
			{"Copy everything to the clipboard", "copy"},
			{"Show everything on screen", "print"},
			{"Done", "done"},
		})
		if err != nil {
			return err
		}
		switch choice {
		case "done":
			if err := ui.TypeWord("Type 'stored' once the record is in your password manager", "", "stored"); err != nil {
				if errors.Is(err, ui.ErrAborted) {
					return contract.NewFault(contract.InputRequired, "the secrets handoff was not confirmed as stored", "The server is set up and the context is saved locally; run server init --resume to show the handoff again.")
				}
				return err
			}
			if clear, err := ui.YesNoDefault("Clear the screen now?", "", true); err == nil && clear {
				_, _ = fmt.Fprint(w, clearScreen)
			}
			return nil
		case "copy":
			_, _ = fmt.Fprint(w, osc52(h.record()))
			_, _ = fmt.Fprintln(w, ui.Note("Copied to your clipboard. Nothing pasted? Pick Show everything on screen."))
		case "print":
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprint(w, h.record())
			_, _ = fmt.Fprintln(w)
		}
	}
}

// osc52 asks the terminal to put s on the system clipboard; it works over SSH.
func osc52(s string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\a"
}

// handoff builds and delivers the record. Without a terminal the early gate has
// already required --secrets-out; with one, init waits for 'stored'.
func (r *serverInit) handoff(_ context.Context, summary *serverInitSummary) error {
	task := r.start("handoff", "Building the secrets handoff")
	// Register the actual secrets, not public labels: the JSON summary and
	// ordinary output stay readable while credentials are redacted.
	r.a.secrets.Add(r.identity.sudoPassword, r.identity.wg.Encode(), r.identity.sshKeyPEM, r.answers.DatabaseURL, r.vaultKeyring)
	h, err := newSecretsHandoff(r.answers.Context, r.handoffGroups())
	if err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	r.a.secrets.Add(h.passphrase)
	out := flagString(r.cmd, "secrets-out")
	if out == "" {
		out = r.pending.SecretsOut
	}
	if out != "" {
		overwrite := r.pending.SecretsOut != "" && samePath(out, r.pending.SecretsOut)
		if err := h.writeRecord(out, overwrite); err != nil {
			task.end(ui.EndFail, "")
			return err
		}
		r.pending.SecretsOut = h.path
		if err := r.savePending(); err != nil {
			task.end(ui.EndFail, "")
			return err
		}
		summary.SecretsOut = h.path
	}
	task.end(ui.EndOK, h.path)
	if !r.interactive {
		return nil
	}
	if err := h.show(r.cmd.ErrOrStderr()); err != nil {
		return err
	}
	return nil
}

func samePath(left, right string) bool {
	l, err1 := filepath.Abs(expandHome(left))
	r, err2 := filepath.Abs(expandHome(right))
	return err1 == nil && err2 == nil && l == r
}

// handoffRecordInput is every field of the server-init record. Both init and
// secrets export build the same groups from it.
type handoffRecordInput struct {
	NodeName                 string
	Context                  string
	Hostname                 string
	PublicAddress            string
	Platform                 string
	SSHHostKey               string
	WireGuardEndpoint        string
	WireGuardNodeAddress     string
	WireGuardNodePublicKey   string
	AdminUser                string
	SudoPassword             string
	OperatorWireGuardAddress string
	OperatorWireGuardKey     string
	OperatorSSHKey           string
	DatabaseURL              string
	VaultKeyring             string
}

// handoffGroups is the password-manager shape of the record. Every value here
// is a handoff exception: it is shown once and never becomes ordinary output.
func handoffGroups(in handoffRecordInput) []handoffGroup {
	return []handoffGroup{
		{title: "Main node " + in.NodeName, fields: []handoffField{
			{"Node name", in.NodeName},
			{"Context", in.Context},
			{"Hostname", in.Hostname},
			{"Public address", in.PublicAddress},
			{"Platform", in.Platform},
			{"SSH host key", in.SSHHostKey},
			{"WireGuard endpoint", in.WireGuardEndpoint},
			{"WireGuard address", in.WireGuardNodeAddress},
			{"WireGuard public key", in.WireGuardNodePublicKey},
		}},
		{title: "Operator", fields: []handoffField{
			{"Admin user", in.AdminUser},
			{"Context", in.Context},
			{"SSH user", in.AdminUser},
			{"Sudo password", in.SudoPassword},
			{"WireGuard address", in.OperatorWireGuardAddress},
			{"WireGuard private key", in.OperatorWireGuardKey},
			{"SSH private key", strings.TrimSpace(in.OperatorSSHKey)},
		}},
		{title: "Control-plane database", fields: []handoffField{
			{"URL", in.DatabaseURL},
		}},
		{title: "Vault keyring", fields: []handoffField{
			{"EREGION_VAULT_KEY_FILE content", in.VaultKeyring},
		}},
		{title: "Break-glass", fields: []handoffField{
			{"When", "Only when WireGuard or the node is unreachable."},
			{"How", "Use the provider console as root and run `eregion node unlock-ssh` (on builds before that command lands: `/usr/local/bin/eregion node-agent access revert`). It reopens public SSH for its fixed window."},
			{"Public SSH", "Closed after server init; sshd accepts the admin user over WireGuard only."},
			{"Recovery", "The transfer bundle above rebuilds the context on another machine. Without it, use the provider console and this record."},
		}},
	}
}

// handoffGroups is the init record built from the answers and operator identity.
func (r *serverInit) handoffGroups() []handoffGroup {
	return handoffGroups(handoffRecordInput{
		NodeName: r.answers.Node, Context: r.answers.Context, Hostname: r.answers.Hostname,
		PublicAddress: r.answers.Host, Platform: r.platform.String(), SSHHostKey: r.hostKey,
		WireGuardEndpoint:    net.JoinHostPort(r.answers.Host, strconv.Itoa(access.ListenPort)),
		WireGuardNodeAddress: access.NodeAddress, WireGuardNodePublicKey: r.pending.NodePublicKey,
		AdminUser: r.answers.AdminUser, SudoPassword: r.identity.sudoPassword,
		OperatorWireGuardAddress: access.OperatorAddress, OperatorWireGuardKey: r.identity.wg.Encode(),
		OperatorSSHKey: strings.TrimSpace(r.identity.sshKeyPEM),
		DatabaseURL:    r.answers.DatabaseURL, VaultKeyring: r.vaultKeyring,
	})
}
