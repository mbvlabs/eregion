package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

// Authenticated HTTPS telemetry. node_exporter stays on loopback. The public
// ingress Caddy owns two exact paths on the main hostname and proxies them to
// a dedicated loopback-only Caddy gateway, which requires basic authentication
// and strips the scraper's credential before the exporter. There is still no
// Prometheus/Grafana deployment, central telemetry node, metric store or
// retained history.
const (
	TelemetryDir = "/etc/eregion-telemetry"
	// TelemetryTargetsPath is the HTTP service-discovery document served by
	// the loopback gateway. It contains no secret.
	TelemetryTargetsPath = TelemetryDir + "/targets.json"
	// TelemetryCaddyPath is the dedicated loopback gateway Caddyfile. It is
	// owned by rotation and by an apply that preserves an existing hash; apply
	// never replaces the credential with an invented one.
	TelemetryCaddyPath = TelemetryDir + "/Caddyfile"
	// TelemetryService is the dedicated loopback gateway instance. Keeping it
	// separate from caddy.service means a missing gateway config or a stopped
	// gateway cannot prevent the public ingress from starting.
	TelemetryService = "eregion-telemetry.service"
	// TelemetryLockPath serializes gateway config writes/reloads with host
	// apply and `metrics rotate`.
	TelemetryLockPath = "/run/eregion-telemetry/rotate.lock"
	// NodeExporterLoopbackAddress is the only node_exporter listener.
	NodeExporterLoopbackAddress = "127.0.0.1:9100"
)

// TelemetryListenAddress is the loopback gateway address, not a public or
// WireGuard listener.
func TelemetryListenAddress() string {
	return telemetry.GatewayListenAddress
}

// telemetryStep installs the endpoint prerequisites: the strict state
// directory, the public-hostname discovery document, the loopback gateway unit
// and removal of the former WireGuard firewall rule. The gateway Caddyfile
// itself is rotation-owned; a fresh node has no credential and no Caddyfile
// until `metrics rotate` runs.
type telemetryStep struct {
	cfg         Config
	exec        Exec
	dir         string
	targetsPath string
	caddyPath   string
	unitFile    string
	lockPath    string
	validate    func(context.Context) error
	restart     func(context.Context) error
	stop        func(context.Context) error
}

