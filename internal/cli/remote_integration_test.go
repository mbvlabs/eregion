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

func runRemoteCLI(t *testing.T, ctx context.Context, binary string, args ...string) []byte {
	t.Helper()
	command := exec.CommandContext(ctx, binary, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err != nil {
		t.Fatalf("remote command: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout)
	}
	if strings.Contains(string(stdout), "\x1b") {
		t.Fatal("remote command emitted terminal escapes")
	}
	return stdout
}

func decodeRemoteData[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		t.Fatalf("remote envelope: %v\n%s", err, raw)
	}
	var value T
	if err := json.Unmarshal(envelope.Data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// TestRemoteBootstrapPlanOverPinnedSSH is an optional end-to-end check: it
// builds the real CLI, starts the disposable SSH fixture, and asks the remote
// first-contact path for a plan. It skips cleanly when Docker or the fixture is
// unavailable. Apply is not run: the fixture is an unprivileged node image, not
// a fresh root VM.
func TestRemoteBootstrapPlanOverPinnedSSH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fixture, err := sshfixture.Start(ctx, sshfixture.Options{Image: "eregion-ssh-fixture-bootstrap2"})
	if err != nil {
		if errors.Is(err, sshfixture.ErrUnavailable) {
			t.Skipf("the SSH fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close(context.Background()) })

	dir := t.TempDir()
	cliBinary := filepath.Join(dir, "eregion")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", cliBinary, "./cmd/eregion")
	build.Dir = "../.."
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build eregion: %v\n%s", err, raw)
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

	// A second invocation is a resume, not a new generation. The bundle path,
	// config hash, generation, and operation ID must all be stable.
	second := exec.CommandContext(ctx, cliBinary,
		"bootstrap", "remote",
		"--ssh-address", fixture.Address,
		"--ssh-user", "eregion",
		"--ssh-identity", fixture.IdentityFile,
		"--ssh-known-hosts", fixture.KnownHostsFile,
		"--role", "application",
		"--node", "fixture",
		"--binary", cliBinary,
		"--dry-run", "--json",
	)
	var secondErr bytes.Buffer
	second.Stderr = &secondErr
	secondOut, err := second.Output()
	if err != nil {
		t.Fatalf("second remote plan: %v\nstderr:\n%s\nstdout:\n%s", err, secondErr.String(), secondOut)
	}
	var secondPlan host.Plan
	if err := json.Unmarshal(func() json.RawMessage {
		var envelope struct {
			OK   bool            `json:"ok"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(secondOut, &envelope); err != nil || !envelope.OK {
			t.Fatalf("second remote plan envelope: %v\n%s", err, secondOut)
		}
		return envelope.Data
	}(), &secondPlan); err != nil {
		t.Fatal(err)
	}
	if secondPlan.ConfigHash != plan.ConfigHash || secondPlan.Generation != plan.Generation || secondPlan.OperationID != plan.OperationID {
		t.Fatalf("remote re-run did not resume: first=%+v second=%+v", plan, secondPlan)
	}
}
