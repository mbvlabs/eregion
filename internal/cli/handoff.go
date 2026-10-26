package cli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/bundle"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
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

// secretsHandoff is the record server init hands to the operator: eregion keeps
// only the operator's SSH and WireGuard keys in the local context, so this is
// the copy that survives losing this machine.
type secretsHandoff struct {
	node       string
	groups     []handoffGroup
	saved      string
	passphrase string
	bundle     []byte
}

// seal encrypts the record into the transfer bundle with a new passphrase,
// as vaire does; eregion login takes the bundle and the passphrase.
func (h *secretsHandoff) seal() error {
	pass, err := bundle.NewPassphrase()
	if err != nil {
		return err
	}
	sealed, err := bundle.Encrypt([]byte(h.body()), pass)
	if err != nil {
		return err
	}
	h.passphrase, h.bundle = pass, sealed
	return nil
}

// record is every value, then the transfer bundle and its passphrase.
func (h *secretsHandoff) record() string {
	return h.body() + "\n## Transfer bundle\n" +
		"On another machine run eregion login and paste this block.\n" +
		"Passphrase: " + h.passphrase + "\n" + string(h.bundle)
}

func (h *secretsHandoff) body() string {
	var b strings.Builder
	b.WriteString("eregion secrets handoff for main node " + h.node + "\n")
	for _, g := range h.groups {
		b.WriteString("\n## " + g.title + "\n")
		for _, f := range g.fields {
			if f.value != "" {
				b.WriteString(f.label + ": " + f.value + "\n")
			}
		}
	}
	return b.String()
}

// show is the handoff gate: copy, save or show the record, then type
// 'stored'. Like vaire, nothing is written unless the operator saves it.
func (h *secretsHandoff) show(w io.Writer) error {
	resume := ui.Suspend()
	defer resume()
	_, _ = fmt.Fprintln(w, "\n"+ui.Header("Secrets handoff", 72))
	_, _ = fmt.Fprintln(w, "The main node's record: the operator's keys, the database URL and the server's identity.")
	_, _ = fmt.Fprintln(w, "eregion doesn't show it again. Store it in your password manager.")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, ui.Dim("Bundle passphrase")+"  "+h.passphrase)
	_, _ = fmt.Fprintln(w, ui.Note("To log in from another machine: eregion login, paste the transfer bundle, then this passphrase."))
	_, _ = fmt.Fprintln(w)
	for {
		options := [][2]string{{"Copy everything to the clipboard", "copy"}}
		if h.saved == "" {
			options = append(options, [2]string{"Save everything to a file", "save"})
		}
		options = append(options, [2]string{"Show everything on screen", "print"}, [2]string{"Done", "done"})
		choice, err := ui.Choose("Secrets handoff", "", options)
		if err != nil {
			return err
		}
		if choice == "done" {
			break
		}
		switch choice {
		case "copy":
			_, _ = fmt.Fprint(w, osc52(h.record()))
			_, _ = fmt.Fprintln(w, ui.Note("Copied to your clipboard. Nothing pasted? Pick Show everything on screen."))
		case "print":
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprint(w, h.record())
			_, _ = fmt.Fprintln(w)
		case "save":
			if err := h.save(); err != nil {
				_, _ = fmt.Fprintln(w, ui.Note(err.Error()))
				continue
			}
			_, _ = fmt.Fprintln(w, ui.Note("Saved everything to "+h.saved+" (plain text, readable only by you)."))
		}
	}
	if err := ui.TypeWord("Type 'stored' once the record is in your password manager", "", "stored"); err != nil {
		if errors.Is(err, ui.ErrAborted) {
			return contract.NewFault(contract.InputRequired, "the secrets handoff was not confirmed as stored", "The server is set up and the context is saved locally.")
		}
		return err
	}
	if clear, err := ui.YesNoDefault("Clear the screen now?", "", true); err == nil && clear {
		_, _ = fmt.Fprint(w, clearScreen)
	}
	return nil
}

// save writes the record to a path relative to the current directory, owner
// only, and never overwrites a file.
func (h *secretsHandoff) save() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path, err := ui.Input("Save everything to (relative to "+cwd+")", "", "./"+h.node+"-secrets.txt")
	if err != nil {
		return err
	}
	path = expandHome(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the operator chose this path.
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New(path + " already exists; pick another path")
		}
		return err
	}
	if _, err := file.WriteString(h.record()); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	h.saved = path
	return nil
}

// osc52 asks the terminal to put s on the system clipboard; it works over SSH.
func osc52(s string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\a"
}
