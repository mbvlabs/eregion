package host

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

// node_exporter is pinned to an upstream release and verified by sha256 for
// each architecture, as in deploycrate-ce. The distribution package is not used:
// it listens on every interface on install.
const (
	nodeExporterVersion = "1.11.1"
	// NodeExporterDir is node_exporter's own state directory. Both it and the
	// textfile directory are root:root 0755 and must not be symlinks.
	NodeExporterDir         = "/var/lib/node_exporter"
	NodeExporterTextfileDir = NodeExporterDir + "/textfile"
	// nodeExporterCollectorUnits is deliberately narrow: eregion's own units,
	// the container/ingress services and its WireGuard interface. It does not
	// collect every systemd unit on the host.
	nodeExporterCollectorUnits = `(eregion-.+|caddy|docker|node-exporter|wg-quick@wg0)[.](service|timer)`
)

// NodeExporterListenAddress is the only node_exporter listener. It is
// loopback: the authenticated telemetry service is the WireGuard-facing edge,
// so a WireGuard peer cannot bypass basic authentication by dialing 9100.
func NodeExporterListenAddress() string {
	return NodeExporterLoopbackAddress
}

// NodeExporterMetricsURL is the node-local read endpoint used by metrics.show
// and doctor. It is not reachable from another host.
func NodeExporterMetricsURL() string {
	return "http://" + NodeExporterListenAddress() + "/metrics"
}

var nodeExporterSHA256 = map[string]string{
	"amd64": "9f5ea48e5bc7b656f8a91a32e7d7deb89f70f73dabd0d974418aca15f37d6810",
	"arm64": "ba1886efbd76cb96b0087c695ea8d1b9cb6e8aa946c996d744e9ee16c8e3591a",
}

func nodeExporterArchive(arch string) string {
	return "node_exporter-" + nodeExporterVersion + ".linux-" + arch + ".tar.gz"
}

func nodeExporterURL(arch string) string {
	return "https://github.com/prometheus/node_exporter/releases/download/v" + nodeExporterVersion + "/" + nodeExporterArchive(arch)
}

// nodeExporterStep installs node_exporter on loopback. It starts with its own
// unit and does not wait for WireGuard; the separate authenticated telemetry
// endpoint is the only WireGuard-facing listener. The step is versioned and
// idempotent like every other host step.
type nodeExporterStep struct {
	cfg  Config
	exec Exec
	// parentDir/textfileDir are normally the constants below. Tests set them
	// to a temporary tree so the strict directory checks can be exercised
	// without root-owned /var/lib paths.
	parentDir   string
	textfileDir string
}

func (s *nodeExporterStep) dirs() (string, string) {
	parent, textfile := s.parentDir, s.textfileDir
	if parent == "" {
		parent = NodeExporterDir
	}
	if textfile == "" {
		textfile = NodeExporterTextfileDir
	}
	return parent, textfile
}

func (s *nodeExporterStep) ID() string { return "host-node-exporter-v2" }
func (s *nodeExporterStep) Describe() string {
	return "Install node_exporter " + nodeExporterVersion + " on loopback with textfile and narrow systemd collectors"
}
func (s *nodeExporterStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *nodeExporterStep) arch() (string, error) {
	arch := s.cfg.Arch
	if arch == "" {
		// A node-local bootstrap has no explicit arch; the running binary is
		// already the node's own build.
		arch = runtime.GOARCH
	}
	switch arch {
	case "amd64", "arm64":
		return arch, nil
	default:
		return "", fmt.Errorf("node_exporter has no build for %s", arch)
	}
}

func (s *nodeExporterStep) unitPath() string {
	return filepath.Join(s.cfg.Layout.SystemdDir, "node-exporter.service")
}

func (s *nodeExporterStep) binaryPath() string {
	return filepath.Join(s.cfg.Layout.BinDir, "node_exporter")
}

func (s *nodeExporterStep) unit() []byte {
	_, textfile := s.dirs()
	return []byte(fmt.Sprintf(`[Unit]
Description=Prometheus node exporter for eregion
Documentation=https://github.com/prometheus/node_exporter
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=node_exporter
Group=node_exporter
ExecStart=%s --web.listen-address=%s --collector.textfile.directory=%s --collector.systemd --collector.systemd.unit-include=%s
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
ProtectSystem=strict
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
SystemCallArchitectures=native
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
`, s.binaryPath(), NodeExporterListenAddress(), textfile, nodeExporterCollectorUnits))
}

func (s *nodeExporterStep) Inputs() ([]byte, error) {
	arch, err := s.arch()
	if err != nil {
		return nil, err
	}
	parent, textfile := s.dirs()
	return canonicalInput(struct {
		Version     string `json:"version"`
		Arch        string `json:"arch"`
		SHA256      string `json:"sha256"`
		Unit        string `json:"unit"`
		UnitPath    string `json:"unit_path"`
		Binary      string `json:"binary"`
		ParentDir   string `json:"parent_dir"`
		TextfileDir string `json:"textfile_dir"`
		Collectors  string `json:"collector_units"`
	}{nodeExporterVersion, arch, nodeExporterSHA256[arch], string(s.unit()), s.unitPath(), s.binaryPath(), parent, textfile, nodeExporterCollectorUnits})
}