func (s *telemetryStep) ID() string { return "host-telemetry-v2" }
func (s *telemetryStep) Describe() string {
	return "Install the authenticated HTTPS telemetry gateway on loopback behind the public ingress"
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

func (s *telemetryStep) hostname() string { return strings.TrimSpace(s.cfg.Hostname) }

func (s *telemetryStep) targets() []byte {
	if s.hostname() == "" {
		return nil
	}
	raw, err := telemetry.TargetsJSON(s.cfg.Node, s.hostname())
	if err != nil {
		// The node name and hostname are validated inputs; a failure is a
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

func (s *telemetryStep) validateCaddy(ctx context.Context) error {
	if s.validate != nil {
		return s.validate(ctx)
	}
	return validateTelemetryCaddy(ctx)
}

func (s *telemetryStep) restartService(ctx context.Context) error {
	if s.restart != nil {
		return s.restart(ctx)
	}
	return restartTelemetryService(ctx)
}

func (s *telemetryStep) stopService(ctx context.Context) error {
	if s.stop != nil {
		return s.stop(ctx)
	}
	return stopTelemetryService(ctx)
}

// legacyTelemetryFirewallRule is the exact rule the previous WireGuard-only
// version created. Removal must match that full rule, never a foreign rule.
func legacyTelemetryFirewallRule() string {
	return fmt.Sprintf("ufw allow in on wg0 to %s port %s proto tcp", access.NodeAddress, telemetry.Port)
}

func firewallCheckScript() string {
	return fmt.Sprintf(`if ! command -v ufw >/dev/null 2>&1; then echo firewall:unavailable; exit 0; fi
if ! added="$(ufw show added 2>/dev/null)"; then echo firewall:error; exit 0; fi
if printf '%%s\n' "$added" | grep -Fqx '%s'; then echo firewall:legacy; else echo firewall:ok; fi`, legacyTelemetryFirewallRule())
}

func firewallRemovalScript() string {
	return fmt.Sprintf(`set -euo pipefail
if command -v ufw >/dev/null 2>&1; then
  added="$(ufw show added 2>/dev/null)"
  if printf '%%s\n' "$added" | grep -Fqx '%s'; then
    ufw --force delete allow in on wg0 to %s port %s proto tcp >/dev/null
  fi
fi`, legacyTelemetryFirewallRule(), access.NodeAddress, telemetry.Port)
}

func (s *telemetryStep) checkScript(caddy string) string {
	return fmt.Sprintf(`if systemctl is-enabled %s >/dev/null 2>&1; then echo enabled:ok; else echo enabled:missing; fi
if [ -f %s ]; then
  if systemctl is-active --quiet %s; then echo active:ok; else echo active:missing; fi
else
  if systemctl is-active --quiet %s; then echo active:unexpected; fi
fi
%s`, TelemetryService, caddy, TelemetryService, TelemetryService, firewallCheckScript())
}

func (s *telemetryStep) unit() []byte {
	return telemetryUnit()
}

// telemetryUnit runs the loopback gateway. It deliberately has no WireGuard
// binds or start dependencies: public HTTPS ingress, applications and the
// gateway must be independent.
func telemetryUnit() []byte {
	return []byte(fmt.Sprintf(`[Unit]
Description=eregion authenticated telemetry gateway
Documentation=https://github.com/mbvlabs/eregion
Wants=network-online.target
After=network-online.target
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
WantedBy=multi-user.target
`, TelemetryCaddyPath, CaddyBinaryPath, TelemetryCaddyPath))
}

func (s *telemetryStep) Inputs() ([]byte, error) {
	dir, targets, caddy := s.paths()
	if s.hostname() != "" && len(s.targets()) == 0 {
		return nil, fmt.Errorf("the telemetry target document is invalid")
	}
	return canonicalInput(struct {
		Node        string `json:"node"`
		Hostname    string `json:"hostname"`
		Listen      string `json:"listen"`
		Upstream    string `json:"upstream"`
		Dir         string `json:"dir"`
		Targets     string `json:"targets"`
		TargetsPath string `json:"targets_path"`
		CaddyPath   string `json:"caddy_path"`
		Unit        string `json:"unit"`
		UnitPath    string `json:"unit_path"`
		LockPath    string `json:"lock_path"`
	}{s.cfg.Node, s.hostname(), TelemetryListenAddress(), "http://" + NodeExporterLoopbackAddress, dir, string(s.targets()), targets, caddy, string(s.unit()), s.unitPath(), s.rotationLockPath()})
}

func (s *telemetryStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, firewallCheckScript())
	if err != nil {
		return engine.CheckResult{}, err
	}
	if strings.Contains(output, "firewall:legacy") {
		return incomplete("the legacy WireGuard telemetry firewall rule is still present")
	}
	if strings.Contains(output, "firewall:error") {
		return incomplete("the legacy WireGuard telemetry firewall state could not be read")
	}
	if s.hostname() == "" {
		legacy, err := checkOutput(ctx, engine.Runtime{}, s.exec, s.serviceStateScript())
		if err != nil {
			return engine.CheckResult{}, err
		}
		if strings.Contains(legacy, "systemctl:unavailable") {
			return incomplete("the legacy telemetry service state could not be observed")
		}
		if strings.Contains(legacy, "enabled:yes") || strings.Contains(legacy, "active:yes") {
			return incomplete("the legacy telemetry service is still enabled or active although the node has no public hostname")
		}
		return complete("the public telemetry endpoint is unconfigured: the node has no public hostname; there is no WireGuard fallback listener")
	}
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
	// enables the gateway.
	configured := false
	if raw, mode, exists, err := fileState(caddy); err != nil {
		return engine.CheckResult{}, err
	} else if exists {
		if mode != 0o640 {
			return incomplete(fmt.Sprintf("%s has mode %04o; want 0640", caddy, mode))
		}
		hash, err := extractTelemetryHash(raw)
		if err != nil {
			return incomplete("the loopback telemetry gateway configuration is malformed and cannot be safely described")
		}
		if !bytes.Equal(raw, telemetryCaddyfile(hash)) {
			return incomplete("the loopback telemetry gateway configuration differs")
		}
		configured = true
	}
	script := s.checkScript(caddy)
	output, err = checkOutput(ctx, engine.Runtime{}, s.exec, script)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if strings.Contains(output, "firewall:error") {
		return incomplete("the legacy WireGuard telemetry firewall state could not be read")
	}
	if !strings.Contains(output, "enabled:ok") {
		return incomplete(TelemetryService + " is not enabled")
	}
	if configured && !strings.Contains(output, "active:ok") {
		return incomplete(TelemetryService + " is not active although its Caddyfile exists")
	}
	if !configured && strings.Contains(output, "active:unexpected") {
		return incomplete(TelemetryService + " is active without a configured Caddyfile")
	}
	detail := "telemetry gateway prerequisites are installed behind the public HTTPS ingress; run eregion metrics rotate to configure the credential"
	if configured {
		detail = "the authenticated telemetry gateway is configured on " + TelemetryListenAddress() + " behind https://" + s.hostname() + telemetry.MetricsPath
	}
	if strings.Contains(output, "firewall:unavailable") {
		detail += "; ufw is unavailable, so the legacy rule was not observed"
	}
	return complete(detail)
}

func (s *telemetryStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	source := "unconfigured"
	if s.hostname() != "" {
		if len(s.targets()) == 0 {
			return engine.ApplyResult{}, fmt.Errorf("the telemetry target document is invalid")
		}
		source = "https://" + s.hostname() + telemetry.MetricsPath
	}
	_, _, caddy := s.paths()
	err := engine.WithFileLock(s.rotationLockPath(), func() error {
		if s.hostname() == "" {
			return s.convergeUnconfiguredLocked(ctx, report)
		}
		snapshots, err := s.snapshotManagedFiles()
		if err != nil {
			return err
		}
		// Validate the complete existing managed Caddyfile under the lock
		// before any discovery, unit or service mutation. Only a supported
		// managed layout with the metrics principal can donate its hash.
		hash := ""
		if snapshots.caddy.existed {
			hash, err = extractTelemetryHash(snapshots.caddy.data)
			if err != nil {
				return contract.NewFault(contract.PreconditionFailed,
					"the existing telemetry gateway configuration cannot be safely reused",
					"Repair or replace the managed telemetry configuration, or run eregion metrics rotate deliberately. No credential was changed.")
			}
		}
		priorService, err := s.readServiceState(ctx)
		if err != nil {
			return err
		}
		if err := s.migrateManagedLocked(ctx, snapshots.caddy.existed, hash, report); err != nil {
			recoveryCtx, cancel := managedRecoveryContext(ctx)
			rollbackErr := s.rollbackManagedLocked(recoveryCtx, snapshots, priorService, report)
			cancel()
			return joinRecoveryError(err, rollbackErr)
		}
		return nil
	})
	if err != nil {
		return engine.ApplyResult{}, err
	}
	notes := []string{"the authenticated telemetry gateway listens only on " + TelemetryListenAddress() + "; node_exporter stays on " + NodeExporterLoopbackAddress}
	if s.hostname() == "" {
		notes = append(notes, "the public telemetry endpoint is unconfigured: the node has no public hostname")
	}
	return engine.ApplyResult{
		Notes: notes,
		Result: map[string]any{
			"listen": TelemetryListenAddress(), "hostname": s.hostname(), "source": source,
			"metrics_path": telemetry.MetricsPath, "discovery_path": telemetry.DiscoveryPath,
			"public": s.hostname() != "", "authentication": "basic",
			"targets": s.targetsPath, "caddyfile": caddy, "unit": s.unitPath(),
			"credential": "rotation-owned; run eregion metrics rotate",
		},
	}, nil
}

type telemetryFileSnapshot struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
	owner   string
	group   string
}

func snapshotTelemetryFile(path, owner, group string) (telemetryFileSnapshot, error) {
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return telemetryFileSnapshot{}, err
	}
	return telemetryFileSnapshot{path: path, data: raw, mode: mode, existed: exists, owner: owner, group: group}, nil
}

