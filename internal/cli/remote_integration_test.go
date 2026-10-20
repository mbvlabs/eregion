package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/sshfixture"
)

func buildRemoteBinaries(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
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
	return cliBinary, nodeBinary
}

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
		"--node-binary", nodeBinary,
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

// TestRemoteBootstrapApplyOverPinnedSSH runs the real root-only apply path
// against the disposable SSH fixture with fake host tools. It skips without
// Docker. It proves bundle stability, the CLI/remote handoff, the journal, and
// an idempotent re-apply; it does not prove package installation, systemd, or a
// fresh-host VM.
func TestRemoteBootstrapApplyOverPinnedSSH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fixture, err := sshfixture.Start(ctx, sshfixture.Options{
		Image: "eregion-ssh-fixture-bootstrap2", Root: true, NodeName: "fixture",
		Files: map[string]string{
			// Installed package/daemon stand-ins: the fixture has no systemd and
			// no package network. They exercise the real Check scripts.
			"/usr/local/bin/dpkg-query": "#!/bin/sh\nprintf 'ii '\n",
			"/usr/local/bin/systemctl":  "#!/bin/sh\ncase \"$1\" in is-enabled) echo enabled ;; is-active) echo active ;; esac\nexit 0\n",
			"/usr/local/bin/docker":     "#!/bin/sh\nexit 0\n",
		},
	})
	if err != nil {
		if errors.Is(err, sshfixture.ErrUnavailable) {
			t.Skipf("the SSH fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close(context.Background()) })

	cliBinary, nodeBinary := buildRemoteBinaries(t, ctx)
	nodeRaw, err := os.ReadFile(nodeBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.InstallFile(ctx, "/usr/local/bin/eregion-node", string(nodeRaw)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "groupadd docker 2>/dev/null || true"); err != nil {
		t.Fatal(err)
	}
	// Match the desired node configuration byte-for-byte so this test does not
	// arm an unrelated access-change rollback window.
	nodeConfig := "{\n  \"node\": \"fixture\",\n  \"state_dir\": \"/var/lib/eregion/node\"\n}\n"
	if _, err := fixture.Exec(ctx, "printf '%s' "+shellPath(nodeConfig)+" > /etc/eregion/node.json && chmod 600 /etc/eregion/node.json"); err != nil {
		t.Fatal(err)
	}

	base := []string{
		"bootstrap", "remote",
		"--ssh-address", fixture.Address,
		"--ssh-user", "root",
		"--ssh-identity", fixture.IdentityFile,
		"--ssh-known-hosts", fixture.KnownHostsFile,
		"--role", "application", "--node", "fixture",
		"--binary", cliBinary, "--node-binary", nodeBinary,
	}
	dry := append(append([]string(nil), base...), "--dry-run", "--json")
	firstPlan := decodeRemoteData[host.Plan](t, runRemoteCLI(t, ctx, cliBinary, dry...))
	secondPlan := decodeRemoteData[host.Plan](t, runRemoteCLI(t, ctx, cliBinary, dry...))
	if firstPlan.ConfigHash != secondPlan.ConfigHash || firstPlan.OperationID != secondPlan.OperationID || firstPlan.Generation != secondPlan.Generation {
		t.Fatalf("dry-runs did not resume: %+v %+v", firstPlan, secondPlan)
	}

	apply := append(append([]string(nil), base...), "--apply", "--json")
	report := decodeRemoteData[host.ApplyReport](t, runRemoteCLI(t, ctx, cliBinary, apply...))
	if report.OperationID != firstPlan.OperationID || report.Generation != firstPlan.Generation {
		t.Fatalf("apply did not use the planned generation: plan=%+v report=%+v", firstPlan, report)
	}
	if len(report.Steps) == 0 {
		t.Fatal("apply reported no steps")
	}
	executed := 0
	for _, step := range report.Steps {
		if !step.Complete {
			t.Fatalf("step did not complete: %+v", step)
		}
		if step.Executed {
			executed++
		}
	}
	if executed == 0 {
		t.Fatal("the first apply executed no step")
	}

	// A completed re-apply is the next generation, but every effect is
	// acknowledged from the journal instead of repeated.
	reapply := decodeRemoteData[host.ApplyReport](t, runRemoteCLI(t, ctx, cliBinary, apply...))
	if reapply.Generation != report.Generation+1 {
		t.Fatalf("re-apply generation: first=%d second=%d", report.Generation, reapply.Generation)
	}
	for _, step := range reapply.Steps {
		if step.Executed {
			t.Fatalf("re-apply repeated %s", step.ID)
		}
	}
}
