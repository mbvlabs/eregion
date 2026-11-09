package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

type telemetryExec struct {
	scripts []string
	err     error
}

func (e *telemetryExec) Output(context.Context, string) (string, error) { return "", nil }
func (e *telemetryExec) Run(_ context.Context, _ string, script string, _ map[string]string, _ engine.Reporter) error {
	e.scripts = append(e.scripts, script)
	return e.err
}

func testTelemetryStep(t *testing.T) (*telemetryStep, *telemetryExec) {
	t.Helper()
	dir := t.TempDir()
	exec := &telemetryExec{}
	cfg := DefaultConfig(RoleMain, "main")
	cfg.Layout.SystemdDir = filepath.Join(dir, "systemd")
	step := &telemetryStep{
		cfg: cfg, exec: exec,
		dir:         dir + "/telemetry",
		targetsPath: dir + "/telemetry/targets.json",
		caddyPath:   dir + "/telemetry/Caddyfile",
		unitFile:    filepath.Join(cfg.Layout.SystemdDir, TelemetryService),
		lockPath:    dir + "/lock/rotate.lock",
	}
	return step, exec
}

func TestTelemetryTargetsAreWireGuardOnlyHTTPDiscovery(t *testing.T) {
	step, _ := testTelemetryStep(t)
	targets := step.targets()
	var entries []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(targets, &entries); err != nil || len(entries) != 1 {
		t.Fatalf("targets = %s err=%v", targets, err)
	}
	if entries[0].Targets[0] != access.NodeAddress+":9101" ||
		entries[0].Labels["__metrics_path__"] != telemetry.MetricsPath ||
		entries[0].Labels["eregion_node"] != "main" ||
		entries[0].Labels["eregion_public"] != "false" ||
		entries[0].Labels["eregion_access"] != "wireguard" {
		t.Fatalf("targets = %+v", entries[0])
	}
}

func TestTelemetryCaddyfileIsDedicatedAuthenticatedAndLoopbackUpstream(t *testing.T) {
	site := string(telemetryCaddyfile("$2a$08$hash"))
	for _, want := range []string{
		"admin off",
		"auto_https off",
		"http://" + access.NodeAddress + ":9101",
		"bind " + access.NodeAddress,
		"basic_auth",
		telemetry.Username + " $2a$08$hash",
		"handle " + telemetry.DiscoveryPath,
		"root * " + TelemetryDir,
		"file_server",
		"handle " + telemetry.MetricsPath,
		"reverse_proxy " + NodeExporterLoopbackAddress,
		"rewrite /metrics",
		"header_up -Authorization",
		"header_up -Cookie",
	} {
		if !strings.Contains(site, want) {
			t.Fatalf("Caddyfile missing %q:\n%s", want, site)
		}
	}
	for _, unwanted := range []string{"0.0.0.0", "https://", "localhost:2019"} {
		if strings.Contains(site, unwanted) {
			t.Fatalf("Caddyfile unexpectedly contains %q:\n%s", unwanted, site)
		}
	}
}

func TestTelemetryUnitIsADedicatedCaddyInstance(t *testing.T) {
	unit := string(telemetryUnit())
	for _, want := range []string{
		"Wants=network-online.target",
		"After=network-online.target wg-quick@wg0.service",
		"BindsTo=wg-quick@wg0.service",
		"ConditionPathExists=" + TelemetryCaddyPath,
		"User=caddy",
		"StateDirectory=eregion-telemetry",
		"ExecStart=/usr/bin/caddy run --config " + TelemetryCaddyPath + " --adapter caddyfile",
		"Restart=on-failure",
		"WantedBy=multi-user.target wg-quick@wg0.service",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "ConditionPathExists=/sys/class/net/wg0") {
		t.Fatalf("unit is skipped when wg0 appears later:\n%s", unit)
	}
	if strings.Contains(unit, "caddy.service.d") || strings.Contains(unit, "reload-or-restart") {
		t.Fatalf("unit still couples to public caddy.service:\n%s", unit)
	}
}

