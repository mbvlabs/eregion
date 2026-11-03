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
		"unlock": {"STATE_DIR": StateDir, "MINUTES": "15"}, "relock": {"STATE_DIR": StateDir},
	} {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(Program(name, env))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
	}
}

func TestSetupAddsTheAdminToTheJournalGroup(t *testing.T) {
	setup := Setup{AdminUser: "admin", OperatorSSHKey: "ssh-ed25519 AAAA x", SudoPassword: "it's", OperatorPublicKey: "k", SSHPort: 22, APIPort: 8080}
	if !strings.Contains(Program("setup", setup.Env()), "usermod --shell /bin/bash --append --groups sudo,systemd-journal \"${ADMIN_USER}\"") {
		t.Fatal("setup does not add the admin user to the systemd-journal group")
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
