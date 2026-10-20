package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
)

func put(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// Every test uses temporary paths and fake commands. No apt, usermod, Docker,
// systemctl, real database migration, or host apply is executed by these tests.
func fixture(t *testing.T, role Role) Config {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig(role, "")
	cfg.Layout = Layout{
		StateDir: filepath.Join(root, "data/bootstrap"), NodeStateDir: filepath.Join(root, "data/node"),
		ConfigDir: filepath.Join(root, "etc"), DataDir: filepath.Join(root, "data"),
		BinDir: filepath.Join(root, "bin"), SystemdDir: filepath.Join(root, "systemd"), RegistryDir: filepath.Join(root, "registry"),
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Layout.User, cfg.Layout.NodeUser = account.Username, account.Username
	cfg.Layout.Group, cfg.Layout.NodeGroup = group.Name, group.Name
	cfg.PlatformPath, cfg.Arch = filepath.Join(root, "os-release"), "amd64"
	put(t, cfg.PlatformPath, "ID=debian\nVERSION_ID=12\n", 0o600)
	cfg.BinaryPath, cfg.NodeBinaryPath = filepath.Join(root, "source-cli"), filepath.Join(root, "source-node")
	put(t, cfg.BinaryPath, "fixture CLI executable", 0o700)
	put(t, cfg.NodeBinaryPath, "fixture node executable", 0o700)
	cfg.DatabaseURL = "postgres://test:private-password@127.0.0.1/test?sslmode=disable"
	cfg.OperatorTokenOut = filepath.Join(root, "exports/operator.token")
	return cfg
}

type fakeExec struct {
	runs      map[string]int
	installed map[string]bool
	fail      string
	scripts   []string
}

func newFakeExec() *fakeExec { return &fakeExec{runs: map[string]int{}, installed: map[string]bool{}} }
func (f *fakeExec) Run(_ context.Context, id, script string, _ map[string]string, _ engine.Reporter) error {
	f.runs[id]++
	f.scripts = append(f.scripts, script)
	if f.fail == id {
		f.fail = ""
		return errors.New("fixture interruption")
	}
	f.installed[id] = true
	return nil
}
func (f *fakeExec) Output(_ context.Context, script string) (string, error) {
	id := ""
	switch {
	case strings.Contains(script, "dpkg-query"):
		id = "host-packages-v1"
	case strings.Contains(script, "getent"):
		id = "host-users-v1"
	case strings.Contains(script, "docker info"):
		id = "host-docker-v1"
	case strings.Contains(script, "--version"):
		return BuildpackVersion, nil
	default:
		return "enabled:ok\nactive:ok", nil
	}
	if f.installed[id] {
		return "fixture:ok", nil
	}
	return "fixture:missing", nil
}

func testRunner(t *testing.T, cfg Config, exec *fakeExec) *Runner {
	t.Helper()
	runner, err := NewRunner(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.Exec, runner.SkipRootCheck = exec, true
	runner.Now = func() time.Time { return time.Unix(1800000000, 0) }
	return runner
}

func TestSupportAndRoleMatrix(t *testing.T) {
	if len(SupportMatrix()) != 8 {
		t.Fatal("matrix must have eight release/architecture pairs")
	}
	for _, entry := range SupportMatrix() {
		raw := fmt.Sprintf("ID=%s\nVERSION_ID=\"%s\"\n", entry.ID, entry.VersionID)
		platform, err := ParsePlatform([]byte(raw), entry.Arch)
		if err != nil || !platform.Supported() {
			t.Fatalf("%+v: %v", entry, err)
		}
	}
	for _, p := range []Platform{{ID: "debian", VersionID: "11", Arch: "amd64"}, {ID: "ubuntu", VersionID: "26.04", Arch: "arm64"}, {ID: "debian", VersionID: "12", Arch: "386"}} {
		if p.Supported() {
			t.Fatalf("unexpected support: %+v", p)
		}
	}
	main, _ := ConfigForRole(RoleMain)
	if !main.ControlPlane || !main.Runtime || !main.Builder || !main.Registry || main.Telemetry {
		t.Fatal(main)
	}
	for _, role := range []Role{"telemetry", "edge", "typo"} {
		if _, err := ConfigForRole(role); err == nil {
			t.Fatal(role)
		}
	}
	if cfg := DefaultConfig(RoleMain, ""); cfg.Node != "local" {
		t.Fatal("main must match the core's local execution node")
	}
}

func TestSSHExecutionIdentityManifest(t *testing.T) {
	for _, role := range []Role{RoleApplication, RoleBuilder} {
		cfg := DefaultConfig(role, "app1")
		placement, _ := cfg.RoleConfig()
		users := &usersStep{cfg: cfg, role: placement}
		var found bool
		for _, account := range users.accounts() {
			if account.User == "eregion-node" {
				found = account.Docker && account.Shell == "/bin/bash" && account.Group == "eregion-node"
			}
		}
		if !found || !strings.Contains(users.applyScript(), "usermod -aG docker eregion-node") || !strings.Contains(users.checkScript(), "id -nG eregion-node") {
			t.Fatal("SSH execution identity lacks Docker membership")
		}
		dirs := (&directoriesStep{cfg: cfg, role: placement}).specs()
		for _, path := range []string{cfg.Layout.DataDir, cfg.Layout.NodeStateDir, users.nodeHome()} {
			found = false
			for _, dir := range dirs {
				if dir.Path == path {
					found = dir.Owner == cfg.Layout.NodeUser && dir.Group == cfg.Layout.NodeGroup
				}
			}
			if !found {
				t.Fatalf("%s is not owned by node execution identity", path)
			}
		}
		for _, dir := range dirs {
			if dir.Path == cfg.Layout.ConfigDir && dir.Group != cfg.Layout.NodeGroup {
				t.Fatal("node cannot traverse config parent")
			}
		}
		f := newFakeExec()
		f.installed["host-users-v1"] = false
		failing := &usersStep{cfg: cfg, role: placement, exec: outputExec{"member:eregion-node:docker:missing"}}
		check, err := failing.Check(context.Background(), engine.Runtime{})
		if err != nil || check.Complete || !strings.Contains(check.Detail, "eregion-node docker membership") {
			t.Fatalf("%+v %v", check, err)
		}
	}
}

type outputExec struct{ output string }

func (o outputExec) Output(context.Context, string) (string, error) { return o.output, nil }
func (o outputExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	return errors.New("unexpected apply")
}

func TestRoleAwareStepsInstallMainStackWithoutTelemetry(t *testing.T) {
	main := fixture(t, RoleMain)
	main.PlatformPath = filepath.Join(t.TempDir(), "os-release")
	put(t, main.PlatformPath, "ID=ubuntu\nVERSION_ID=24.04\n", 0o600)
	runner := testRunner(t, main, newFakeExec())
	steps, err := runner.steps(Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, step := range steps {
		ids[step.ID()] = true
	}
	for _, required := range []string{"host-platform-v1", "host-packages-v1", "host-docker-v1", "host-buildpacks-v1", "host-registry-v1", "host-database-v1", "host-api-service-v1", "host-worker-service-v1"} {
		if !ids[required] {
			t.Fatalf("main role is missing %s: %v", required, ids)
		}
	}
	role, _ := main.RoleConfig()
	if role.Telemetry {
		t.Fatal("main role must not install telemetry")
	}
	application := fixture(t, RoleApplication)
	appRunner := testRunner(t, application, newFakeExec())
	appSteps, err := appRunner.steps(Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range appSteps {
		switch step.ID() {
		case "host-buildpacks-v1", "host-registry-v1", "host-database-v1", "host-api-service-v1", "host-worker-service-v1":
			t.Fatalf("application role must not run %s", step.ID())
		}
		if step.ID() == "host-docker-v1" || step.ID() == "host-binary-v1" || step.ID() == "host-node-config-v1" {
			ids[step.ID()] = true
		}
	}
	if !ids["host-docker-v1"] || !ids["host-binary-v1"] || !ids["host-node-config-v1"] {
		t.Fatal("application role lacks its runtime, node binary, or node config")
	}
}

func TestPlanResumeAndDrift(t *testing.T) {
	ctx := context.Background()
	cfg := fixture(t, RoleApplication)
	exec := newFakeExec()
	runner := testRunner(t, cfg, exec)
	plan, err := runner.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || !plan.Supported || plan.ApplyBlocked != "" {
		t.Fatalf("%+v", plan)
	}
	if _, err := os.Stat(cfg.Layout.StateDir); !os.IsNotExist(err) {
		t.Fatal("plan wrote bootstrap state")
	}
	if len(exec.scripts) != 0 {
		t.Fatal("plan executed mutation")
	}
	if _, err := runner.Apply(ctx, "invalid", false); err == nil {
		t.Fatal("unconfirmed apply")
	}
	if _, err := os.Stat(cfg.Layout.StateDir); !os.IsNotExist(err) {
		t.Fatal("bad confirmation wrote state")
	}
	exec.fail = "host-docker-v1"
	first, err := runner.Apply(ctx, plan.Confirmation.Token, false)
	if err == nil {
		t.Fatal("expected interrupted apply")
	}
	if _, err := os.Stat(JournalPath(cfg)); err != nil {
		t.Fatal("no pre-database journal", err)
	}
	resumedPlan, err := runner.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !resumedPlan.Resumed || resumedPlan.OperationID != first.OperationID {
		t.Fatal("did not preserve operation identity")
	}
	resumed, err := runner.Apply(ctx, resumedPlan.Confirmation.Token, false)
	if err != nil {
		t.Fatal(err)
	}
	if exec.runs["host-packages-v1"] != 1 || exec.runs["host-users-v1"] != 1 || exec.runs["host-docker-v1"] != 2 {
		t.Fatal(exec.runs)
	}
	if resumed.Generation != first.Generation {
		t.Fatal("resumed a new generation")
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.StateDir, "secrets/operator.token")); !os.IsNotExist(err) {
		t.Fatal("node role generated an irrelevant operator credential")
	}
	if err := os.Chmod(cfg.Layout.NodeStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	drift, err := runner.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if drift.Ready || drift.Generation != resumed.Generation+1 {
		t.Fatal("drift not detected")
	}
	if _, err := runner.Apply(ctx, drift.Confirmation.Token, false); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(cfg.Layout.NodeStateDir)
	if info.Mode().Perm() != 0o700 {
		t.Fatal("drift not repaired")
	}
	for _, script := range exec.scripts {
		if strings.Contains(script, "usermod") && !exec.installed["host-packages-v1"] {
			t.Fatal("users precede packages")
		}
	}
}

func TestAccessChangeRequiresCommitAndRollsBack(t *testing.T) {
	ctx := context.Background()
	cfg := fixture(t, RoleApplication)
	// An existing different node binary is an access-changing update.
	dest := filepath.Join(cfg.Layout.BinDir, "eregion-node")
	put(t, dest, "old node binary", 0o755)
	exec := newFakeExec()
	runner := testRunner(t, cfg, exec)
	var scriptPath string
	runner.SpawnWatchdog = func(path string) error { scriptPath = path; return nil }
	plan, err := runner.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.AccessChanges) == 0 {
		t.Fatal("binary update was not reported as an access change")
	}
	report, err := runner.Apply(ctx, plan.Confirmation.Token, false)
	var fault *contract.Fault
	if !errors.As(err, &fault) || fault.Code != contract.ConfirmationRequired {
		t.Fatalf("access-changing apply did not require commit: %v", err)
	}
	if !report.Pending || report.OperationID == "" {
		t.Fatalf("no pending commit identity: %+v", report)
	}
	state, exists, err := loadState(cfg)
	if err != nil || !exists || state.CommitID != report.OperationID || !state.Active {
		t.Fatalf("pending commit not journaled: %+v %v", state, err)
	}
	if scriptPath == "" {
		t.Fatal("watchdog was not armed")
	}
	raw, err := os.ReadFile(scriptPath)
	if err != nil || !bytes.Contains(raw, []byte("confirmed")) || !bytes.Contains(raw, []byte("rolled-back")) {
		t.Fatalf("watchdog script does not implement commit-confirm: %q %v", raw, err)
	}
	if err := runner.rollbackCommit(report.OperationID); err != nil {
		t.Fatal(err)
	}
	restored, _, _, err := fileState(dest)
	if err != nil || string(restored) != "old node binary" {
		t.Fatalf("rollback did not restore the previous binary: %q %v", restored, err)
	}
	if _, err := os.Stat(commitRolledBackPath(cfg, report.OperationID)); err != nil {
		t.Fatal("rollback marker missing", err)
	}
}

func TestCommitConfirmClosesWindow(t *testing.T) {
	ctx := context.Background()
	cfg := fixture(t, RoleApplication)
	put(t, filepath.Join(cfg.Layout.BinDir, "eregion-node"), "old node binary", 0o755)
	exec := newFakeExec()
	runner := testRunner(t, cfg, exec)
	runner.SpawnWatchdog = func(string) error { return nil }
	plan, err := runner.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Apply(ctx, plan.Confirmation.Token, false)
	if err == nil || report.OperationID == "" {
		t.Fatalf("expected pending commit: %v", err)
	}
	confirmed, err := runner.Confirm(ctx, report.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.AlreadyConfirmed || confirmed.OperationID != report.OperationID {
		t.Fatalf("bad confirmation: %+v", confirmed)
	}
	state, _, err := loadState(cfg)
	if err != nil || state.Active || state.CommitID != "" || state.CompletedAt.IsZero() {
		t.Fatalf("commit did not finish state: %+v %v", state, err)
	}
	if _, err := os.Stat(commitConfirmedPath(cfg, report.OperationID)); err != nil {
		t.Fatal("confirmation marker missing", err)
	}
	if again, err := runner.Confirm(ctx, report.OperationID); err != nil || !again.AlreadyConfirmed {
		t.Fatalf("repeat confirmation: %+v %v", again, err)
	}
}

func TestCommitConfirmRejectsExpiredWindow(t *testing.T) {
	ctx := context.Background()
	cfg := fixture(t, RoleApplication)
	put(t, filepath.Join(cfg.Layout.BinDir, "eregion-node"), "old node binary", 0o755)
	runner := testRunner(t, cfg, newFakeExec())
	runner.SpawnWatchdog = func(string) error { return nil }
	plan, err := runner.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Apply(ctx, plan.Confirmation.Token, false)
	if err == nil || report.OperationID == "" {
		t.Fatalf("expected pending commit: %v", err)
	}
	runner.Now = func() time.Time { return time.Unix(1800000000, 0).Add(DefaultCommitConfirmWindow + time.Second) }
	if _, err := runner.Confirm(ctx, report.OperationID); err == nil {
		t.Fatal("expired confirmation accepted")
	}
	hash, err := cfg.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	projected, err := projectState(cfg, hash, runner.Now())
	if err != nil || projected.CommitID != "" || !projected.Active {
		t.Fatalf("expired window did not clear for a fresh plan: %+v %v", projected, err)
	}
}

func TestConfirmationBindingAndExpiry(t *testing.T) {
	now := time.Unix(1800000000, 0)
	inputs := []byte("input")
	confirmation, err := issueConfirmation("host.apply", "local/main", "revision", inputs, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyConfirmation(confirmation.Token, "host.apply", "local/main", "revision", inputs, now); err != nil {
		t.Fatal(err)
	}
	for _, args := range []struct {
		target, revision string
		at               time.Time
	}{{"other/main", "revision", now}, {"local/main", "changed", now}, {"local/main", "revision", now.Add(ConfirmationTTL + time.Second)}} {
		if err := verifyConfirmation(confirmation.Token, "host.apply", args.target, args.revision, inputs, args.at); err == nil {
			t.Fatal("accepted wrong/expired binding")
		}
	}
}

func TestPrivateContextAndExport(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contexts.json")
	store, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 43)
	view, err := store.Login("production", "https://api.example.com", token, []string{"system:read"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), token) {
		t.Fatal("view leaks token")
	}
	data, _ = json.Marshal(store.List())
	if strings.Contains(string(data), token) {
		t.Fatal("list leaks token")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("unsafe store mode")
	}
	reloaded, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Current != "production" {
		t.Fatal("current not persisted")
	}
	out := filepath.Join(root, "export/token")
	report, err := reloaded.Export("", out, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(report)
	if strings.Contains(string(data), token) {
		t.Fatal("export result leaks token")
	}
	raw, _ := os.ReadFile(out)
	info, _ = os.Stat(out)
	if strings.TrimSpace(string(raw)) != token || info.Mode().Perm() != 0o600 {
		t.Fatal("bad export")
	}
	other := filepath.Join(root, "other")
	put(t, other, strings.Repeat("b", 43), 0o600)
	if _, err := reloaded.Export("", other, false); err == nil {
		t.Fatal("overwrote another token")
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(other, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Export("", symlink, true); err == nil {
		t.Fatal("followed symlink")
	}
	if _, err := ReadTokenFile(symlink); err == nil {
		t.Fatal("read private input through symlink")
	}
	for _, origin := range []string{"http://api.example.com", "https://api.example.com/path", "https://user:password@api.example.com"} {
		if _, err := store.Login("unsafe", origin, token, nil); err == nil {
			t.Fatal("unsafe API origin", origin)
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContextStore(path); err == nil {
		t.Fatal("accepted public credential store")
	}
}

func TestCredentialHandoffNoPlaintextInputs(t *testing.T) {
	cfg := fixture(t, RoleMain)
	preview, err := LoadCredentials(cfg, false)
	if err != nil || preview.Operator != "" {
		t.Fatalf("%+v %v", preview, err)
	}
	credentials, err := LoadCredentials(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	step := &credentialsStep{cfg: cfg, credentials: credentials}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(inputs, []byte(credentials.Operator)) || bytes.Contains(inputs, []byte("private-password")) {
		t.Fatal("secret in engine inputs")
	}
	// Use the fixture's existing account, not a real service account.
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	auth, err := api.LoadAuth(step.authPath())
	if err != nil || auth == nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(step.authPath())
	if bytes.Contains(raw, []byte(credentials.Operator)) {
		t.Fatal("auth file contains token")
	}
	for _, path := range []string{step.authPath(), step.envPath(), cfg.OperatorTokenOut, filepath.Join(cfg.Layout.StateDir, "secrets/operator.token")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe credential file %s: %v", path, err)
		}
	}
	if err := os.Chmod(filepath.Join(cfg.Layout.StateDir, "secrets/operator.token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(cfg, false); err == nil {
		t.Fatal("accepted unsafe cached token")
	}
}

func TestCredentialAddBoundAndScoped(t *testing.T) {
	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	token := strings.Repeat("o", 43)
	if err := WriteAuthFile(authPath, []api.Principal{principal(contract.Actor{Kind: "operator", Subject: "operator"}, token, KnownScopes())}, "", ""); err != nil {
		t.Fatal(err)
	}
	r := CredentialAddRequest{AuthFile: authPath, Actor: contract.Actor{Kind: "ci", Subject: "ci"}, Scopes: []string{"operations:read"}, TokenOut: filepath.Join(root, "ci.token")}
	now := time.Unix(1800000000, 0)
	plan, err := r.Confirmation(now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Apply(plan.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	principals, err := ReadAuthFile(authPath)
	if err != nil || len(principals) != 2 {
		t.Fatalf("%v %v", principals, err)
	}
	if len(principals[1].Scopes) != 1 || principals[1].Scopes[0] != "operations:read" {
		t.Fatal("scope broadened")
	}
	raw, _ := os.ReadFile(r.TokenOut)
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, bytes.TrimSpace(raw)) {
		t.Fatal("credential-add output leaks token")
	}
	if _, err := r.Apply(plan.Token, now); err == nil {
		t.Fatal("stale auth revision accepted")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloaderChecksumLimitAndRedirect(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("artifact")), Header: http.Header{}, Request: r}, nil
	})}
	downloader := HTTPDownloader{Client: client}
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 100, contentSHA256([]byte("artifact"))); err != nil {
		t.Fatal(err)
	}
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 100, strings.Repeat("0", 64)); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 2, contentSHA256([]byte("artifact"))); err == nil {
		t.Fatal("oversized artifact accepted")
	}
	if _, err := downloader.Get(context.Background(), "http://example.com/file", 100, ""); err == nil {
		t.Fatal("plaintext artifact accepted")
	}
	client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": {"http://untrusted.example/file"}}, Request: r}, nil
	})
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 100, ""); err == nil {
		t.Fatal("downgrade redirect accepted")
	}
}

func TestPackExtractionRejectsLinksAndTraversal(t *testing.T) {
	archive := func(name string, kind byte) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		header := &tar.Header{Name: name, Typeflag: kind, Mode: 0o755}
		if kind == tar.TypeReg {
			header.Size = 4
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			_, _ = tw.Write([]byte("pack"))
		}
		_ = tw.Close()
		_ = gz.Close()
		return buf.Bytes()
	}
	if raw, err := extractPack(archive("pack", tar.TypeReg), 10); err != nil || string(raw) != "pack" {
		t.Fatalf("%q %v", raw, err)
	}
	for _, raw := range [][]byte{archive("../pack", tar.TypeReg), archive("pack", tar.TypeSymlink), []byte("not-gzip")} {
		if _, err := extractPack(raw, 10); err == nil {
			t.Fatal("unsafe archive accepted")
		}
	}
	if _, err := extractPack(archive("pack", tar.TypeReg), 2); err == nil {
		t.Fatal("oversized member accepted")
	}
}

func TestDatabasePolicyAndStep(t *testing.T) {
	for _, dsn := range []string{"postgres://u:p@db.example/test?sslmode=disable", "postgres://u:p@db.example/test?sslmode=require", "postgres://u:p@127.0.0.1/test?sslmode=disable&host=evil.example"} {
		if _, err := controlplane.ParseDatabaseConfig(dsn); err == nil {
			t.Fatal("unsafe database policy", dsn)
		}
	}
	if _, err := controlplane.ParseDatabaseConfig("postgres://u:p@db.example/test?sslmode=verify-full"); err != nil {
		t.Fatal(err)
	}
	db := &fakeDatabase{}
	step := &databaseStep{cfg: fixture(t, RoleMain), database: db}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatal(check, err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	check, err = step.Check(context.Background(), engine.Runtime{})
	if err != nil || !check.Complete || db.migrations != 1 {
		t.Fatal(check, err)
	}
}

type fakeDatabase struct {
	current    bool
	migrations int
}

func (f *fakeDatabase) Inspect(context.Context, string) (DatabaseState, error) {
	state := DatabaseState{ServerVersion: 170000, TLS: true, CanCreateSchema: true, Required: 4, RiverRequired: 6}
	if f.current {
		state.Current, state.RiverCurrent = 4, 6
	}
	return state, nil
}
func (f *fakeDatabase) Migrate(ctx context.Context, dsn string) (DatabaseState, error) {
	f.current = true
	f.migrations++
	return f.Inspect(ctx, dsn)
}

func TestAuthenticatedReadiness(t *testing.T) {
	token := strings.Repeat("r", 43)
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			seen = r.Header.Get("Authorization") == "Bearer "+token
			if !seen {
				w.WriteHeader(401)
			}
		}
	}))
	defer server.Close()
	cfg := fixture(t, RoleMain)
	cfg.APIListen = strings.TrimPrefix(server.URL, "http://")
	if err := (httpProbe{}).Health(context.Background(), cfg, token); err != nil || !seen {
		t.Fatal("no authenticated readiness", err)
	}
}
