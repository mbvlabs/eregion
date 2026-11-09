package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

func TestNodeExporterV2EnablesScopedLocalCollectors(t *testing.T) {
	step := &nodeExporterStep{cfg: DefaultConfig(RoleMain, "")}
	if got := step.ID(); got != "host-node-exporter-v2" {
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

func TestNodeExporterV2ReplacesTheV1Step(t *testing.T) {
	steps, err := (&Runner{Config: fixture(t, RoleMain)}).steps(Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, step := range steps {
		ids[step.ID()] = true
	}
	if !ids["host-node-exporter-v2"] {
		t.Fatal("main role does not select host-node-exporter-v2")
	}
	if ids["host-node-exporter-v1"] {
		t.Fatal("old host-node-exporter-v1 is still selected")
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

func TestNodeExporterV2CheckUsesStrictDirectoryState(t *testing.T) {
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
	if err == nil {
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

func TestNodeExporterV2DiffNamesTextfileAndCollectorChanges(t *testing.T) {
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
