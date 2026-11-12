package host

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

// The registry image is pinned by digest in platform.RegistryImage, which the
// host step and the retention garbage-collect effect share.

type registryStep struct {
	cfg  Config
	exec Exec
}

func (s *registryStep) ID() string { return "host-registry-v2" }
func (s *registryStep) Describe() string {
	return "Install the local OCI registry with manifest deletion enabled and a garbage-collect path"
}
func (s *registryStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *registryStep) configPath() string {
	return filepath.Join(s.cfg.Layout.ConfigDir, "registry", "config.yml")
}
func (s *registryStep) unitPath() string {
	return filepath.Join(s.cfg.Layout.SystemdDir, "eregion-registry.service")
}

func (s *registryStep) config() []byte {
	return []byte(`version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: /var/lib/registry
  delete:
    enabled: true
http:
  addr: :5000
  headers:
    X-Content-Type-Options: [nosniff]
`)
}

func (s *registryStep) unit() []byte {
	return []byte(fmt.Sprintf(`[Unit]
Description=eregion local OCI registry
Documentation=https://github.com/mbvlabs/eregion
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
Restart=on-failure
RestartSec=5s
ExecStartPre=/usr/bin/docker pull %s
ExecStart=/usr/bin/docker run --rm --name `+platform.RegistryContainer+` --env REGISTRY_STORAGE_DELETE_ENABLED=true --publish %s:5000 --volume %s:/var/lib/registry --volume %s:/etc/docker/registry/config.yml:ro %s
ExecStop=/usr/bin/docker stop `+platform.RegistryContainer+`
TimeoutStopSec=30s

[Install]
WantedBy=multi-user.target
`, platform.RegistryImage, s.cfg.RegistryListen, s.cfg.Layout.RegistryDir, s.configPath(), platform.RegistryImage))
}

func (s *registryStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Image  string `json:"image"`
		Listen string `json:"listen"`
		Config string `json:"config"`
		Unit   string `json:"unit"`
		Dir    string `json:"dir"`
	}{platform.RegistryImage, s.cfg.RegistryListen, contentSHA256(s.config()), contentSHA256(s.unit()), s.cfg.Layout.RegistryDir})
}

func (s *registryStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	configOK, _, err := sameFileContent(s.configPath(), s.config())
	if err != nil {
		return engine.CheckResult{}, err
	}
	unitOK, _, err := sameFileContent(s.unitPath(), s.unit())
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !configOK || !unitOK {
		return incomplete("registry configuration or unit file differs from the desired pinned image")
	}
	script := fmt.Sprintf(`if systemctl is-enabled eregion-registry.service >/dev/null 2>&1; then echo enabled:ok; else echo enabled:missing; fi
if systemctl is-active eregion-registry.service >/dev/null 2>&1; then echo active:ok; else echo active:missing; fi
if docker image inspect %s >/dev/null 2>&1; then echo image:ok; else echo image:missing; fi
if docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' eregion-registry 2>/dev/null | grep -q '^REGISTRY_STORAGE_DELETE_ENABLED=true$'; then echo deletion:ok; else echo deletion:missing; fi`, shellQuote(platform.RegistryImage))
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
		return incomplete("registry runtime not ready: " + strings.Join(missing, ", "))
	}
	return complete("pinned registry image is installed and the service is enabled and active on " + s.cfg.RegistryListen)
}

func (s *registryStep) Apply(ctx context.Context, runtime engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if err := writePublicFile(s.configPath(), s.config(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(s.unitPath(), s.unit(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	script := fmt.Sprintf(`set -euo pipefail
systemctl daemon-reload
/usr/bin/docker pull %s
systemctl enable eregion-registry.service
systemctl restart eregion-registry.service
systemctl is-active --quiet eregion-registry.service`, shellQuote(platform.RegistryImage))
	if err := s.exec.Run(ctx, s.ID(), script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"local registry running at " + s.cfg.RegistryListen + " with manifest deletes enabled"}, Result: map[string]any{"image": platform.RegistryImage, "listen": s.cfg.RegistryListen}}, nil
}

func (s *registryStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	out := []string{"+ registry image " + platform.RegistryImage, "+ registry listen " + s.cfg.RegistryListen, "+ registry manifest deletion enabled with garbage collection"}
	raw, _, exists, err := fileState(s.configPath())
	if err != nil {
		return nil, err
	}
	if !exists || string(raw) != string(s.config()) {
		out = append(out, simpleDiff(s.configPath(), raw, s.config())...)
	}
	raw, _, exists, err = fileState(s.unitPath())
	if err != nil {
		return nil, err
	}
	if !exists || string(raw) != string(s.unit()) {
		out = append(out, simpleDiff(s.unitPath(), raw, s.unit())...)
	}
	return out, nil
}

func (s *registryStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
