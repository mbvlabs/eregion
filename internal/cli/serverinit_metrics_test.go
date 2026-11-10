package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
)

func TestInitGeneratesTelemetryCredentialAndSendsOnlyItsHash(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	mainDir := prepareInitAccess(t, "main")
	r := &serverInit{
		a: &app{secrets: &secrets.Registry{}}, cmd: &cobra.Command{Use: "init"},
		answers: serverInitAnswers{Context: "main", Node: "main", Hostname: "main.example.com"},
		pending: &host.PendingInit{Server: "main", Phases: map[string]string{}},
	}
	wg, err := transport.GenerateWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	r.identity = &operatorIdentity{wg: wg, sshKeyPEM: "ssh-ed25519 test", sudoPassword: "sudo"}
	backend := newMetricsAccessBackend(false)
	r.metricsClient = backend
	if err := r.ensureTelemetryCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	gotHash, gotConfirm := backend.hash, backend.confirm
	backend.mu.Unlock()
	if gotConfirm != "main/metrics" || !telemetry.ValidHash(gotHash) {
		t.Fatalf("rotate received hash=%q confirm=%q", gotHash, gotConfirm)
	}
	credential, err := loadMetricsCredential(mainDir)
	if err != nil || credential == nil || !telemetry.VerifyPassword(gotHash, credential.Password) {
		t.Fatalf("local credential = %+v err=%v", credential, err)
	}
	groups := r.handoffGroups()
	found := false
	for _, group := range groups {
		if group.title != "Node metrics" {
			continue
		}
		found = true
		values := map[string]string{}
		for _, field := range group.fields {
			values[field.label] = field.value
		}
		if values["Username"] != telemetry.Username || values["Password"] != credential.Password ||
			values["Metrics URL"] != "https://main.example.com/metrics/node" ||
			values["Discovery URL"] != "https://main.example.com/metrics/targets.json" ||
			strings.Contains(values["Metrics URL"], "10.78.0.1") {
			t.Fatalf("handoff metrics group = %+v", values)
		}
	}
	if !found {
		t.Fatal("the handoff omitted the telemetry credential")
	}
}

func TestInitReusesAnExistingLocalTelemetryCredential(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	mainDir := prepareInitAccess(t, "main")
	existing, err := telemetry.NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveMetricsCredential(mainDir, existing); err != nil {
		t.Fatal(err)
	}
	r := &serverInit{
		a: &app{secrets: &secrets.Registry{}}, cmd: &cobra.Command{Use: "init"},
		answers: serverInitAnswers{Context: "main", Node: "main", Hostname: "main.example.com"},
		pending: &host.PendingInit{Server: "main", Phases: map[string]string{}},
	}
	backend := newMetricsAccessBackend(false)
	r.metricsClient = backend
	if err := r.ensureTelemetryCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	hash := backend.hash
	backend.mu.Unlock()
	if !telemetry.VerifyPassword(hash, existing.Password) {
		t.Fatal("init generated a different password instead of reusing the local credential")
	}
}

func TestLoginRestoresTelemetryCredentialFile(t *testing.T) {
	credential, err := telemetry.NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	record := parseHandoffRecord(strings.Join([]string{
		"## Node metrics",
		"- Username: " + credential.Username,
		"- Password: " + credential.Password,
	}, "\n"))
	dir := t.TempDir()
	password, err := restoreMetricsCredential(dir, record)
	if err != nil {
		t.Fatal(err)
	}
	if password != credential.Password {
		t.Fatalf("password returned for redaction = %q", password)
	}
	raw, err := os.ReadFile(filepath.Join(dir, metricsCredentialFile))
	if err != nil {
		t.Fatal(err)
	}
	var restored telemetry.Credential
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Username != credential.Username || restored.Password != credential.Password {
		t.Fatalf("restored credential = %+v", restored)
	}
	info, err := os.Stat(filepath.Join(dir, metricsCredentialFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %v err=%v", info.Mode().Perm(), err)
	}
}

func TestLoginRecordRestoresTelemetryCredentialShape(t *testing.T) {
	record := parseHandoffRecord(strings.Join([]string{
		"## Node metrics",
		"- Metrics URL: https://main.example.com/metrics/node",
		"- Username: metrics",
		"- Password: abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
	}, "\n"))
	if got := record.field("Node metrics", "Username"); got != telemetry.Username {
		t.Fatalf("username = %q", got)
	}
	if got := record.field("Node metrics", "Password"); len(got) < 32 {
		t.Fatalf("password = %q", got)
	}
}

func prepareInitAccess(t *testing.T, name string) string {
	t.Helper()
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	access := host.WorkspaceAccess{Name: name, SSH: &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/dev/null"}}
	if _, ok := store.AccessByName(name); !ok {
		if _, err := store.PublishUnconfiguredAccess(access); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := store.CredentialDirFor(access)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
