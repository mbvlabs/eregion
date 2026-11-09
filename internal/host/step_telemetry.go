package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

// Authenticated local telemetry. node_exporter stays on loopback. A dedicated
// Caddy instance, separate from the public ingress Caddy, serves exactly two
// authenticated paths on the WireGuard address: the metrics proxy and the HTTP
// service-discovery list. There is still no Prometheus/Grafana deployment,
// central telemetry node, metric store or retained history.
const (
	TelemetryDir = "/etc/eregion-telemetry"
	// TelemetryTargetsPath is the HTTP service-discovery document served by
	// telemetry Caddy. It contains no secret.
	TelemetryTargetsPath = TelemetryDir + "/targets.json"
	// TelemetryCaddyPath is the dedicated telemetry Caddyfile. It is written
	// by rotation, not by apply, so an apply never replaces a newer credential
	// with an older one.
	TelemetryCaddyPath = TelemetryDir + "/Caddyfile"
	// TelemetryService is the dedicated telemetry Caddy instance. Keeping it
	// separate from caddy.service means a missing wg0 or a bad telemetry
	// config cannot prevent the public ingress from starting.
	TelemetryService = "eregion-telemetry.service"
	// TelemetryLockPath serializes telemetry config writes/reloads with host
	// apply. It is not shared with the public Caddy route writer.
	TelemetryLockPath = "/run/eregion-telemetry/rotate.lock"
	// NodeExporterLoopbackAddress is the only node_exporter listener.
	NodeExporterLoopbackAddress = "127.0.0.1:9100"
)

func TelemetryListenAddress() string {
	return telemetry.EndpointListenAddress(access.NodeAddress)
}

func TelemetryMetricsURL() string {
	return telemetry.EndpointMetricsURL(access.NodeAddress)
}

func TelemetryDiscoveryURL() string {
	return telemetry.EndpointDiscoveryURL(access.NodeAddress)
}

// telemetryStep installs the endpoint prerequisites: the strict state
// directory, the discovery document, the dedicated Caddy unit and the
// WireGuard-only firewall rule. The Caddyfile itself is rotation-owned; a fresh
// node has no credential and no telemetry Caddyfile until `metrics rotate` runs.
type telemetryStep struct {
	cfg         Config
	exec        Exec
	dir         string
	targetsPath string
	caddyPath   string
	unitFile    string
	lockPath    string
}

func (s *telemetryStep) ID() string { return "host-telemetry-v1" }
func (s *telemetryStep) Describe() string {
	return "Install the authenticated telemetry endpoint prerequisites on the WireGuard address"
}
func (s *telemetryStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *telemetryStep) paths() (dir, targets, caddy string) {
	dir, targets, caddy = s.dir, s.targetsPath, s.caddyPath
	if dir == "" {
		dir = TelemetryDir
	}
	if targets == "" {
		targets = TelemetryTargetsPath
	}
	if caddy == "" {
		caddy = TelemetryCaddyPath
	}
	return
}

func (s *telemetryStep) targets() []byte {
	raw, err := telemetry.TargetsJSON(s.cfg.Node, access.NodeAddress)
	if err != nil {
		// The node and WireGuard constant are fixed; a failure is a
		// programming error, represented as an empty document which Apply
		// refuses before writing.
		return nil
	}
	return raw
}

func (s *telemetryStep) unitPath() string {
	if s.unitFile != "" {
		return s.unitFile
	}
	return filepath.Join(s.cfg.Layout.SystemdDir, TelemetryService)
}

func (s *telemetryStep) rotationLockPath() string {
	if s.lockPath != "" {
		return s.lockPath
	}
	return TelemetryLockPath
}

// telemetryFirewallRule is the exact ufw rule Check looks for in `ufw show
// added` and Apply converges. ufw normalises rules with this same full syntax.
func telemetryFirewallRule() string {
	return fmt.Sprintf("ufw allow in on wg0 to %s port %s proto tcp", access.NodeAddress, telemetry.Port)
}

func telemetryFirewallMissing(output string) bool {
	return strings.Contains(output, "firewall:missing")
}