func (s telemetryFileSnapshot) restore() error {
	if !s.existed {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if s.mode == 0 {
		return fmt.Errorf("refusing to restore %s with mode 0000", s.path)
	}
	return writeFile(s.path, s.data, s.mode, s.owner, s.group)
}

type telemetryManagedSnapshot struct {
	targets telemetryFileSnapshot
	caddy   telemetryFileSnapshot
	unit    telemetryFileSnapshot
}

func (s *telemetryStep) snapshotManagedFiles() (telemetryManagedSnapshot, error) {
	_, targets, caddy := s.paths()
	targetSnapshot, err := snapshotTelemetryFile(targets, "root", "caddy")
	if err != nil {
		return telemetryManagedSnapshot{}, err
	}
	caddySnapshot, err := snapshotTelemetryFile(caddy, "root", "caddy")
	if err != nil {
		return telemetryManagedSnapshot{}, err
	}
	unitSnapshot, err := snapshotTelemetryFile(s.unitPath(), "root", "root")
	if err != nil {
		return telemetryManagedSnapshot{}, err
	}
	return telemetryManagedSnapshot{targets: targetSnapshot, caddy: caddySnapshot, unit: unitSnapshot}, nil
}

type telemetryServiceState struct {
	installed bool
	enabled   bool
	active    bool
}

func (s *telemetryStep) serviceStateScript() string {
	return fmt.Sprintf(`if ! command -v systemctl >/dev/null 2>&1; then echo systemctl:unavailable; exit 0; fi
if systemctl cat %[1]s >/dev/null 2>&1; then
  echo unit:present
  if systemctl is-enabled --quiet %[1]s; then echo enabled:yes; else echo enabled:no; fi
  if systemctl is-active --quiet %[1]s; then echo active:yes; else echo active:no; fi
else
  echo unit:absent
fi`, TelemetryService)
}

func (s *telemetryStep) readServiceState(ctx context.Context) (telemetryServiceState, error) {
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, s.serviceStateScript())
	if err != nil {
		return telemetryServiceState{}, interruptedError(ctx, err)
	}
	if strings.Contains(output, "systemctl:unavailable") {
		return telemetryServiceState{}, fmt.Errorf("systemd is unavailable for the telemetry gateway")
	}
	var state telemetryServiceState
	switch {
	case strings.Contains(output, "unit:absent"):
		return state, nil
	case strings.Contains(output, "unit:present"):
		state.installed = true
	default:
		return telemetryServiceState{}, fmt.Errorf("the telemetry gateway service state could not be observed")
	}
	if !strings.Contains(output, "enabled:yes") && !strings.Contains(output, "enabled:no") {
		return telemetryServiceState{}, fmt.Errorf("the telemetry gateway enabled state could not be observed")
	}
	if !strings.Contains(output, "active:yes") && !strings.Contains(output, "active:no") {
		return telemetryServiceState{}, fmt.Errorf("the telemetry gateway active state could not be observed")
	}
	state.enabled = strings.Contains(output, "enabled:yes")
	state.active = strings.Contains(output, "active:yes")
	return state, nil
}

