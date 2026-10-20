package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/sshfixture"
)

// TestRemoteBootstrapPlanOverPinnedSSH is an optional end-to-end check: it
// builds the real CLI, starts the disposable SSH fixture, and asks the remote
// first-contact path for a plan. It skips cleanly when Docker or the fixture is
// unavailable. Apply is not run: the fixture is an unprivileged node image, not
// a fresh root VM.
func TestRemoteBootstrapPlanOverPinnedSSH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fixture, err := sshfixture.Start(ctx, sshfixture.Options{})
	if err != nil {
		if errors.Is(err, sshfixture.ErrUnavailable) {
			t.Skipf("the SSH fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close(context.Background()) })

	dir := t.TempDir()
	cliBinary := filepath.Join(dir, "eregion")
	nodeBinary := filepath.Join(dir, "eregion-node")
	for target, output := range map[string]string{"./cmd/eregion": cliBinary, "./cmd/eregion-node": nodeBinary} {
		command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", output, target)
		command.Dir = "../.."
		if raw, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", target, err, raw)
		}
	}
	command := exec.CommandContext(ctx, cliBinary,
		"bootstrap", "remote",
		"--ssh-address", fixture.Address,
		"--ssh-user", "eregion",
		"--ssh-identity", fixture.IdentityFile,
		"--ssh-known-hosts", fixture.KnownHostsFile,
		"--role", "application",
		"--node", "fixture",
		"--binary", cliBinary,
		"--node-binary", nodeBinary,
		"--dry-run", "--json",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err != nil {
		t.Fatalf("remote plan: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout)
	}
	if strings.Contains(string(stdout), "\x1b") {
		t.Fatal("remote plan emitted terminal escapes")
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil || !envelope.OK {
		t.Fatalf("remote plan envelope: %v\n%s", err, stdout)
	}
	var plan host.Plan
	if err := json.Unmarshal(envelope.Data, &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.Supported || plan.Role != host.RoleApplication {
		t.Fatalf("unexpected remote plan: %+v", plan)
	}
	if plan.Confirmation.Token == "" || plan.Journal == "" {
		t.Fatalf("remote plan lacks binding identity: %+v", plan)
	}
}
