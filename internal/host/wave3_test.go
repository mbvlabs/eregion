package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestStatusReadsStateAndJournalWithoutApplyInputs(t *testing.T) {
	cfg := fixture(t, RoleMain)
	// Status must not require these apply-only inputs.
	cfg.DatabaseURL = ""
	cfg.BinaryPath = ""

	state := State{
		Schema: stateSchema, Node: "main", Role: RoleMain, ConfigHash: "fixture-hash",
		Generation: 3, OperationID: operationID("main", "fixture-hash", 3), Active: false,
		StartedAt: time.Unix(1800000000, 0), CompletedAt: time.Unix(1800000100, 0),
	}
	if err := ensureDir(cfg.Layout.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if err := writeReplacingSecret(statePath(cfg), append(raw, '\n'), "", ""); err != nil {
		t.Fatal(err)
	}
	journal := engine.NewJournal("main")
	journal.Tasks[state.OperationID+":host-packages-v1"] = engine.Task{
		OperationID: state.OperationID, Step: "host-packages-v1", Attempt: 3,
		Status: engine.TaskCompleted, AppliedAttempt: 3,
	}
	if err := journal.Save(JournalPath(cfg), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}

	runner, err := NewStatusRunner(cfg, &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	status, err := runner.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Bootstrapped || !status.Complete || status.Active || status.State == nil {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.State.Generation != 3 || len(status.Steps) != 1 || status.Steps[0].ID != "host-packages-v1" {
		t.Fatalf("journal was not reported: %+v", status)
	}
	if status.Journal != JournalPath(cfg) {
		t.Fatalf("journal path: %s", status.Journal)
	}
}

func TestStatusWithoutStateIsNotAnError(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.DatabaseURL, cfg.BinaryPath = "", ""
	runner, err := NewStatusRunner(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := runner.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Bootstrapped || status.Complete || status.State != nil || len(status.Steps) != 0 {
		t.Fatalf("empty status: %+v", status)
	}
}

func TestNodeConfigPreservesOperatorWireGuardHelper(t *testing.T) {
	cfg := fixture(t, RoleApplication)
	path := filepath.Join(cfg.Layout.ConfigDir, "node.json")
	put(t, path, `{
  "node": "old-name",
  "state_dir": "/old/state",
  "wireguard_apply": ["/usr/local/sbin/eregion-wg-apply"],
  "task_command": ["/usr/local/bin/eregion", "node-agent", "task"]
}`, 0o600)
	step := &nodeConfigStep{cfg: cfg}
	raw, err := step.config()
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if values["node"] != cfg.Node || values["state_dir"] != cfg.Layout.NodeStateDir {
		t.Fatalf("bootstrap-owned keys were not updated: %s", raw)
	}
	helper, ok := values["wireguard_apply"].([]any)
	if !ok || len(helper) != 1 || helper[0] != "/usr/local/sbin/eregion-wg-apply" {
		t.Fatalf("wireguard_apply was not preserved: %s", raw)
	}
	if _, ok := values["task_command"]; !ok {
		t.Fatalf("task_command was not preserved: %s", raw)
	}
}

func TestControlPlaneEnvMergesOperatorLines(t *testing.T) {
	cfg := fixture(t, RoleMain)
	credentials, err := LoadCredentials(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	step := &credentialsStep{cfg: cfg, credentials: credentials}
	envPath := filepath.Join(cfg.Layout.ConfigDir, "control-plane.env")
	put(t, envPath, `EREGION_DATABASE_URL="stale"
EREGION_NODE_TRANSPORT="ssh"
OPERATOR_TUNING="keep-me"
# operator comment
`, 0o600)

	merged := string(step.env())
	for _, want := range []string{`OPERATOR_TUNING="keep-me"`, "# operator comment", `EREGION_NODE_NAME="` + cfg.Node + `"`, `EREGION_NODE_SSH_IDENTITY="` + filepath.Join(cfg.Layout.ConfigDir, "control-plane-key") + `"`, `EREGION_NODE_WG_KEY_DIR="` + filepath.Join(cfg.Layout.DataDir, "control-plane", "wireguard") + `"`} {
		if !strings.Contains(merged, want) {
			t.Fatalf("merged environment lacks %q:\n%s", want, merged)
		}
	}
	if strings.Contains(merged, `EREGION_DATABASE_URL="stale"`) || strings.Contains(merged, `EREGION_NODE_TRANSPORT="ssh"`) {
		t.Fatalf("managed keys were not replaced:\n%s", merged)
	}
	preserved := strings.Join(step.envPreserved(), ",")
	if !strings.Contains(preserved, "OPERATOR_TUNING") {
		t.Fatalf("preserved keys were not reported: %s", preserved)
	}

	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("OPERATOR_TUNING")) {
		t.Fatalf("apply deleted an operator line: %s", raw)
	}
	if check, err := step.Check(context.Background(), engine.Runtime{}); err != nil || !check.Complete {
		t.Fatalf("operator lines made check fail: %+v %v", check, err)
	}
}

func TestReconcileExpiredCommitWithoutWatchdog(t *testing.T) {
	cfg := fixture(t, RoleApplication)
	operation := "op_reboot"
	dir := commitPath(cfg, operation)
	if err := ensureDir(filepath.Join(dir, "backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("new-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backup", "snapshot"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := commitManifest{
		Schema: commitSchema, OperationID: operation, Deadline: time.Unix(1800000000, 0).Add(-time.Minute),
		Files: []commitFile{{Path: target, Exists: true, Backup: "backup/snapshot", Mode: 0o600, UID: -1, GID: -1}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := commitRunner(t, cfg)
	if err := runner.reconcileCommits(operation); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(target)
	if string(restored) != "original" {
		t.Fatalf("expired snapshot was not restored: %q", restored)
	}
	if _, err := os.Stat(commitRolledBackPath(cfg, operation)); err != nil {
		t.Fatal("rollback marker missing", err)
	}

	// A future unconfirmed window from another operation blocks mutation.
	other := commitPath(cfg, "op_other")
	if err := ensureDir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(commitManifest{Schema: commitSchema, OperationID: "op_other", Deadline: time.Unix(1800000000, 0).Add(time.Minute)})
	if err := os.WriteFile(filepath.Join(other, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.reconcileCommits(operation); err == nil {
		t.Fatal("a second rollback window was allowed")
	}
}

func TestCredentialAddStoresActionGrants(t *testing.T) {
	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	token := strings.Repeat("o", 43)
	if err := WriteAuthFile(authPath, []api.Principal{principal(contract.Actor{Kind: "operator", Subject: "operator"}, token, KnownScopes())}, "", ""); err != nil {
		t.Fatal(err)
	}
	grant, err := ParseActionGrant("platform.deploy:env/api/production")
	if err != nil {
		t.Fatal(err)
	}
	request := CredentialAddRequest{
		AuthFile: authPath, Actor: contract.Actor{Kind: "ci", Subject: "ci-deploy"},
		Scopes: []string{"operations:submit"}, Grants: []api.ActionGrant{grant},
		TokenOut: filepath.Join(root, "ci.token"),
	}
	now := time.Unix(1800000000, 0)
	plan, err := request.Confirmation(now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := request.Apply(plan.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	principals, err := ReadAuthFile(authPath)
	if err != nil || len(principals) != 2 {
		t.Fatalf("principals: %+v %v", principals, err)
	}
	if len(principals[1].ActionGrants) != 1 || principals[1].ActionGrants[0] != grant {
		t.Fatalf("grant was not stored: %+v", principals[1].ActionGrants)
	}
	// The stored file must remain acceptable to the server to which it will be
	// copied; NewAuth applies the same grant validation as authorization.
	auth, err := api.NewAuth(principals)
	if err != nil {
		t.Fatalf("server rejected the created credential: %v", err)
	}
	encoded, _ := json.Marshal(result)
	raw, _ := os.ReadFile(request.TokenOut)
	if bytes.Contains(encoded, bytes.TrimSpace(raw)) {
		t.Fatal("credential result leaked the token")
	}
	// Exercise the real API authorization path with the created token: the
	// granted target is accepted for a dry-run, another environment is not.
	apiToken := strings.TrimSpace(string(raw))
	handler := api.NewHandler(auth, nil, nil)
	releaseID := "rel_" + strings.Repeat("a", 32)
	for _, testCase := range []struct {
		target string
		status int
	}{
		{"env/api/production", http.StatusOK},
		{"env/api/staging", http.StatusForbidden},
	} {
		body := fmt.Sprintf(`{"kind":"platform.deploy","target":%q,"dry_run":true,"input":{"release_id":%q}}`, testCase.target, releaseID)
		requestHTTP := httptest.NewRequest(http.MethodPost, "/v1/operations", strings.NewReader(body))
		requestHTTP.Header.Set("Authorization", "Bearer "+apiToken)
		requestHTTP.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, requestHTTP)
		if recorder.Code != testCase.status {
			t.Fatalf("authorization for %s: got %d want %d: %s", testCase.target, recorder.Code, testCase.status, recorder.Body.String())
		}
	}
	for _, bad := range []string{"platform.deploy", "platform.deploy:", ":env/api/production", "platform.deploy:env/*/production", "platform.*:env/api/production"} {
		if _, err := ParseActionGrant(bad); err == nil {
			t.Fatalf("accepted invalid action grant %q", bad)
		}
	}
	// A duplicate grant is rejected by the same server validation.
	if err := validateActionGrants([]api.ActionGrant{grant, grant}); err == nil {
		t.Fatal("accepted duplicate action grants")
	}
}

func TestWatchdogScriptHonorsMarkersAndDisarms(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX shell")
	}
	runScript := func(t *testing.T, path string) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "sh", path)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("watchdog: %v\n%s", err, out)
		}
		return err
	}

	scenario := func(t *testing.T) (string, string, string) {
		t.Helper()
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		backupDir := filepath.Join(dir, "backup")
		if err := os.MkdirAll(backupDir, 0o700); err != nil {
			t.Fatal(err)
		}
		backup := filepath.Join(backupDir, "snapshot")
		if err := os.WriteFile(backup, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("new-config"), 0o600); err != nil {
			t.Fatal(err)
		}
		scriptPath := filepath.Join(dir, "watchdog.sh")
		return dir, target, scriptPath
	}
	manifest := func(dir string, deadline time.Time) commitManifest {
		return commitManifest{
			Schema: commitSchema, OperationID: "op_watchdog", Deadline: deadline,
			Files: []commitFile{{Path: filepath.Join(dir, "target"), Exists: true, Backup: "backup/snapshot", Mode: 0o600, UID: -1, GID: -1}},
		}
	}
	writeScript := func(t *testing.T, dir, scriptPath string, deadline time.Time) {
		t.Helper()
		if err := os.WriteFile(scriptPath, []byte(commitScript(dir, manifest(dir, deadline))), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("rolls back at the deadline", func(t *testing.T) {
		dir, target, scriptPath := scenario(t)
		writeScript(t, dir, scriptPath, time.Now().Add(1*time.Second))
		runScript(t, scriptPath)
		raw, _ := os.ReadFile(target)
		if string(raw) != "original" {
			t.Fatalf("not restored: %q", raw)
		}
		if _, err := os.Stat(filepath.Join(dir, "rolled-back")); err != nil {
			t.Fatal("no rolled-back marker", err)
		}
	})

	t.Run("confirmation disarms", func(t *testing.T) {
		dir, target, scriptPath := scenario(t)
		writeScript(t, dir, scriptPath, time.Now().Add(1*time.Second))
		if err := os.WriteFile(filepath.Join(dir, "confirmed"), []byte("ok\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runScript(t, scriptPath)
		raw, _ := os.ReadFile(target)
		if string(raw) != "new-config" {
			t.Fatalf("confirmed configuration was restored: %q", raw)
		}
	})

	t.Run("rolled-back marker is terminal", func(t *testing.T) {
		dir, target, scriptPath := scenario(t)
		writeScript(t, dir, scriptPath, time.Now().Add(1*time.Second))
		if err := os.WriteFile(filepath.Join(dir, "rolled-back"), []byte("done\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runScript(t, scriptPath)
		raw, _ := os.ReadFile(target)
		if string(raw) != "new-config" {
			t.Fatalf("a rolled-back watchdog overwrote newer configuration: %q", raw)
		}
	})

	t.Run("explicit rollback disarms an old watchdog", func(t *testing.T) {
		cfg := fixture(t, RoleApplication)
		commitDir := commitPath(cfg, "op_watchdog")
		if err := ensureDir(filepath.Join(commitDir, "backup"), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(commitDir, "target")
		if err := os.WriteFile(filepath.Join(commitDir, "backup", "snapshot"), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("new-config"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := manifest(commitDir, time.Now().Add(1*time.Second))
		m.Files[0].Path = target
		raw, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(commitDir, "manifest.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		runner := commitRunner(t, cfg)
		if err := runner.rollbackCommit("op_watchdog"); err != nil {
			t.Fatal(err)
		}
		// A newer configuration now lives at the target. The old script must not
		// restore over it once rolled-back exists.
		if err := os.WriteFile(target, []byte("newer-config"), 0o600); err != nil {
			t.Fatal(err)
		}
		scriptPath := filepath.Join(commitDir, "watchdog.sh")
		if err := os.WriteFile(scriptPath, []byte(commitScript(commitDir, m)), 0o700); err != nil {
			t.Fatal(err)
		}
		runScript(t, scriptPath)
		raw, _ = os.ReadFile(target)
		if string(raw) != "newer-config" {
			t.Fatalf("old watchdog overwrote newer configuration: %q", raw)
		}
	})
}

// commitRunner drives only the commit-confirm file journal; it runs no host
// commands.
func commitRunner(t *testing.T, cfg Config) *Runner {
	t.Helper()
	runner, err := NewConfirmRunner(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.Now = func() time.Time { return time.Unix(1800000000, 0) }
	return runner
}
