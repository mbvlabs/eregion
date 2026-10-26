package host

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/engine"
)

// node_exporter is pinned to an upstream release and verified by sha256 for
// each architecture, as in deploycrate-ce. The distribution package is not used:
// it listens on every interface on install.
const (
	nodeExporterVersion = "1.11.1"
	nodeExporterPort    = "9100"
)

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

// nodeExporterStep installs node_exporter on the node's WireGuard address only.
// The access setup opens 9100 on wg0 and starts the unit once wg0 exists; the
// step itself is versioned and idempotent like every other host step.
type nodeExporterStep struct {
	cfg  Config
	exec Exec
}

func (s *nodeExporterStep) ID() string { return "host-node-exporter-v1" }
func (s *nodeExporterStep) Describe() string {
	return "Install node_exporter " + nodeExporterVersion + " on the WireGuard address"
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
	return []byte(fmt.Sprintf(`[Unit]
Description=Prometheus node exporter for eregion
Documentation=https://github.com/prometheus/node_exporter
Wants=network-online.target wg-quick@wg0.service
After=network-online.target wg-quick@wg0.service
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=node_exporter
Group=node_exporter
ExecStart=%s --web.listen-address=%s
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
`, s.binaryPath(), access.NodeAddress+":"+nodeExporterPort))
}

func (s *nodeExporterStep) Inputs() ([]byte, error) {
	arch, err := s.arch()
	if err != nil {
		return nil, err
	}
	return canonicalInput(struct {
		Version  string `json:"version"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
		Unit     string `json:"unit"`
		UnitPath string `json:"unit_path"`
		Binary   string `json:"binary"`
	}{nodeExporterVersion, arch, nodeExporterSHA256[arch], string(s.unit()), s.unitPath(), s.binaryPath()})
}

func (s *nodeExporterStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	arch, err := s.arch()
	if err != nil {
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
if ip -4 addr show wg0 2>/dev/null | grep -q 'inet %[3]s/'; then echo wg:up; else echo wg:down; fi
if systemctl is-active --quiet node-exporter.service; then echo active:ok; else echo active:missing; fi`,
		s.binaryPath(), nodeExporterVersion, access.NodeAddress)
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
	// The unit can only bind after wg0 exists, so a host that has not brought
	// up WireGuard yet is complete once the unit is installed and enabled.
	if !strings.Contains(output, "wg:up") {
		return complete("node_exporter " + nodeExporterVersion + " is installed and enabled; it starts with wg0 (arch " + arch + ")")
	}
	if !strings.Contains(output, "active:ok") {
		return incomplete("node-exporter.service is not active although wg0 is up")
	}
	return complete("node_exporter " + nodeExporterVersion + " answers on " + access.NodeAddress + ":" + nodeExporterPort)
}

func (s *nodeExporterStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	arch, err := s.arch()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	script := fmt.Sprintf(`set -euo pipefail
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
		nodeExporterSHA256[arch], arch)
	if err := s.exec.Run(ctx, s.ID()+"-binary", script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(s.unitPath(), s.unit(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	enable := `set -euo pipefail
if command -v ufw >/dev/null 2>&1; then
  ufw allow in on wg0 to ` + access.NodeAddress + ` port ` + nodeExporterPort + ` proto tcp >/dev/null || true
fi
systemctl daemon-reload
systemctl enable node-exporter.service
if ip -4 addr show wg0 2>/dev/null | grep -q 'inet ` + access.NodeAddress + `/'; then
  systemctl restart node-exporter.service
  systemctl is-active --quiet node-exporter.service
fi`
	if err := s.exec.Run(ctx, s.ID()+"-service", enable, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{
		Notes: []string{
			"node_exporter " + nodeExporterVersion + " is installed; it listens only on " + access.NodeAddress + ":" + nodeExporterPort + " and starts with wg0",
		},
		Result: map[string]any{"version": nodeExporterVersion, "listen": access.NodeAddress + ":" + nodeExporterPort, "unit": s.unitPath()},
	}, nil
}

func (s *nodeExporterStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	arch, err := s.arch()
	if err != nil {
		return nil, err
	}
	out := []string{
		"+ install node_exporter " + nodeExporterVersion + " (" + arch + ", sha256 " + nodeExporterSHA256[arch] + ")",
		"+ write " + s.unitPath() + " listening on " + access.NodeAddress + ":" + nodeExporterPort,
		"+ allow " + nodeExporterPort + "/tcp on wg0 only",
	}
	if ok, _, _ := sameFileContent(s.unitPath(), s.unit()); !ok {
		out = append(out, "~ restart node-exporter.service for the desired unit")
	}
	return out, nil
}

func (s *nodeExporterStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
