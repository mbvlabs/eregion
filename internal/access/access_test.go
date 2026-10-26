package access

import (
	"os/exec"
	"strings"
	"testing"
)

func TestScriptsParse(t *testing.T) {
	setup := Setup{AdminUser: "admin", OperatorSSHKey: "ssh-ed25519 AAAA x", SudoPassword: "it's", OperatorPublicKey: "k", SSHPort: 22, APIPort: 8080}
	for name, env := range map[string]map[string]string{
		"setup": setup.Env(), "lockdown": {"STATE_DIR": StateDir, "TIMEOUT_S": "120"},
		"confirm": {"STATE_DIR": StateDir}, "revert": {"STATE_DIR": StateDir},
	} {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(Program(name, env))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
	}
}

func TestDropInLimitsForwardingToTheAPI(t *testing.T) {
	d := SSHDropIn("admin", 8080)
	for _, want := range []string{"PermitRootLogin no", "AllowUsers admin", "AllowTcpForwarding local", "PermitOpen 127.0.0.1:8080", "PasswordAuthentication no"} {
		if !strings.Contains(d, want+"\n") {
			t.Errorf("drop-in lacks %q", want)
		}
	}
}