func (s *telemetryStep) checkScript(caddy string) string {
	return fmt.Sprintf(`if systemctl is-enabled %s >/dev/null 2>&1; then echo enabled:ok; else echo enabled:missing; fi
if [ -f %s ]; then
  if systemctl is-active --quiet %s; then echo active:ok; else echo active:missing; fi
fi
if command -v ufw >/dev/null 2>&1; then
  if ufw show added 2>/dev/null | grep -Fqx '%s'; then echo firewall:ok; else echo firewall:missing; fi
else
  echo firewall:unavailable
fi`, TelemetryService, caddy, TelemetryService, telemetryFirewallRule())
}

func (s *telemetryStep) unit() []byte {
	return telemetryUnit()
}

func telemetryUnit() []byte {
	return []byte(fmt.Sprintf(`[Unit]
Description=eregion authenticated telemetry endpoint
Documentation=https://github.com/mbvlabs/eregion
Wants=network-online.target
After=network-online.target wg-quick@wg0.service
BindsTo=wg-quick@wg0.service
ConditionPathExists=%s
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=caddy
Group=caddy
Environment=XDG_CONFIG_HOME=/var/lib/eregion-telemetry/config
Environment=XDG_DATA_HOME=/var/lib/eregion-telemetry/data
StateDirectory=eregion-telemetry
StateDirectoryMode=0700
ExecStart=%s run --config %s --adapter caddyfile
Restart=on-failure
RestartSec=5s
NoNewPrivileges=yes
PrivateDevices=yes
PrivateTmp=yes
ProtectClock=yes
ProtectControlGroups=yes
ProtectHome=yes
ProtectHostname=yes
ProtectKernelLogs=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectSystem=full
LockPersonality=yes
MemoryDenyWriteExecute=yes
MemoryMax=64M
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
SystemCallArchitectures=native
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target wg-quick@wg0.service
`, TelemetryCaddyPath, CaddyBinaryPath, TelemetryCaddyPath))
}

func (s *telemetryStep) Inputs() ([]byte, error) {
	dir, targets, caddy := s.paths()
	if len(s.targets()) == 0 {
		return nil, fmt.Errorf("the telemetry target document is invalid")
	}
	return canonicalInput(struct {
		Node        string `json:"node"`
		Listen      string `json:"listen"`
		Upstream    string `json:"upstream"`
		Dir         string `json:"dir"`
		Targets     string `json:"targets"`
		TargetsPath string `json:"targets_path"`
		CaddyPath   string `json:"caddy_path"`
		Unit        string `json:"unit"`
		UnitPath    string `json:"unit_path"`
		LockPath    string `json:"lock_path"`
	}{s.cfg.Node, TelemetryListenAddress(), "http://" + NodeExporterLoopbackAddress, dir, string(s.targets()), targets, caddy, string(s.unit()), s.unitPath(), s.rotationLockPath()})
}

func (s *telemetryStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	dir, targets, caddy := s.paths()
	if err := validateTelemetryDir(dir); err != nil {
		return incomplete(err.Error())
	}
	if ok, mode, err := sameFileContent(targets, s.targets()); err != nil {
		return engine.CheckResult{}, err
	} else if !ok {
		return incomplete("the telemetry discovery document differs")
	} else if mode != 0o640 {
		return incomplete(fmt.Sprintf("%s has mode %04o; want 0640", targets, mode))
	}
	if ok, mode, err := sameFileContent(s.unitPath(), s.unit()); err != nil {
		return engine.CheckResult{}, err
	} else if !ok {
		return incomplete("the telemetry service unit differs")
	} else if mode != 0o644 {
		return incomplete(fmt.Sprintf("%s has mode %04o; want 0644", s.unitPath(), mode))
	}
	// A missing Caddyfile means no credential has been configured yet. Apply
	// installs the prerequisites; `metrics rotate` is the explicit step that
	// enables the endpoint.
	configured := false
	if _, _, exists, err := fileState(caddy); err != nil {
		return engine.CheckResult{}, err
	} else if exists {
		configured = true
		if mode := caddyMode(caddy); mode != 0o640 {
			return incomplete(fmt.Sprintf("%s has mode %04o; want 0640", caddy, mode))
		}
	}
	script := s.checkScript(caddy)
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, script)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !strings.Contains(output, "enabled:ok") {
		return incomplete(TelemetryService + " is not enabled")
	}
	if configured && !strings.Contains(output, "active:ok") {
		return incomplete(TelemetryService + " is not active although its Caddyfile exists")
	}
	if telemetryFirewallMissing(output) {
		return incomplete("the WireGuard telemetry firewall rule is missing")
	}
	detail := "telemetry prerequisites are installed; run eregion metrics rotate to enable the authenticated endpoint"
	if configured {
		detail = "the dedicated authenticated telemetry endpoint is configured on " + TelemetryListenAddress()
	}
	if strings.Contains(output, "firewall:unavailable") {
		detail += "; ufw is unavailable, so the firewall rule was not observed"
	}
	return complete(detail)
}

