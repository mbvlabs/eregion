package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

type identityFailureDatabase struct{ fakeWorkspaceDatabase }

func (d *identityFailureDatabase) ReadWorkspace(_ context.Context, dsn string) (controlplane.WorkspaceIdentity, error) {
	return controlplane.WorkspaceIdentity{}, errors.New("database failure with " + dsn)
}

type identityStepExec struct{ runs int }

func (e *identityStepExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	e.runs++
	return nil
}
func (*identityStepExec) Output(context.Context, string) (string, error) {
	return "version:ok\nenabled:ok\nactive:ok\n", nil
}

func TestNodeExporterIdentityStepApplyAndNoop(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Node = "node-actual"
	cfg.Workspace = "local-alias"
	database := &fakeWorkspaceDatabase{identity: controlplane.WorkspaceIdentity{Configured: true, Name: "thelab", ID: testWorkspaceID}}
	parent := filepath.Join(t.TempDir(), "node_exporter")
	textfile := filepath.Join(parent, "textfile")
	if err := os.MkdirAll(textfile, 0o755); err != nil {
		t.Fatal(err)
	}
	exec := &identityStepExec{}
	step := &nodeExporterStep{cfg: cfg, database: database, exec: exec, parentDir: parent, textfileDir: textfile,
		// Production always performs the root ownership and trusted-ancestor
		// checks. Only this unprivileged, isolated fixture substitutes them.
		applyDirectoryCheck: func(string, string) error { return nil }, strictDirectoryCheck: func(string, string) error { return nil },
	}
	if err := os.MkdirAll(cfg.Layout.SystemdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(step.unitPath(), step.unit(), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check, err := step.Check(ctx, engine.Runtime{})
	if err != nil || check.Complete || !strings.Contains(check.Detail, "identity metric differs") {
		t.Fatalf("initial check = %+v, %v", check, err)
	}
	eng, err := engine.New(t.TempDir(), cfg.Node, &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	run := func() engine.TaskStatus {
		t.Helper()
		id, err := contract.NewOperationID()
		if err != nil {
			t.Fatal(err)
		}
		status, err := eng.Run(ctx, step, engine.Execution{OperationID: id, Attempt: 1, Step: step.ID()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return status
	}
	if status := run(); !status.Executed || exec.runs != 2 {
		t.Fatalf("first apply = %+v, runs=%d", status, exec.runs)
	}
	path := filepath.Join(textfile, "eregion_node.prom")
	before, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(before), `eregion_node="node-actual",eregion_workspace="thelab",eregion_workspace_id="`+testWorkspaceID+`"`) || strings.Contains(string(before), "local-alias") {
		t.Fatalf("written identity = %q, %v", before, err)
	}
	check, err = step.Check(ctx, engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("post-apply check = %+v, %v", check, err)
	}
	if status := run(); status.Executed || exec.runs != 2 {
		t.Fatalf("new-operation no-op = %+v, runs=%d", status, exec.runs)
	}
	database.identity.Name = "otherlab"
	check, err = step.Check(ctx, engine.Runtime{})
	if err != nil || check.Complete || !strings.Contains(check.Detail, "identity metric differs") {
		t.Fatalf("authoritative identity drift check = %+v, %v", check, err)
	}
	database.identity.Name = "thelab"
	step.database = &identityFailureDatabase{}
	check, err = step.Check(ctx, engine.Runtime{})
	if err == nil || check.Complete || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("failed database check = %+v, %v", check, err)
	}
	_, err = step.Apply(ctx, engine.Runtime{}, nil)
	if err == nil || strings.Contains(err.Error(), "private-password") || exec.runs != 2 {
		t.Fatalf("failed database apply = %v, runs=%d", err, exec.runs)
	}
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = eng.Run(ctx, step, engine.Execution{OperationID: id, Attempt: 1, Step: step.ID()}, nil); err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("failed engine run error = %v", err)
	}
	journal, err := os.ReadFile(eng.JournalPath())
	if err != nil || strings.Contains(string(journal), "private-password") {
		t.Fatalf("failure journal leaked a secret or was unreadable: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || strings.Contains(string(after), "private-password") {
		t.Fatalf("database failure changed or leaked identity file: %q, %v", after, err)
	}
}

func TestNodeExporterIdentityMetricUsesDatabaseAndCurrentNode(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Node = "node-actual"
	cfg.Workspace = "not-authoritative"
	cfg.DatabaseURL = "credential-must-not-appear"
	database := &fakeWorkspaceDatabase{identity: controlplane.WorkspaceIdentity{
		Configured: true, ID: testWorkspaceID, Name: "thelab",
	}}
	dir := t.TempDir()
	step := &nodeExporterStep{cfg: cfg, database: database, textfileDir: dir}
	metric, err := step.identityMetric(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := step.writeIdentityMetric(metric); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "eregion_node.prom")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `eregion_node_info{eregion_node="node-actual",eregion_workspace="thelab",eregion_workspace_id="` + testWorkspaceID + `"} 1` + "\n"
	if !strings.HasSuffix(string(raw), want) || database.reads != 1 {
		t.Fatalf("identity metric = %q; reads = %d", raw, database.reads)
	}
	// Both a direct public scrape and an HTTP-discovered scrape reach the
	// same exporter textfile; discovery does not need to inject these labels.
	discovery, err := telemetry.TargetsJSON(cfg.Node, "main.example.com")
	if err != nil || !strings.Contains(string(discovery), `"eregion_node": "node-actual"`) || !strings.Contains(string(discovery), telemetry.MetricsPath) {
		t.Fatalf("discovery does not name this node and metrics path: %s, %v", discovery, err)
	}
	if gateway := string(telemetryCaddyfile("fixture-hash")); !strings.Contains(gateway, "handle "+telemetry.GatewayMetricsPath) || !strings.Contains(gateway, "reverse_proxy "+NodeExporterLoopbackAddress) || !strings.Contains(gateway, "rewrite /metrics") {
		t.Fatal("public metrics route no longer proxies the exporter")
	}
	for _, forbidden := range []string{"not-authoritative", "credential-must-not-appear", "node_uname_info"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("metric contains %q", forbidden)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("identity metric mode: %v %v", info, err)
	}

	// Before workspace assignment an observation must not reuse an earlier identity.
	database.identity = controlplane.WorkspaceIdentity{}
	metric, err = step.identityMetric(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := step.writeIdentityMetric(metric); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(raw), "eregion_node_info{eregion_node=\"node-actual\"} 1\n") || strings.Contains(string(raw), "eregion_workspace") {
		t.Fatalf("unconfigured identity metric = %q, %v", raw, err)
	}
}

func TestNodeExporterIdentityMetricRejectsInvalidLabels(t *testing.T) {
	for _, tc := range []struct{ node, name, id string }{
		{"node\"\nmalicious 1", "thelab", testWorkspaceID},
		{"main", "thelab\"\nmalicious 1", testWorkspaceID},
		{"main", "thelab", "ws_\"\\malicious"},
	} {
		cfg := fixture(t, RoleMain)
		cfg.Node = tc.node
		step := &nodeExporterStep{cfg: cfg, database: &fakeWorkspaceDatabase{identity: controlplane.WorkspaceIdentity{Configured: true, Name: tc.name, ID: tc.id}}}
		if metric, err := step.identityMetric(context.Background()); err == nil {
			t.Fatalf("accepted unsafe identity: %q", metric)
		}
	}
	step := &nodeExporterStep{cfg: fixture(t, RoleMain)}
	if metric, err := step.identityMetric(context.Background()); err == nil || metric != nil {
		t.Fatalf("missing authority produced identity metric %q, %v", metric, err)
	}
}

func TestNodeExporterV3EnablesScopedLocalCollectors(t *testing.T) {
	step := &nodeExporterStep{cfg: DefaultConfig(RoleMain, "")}
	if got := step.ID(); got != "host-node-exporter-v3" {
		t.Fatalf("step ID = %q", got)
	}
	unit := string(step.unit())
	for _, want := range []string{
		"--web.listen-address=" + NodeExporterListenAddress(),
		"--collector.textfile.directory=" + NodeExporterTextfileDir,
		"--collector.systemd",
		"--collector.systemd.unit-include=" + nodeExporterCollectorUnits,
		"ProtectSystem=strict",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("node_exporter unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "0.0.0.0:9100") || strings.Contains(unit, "[::]:9100") {
		t.Fatalf("node_exporter unit exposes a public listener:\n%s", unit)
	}
	if strings.Contains(unit, "wg-quick@wg0.service") {
		t.Fatalf("loopback node_exporter retains a WireGuard dependency:\n%s", unit)
	}
	if strings.Contains(nodeExporterCollectorUnits, "postgresql") || strings.Contains(nodeExporterCollectorUnits, ".*") {
		t.Fatalf("systemd collector set is not narrow: %s", nodeExporterCollectorUnits)
	}
	if !strings.Contains(nodeExporterCollectorUnits, "eregion-.+") || !strings.Contains(nodeExporterCollectorUnits, "wg-quick@wg0") {
		t.Fatalf("systemd collector set omits eregion or WireGuard units: %s", nodeExporterCollectorUnits)
	}
}

func TestNodeExporterV3ReplacesEarlierSteps(t *testing.T) {
	steps, err := (&Runner{Config: fixture(t, RoleMain)}).steps(Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, step := range steps {
		ids[step.ID()] = true
	}
	if !ids["host-node-exporter-v3"] {
		t.Fatal("main role does not select host-node-exporter-v3")
	}
	if ids["host-node-exporter-v1"] || ids["host-node-exporter-v2"] {
		t.Fatal("an older node exporter step is still selected")
	}
}

func TestNodeExporterDirInfoApplyValidationRejectsUnsafeParents(t *testing.T) {
	for name, tc := range map[string]struct {
		info nodeExporterDirInfo
		want string
	}{
		"valid root 0755":   {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o755}},
		"missing":           {info: nodeExporterDirInfo{}},
		"safe wrong mode":   {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o700}},
		"safe setuid mode":  {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o4755}},
		"safe setgid mode":  {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o2755}},
		"safe sticky mode":  {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o1755}},
		"symlink":           {info: nodeExporterDirInfo{Exists: true, Symlink: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o777}, want: "symlink"},
		"not a directory":   {info: nodeExporterDirInfo{Exists: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o644}, want: "not a directory"},
		"unknown ownership": {info: nodeExporterDirInfo{Exists: true, Directory: true, Mode: 0o755}, want: "ownership cannot be verified"},
		"non-root owner":    {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 1000, GID: 1000, Mode: 0o755}, want: "owned 1000:1000"},
		"group writable":    {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o775}, want: "group/other writable"},
		"other writable":    {info: nodeExporterDirInfo{Exists: true, Directory: true, OwnerKnown: true, UID: 0, GID: 0, Mode: 0o757}, want: "group/other writable"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.info.validateForApply("parent")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("safe/missing path = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

type nodeExporterCheckExec struct{ output string }

func (nodeExporterCheckExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	return nil
}
func (e nodeExporterCheckExec) Output(context.Context, string) (string, error) { return e.output, nil }

type nodeExporterApplyExec struct {
	runs int
}

func (e *nodeExporterApplyExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	e.runs++
	return nil
}
func (*nodeExporterApplyExec) Output(context.Context, string) (string, error) { return "", nil }

func TestFileModeBitsPreservesSpecialBits(t *testing.T) {
	for name, tc := range map[string]struct {
		mode os.FileMode
		want uint32
	}{
		"ordinary":    {mode: 0o755, want: 0o755},
		"setuid":      {mode: 0o755 | os.ModeSetuid, want: 0o4755},
		"setgid":      {mode: 0o755 | os.ModeSetgid, want: 0o2755},
		"sticky":      {mode: 0o755 | os.ModeSticky, want: 0o1755},
		"all special": {mode: 0o755 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky, want: 0o7755},
	} {
		t.Run(name, func(t *testing.T) {
			if got := fileModeBits(tc.mode); got != tc.want {
				t.Fatalf("mode bits = %04o, want %04o", got, tc.want)
			}
		})
	}
}

func TestNodeExporterV3CheckUsesStrictDirectoryState(t *testing.T) {
	cfg := fixture(t, RoleMain)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(cfg.Layout.SystemdDir, "node-exporter.service")), 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(t.TempDir(), "node_exporter")
	textfile := filepath.Join(parent, "textfile")
	step := &nodeExporterStep{cfg: cfg, exec: nodeExporterCheckExec{output: strings.Join([]string{
		"version:ok", "enabled:ok", "wg:down",
	}, "\n")}, parentDir: parent, textfileDir: textfile}
	if err := os.WriteFile(step.unitPath(), step.unit(), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || result.Complete || !strings.Contains(result.Detail, "missing") {
		t.Fatalf("missing-directory check = %+v, %v", result, err)
	}
}

func TestNodeExporterApplyRefusesAttackerWritableParentBeforeMutation(t *testing.T) {
	cfg := fixture(t, RoleMain)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	exec := &nodeExporterApplyExec{}
	step := &nodeExporterStep{cfg: cfg, exec: exec, parentDir: parent, textfileDir: filepath.Join(parent, "textfile")}
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "node_exporter") {
		t.Fatal("apply accepted an attacker-writable/non-root parent")
	}
	if exec.runs != 0 {
		t.Fatalf("apply ran %d mutation scripts before refusing the unsafe parent", exec.runs)
	}
}

func TestNodeExporterDirectoryScriptRefusesSymlinksBeforeMutation(t *testing.T) {
	script := nodeExporterDirectoryScript(NodeExporterDir, NodeExporterTextfileDir)
	for _, want := range []string{
		"refusing to use symlinked node_exporter path",
		"refusing non-directory node_exporter path",
		"install -d -m 0755 -o root -g root",
		"chown root:root",
		nodeExporterDirectoryModeClear,
		"refusing to leave node_exporter path",
		NodeExporterDir,
		NodeExporterTextfileDir,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("directory script lacks %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "refusing to use symlinked") > strings.Index(script, "install -d") {
		t.Fatal("directory script mutates before its symlink refusal")
	}
	if strings.Index(script, "install -d") > strings.Index(script, nodeExporterDirectoryModeClear) {
		t.Fatal("directory script clears special bits before installing the directories")
	}
}

func TestNodeExporterDirectoryModeClearRemovesSpecialBits(t *testing.T) {
	for name, special := range map[string]os.FileMode{
		"setgid":            os.ModeSetgid,
		"sticky":            os.ModeSticky,
		"setgid and sticky": os.ModeSetgid | os.ModeSticky,
		"all special":       os.ModeSetuid | os.ModeSetgid | os.ModeSticky,
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			parent := filepath.Join(base, "node_exporter")
			textfile := filepath.Join(parent, "textfile")
			if err := os.MkdirAll(textfile, 0o755); err != nil {
				t.Fatal(err)
			}
			wantBefore := fileModeBits(0o755 | special)
			for _, path := range []string{parent, textfile} {
				if err := os.Chmod(path, 0o755|special); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := fileModeBits(info.Mode()); got != wantBefore {
					if runtime.GOOS != "linux" {
						t.Skipf("filesystem did not retain special bits %04o on %s", wantBefore, path)
					}
					t.Fatalf("test setup mode = %04o on %s, want %04o", got, path, wantBefore)
				}
			}

			// Run the exact commands used by the host step. The paths are
			// passed as shell arguments, so a temporary directory name cannot
			// change the command text.
			command := exec.Command("sh", "-c", `parent="$1"; textfile="$2"; `+nodeExporterDirectoryModeClear, "sh", parent, textfile)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("mode clear command failed: %v\n%s", err, output)
			}
			for _, path := range []string{parent, textfile} {
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := fileModeBits(info.Mode()); got != 0o755 {
					t.Fatalf("mode after clear = %04o on %s, want 0755", got, path)
				}
			}
		})
	}
}

func TestNodeExporterV3DiffNamesTextfileAndCollectorChanges(t *testing.T) {
	step := &nodeExporterStep{cfg: DefaultConfig(RoleMain, "")}
	diff, err := step.Diff(context.Background(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(diff, "\n")
	if !strings.Contains(joined, NodeExporterTextfileDir) || !strings.Contains(joined, "systemd") {
		t.Fatalf("diff = %s", joined)
	}
}