func (s *telemetryStep) convergeUnconfiguredLocked(ctx context.Context, report engine.Reporter) error {
	script := firewallRemovalScript() + fmt.Sprintf(`set -euo pipefail
if ! command -v systemctl >/dev/null 2>&1; then echo "systemd is unavailable" >&2; exit 1; fi
if systemctl cat %[1]s >/dev/null 2>&1; then
  systemctl disable %[1]s
  systemctl stop %[1]s
fi
if systemctl is-enabled --quiet %[1]s; then echo "telemetry service is still enabled" >&2; exit 1; fi
if systemctl is-active --quiet %[1]s; then echo "telemetry service is still active" >&2; exit 1; fi`, TelemetryService)
	return interruptedError(ctx, s.exec.Run(ctx, s.ID()+"-unconfigured", script, nil, report))
}

func (s *telemetryStep) migrateManagedLocked(ctx context.Context, caddyExists bool, hash string, report engine.Reporter) error {
	dir, targets, caddy := s.paths()
	if err := ensureDir(dir, 0o755); err != nil {
		return err
	}
	if err := chownIfRoot(dir, "root", "root"); err != nil {
		return err
	}
	if err := writeFile(targets, s.targets(), 0o640, "root", "caddy"); err != nil {
		return err
	}
	if err := writePublicFile(s.unitPath(), s.unit(), "root", "root"); err != nil {
		return err
	}
	if caddyExists {
		if err := writeFile(caddy, telemetryCaddyfile(hash), 0o640, "root", "caddy"); err != nil {
			return err
		}
		if err := s.validateCaddy(ctx); err != nil {
			return err
		}
	}
	if err := s.exec.Run(ctx, s.ID()+"-service", s.serviceActivationScript(caddyExists), nil, report); err != nil {
		return interruptedError(ctx, err)
	}
	if caddyExists {
		return s.restartService(ctx)
	}
	return nil
}

func (s *telemetryStep) serviceActivationScript(caddyExists bool) string {
	return fmt.Sprintf(`set -euo pipefail
systemctl daemon-reload
systemctl enable %[1]s >/dev/null
%[2]s
if ! systemctl is-enabled --quiet %[1]s; then echo "telemetry service is not enabled" >&2; exit 1; fi
if [ %[3]t = false ]; then
  if systemctl is-active --quiet %[1]s; then systemctl stop %[1]s; fi
  if systemctl is-active --quiet %[1]s; then echo "telemetry service is still active without a Caddyfile" >&2; exit 1; fi
fi`, TelemetryService, firewallRemovalScript(), caddyExists)
}

