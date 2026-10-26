// Package access hardens a main node and moves operator SSH onto WireGuard,
// the way vaire does: an admin user with the operator's key, wg0 with the
// operator as its peer, ufw, an sshd drop-in, and a commit-confirm lockdown
// that closes public SSH and reopens it unless confirmed in time. The host
// changes are small bash scripts; server init runs setup over public SSH as
// root, and lockdown, confirm and revert run on the node as
// `eregion node-agent access <action>`.
package access

import (
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

//go:embed scripts/*.sh
var scripts embed.FS

// Defaults for the operator network. Platform nodes use 10.77.0.0/24.
const (
	AdminUser       = "admin"
	NodeAddress     = "10.78.0.1"
	OperatorAddress = "10.78.0.100"
	Prefix          = 24
	ListenPort      = 51820
	StateDir        = "/var/lib/eregion/access"
	// LockdownSeconds is the revert window, as in vaire.
	LockdownSeconds = 120
)

// Script returns an embedded script by name, without the .sh suffix.
func Script(name string) string {
	raw, err := scripts.ReadFile("scripts/" + name + ".sh")
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// Setup is everything the setup script needs.
type Setup struct {
	AdminUser         string
	OperatorSSHKey    string // authorized_keys line
	SudoPassword      string
	OperatorPublicKey string // WireGuard, base64
	SSHPort           int
	APIPort           int
}

// SSHDropIn is the sshd drop-in. It sorts first so it wins over provider
// drop-ins. Forwarding is limited to the loopback API, which the CLI reaches
// through SSH inside WireGuard.
func SSHDropIn(admin string, apiPort int) string {
	return strings.Join([]string{
		"# managed by eregion: SSH is reachable only over WireGuard once locked down (ufw)",
		"PermitRootLogin no",
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PermitEmptyPasswords no",
		"X11Forwarding no",
		"AllowTcpForwarding local",
		"PermitOpen 127.0.0.1:" + strconv.Itoa(apiPort),
		"AllowAgentForwarding no",
		"AllowUsers " + admin,
		"MaxAuthTries 3",
		"MaxSessions 10",
		"LoginGraceTime 30",
		"ClientAliveInterval 300",
		"ClientAliveCountMax 2",
		"",
	}, "\n")
}

// Env is the setup script environment. It holds the sudo password, so it is
// passed on the script's stdin, never on a command line.
func (s Setup) Env() map[string]string {
	return map[string]string{
		"ADMIN_USER":          s.AdminUser,
		"AUTHORIZED_KEYS":     strings.TrimSpace(s.OperatorSSHKey) + "\n",
		"SUDO_PASSWORD":       s.SudoPassword,
		"SUDOERS":             "# managed by eregion\n" + s.AdminUser + " ALL=(root) NOPASSWD: /usr/local/bin/eregion node-agent access *\n",
		"WG_ADDRESS":          NodeAddress + "/" + strconv.Itoa(Prefix),
		"WG_PORT":             strconv.Itoa(ListenPort),
		"OPERATOR_PUBLIC_KEY": s.OperatorPublicKey,
		"OPERATOR_ADDRESS":    OperatorAddress,
		"SSH_PORT":            strconv.Itoa(s.SSHPort),
		"SSHD_DROPIN":         SSHDropIn(s.AdminUser, s.APIPort),
		"STATE_DIR":           StateDir,
	}
}

// Program renders a script with its environment as one bash program for
// `bash -s`: the values are exported from single-quoted literals first.
func Program(name string, env map[string]string) string {
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export " + k + "=" + quote(env[k]) + "\n")
	}
	b.WriteString(Script(name))
	return b.String()
}

func quote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// PublicKey reads the node's WireGuard public key from setup output.
func PublicKey(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "WG_PUBLIC_KEY="); ok && v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("setup did not report the node's WireGuard key")
}

// Run is `eregion node-agent access <lockdown|confirm|revert>` on the node.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: eregion node-agent access <lockdown|confirm|revert>")
	}
	env := map[string]string{"STATE_DIR": StateDir}
	switch args[0] {
	case "lockdown":
		env["TIMEOUT_S"] = strconv.Itoa(LockdownSeconds)
	case "confirm", "revert":
	default:
		return fmt.Errorf("unknown access action %q", args[0])
	}
	cmd := exec.Command("/bin/bash", "-s") //nolint:gosec // G204: an embedded script.
	cmd.Stdin = strings.NewReader(Program(args[0], env))
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = append(os.Environ(), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	return cmd.Run()
}
