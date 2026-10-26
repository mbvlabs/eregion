package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// loginSpecs is `eregion login`: take the secrets record from server init on
// another machine (paste it, or --file) and turn it into a working context.
func (a *app) loginSpecs() []spec {
	meta := readMeta("Log in to a main node with its secrets record",
		[]string{"Use the secrets record server init handed off to operate the node from this machine."},
		[]string{"eregion login", "eregion login --file main-secrets.txt --json"})
	meta.Mutating, meta.Prompts, meta.Remote = true, true, true
	meta.DryRunReason = "Login only writes the local context store and identity files."
	meta.InputFlags = []string{"file"}
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed)
	return []spec{{
		use:  "login",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("file", "", "Secrets record file; otherwise it is read from standard input")
		},
		run: a.login,
	}}
}

// handoffRecord is the parsed secrets record: section title → label → value.
type handoffRecord map[string]map[string]string

func parseHandoffRecord(text string) handoffRecord {
	record := handoffRecord{}
	section := ""
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		if title, ok := strings.CutPrefix(line, "## "); ok {
			section = strings.TrimSpace(title)
			record[section] = map[string]string{}
			continue
		}
		label, value, ok := strings.Cut(line, ":")
		if section == "" || !ok || strings.HasPrefix(line, " ") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			var block []string
			for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" && !strings.HasPrefix(lines[i+1], "## ") {
				i++
				block = append(block, strings.TrimRight(lines[i], " \t"))
			}
			value = strings.Join(block, "\n")
		}
		record[section][strings.TrimSpace(label)] = value
	}
	return record
}

// field finds label in the first section whose title starts with prefix.
func (h handoffRecord) field(prefix, label string) string {
	for title, fields := range h {
		if strings.HasPrefix(title, prefix) {
			return fields[label]
		}
	}
	return ""
}

func (a *app) login(cmd *cobra.Command, _ []string) (result, error) {
	path, _ := cmd.Flags().GetString("file")
	var raw []byte
	var err error
	interactive := path == "" && term.IsTerminal(int(os.Stdin.Fd())) //nolint:gosec // G115: file descriptors fit in int.
	switch {
	case path != "":
		raw, err = os.ReadFile(expandHome(path)) //nolint:gosec // G304: the operator's record.
	case interactive:
		fmt.Fprintln(cmd.ErrOrStderr(), "\n"+ui.Header("eregion login", 72))
		fmt.Fprintln(cmd.ErrOrStderr(), "Paste the secrets record from eregion server init, then press Ctrl-D.")
		raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	default:
		raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	}
	if err != nil {
		return result{}, contract.NewFault(contract.InputRequired, "could not read the secrets record", err.Error())
	}
	rec := parseHandoffRecord(string(raw))
	get := func(section, label string) string { return rec.field(section, label) }
	name := get("Control plane API", "Context")
	token := get("Control plane API", "Operator token")
	sshKey := get("Operator", "SSH private key")
	wgKey := get("Operator", "WireGuard private key")
	required := map[string]string{
		"context": name, "operator token": token, "SSH private key": sshKey, "WireGuard private key": wgKey,
		"WireGuard endpoint":   get("Main node", "WireGuard endpoint"),
		"WireGuard public key": get("Main node", "WireGuard public key"),
		"SSH host key":         get("Main node", "SSH host key"),
	}
	for label, value := range required {
		if value == "" {
			return result{}, contract.NewFault(contract.ValidationFailed, "the secrets record has no "+label, "Paste the whole record eregion server init showed.")
		}
	}
	for _, v := range []string{token, sshKey, wgKey, get("Operator", "Sudo password")} {
		a.secrets.Add(v)
	}
	apiURL := strings.TrimSpace(strings.SplitN(get("Control plane API", "URL"), " ", 2)[0])

	dir, err := identityDir(name)
	if err != nil {
		return result{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result{}, err
	}
	sshPath, wgPath := filepath.Join(dir, "ssh_ed25519"), filepath.Join(dir, "wireguard.key")
	for path, content := range map[string]string{sshPath: sshKey + "\n", wgPath: wgKey + "\n", filepath.Join(dir, "sudo-password"): get("Operator", "Sudo password") + "\n"} {
		_ = os.Remove(path)
		if err := writePrivate(path, []byte(content)); err != nil {
			return result{}, err
		}
	}
	if _, err := transport.ReadWireGuardKey(wgPath); err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, "the record's WireGuard key is not usable", err.Error())
	}
	knownHosts, err := knownHostsPath()
	if err != nil {
		return result{}, err
	}
	route := &host.ContextSSH{
		Address: get("Main node", "WireGuard address") + ":22", User: get("Operator", "Admin user"),
		Identity: sshPath, KnownHosts: knownHosts,
		WireGuard: &host.ContextWireGuard{
			PrivateKeyFile: wgPath, Address: get("Operator", "WireGuard address"),
			NodeAddress: get("Main node", "WireGuard address"), NodePublicKey: get("Main node", "WireGuard public key"),
			Endpoint: get("Main node", "WireGuard endpoint"),
		},
	}
	// Pin the host key seen through the tunnel when it matches the record.
	if !hostKeyPinned(knownHosts, route.Address) {
		key, err := probeOverWireGuard(cmd.Context(), route)
		if err != nil {
			return result{}, contract.NewFault(contract.Unavailable, "the node does not answer over WireGuard", err.Error())
		}
		want := get("Main node", "SSH host key")
		if got := key.Type() + " " + ssh.FingerprintSHA256(key); got != want {
			return result{}, contract.NewFault(contract.PreconditionFailed, "the node's host key "+got+" does not match the record", "Do not continue until the server is checked.")
		}
		if err := pinHostKey(knownHosts, route.Address, key); err != nil {
			return result{}, err
		}
	}
	store, err := host.LoadContextStore("")
	if err != nil {
		return result{}, err
	}
	if _, err := store.Login(name, apiURL, token, host.KnownScopes()); err != nil {
		return result{}, err
	}
	if err := store.SetSSH(name, route); err != nil {
		return result{}, err
	}
	client, err := a.contextClient(cmd)
	if err == nil {
		err = client.Ready(cmd.Context())
	}
	if err != nil {
		return result{}, err
	}
	data := map[string]string{"context": name, "api": apiURL, "route": route.User + "@" + route.Address + " over WireGuard"}
	if interactive {
		fmt.Fprintln(cmd.ErrOrStderr(), ui.Card("Logged in to "+name, [][2]string{{"context", name + " (current)"}, {"route", data["route"]}}, nil))
		return result{data: data, streamed: true}, nil
	}
	return result{data: data, plain: "logged in to " + name}, nil
}

// probeOverWireGuard reads the node's SSH host key through the tunnel.
func probeOverWireGuard(ctx context.Context, route *host.ContextSSH) (ssh.PublicKey, error) {
	dial, err := wireGuardDial(route.WireGuard)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", route.Address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var key ssh.PublicKey
	_, _, _, _ = ssh.NewClientConn(conn, route.Address, &ssh.ClientConfig{
		User: "probe",
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return fmt.Errorf("probed")
		},
	})
	if key == nil {
		return nil, fmt.Errorf("no host key from %s", route.Address)
	}
	return key, nil
}