func (s *nodeExporterStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	if _, err := s.arch(); err != nil {
		return engine.CheckResult{}, err
	}
	ok, _, err := sameFileContent(s.unitPath(), s.unit())
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok {
		return incomplete("unit " + s.unitPath() + " differs from the desired node_exporter definition")
	}
	script := fmt.Sprintf(`if [ -x %[1]s ] && %[1]s --version 2>/dev/null | grep -q '%[2]s'; then echo version:ok; else echo version:missing; fi
if systemctl is-enabled node-exporter.service >/dev/null 2>&1; then echo enabled:ok; else echo enabled:missing; fi
if systemctl is-active --quiet node-exporter.service; then echo active:ok; else echo active:missing; fi`,
		s.binaryPath(), nodeExporterVersion)
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, script)
	if err != nil {
		return engine.CheckResult{}, err
	}
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case "version:missing":
			return incomplete("node_exporter " + nodeExporterVersion + " is not installed at " + s.binaryPath())
		case "enabled:missing":
			return incomplete("node-exporter.service is not enabled")
		}
	}
	parent, textfile := s.dirs()
	if err := validateNodeExporterApplyDirectories(parent, textfile); err != nil {
		return incomplete(err.Error())
	}
	if err := nodeExporterStrictDirectoryState(parent, textfile); err != nil {
		return incomplete(err.Error())
	}
	if !strings.Contains(output, "active:ok") {
		return incomplete("node-exporter.service is not active")
	}
	return complete("node_exporter " + nodeExporterVersion + " answers on " + NodeExporterListenAddress() + " with textfile and narrow systemd collectors")
}

// nodeExporterDirectoryModeClear deliberately clears special bits in a second
// command. chmod with a numeric mode can preserve setgid on a directory (GNU
// coreutils may do so), so 0755 alone is not an exact-mode repair. Tests
// execute these same commands against real temporary directories with
// setgid/sticky bits.
const nodeExporterDirectoryModeClear = `chmod 0755 "$parent" "$textfile"
chmod u-s,g-s,o-t "$parent" "$textfile"`

// nodeExporterDirectoryScript is the shell fallback that refuses symlinks and
// non-directories again immediately before install. The Go preflight in Apply
// also requires trusted ancestors and safe root ownership/writability first, so
// an unprivileged parent cannot swap the child between these checks.
func nodeExporterDirectoryScript(parent, textfile string) string {
	return fmt.Sprintf(`parent=%q
textfile=%q
for path in "$parent" "$textfile"; do
  if [ -L "$path" ]; then
    echo "refusing to use symlinked node_exporter path ${path}" >&2
    exit 1
  fi
  if [ -e "$path" ] && [ ! -d "$path" ]; then
    echo "refusing non-directory node_exporter path ${path}" >&2
    exit 1
  fi
done
install -d -m 0755 -o root -g root "$parent" "$textfile"
chown root:root "$parent" "$textfile"
%s
for path in "$parent" "$textfile"; do
  mode="$(stat -c '%%a' "$path")"
  if [ "$mode" != "755" ]; then
    echo "refusing to leave node_exporter path ${path} with mode ${mode}; want 0755" >&2
    exit 1
  fi
done`, parent, textfile, nodeExporterDirectoryModeClear)
}

func (s *nodeExporterStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	arch, err := s.arch()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	parent, textfile := s.dirs()
	if err := validateNodeExporterApplyDirectories(parent, textfile); err != nil {
		return engine.ApplyResult{}, err
	}
	directoryScript := nodeExporterDirectoryScript(parent, textfile)
	script := fmt.Sprintf(`set -euo pipefail
%[7]s
if ! id node_exporter >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin node_exporter
fi
if ! { [ -x %[1]s ] && %[1]s --version 2>/dev/null | grep -q '%[2]s'; }; then
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT
  curl --fail --silent --show-error --location --retry 3 --output "${temporary}/%[3]s" %[4]s
  printf '%%s  %%s\n' %[5]s "${temporary}/%[3]s" | sha256sum --check --status
  tar --extract --gzip --file "${temporary}/%[3]s" --directory "${temporary}"
  install -o root -g root -m 0755 "${temporary}/node_exporter-%[2]s.linux-%[6]s/node_exporter" %[1]s
fi`,
		s.binaryPath(), nodeExporterVersion, nodeExporterArchive(arch), nodeExporterURL(arch),
		nodeExporterSHA256[arch], arch, directoryScript)
	if err := s.exec.Run(ctx, s.ID()+"-binary", script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(s.unitPath(), s.unit(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	// node_exporter is loopback-only and independent of WireGuard. It starts
	// with its own unit.
	enable := `set -euo pipefail
systemctl daemon-reload
systemctl enable node-exporter.service
systemctl restart node-exporter.service
systemctl is-active --quiet node-exporter.service`
	if err := s.exec.Run(ctx, s.ID()+"-service", enable, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{
		Notes: []string{
			"node_exporter " + nodeExporterVersion + " is installed; it listens only on " + NodeExporterListenAddress() + " and reads " + textfile + " plus the narrow systemd unit set",
		},
		Result: map[string]any{"version": nodeExporterVersion, "listen": NodeExporterListenAddress(), "unit": s.unitPath(), "parent_dir": parent, "textfile_dir": textfile, "collector_units": nodeExporterCollectorUnits},
	}, nil
}

func (s *nodeExporterStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	arch, err := s.arch()
	if err != nil {
		return nil, err
	}
	parent, textfile := s.dirs()
	out := []string{
		"+ install node_exporter " + nodeExporterVersion + " (" + arch + ", sha256 " + nodeExporterSHA256[arch] + ")",
		"+ write " + s.unitPath() + " listening on " + NodeExporterListenAddress(),
		"+ enable the textfile collector at " + textfile + " (root:root 0755, no symlink; trusted parent " + parent + ") and the narrow systemd unit set " + nodeExporterCollectorUnits,
		"+ allow no WireGuard port for node_exporter; the authenticated telemetry endpoint is the only WireGuard listener",
	}
	if ok, _, _ := sameFileContent(s.unitPath(), s.unit()); !ok {
		out = append(out, "~ restart node-exporter.service for the desired unit")
	}
	return out, nil
}

func (s *nodeExporterStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
