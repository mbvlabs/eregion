package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

type serviceStep struct {
	cfg          Config
	exec         Exec
	name         string
	service      string
	describe     string
	unit         func() []byte
	dependencies []string
}

func (s *serviceStep) ID() string                      { return "host-" + s.name + "-v1" }
func (s *serviceStep) Describe() string                { return s.describe }
func (s *serviceStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *serviceStep) unitPath() string {
	return filepath.Join(s.cfg.Layout.SystemdDir, s.service)
}

func (s *serviceStep) revisionPath() string {
	return filepath.Join(s.cfg.Layout.StateDir, s.name+".revision")
}

// revision covers the unit plus every file a running process loaded at start.
// Without it, replacing the binary or TLS material would leave Check reporting
// a healthy old process and no restart would happen.
func (s *serviceStep) revision() (string, error) {
	hash := sha256.New()
	hash.Write([]byte("eregion/host/service-revision/v1\x00" + s.service + "\x00"))
	hash.Write(s.unit())
	hash.Write([]byte{0})
	for _, path := range s.dependencies {
		raw, _, exists, err := fileState(path)
		if err != nil {
			return "", err
		}
		if exists {
			hash.Write([]byte(contentSHA256(raw)))
		} else {
			hash.Write([]byte("missing"))
		}
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *serviceStep) Inputs() ([]byte, error) {
	body := s.unit()
	revision, err := s.revision()
	if err != nil {
		return nil, err
	}
	return canonicalInput(struct {
		Unit     string   `json:"unit"`
		Path     string   `json:"path"`
		Hash     string   `json:"hash"`
		Revision string   `json:"revision"`
		Deps     []string `json:"deps"`
	}{string(body), s.unitPath(), contentSHA256(body), revision, s.dependencies})
}

func (s *serviceStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	want := s.unit()
	ok, _, err := sameFileContent(s.unitPath(), want)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok {
		return incomplete("unit " + s.unitPath() + " differs from the desired service definition")
	}
	revision, err := s.revision()
	if err != nil {
		return engine.CheckResult{}, err
	}
	ok, _, err = sameFileContent(s.revisionPath(), []byte(revision+"\n"))
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok {
		return incomplete(s.service + " has not been restarted for the current binary/configuration revision")
	}
	script := fmt.Sprintf(`if systemctl is-enabled %s >/dev/null 2>&1; then echo enabled:ok; else echo enabled:missing; fi
if systemctl is-active %s >/dev/null 2>&1; then echo active:ok; else echo active:missing; fi`, s.service, s.service)
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, script)
	if err != nil {
		return engine.CheckResult{}, err
	}
	var missing []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasSuffix(line, ":missing") {
			missing = append(missing, strings.TrimSuffix(line, ":missing"))
		}
	}
	if len(missing) > 0 {
		return incomplete(s.service + " not ready: " + strings.Join(missing, ", "))
	}
	return complete(s.service + " is enabled and active with the desired unit")
}