func (s *telemetryStep) rollbackManagedLocked(ctx context.Context, snapshots telemetryManagedSnapshot, prior telemetryServiceState, report engine.Reporter) error {
	var errs []error
	for _, snapshot := range []telemetryFileSnapshot{snapshots.targets, snapshots.caddy, snapshots.unit} {
		if err := snapshot.restore(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.restoreServiceState(ctx, prior, report); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *telemetryStep) restoreServiceState(ctx context.Context, prior telemetryServiceState, report engine.Reporter) error {
	script := "set -euo pipefail\nsystemctl daemon-reload\n"
	if prior.installed {
		if prior.enabled {
			script += "systemctl enable " + TelemetryService + " >/dev/null\n"
			script += "if ! systemctl is-enabled --quiet " + TelemetryService + "; then echo 'telemetry service was not re-enabled' >&2; exit 1; fi\n"
		} else {
			script += "systemctl disable " + TelemetryService + " >/dev/null 2>&1 || true\n"
			script += "if systemctl is-enabled --quiet " + TelemetryService + "; then echo 'telemetry service is still enabled' >&2; exit 1; fi\n"
		}
	} else {
		script += "systemctl stop " + TelemetryService + " >/dev/null 2>&1 || true\n"
		script += "systemctl disable " + TelemetryService + " >/dev/null 2>&1 || true\n"
		script += "if systemctl is-active --quiet " + TelemetryService + "; then echo 'telemetry service is still active' >&2; exit 1; fi\n"
		script += "if systemctl is-enabled --quiet " + TelemetryService + "; then echo 'telemetry service is still enabled' >&2; exit 1; fi\n"
	}
	if err := s.exec.Run(ctx, s.ID()+"-restore-service", script, nil, report); err != nil {
		return interruptedError(ctx, err)
	}
	if !prior.installed {
		return nil
	}
	if prior.active {
		return s.restartService(ctx)
	}
	return s.stopService(ctx)
}

func (s *telemetryStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	dir, targets, caddy := s.paths()
	if s.hostname() == "" {
		return []string{
			"+ remove only the exact legacy WireGuard 9101 rule when present",
			"+ leave the telemetry public endpoint unconfigured: the node has no public hostname and there is no WireGuard fallback",
		}, nil
	}
	out := []string{
		"+ ensure " + dir + " is root:root 0755",
		"+ write " + targets + " (public HTTPS service discovery for " + s.hostname() + ":443, no secret)",
		"+ install " + TelemetryService + " as a loopback-only Caddy gateway",
		"+ remove only the exact legacy WireGuard 9101 rule when present; add no public telemetry listener",
		"+ keep " + caddy + " rotation-owned while preserving its existing bcrypt hash on apply",
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

// telemetryNodeIdentity reads the non-secret node name and public hostname.
func telemetryNodeIdentity() (node, hostname string) {
	node = "local"
	if config, err := LoadNodeFile(DefaultNodeConfigPath()); err == nil {
		if value := strings.TrimSpace(config.Node); value != "" {
			node = value
		}
		hostname = strings.TrimSpace(config.Hostname)
	}
	return node, hostname
}

// TelemetryEndpointInfo describes the endpoint without carrying the hash. A
// malformed managed gateway config fails closed rather than being reported as
// configured.
func TelemetryEndpointInfo() (telemetry.EndpointInfo, error) {
	node, hostname := telemetryNodeIdentity()
	raw, _, exists, err := fileState(TelemetryCaddyPath)
	if err != nil {
		return telemetry.EndpointInfo{}, err
	}
	return telemetryEndpointInfo(node, hostname, raw, exists)
}

// telemetryEndpointInfo describes a managed gateway file without opening it.
// A valid hash on an older render is needs_apply, not configured.
func telemetryEndpointInfo(node, hostname string, raw []byte, exists bool) (telemetry.EndpointInfo, error) {
	credential := false
	needsApply := false
	if exists {
		hash, err := extractTelemetryHash(raw)
		if err != nil {
			return telemetry.EndpointInfo{}, contract.NewFault(contract.PreconditionFailed,
				"the telemetry gateway configuration is malformed; refusing to describe or replace its credential",
				"Repair or replace the managed telemetry gateway configuration before continuing.")
		}
		credential = true
		needsApply = !bytes.Equal(raw, telemetryCaddyfile(hash))
	}
	info, err := telemetry.BuildEndpointInfo(node, hostname, "http://"+NodeExporterLoopbackAddress, credential)
	if err != nil {
		return telemetry.EndpointInfo{}, err
	}
	if needsApply && info.State != telemetry.EndpointStateUnconfigured {
		info.State = telemetry.EndpointStateNeedsApply
		info.Note = "A managed gateway credential exists, but the file is not the current loopback render. The URL is desired, not active; run eregion apply to migrate the gateway."
	}
	return info, nil
}

type managedCaddyNode struct {
	tokens   []string
	children []*managedCaddyNode
}

// parseManagedCaddyTree parses only the line-oriented block structure eregion
// writes. It is deliberately not a general Caddyfile parser: a managed file
// with unknown directives, missing outer structure or unbalanced braces must be
// rejected before its credential hash can be reused.
func parseManagedCaddyTree(raw []byte) ([]*managedCaddyNode, error) {
	if len(raw) > 64<<10 {
		return nil, fmt.Errorf("managed telemetry configuration is too large")
	}
	root := &managedCaddyNode{}
	stack := []*managedCaddyNode{root}
	header := false
	sawContent := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if !sawContent && !header && strings.HasPrefix(trimmed, "# managed by eregion") {
				header = true
				continue
			}
			return nil, fmt.Errorf("managed telemetry configuration has an unsupported comment")
		}
		if !header {
			return nil, fmt.Errorf("managed telemetry configuration has no managed header")
		}
		sawContent = true
		if trimmed == "}" {
			if len(stack) == 1 {
				return nil, fmt.Errorf("managed telemetry configuration has an unmatched closing brace")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		opening := strings.HasSuffix(trimmed, "{")
		if opening {
			trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "{"))
		} else if strings.ContainsAny(trimmed, "{}") {
			return nil, fmt.Errorf("managed telemetry configuration has invalid brace placement")
		}
		node := &managedCaddyNode{tokens: strings.Fields(trimmed)}
		parent := stack[len(stack)-1]
		parent.children = append(parent.children, node)
		if opening {
			stack = append(stack, node)
		}
	}
	if !header || len(stack) != 1 {
		return nil, fmt.Errorf("managed telemetry configuration has unbalanced blocks")
	}
	return root.children, nil
}

func managedTokens(node *managedCaddyNode, want ...string) bool {
	if node == nil || len(node.tokens) != len(want) {
		return false
	}
	for i := range want {
		if node.tokens[i] != want[i] {
			return false
		}
	}
	return true
}

func managedLeaf(node *managedCaddyNode, want ...string) bool {
	return managedTokens(node, want...) && len(node.children) == 0
}

func validateManagedReverseProxy(node *managedCaddyNode) error {
	if !managedTokens(node, "reverse_proxy", NodeExporterLoopbackAddress) {
		return fmt.Errorf("managed telemetry configuration has an unsupported metrics upstream")
	}
	rewrite, authorization, cookie, transports := 0, 0, 0, 0
	var transport *managedCaddyNode
	for _, child := range node.children {
		switch {
		case managedLeaf(child, "rewrite", "/metrics"):
			rewrite++
			if rewrite != 1 {
				return fmt.Errorf("managed telemetry configuration has a duplicate proxy rewrite")
			}
		case managedLeaf(child, "header_up", "-Authorization"):
			authorization++
			if authorization != 1 {
				return fmt.Errorf("managed telemetry configuration has a duplicate proxy header")
			}
		case managedLeaf(child, "header_up", "-Cookie"):
			cookie++
			if cookie != 1 {
				return fmt.Errorf("managed telemetry configuration has a duplicate proxy header")
			}
		case managedTokens(child, "transport", "http"):
			transports++
			if transports != 1 {
				return fmt.Errorf("managed telemetry configuration has a duplicate proxy transport")
			}
			transport = child
		default:
			return fmt.Errorf("managed telemetry configuration has an unsupported proxy directive")
		}
	}
	if rewrite != 1 || authorization != 1 || cookie != 1 {
		return fmt.Errorf("managed telemetry configuration is missing the metrics proxy contract")
	}
	if transport == nil {
		return nil
	}
	dial, response, read := false, false, false
	for _, child := range transport.children {
		switch {
		case managedLeaf(child, "dial_timeout", "2s"):
			dial = true
		case managedLeaf(child, "response_header_timeout", "15s"):
			response = true
		case managedLeaf(child, "read_timeout", "30s"):
			read = true
		default:
			return fmt.Errorf("managed telemetry configuration has an unsupported transport directive")
		}
	}
	if !dial || !response || !read || len(transport.children) != 3 {
		return fmt.Errorf("managed telemetry configuration is missing a proxy timeout")
	}
	return nil
}

// extractTelemetryHash validates the complete supported managed gateway layout
// and returns its single metrics bcrypt principal. It accepts the current
// loopback layout and the legacy WireGuard layout for a normal upgrade, and
// rejects everything else. Error messages never include file content, a hash or
// a password.
func extractTelemetryHash(raw []byte) (string, error) {
	children, err := parseManagedCaddyTree(raw)
	if err != nil {
		return "", err
	}
	if len(children) != 2 || !managedTokens(children[0]) || len(children[0].children) == 0 {
		return "", fmt.Errorf("managed telemetry configuration has an unsupported root layout")
	}
	options, site := children[0], children[1]
	if len(site.tokens) != 1 {
		return "", fmt.Errorf("managed telemetry configuration has an unsupported site")
	}
	siteHost := ""
	switch site.tokens[0] {
	case "http://" + telemetry.GatewayListenAddress:
		siteHost = telemetry.GatewayHost
	case "http://" + access.NodeAddress + ":" + telemetry.Port:
		siteHost = access.NodeAddress
	default:
		return "", fmt.Errorf("managed telemetry configuration has an unsupported listener")
	}
	admin, autoHTTPS := false, false
	for _, child := range options.children {
		switch {
		case managedLeaf(child, "admin", "off"):
			admin = true
		case managedLeaf(child, "auto_https", "off"):
			autoHTTPS = true
		default:
			return "", fmt.Errorf("managed telemetry configuration has an unsupported global option")
		}
	}
	if !admin || !autoHTTPS || len(options.children) != 2 {
		return "", fmt.Errorf("managed telemetry configuration is missing a global option")
	}
	bind := false
	hash := ""
	basicBlocks := 0
	discoveryHandlers, metricsHandlers, fallbackHandlers := 0, 0, 0
	for _, child := range site.children {
		switch {
		case len(child.tokens) == 2 && child.tokens[0] == "bind":
			if child.tokens[1] != siteHost || len(child.children) != 0 || bind {
				return "", fmt.Errorf("managed telemetry configuration has an unsupported bind")
			}
			bind = true
		case managedTokens(child, "basic_auth"):
			basicBlocks++
			if len(child.children) != 1 || len(child.children[0].tokens) != 2 ||
				child.children[0].tokens[0] != telemetry.Username ||
				!telemetry.ValidHash(child.children[0].tokens[1]) || len(child.children[0].children) != 0 {
				return "", fmt.Errorf("managed telemetry configuration has an invalid metrics principal")
			}
			hash = child.children[0].tokens[1]
		case managedTokens(child, "handle", telemetry.GatewayDiscoveryPath):
			discoveryHandlers++
			if len(child.children) != 2 || !managedLeaf(child.children[0], "root", "*", TelemetryDir) ||
				!managedLeaf(child.children[1], "file_server") {
				return "", fmt.Errorf("managed telemetry configuration has an unsupported discovery handler")
			}
		case managedTokens(child, "handle", telemetry.GatewayMetricsPath):
			metricsHandlers++
			if len(child.children) != 1 {
				return "", fmt.Errorf("managed telemetry configuration has an unsupported metrics handler")
			}
			if err := validateManagedReverseProxy(child.children[0]); err != nil {
				return "", err
			}
		case managedTokens(child, "handle"):
			fallbackHandlers++
			if len(child.children) != 1 || !managedLeaf(child.children[0], "respond", "404") {
				return "", fmt.Errorf("managed telemetry configuration has an unsupported fallback")
			}
		default:
			return "", fmt.Errorf("managed telemetry configuration has an unsupported site directive")
		}
	}
	if !bind || basicBlocks != 1 || hash == "" || discoveryHandlers != 1 || metricsHandlers != 1 || fallbackHandlers != 1 {
		return "", fmt.Errorf("managed telemetry configuration is missing a required contract")
	}
	return hash, nil
}

// telemetryCaddyfile is the loopback-only dedicated gateway. The bcrypt hash is
// the only credential material; the plaintext password never reaches the node.
// `admin off` avoids clashing with the public Caddy admin endpoint; rotation
// and apply restart this unit after validating the file. The reverse proxy
// strips the scraper credential before the raw exporter.
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
			transport http {
				dial_timeout 2s
				response_header_timeout 15s
				read_timeout 30s
			}
		}
	}
	handle {
		respond 404
	}
}
`, telemetry.GatewayListenAddress, telemetry.GatewayHost, telemetry.Username, hash, telemetry.GatewayDiscoveryPath, TelemetryDir, telemetry.GatewayMetricsPath, NodeExporterLoopbackAddress))
}

// activateTelemetryCaddy writes the rendered gateway config, validates it,
// restarts only the dedicated unit, and restores the previous managed file
// when validation or restart fails. The caller holds the telemetry lock.
func activateTelemetryCaddy(ctx context.Context, path string, previous []byte, previousMode os.FileMode, existed bool, rendered []byte, validate func(context.Context) error, restart func(context.Context) error, stop func(context.Context) error) error {
	if err := writeFile(path, rendered, 0o640, "root", "caddy"); err != nil {
		return err
	}
	if err := validate(ctx); err != nil {
		recoveryCtx, cancel := managedRecoveryContext(ctx)
		restoreErr := restoreTelemetryCaddy(recoveryCtx, path, previous, previousMode, existed, restart, stop)
		cancel()
		return joinRecoveryError(err, restoreErr)
	}
	if err := restart(ctx); err != nil {
		recoveryCtx, cancel := managedRecoveryContext(ctx)
		restoreErr := restoreTelemetryCaddy(recoveryCtx, path, previous, previousMode, existed, restart, stop)
		cancel()
		return joinRecoveryError(err, restoreErr)
	}
	return nil
}

func restoreTelemetryCaddy(ctx context.Context, path string, previous []byte, mode os.FileMode, existed bool, restart func(context.Context) error, stop func(context.Context) error) error {
	if !existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return stop(ctx)
	}
	if mode != 0 {
		if err := writeFile(path, previous, mode, "root", "caddy"); err != nil {
			return err
		}
	}
	return restart(ctx)
}

// buildTelemetryRotation is the non-secret rotation result. It contains only
// the desired public URLs; no credential material is part of it.
func buildTelemetryRotation(node, hostname string) (telemetry.Rotation, error) {
	metricsURL, err := telemetry.PublicMetricsURL(hostname)
	if err != nil {
		return telemetry.Rotation{}, err
	}
	discoveryURL, err := telemetry.PublicDiscoveryURL(hostname)
	if err != nil {
		return telemetry.Rotation{}, err
	}
	return telemetry.Rotation{
		Schema: telemetry.RotationSchema, Node: node, Hostname: hostname, Username: telemetry.Username,
		MetricsURL: metricsURL, Discovery: discoveryURL,
	}, nil
}

// RotateTelemetryCredential replaces the dedicated telemetry gateway hash. It
// follows vaire's rotate path: validate the new configuration, restart the
// gateway instance, and restore the previous file when either fails. A
// node-local lock serializes telemetry writes/reloads with host apply; public
// route writers and the public Caddy are never touched.
func RotateTelemetryCredential(ctx context.Context, confirm string, dryRun bool, hash string) (telemetry.Rotation, error) {
	node, hostname := telemetryNodeIdentity()
	if hostname == "" {
		return telemetry.Rotation{}, contract.NewFault(contract.PreconditionFailed,
			"the node has no public hostname, so the HTTPS telemetry endpoint is unconfigured",
			"Configure the main node's public hostname and public ingress before rotating the telemetry credential; there is no WireGuard fallback.")
	}
	rotation, err := buildTelemetryRotation(node, hostname)
	if err != nil {
		return telemetry.Rotation{}, contract.NewFault(contract.ValidationFailed, "the node's telemetry hostname is invalid", "Run eregion apply with a valid main hostname.")
	}
	token := node + "/metrics"
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
	err = engine.WithFileLock(TelemetryLockPath, func() error {
		previous, previousMode, existed, err := fileState(TelemetryCaddyPath)
		if err != nil {
			return err
		}
		return activateTelemetryCaddy(ctx, TelemetryCaddyPath, previous, previousMode, existed, telemetryCaddyfile(hash), func(ctx context.Context) error {
			return validateTelemetryCaddy(ctx)
		}, func(ctx context.Context) error {
			return restartTelemetryService(ctx)
		}, func(ctx context.Context) error {
			return stopTelemetryService(ctx)
		})
	})
	if err != nil {
		return telemetry.Rotation{}, err
	}
	return rotation, nil
}

type hostCommandRunner interface {
	Run(context.Context, string, ...string) error
}

type execHostCommandRunner struct{}

func (execHostCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	_, err := command.CombinedOutput()
	return err
}

func validateTelemetryCaddy(ctx context.Context) error {
	return validateTelemetryCaddyWithRunner(ctx, execHostCommandRunner{})
}

func validateTelemetryCaddyWithRunner(ctx context.Context, runner hostCommandRunner) error {
	if err := runner.Run(ctx, CaddyBinaryPath, "validate", "--config", TelemetryCaddyPath, "--adapter", "caddyfile"); err != nil {
		if cause := contextInterruption(ctx, err); cause != nil {
			return cause
		}
		return contract.NewFault(contract.PreconditionFailed,
			"Caddy refused the telemetry gateway configuration",
			"Check the generated loopback gateway configuration and retry.")
	}
	return nil
}

func restartTelemetryService(ctx context.Context) error {
	return restartTelemetryServiceWithRunner(ctx, execHostCommandRunner{})
}

func restartTelemetryServiceWithRunner(ctx context.Context, runner hostCommandRunner) error {
	if err := runner.Run(ctx, "systemctl", "restart", TelemetryService); err != nil {
		if cause := contextInterruption(ctx, err); cause != nil {
			return cause
		}
		return contract.NewFault(contract.Unavailable,
			"the telemetry gateway did not restart",
			"Check eregion-telemetry.service and the generated Caddyfile.")
	}
	if err := runner.Run(ctx, "systemctl", "is-active", "--quiet", TelemetryService); err != nil {
		if cause := contextInterruption(ctx, err); cause != nil {
			return cause
		}
		return contract.NewFault(contract.Unavailable,
			"the telemetry gateway is not active after restart",
			"Check eregion-telemetry.service and the generated Caddyfile.")
	}
	return nil
}

func stopTelemetryService(ctx context.Context) error {
	return stopTelemetryServiceWithRunner(ctx, execHostCommandRunner{})
}

func stopTelemetryServiceWithRunner(ctx context.Context, runner hostCommandRunner) error {
	if err := runner.Run(ctx, "systemctl", "stop", TelemetryService); err != nil {
		if cause := contextInterruption(ctx, err); cause != nil {
			return cause
		}
		return contract.NewFault(contract.Unavailable,
			"the telemetry gateway did not stop",
			"Check eregion-telemetry.service.")
	}
	if err := runner.Run(ctx, "systemctl", "is-active", "--quiet", TelemetryService); err == nil {
		return contract.NewFault(contract.Unavailable,
			"the telemetry gateway is still active after stop",
			"Check eregion-telemetry.service.")
	} else if cause := contextInterruption(ctx, err); cause != nil {
		return cause
	}
	return nil
}