func TestTelemetryApplyWritesDedicatedUnitAndDiscovery(t *testing.T) {
	step, exec := testTelemetryStep(t)
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, mode, exists, err := fileState(step.targetsPath)
	if err != nil || !exists || mode != 0o640 || !strings.Contains(string(raw), "10.78.0.1:9101") {
		t.Fatalf("targets = %q mode=%04o exists=%t err=%v", raw, mode, exists, err)
	}
	unit, unitMode, unitExists, err := fileState(step.unitPath())
	if err != nil || !unitExists || unitMode != 0o644 || !strings.Contains(string(unit), "ExecStart=/usr/bin/caddy run") {
		t.Fatalf("unit = %q mode=%04o exists=%t err=%v", unit, unitMode, unitExists, err)
	}
	if len(exec.scripts) != 1 || !strings.Contains(exec.scripts[0], "systemctl enable "+TelemetryService) ||
		!strings.Contains(exec.scripts[0], "ufw allow in on wg0 to "+access.NodeAddress+" port 9101") {
		t.Fatalf("service scripts = %v", exec.scripts)
	}
	if strings.Contains(exec.scripts[0], "|| true") {
		t.Fatalf("firewall failure is swallowed:\n%s", exec.scripts[0])
	}
	if _, _, exists, err := fileState(step.caddyPath); err != nil || exists {
		t.Fatalf("apply wrote a rotation-owned Caddyfile: exists=%t err=%v", exists, err)
	}
}

func TestTelemetryApplyReportsAFirewallFailure(t *testing.T) {
	step, exec := testTelemetryStep(t)
	exec.err = errors.New("ufw refused the rule")
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err == nil || !strings.Contains(err.Error(), "ufw refused") {
		t.Fatalf("apply error = %v", err)
	}
}

func TestTelemetryCaddyfileValidatesWithPinnedCaddy(t *testing.T) {
	bin := os.Getenv("EREGION_CADDY_BIN")
	if bin == "" {
		t.Skip("set EREGION_CADDY_BIN to a pinned Caddy binary to validate the generated Caddyfile")
	}
	// A real bcrypt hash from Caddy's own basic_auth example. The hash format,
	// bind, admin and handler syntax are what this test validates.
	const hash = "$2a$14$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	files := map[string][]byte{
		"telemetry": telemetryCaddyfile(hash),
	}
	for name, content := range files {
		path := filepath.Join(t.TempDir(), name+".Caddyfile")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "validate", "--config", path, "--adapter", "caddyfile")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s Caddyfile rejected by %s: %v\n%s", name, bin, err, output)
		}
	}
}

func TestTelemetryFirewallCheckUsesTheExactAddedRule(t *testing.T) {
	step, _ := testTelemetryStep(t)
	rule := telemetryFirewallRule()
	if rule != "ufw allow in on wg0 to "+access.NodeAddress+" port 9101 proto tcp" {
		t.Fatalf("rule = %q", rule)
	}
	script := step.checkScript(step.caddyPath)
	if !strings.Contains(script, "ufw show added") || !strings.Contains(script, "grep -Fqx '"+rule+"'") {
		t.Fatalf("check script does not observe the exact ufw rule:\n%s", script)
	}
	if !telemetryFirewallMissing("enabled:ok\nfirewall:missing") {
		t.Fatal("missing firewall rule was not detected")
	}
	if telemetryFirewallMissing("enabled:ok\nfirewall:ok") {
		t.Fatal("present firewall rule was reported missing")
	}
	if telemetryFirewallMissing("enabled:ok\nfirewall:unavailable") {
		t.Fatal("unavailable ufw was reported as missing")
	}
}

func TestTelemetryApplyDoesNotOverwriteAnExistingCredentialFile(t *testing.T) {
	step, _ := testTelemetryStep(t)
	if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := []byte("# rotation-owned\n")
	if err := os.WriteFile(step.caddyPath, existing, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(step.caddyPath)
	if err != nil || string(raw) != string(existing) {
		t.Fatalf("rotation-owned Caddyfile changed: %q err=%v", raw, err)
	}
}