func (s *telemetryStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	dir, targets, caddy := s.paths()
	if len(s.targets()) == 0 {
		return engine.ApplyResult{}, fmt.Errorf("the telemetry target document is invalid")
	}
	if err := ensureDir(dir, 0o755); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := chownIfRoot(dir, "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFile(targets, s.targets(), 0o640, "root", "caddy"); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(s.unitPath(), s.unit(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	configured := telemetryFileExists(caddy)
	script := fmt.Sprintf(`set -euo pipefail
systemctl daemon-reload
systemctl enable %s
if command -v ufw >/dev/null 2>&1; then
  %s >/dev/null
fi
if [ %t = true ] && ip -4 addr show wg0 2>/dev/null | grep -q 'inet %s/'; then
  systemctl restart %s
fi`, TelemetryService, telemetryFirewallRule(), configured, access.NodeAddress, TelemetryService)
	err := engine.WithFileLock(s.rotationLockPath(), func() error {
		return s.exec.Run(ctx, s.ID()+"-service", script, nil, report)
	})
	if err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{
		Notes: []string{
			"telemetry endpoint prerequisites are installed; node_exporter stays on " + NodeExporterLoopbackAddress,
		},
		Result: map[string]any{
			"listen": TelemetryListenAddress(), "metrics_path": telemetry.MetricsPath,
			"discovery_path": telemetry.DiscoveryPath, "public": false, "authentication": "basic",
			"targets": targets, "caddyfile": caddy, "unit": s.unitPath(),
			"credential": "rotation-owned; run eregion metrics rotate",
		},
	}, nil
}

func (s *telemetryStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	dir, targets, caddy := s.paths()
	out := []string{
		"+ ensure " + dir + " is root:root 0755",
		"+ write " + targets + " (HTTP service discovery, no secret)",
		"+ install " + TelemetryService + " as a dedicated Caddy instance after wg0",
		"+ allow 9101 on wg0 only",
		"+ keep " + caddy + " owned by rotation; an apply never replaces a credential",
	}
	return out, nil
}

func (s *telemetryStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}

func telemetryFileExists(path string) bool {
	_, _, exists, err := fileState(path)
	return err == nil && exists
}

func caddyMode(path string) os.FileMode {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
}

func validateTelemetryDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s is missing", path)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", path)
	}
	if info.Mode().Perm() != 0o755 {
		return fmt.Errorf("%s has mode %04o; want 0755", path, info.Mode().Perm())
	}
	ok, err := ownershipMatches(info, "root", "root")
	if err != nil || !ok {
		return fmt.Errorf("%s must be owned root:root", path)
	}
	return nil
}

// TelemetryConfigured reports whether rotation has installed the Caddyfile.
// It is non-secret and used by doctor and `metrics endpoint`.
func TelemetryConfigured() bool {
	return telemetryFileExists(TelemetryCaddyPath)
}

// TelemetryEndpointInfo describes the node endpoint without carrying the hash.
func TelemetryEndpointInfo() (telemetry.EndpointInfo, error) {
	node := "local"
	if config, err := LoadNodeFile(DefaultNodeConfigPath()); err == nil && strings.TrimSpace(config.Node) != "" {
		node = strings.TrimSpace(config.Node)
	}
	return telemetry.BuildEndpointInfo(node, access.NodeAddress, "http://"+NodeExporterLoopbackAddress, TelemetryConfigured()), nil
}

// telemetryCaddyfile is the WireGuard-only dedicated Caddyfile. The bcrypt
// hash is the only credential material; the plaintext password never reaches
// the node. `admin off` avoids clashing with the public Caddy admin endpoint;
// rotation restarts this unit after validating the file.
func telemetryCaddyfile(hash string) []byte {
	return []byte(fmt.Sprintf(`# managed by eregion metrics rotate; contains a bcrypt hash, not a password
{
	admin off
	auto_https off
}
http://%s {
	bind %s
	basic_auth {
		%s %s
	}
	handle %s {
		root * %s
		file_server
	}
	handle %s {
		reverse_proxy %s {
			rewrite /metrics
			header_up -Authorization
			header_up -Cookie
		}
	}
	handle {
		respond 404
	}
}
`, TelemetryListenAddress(), access.NodeAddress, telemetry.Username, hash, telemetry.DiscoveryPath, TelemetryDir, telemetry.MetricsPath, NodeExporterLoopbackAddress))
}