func (s *serviceStep) Apply(ctx context.Context, runtime engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if err := applyUnit(ctx, s.exec, s.unitPath(), s.unit(), s.service, report); err != nil {
		return engine.ApplyResult{}, err
	}
	revision, err := s.revision()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFile(s.revisionPath(), []byte(revision+"\n"), 0o600, "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{s.service + " is enabled and active"}, Result: map[string]any{"service": s.service, "unit": s.unitPath(), "revision": revision}}, nil
}

func (s *serviceStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	current, _, exists, err := fileState(s.unitPath())
	if err != nil {
		return nil, err
	}
	if !exists {
		return simpleDiff(s.unitPath(), nil, s.unit()), nil
	}
	if string(current) == string(s.unit()) {
		return []string{"+ unit is current; ensure " + s.service + " is enabled and active"}, nil
	}
	return simpleDiff(s.unitPath(), current, s.unit()), nil
}

func (s *serviceStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}

func applyUnit(ctx context.Context, exec Exec, path string, desired []byte, service string, report engine.Reporter) error {
	previous, mode, exists, err := fileState(path)
	if err != nil {
		return err
	}
	if err := writePublicFile(path, desired, "root", "root"); err != nil {
		return err
	}
	script := fmt.Sprintf("set -euo pipefail\nsystemctl daemon-reload\nsystemctl enable %s\nsystemctl restart %s\nsystemctl is-active --quiet %s", service, service, service)
	if err := exec.Run(ctx, "host-"+service+"-apply", script, nil, report); err != nil {
		// Best-effort immediate failure cleanup, NOT timed commit-confirm.
		if exists {
			_ = writeFile(path, previous, mode, "root", "root")
		} else {
			_ = os.Remove(path)
			_ = exec.Run(ctx, "host-"+service+"-rollback", "systemctl disable "+service+" 2>/dev/null || true\nsystemctl stop "+service+" 2>/dev/null || true\nsystemctl daemon-reload", nil, report)
		}
		if exists {
			_ = exec.Run(ctx, "host-"+service+"-rollback", "systemctl daemon-reload\nsystemctl restart "+service+" 2>/dev/null || true", nil, report)
		}
		return err
	}
	return nil
}

func apiServiceUnit(cfg Config) []byte {
	args := []string{"node", "serve", "--listen", cfg.APIListen, "--workers=false"}
	if cfg.APITLSCert != "" {
		args = append(args, "--tls-cert", filepath.Join(cfg.Layout.ConfigDir, "tls", "server.crt"))
		args = append(args, "--tls-key", filepath.Join(cfg.Layout.ConfigDir, "tls", "server.key"))
	}
	return []byte(fmt.Sprintf(`[Unit]
Description=eregion control-plane API
Documentation=https://github.com/mbvlabs/eregion
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
SupplementaryGroups=docker
EnvironmentFile=%s
ExecStart=%s %s
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=%s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, cfg.Layout.User, cfg.Layout.Group, filepath.Join(cfg.Layout.ConfigDir, "control-plane.env"), filepath.Join(cfg.Layout.BinDir, "eregion"), strings.Join(args, " "), cfg.Layout.DataDir))
}

func workerServiceUnit(cfg Config) []byte {
	return []byte(fmt.Sprintf(`[Unit]
Description=eregion River worker
Documentation=https://github.com/mbvlabs/eregion
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
SupplementaryGroups=docker
EnvironmentFile=%s
ExecStart=%s node worker --concurrency %d
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=%s -%s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, cfg.Layout.User, cfg.Layout.Group, filepath.Join(cfg.Layout.ConfigDir, "control-plane.env"), filepath.Join(cfg.Layout.BinDir, "eregion"), cfg.WorkerConcurrency, cfg.Layout.DataDir, platform.CaddyRoutesDir))
}

func newAPIService(cfg Config, exec Exec) *serviceStep {
	deps := []string{filepath.Join(cfg.Layout.BinDir, "eregion"), filepath.Join(cfg.Layout.ConfigDir, "control-plane.env")}
	if cfg.APITLSCert != "" {
		deps = append(deps, filepath.Join(cfg.Layout.ConfigDir, "tls", "server.crt"), filepath.Join(cfg.Layout.ConfigDir, "tls", "server.key"))
	}
	step := &serviceStep{
		cfg: cfg, exec: exec, name: "api-service", service: "eregion-api.service",
		describe: "Install and start the systemd API service", unit: func() []byte { return apiServiceUnit(cfg) },
		dependencies: deps,
	}
	return step
}

func newWorkerService(cfg Config, exec Exec) *serviceStep {
	return &serviceStep{
		cfg: cfg, exec: exec, name: "worker-service", service: "eregion-worker.service",
		describe: "Install and start the systemd River worker service", unit: func() []byte { return workerServiceUnit(cfg) },
		dependencies: []string{filepath.Join(cfg.Layout.BinDir, "eregion"), filepath.Join(cfg.Layout.ConfigDir, "control-plane.env")},
	}
}
