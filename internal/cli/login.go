package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/bundle"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// loginSpecs is `eregion login`, as vaire login: decrypt the transfer bundle
// from server init's secrets handoff with its passphrase and turn it into a
// working context on this machine.
func (a *app) loginSpecs() []spec {
	meta := readMeta("Log in to a main node with its transfer bundle",
		[]string{"Paste the transfer bundle from server init's secrets handoff (or the whole copied handoff), then its passphrase."},
		[]string{"eregion login", "eregion login --file main-secrets.md --passphrase-file pass.txt --json"})
	meta.Mutating, meta.Prompts, meta.Remote = true, true, true
	meta.DryRunReason = "Login only writes the local context store and identity files."
	meta.InputFlags = []string{"file", "passphrase-file", "replace"}
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.Conflict)
	return []spec{{
		use:  "login",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("file", "", "File with the transfer bundle (or the whole handoff); otherwise it is read from standard input")
			cmd.Flags().String("passphrase-file", "", "File whose first line is the bundle passphrase; asked for in a terminal")
			cmd.Flags().String("replace", "", "Replace this existing context; must name the context in the bundle exactly")
		},
		run: a.login,
	}}
}

// loginSeams replace the network probe and readiness check in tests. The zero
// value uses the real WireGuard probe and node RPC.
type loginSeams struct {
	probe func(context.Context, *host.ContextSSH, string) (ssh.PublicKey, error)
	ready func(context.Context, *host.ContextSSH) error
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
		item, ok := strings.CutPrefix(line, "- ")
		if section == "" || !ok {
			continue
		}
		label, value, ok := strings.Cut(item, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			// A multi-line value is the fenced block that follows.
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j < len(lines) && strings.TrimSpace(lines[j]) == "```" {
				var block []string
				for j++; j < len(lines) && strings.TrimSpace(lines[j]) != "```"; j++ {
					block = append(block, strings.TrimRight(lines[j], " \t"))
				}
				value, i = strings.Join(block, "\n"), j
			}
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
	passFile, _ := cmd.Flags().GetString("passphrase-file")
	interactive := humanAtTerminal(cmd)
	var armored []byte
	var err error
	if path != "" {
		var raw []byte
		if raw, err = os.ReadFile(expandHome(path)); err == nil { //nolint:gosec // G304: the operator's bundle.
			armored, err = bundle.Read(bytes.NewReader(raw))
		}
	} else {
		if interactive {
			fmt.Fprintln(cmd.ErrOrStderr(), "\n"+ui.Header("eregion login", 72))
			fmt.Fprintln(cmd.ErrOrStderr(), "Paste the transfer bundle (from "+bundle.Header+" to "+bundle.Footer+"), or the whole copied handoff:")
		}
		armored, err = bundle.Read(cmd.InOrStdin())
	}
	if err != nil {
		return result{}, contract.NewFault(contract.InputRequired, "could not read the transfer bundle", err.Error())
	}
	var pass string
	switch {
	case passFile != "":
		raw, err := os.ReadFile(expandHome(passFile)) //nolint:gosec // G304: the operator's passphrase file.
		if err != nil {
			return result{}, contract.NewFault(contract.InputRequired, "could not read the passphrase file", err.Error())
		}
		pass = strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	case interactive:
		if pass, err = ui.Secret("Bundle passphrase"); err != nil {
			return result{}, err
		}
	default:
		return result{}, contract.NewFault(contract.InputRequired, "--passphrase-file is required without a terminal", "Pass the bundle passphrase in a file.")
	}
	a.secrets.Add(pass)
	plain, err := bundle.Decrypt(armored, pass)
	if err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, err.Error(), "Check the passphrase and the bundle, then retry.")
	}
	rec := parseHandoffRecord(string(plain))
	get := func(section, label string) string { return rec.field(section, label) }
	name := get("Main node", "Context")
	sshKey := get("Operator", "SSH private key")
	wgKey := get("Operator", "WireGuard private key")
	wireGuardAddress := get("Main node", "WireGuard address")
	adminUser := get("Operator", "Admin user")
	required := map[string]string{
		"context": name, "SSH private key": sshKey, "WireGuard private key": wgKey,
		"WireGuard address":    wireGuardAddress,
		"admin user":           adminUser,
		"WireGuard endpoint":   get("Main node", "WireGuard endpoint"),
		"WireGuard public key": get("Main node", "WireGuard public key"),
		"SSH host key":         get("Main node", "SSH host key"),
	}
	for label, value := range required {
		if value == "" {
			return result{}, contract.NewFault(contract.ValidationFailed, "the transfer bundle has no "+label, "Use the bundle from eregion server init's secrets handoff.")
		}
	}
	for _, v := range []string{sshKey, wgKey, get("Operator", "Sudo password")} {
		a.secrets.Add(v)
	}

	if err := host.ValidateContextName(name); err != nil {
		return result{}, err
	}
	if err := requireAgreedContext(cmd, name); err != nil {
		return result{}, err
	}
	store, err := host.LoadContextStore("")
	if err != nil {
		return result{}, err
	}
	replace, _ := cmd.Flags().GetString("replace")
	replace = strings.TrimSpace(replace)
	if replace != "" && replace != name {
		return result{}, contract.NewFault(contract.Conflict, "--replace must name the context in the bundle ("+name+")", "Use --replace "+name+", or omit --replace to keep the existing context.")
	}
	existing, lookupErr := store.Get(name)
	exists := lookupErr == nil
	if lookupErr != nil {
		var fault *contract.Fault
		if !errors.As(lookupErr, &fault) || fault.Code != contract.NotFound {
			return result{}, lookupErr
		}
	}
	if exists && replace != name {
		return result{}, contract.NewFault(contract.Conflict, "context "+name+" already exists", "Use eregion login --replace "+name+" to replace it, or choose another server.")
	}

	dir, err := identityDir(name)
	if err != nil {
		return result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return result{}, err
	}
	// Build and prove the identity in a sibling directory first; the existing
	// context is swapped only after probe and ready both succeed.
	tempDir, err := os.MkdirTemp(filepath.Dir(dir), ".eregion-login-"+name+"-")
	if err != nil {
		return result{}, err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	sshPath, wgPath := filepath.Join(tempDir, "ssh_ed25519"), filepath.Join(tempDir, "wireguard.key")
	for path, content := range map[string]string{sshPath: sshKey + "\n", wgPath: wgKey + "\n", filepath.Join(tempDir, "sudo-password"): get("Operator", "Sudo password") + "\n"} {
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
	knownHostsBefore, hadKnownHosts, err := readOptionalFile(knownHosts)
	if err != nil {
		return result{}, err
	}
	tempKnownHosts := filepath.Join(tempDir, "known_hosts")
	if hadKnownHosts {
		if err := os.WriteFile(tempKnownHosts, knownHostsBefore, 0o600); err != nil {
			return result{}, err
		}
	}
	route := &host.ContextSSH{
		Address: net.JoinHostPort(wireGuardAddress, "22"), User: adminUser,
		Identity: sshPath, KnownHosts: tempKnownHosts,
		WireGuard: &host.ContextWireGuard{
			PrivateKeyFile: wgPath, Address: get("Operator", "WireGuard address"),
			NodeAddress: wireGuardAddress, NodePublicKey: get("Main node", "WireGuard public key"),
			Endpoint: get("Main node", "WireGuard endpoint"),
		},
	}
	// Pin the host key seen through the tunnel when it matches the record.
	if !hostKeyPinned(tempKnownHosts, route.Address) {
		want := get("Main node", "SSH host key")
		keyType, _, _ := strings.Cut(want, " ")
		probe := a.loginSeams.probe
		if probe == nil {
			probe = probeOverWireGuard
		}
		key, err := probe(cmd.Context(), route, keyType)
		if err != nil {
			return result{}, contract.NewFault(contract.Unavailable, "the node does not answer over WireGuard", err.Error())
		}
		if got := key.Type() + " " + ssh.FingerprintSHA256(key); got != want {
			return result{}, contract.NewFault(contract.PreconditionFailed, "the node's host key "+got+" does not match the record", "Do not continue until the server is checked.")
		}
		if err := pinHostKey(tempKnownHosts, route.Address, key); err != nil {
			return result{}, err
		}
	}
	ready := a.loginSeams.ready
	if ready == nil {
		autoUpgrade := boolFlag(cmd, "auto-upgrade-nodes")
		if exists {
			autoUpgrade = autoUpgrade || existing.AutoUpgradeNodes
		}
		stderr := cmd.ErrOrStderr()
		ready = func(ctx context.Context, route *host.ContextSSH) error {
			return readyRoute(ctx, route, autoUpgrade, stderr)
		}
	}
	if err := ready(cmd.Context(), route); err != nil {
		return result{}, err
	}
	mergedKnownHosts, hadTempKnownHosts, err := readOptionalFile(tempKnownHosts)
	if err != nil {
		return result{}, err
	}

	// Swap atomically: the old identity is the backup until the new context,
	// directory and pinned key are all in place.
	backup := dir + ".replace-" + randomHex(6)
	hadOld := false
	if _, statErr := os.Stat(dir); statErr == nil {
		hadOld = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result{}, statErr
	}
	restoreStore := *store
	restoreStore.Contexts = append([]host.Context(nil), store.Contexts...)
	restore := func() {
		_ = os.RemoveAll(dir)
		if hadOld {
			_ = os.Rename(backup, dir)
		}
		_ = restoreStore.Save()
		if hadKnownHosts {
			_ = engine.WriteFileAtomic(knownHosts, knownHostsBefore, 0o600)
		} else {
			_ = os.Remove(knownHosts)
		}
	}
	if hadOld {
		if err := os.Rename(dir, backup); err != nil {
			return result{}, err
		}
	}
	if err := os.Rename(tempDir, dir); err != nil {
		if hadOld {
			_ = os.Rename(backup, dir)
		}
		return result{}, err
	}
	if hadTempKnownHosts {
		if err := engine.WriteFileAtomic(knownHosts, mergedKnownHosts, 0o600); err != nil {
			restore()
			return result{}, err
		}
	}
	savedRoute := *route
	savedRoute.Identity = filepath.Join(dir, "ssh_ed25519")
	savedRoute.KnownHosts = knownHosts
	if route.WireGuard != nil {
		wg := *route.WireGuard
		wg.PrivateKeyFile = filepath.Join(dir, "wireguard.key")
		savedRoute.WireGuard = &wg
	}
	if _, err := store.SaveContext(host.Context{Name: name, SSH: &savedRoute}); err != nil {
		restore()
		return result{}, err
	}
	if hadOld {
		_ = os.RemoveAll(backup)
	}
	data := map[string]string{"context": name, "route": route.User + "@" + route.Address + " over WireGuard"}
	if interactive {
		fmt.Fprintln(cmd.ErrOrStderr(), ui.Card("Logged in to "+name, [][2]string{{"context", name + " (current)"}, {"route", data["route"]}}, nil))
		return result{data: data, streamed: true}, nil
	}
	return result{data: data, plain: "logged in to " + name}, nil
}

// readOptionalFile reads path and reports whether it exists.
func readOptionalFile(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: eregion's own file.
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// readyRoute checks a not-yet-saved route without touching the context store.
func readyRoute(ctx context.Context, route *host.ContextSSH, autoUpgrade bool, stderr io.Writer) error {
	remote, err := routeRemote(route, false)
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "the context's SSH route is not usable", err.Error())
	}
	client := &nodeClient{remote: remote, route: route, operator: route.User, autoUpgrade: autoUpgrade, stderr: stderr}
	return client.Ready(ctx)
}

// probeOverWireGuard reads the node's SSH host key of keyType through the
// tunnel; the node offers several types and the record names one.
func probeOverWireGuard(ctx context.Context, route *host.ContextSSH, keyType string) (ssh.PublicKey, error) {
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
		User:              "probe",
		HostKeyAlgorithms: hostKeyAlgorithms(keyType),
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

// hostKeyAlgorithms are the algorithms that make the server present a key of
// keyType (an ssh-rsa key is signed with rsa-sha2-*).
func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}