// RotateTelemetryCredential replaces the dedicated telemetry Caddy hash. It
// follows vaire's rotate path: validate the new configuration, restart the
// telemetry instance, and restore the previous file when either fails. A
// node-local lock serializes telemetry writes/reloads with host apply; public
// route writers and the public Caddy are never touched.
func RotateTelemetryCredential(ctx context.Context, confirm string, dryRun bool, hash string) (telemetry.Rotation, error) {
	node := "local"
	if config, err := LoadNodeFile(DefaultNodeConfigPath()); err == nil && strings.TrimSpace(config.Node) != "" {
		node = strings.TrimSpace(config.Node)
	}
	token := node + "/metrics"
	rotation := telemetry.Rotation{
		Schema: telemetry.RotationSchema, Node: node, Username: telemetry.Username,
		MetricsURL: TelemetryMetricsURL(), Discovery: TelemetryDiscoveryURL(),
	}
	if dryRun {
		return rotation, nil
	}
	if strings.TrimSpace(confirm) != token {
		fault := contract.NewFault(contract.ConfirmationRequired,
			"rotating the telemetry credential locks every scraper out until it gets the new password",
			"Pass --confirm "+token+".")
		fault.Details = map[string]any{"confirm_token": token}
		return telemetry.Rotation{}, fault
	}
	if !telemetry.ValidHash(hash) {
		return telemetry.Rotation{}, contract.NewFault(contract.ValidationFailed, "the telemetry credential hash is missing or not bcrypt", "Retry eregion metrics rotate.")
	}
	if !telemetryFileExists(TelemetryTargetsPath) {
		return telemetry.Rotation{}, contract.NewFault(contract.PreconditionFailed,
			"the telemetry endpoint prerequisites are not installed",
			"Run eregion apply on the node, then retry eregion metrics rotate.")
	}
	err := engine.WithFileLock(TelemetryLockPath, func() error {
		previous, previousMode, existed, err := fileState(TelemetryCaddyPath)
		if err != nil {
			return err
		}
		if err := writeFile(TelemetryCaddyPath, telemetryCaddyfile(hash), 0o640, "root", "caddy"); err != nil {
			return err
		}
		if err := validateTelemetryCaddy(ctx); err != nil {
			restoreTelemetryCaddyLocked(previous, previousMode, existed)
			return err
		}
		if err := restartTelemetryService(ctx); err != nil {
			restoreTelemetryCaddyLocked(previous, previousMode, existed)
			return err
		}
		return nil
	})
	if err != nil {
		return telemetry.Rotation{}, err
	}
	return rotation, nil
}

func restoreTelemetryCaddyLocked(previous []byte, mode os.FileMode, existed bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !existed {
		_ = os.Remove(TelemetryCaddyPath)
		_ = exec.CommandContext(ctx, "systemctl", "stop", TelemetryService).Run()
		return
	}
	if mode != 0 {
		_ = writeFile(TelemetryCaddyPath, previous, mode, "root", "caddy")
	}
	_ = exec.CommandContext(ctx, "systemctl", "restart", TelemetryService).Run()
}

func validateTelemetryCaddy(ctx context.Context) error {
	validate := exec.CommandContext(ctx, CaddyBinaryPath, "validate", "--config", TelemetryCaddyPath, "--adapter", "caddyfile")
	if output, err := validate.CombinedOutput(); err != nil {
		return contract.NewFault(contract.PreconditionFailed,
			"Caddy refused the telemetry configuration",
			strings.TrimSpace(string(output)))
	}
	return nil
}

func restartTelemetryService(ctx context.Context) error {
	restart := exec.CommandContext(ctx, "systemctl", "restart", TelemetryService)
	if output, err := restart.CombinedOutput(); err != nil {
		return contract.NewFault(contract.Unavailable,
			"the telemetry endpoint did not restart",
			strings.TrimSpace(string(output)))
	}
	return nil
}
