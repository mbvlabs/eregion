package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
)

type dockerStep struct {
	exec Exec
}

const dockerDaemonConfig = "/etc/docker/daemon.json"

func (s *dockerStep) ID() string                      { return "host-docker-v2" }
func (s *dockerStep) Describe() string                { return "Enable Docker with bounded local container logging" }
func (s *dockerStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *dockerStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Service string            `json:"service"`
		Driver  string            `json:"driver"`
		Options map[string]string `json:"options"`
	}{"docker.service", "local", dockerLogOptions()})
}

func dockerLogOptions() map[string]string {
	return map[string]string{"max-size": "20m", "max-file": "5", "compress": "true"}
}

// Preserve unrelated daemon configuration while making log options exact.
func dockerDesiredConfig(raw []byte) ([]byte, bool, error) {
	config := map[string]json.RawMessage{}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &config); err != nil || config == nil {
			return nil, false, fmt.Errorf("Docker daemon configuration is not a JSON object")
		}
	}
	var driver string
	var options map[string]string
	_ = json.Unmarshal(config["log-driver"], &driver)
	_ = json.Unmarshal(config["log-opts"], &options)
	want := dockerLogOptions()
	matches := driver == "local" && len(options) == len(want)
	for key, value := range want {
		matches = matches && options[key] == value
	}
	config["log-driver"], _ = json.Marshal("local")
	config["log-opts"], _ = json.Marshal(want)
	result, err := json.MarshalIndent(config, "", "  ")
	return append(result, '\n'), matches, err
}

func (s *dockerStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	raw, _, exists, err := fileState(dockerDaemonConfig)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if exists && len(raw) == 0 {
		return engine.CheckResult{}, fmt.Errorf("Docker daemon configuration is empty")
	}
	_, matches, err := dockerDesiredConfig(raw)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !exists || !matches {
		return incomplete("Docker daemon logging configuration differs from the bounded local default")
	}
	script := `if systemctl is-enabled docker.service >/dev/null 2>&1; then echo enabled:ok; else echo enabled:missing; fi
if systemctl is-active docker.service >/dev/null 2>&1; then echo active:ok; else echo active:missing; fi
if [ "$(docker info --format '{{.LoggingDriver}}' 2>/dev/null)" = local ]; then echo docker:ok; else echo docker:missing; fi
if dockerd --validate --config-file /etc/docker/daemon.json >/dev/null 2>&1; then echo config:ok; else echo config:missing; fi
started=$(systemctl show docker.service -p ExecMainStartTimestamp --value)
started_epoch=$(date -d "$started" +%s 2>/dev/null || true)
config_epoch=$(stat -c %Y /etc/docker/daemon.json 2>/dev/null || true)
if [ -n "$started_epoch" ] && [ -n "$config_epoch" ] && [ "$started_epoch" -ge "$config_epoch" ]; then echo loaded:ok; else echo loaded:missing; fi`
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
		return incomplete("docker runtime not ready: " + strings.Join(missing, ", "))
	}
	return complete("docker.service is enabled and active with bounded local logging configured")
}

func (s *dockerStep) Apply(ctx context.Context, runtime engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	raw, mode, exists, err := fileState(dockerDaemonConfig)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if exists && len(raw) == 0 {
		return engine.ApplyResult{}, fmt.Errorf("Docker daemon configuration is empty")
	}
	previousState, err := s.exec.Output(ctx, `if systemctl is-active --quiet docker.service; then echo active; else echo inactive; fi`)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	wasActive := strings.TrimSpace(previousState) == "active"
	desired, matches, err := dockerDesiredConfig(raw)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	needsRestart := !matches
	if matches && wasActive {
		loaded, err := s.exec.Output(ctx, `started=$(systemctl show docker.service -p ExecMainStartTimestamp --value)
started_epoch=$(date -d "$started" +%s 2>/dev/null || true)
config_epoch=$(stat -c %Y /etc/docker/daemon.json 2>/dev/null || true)
if [ "$(docker info --format '{{.LoggingDriver}}' 2>/dev/null)" = local ] && [ -n "$started_epoch" ] && [ -n "$config_epoch" ] && [ "$started_epoch" -ge "$config_epoch" ]; then echo yes; else echo no; fi`)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		needsRestart = strings.TrimSpace(loaded) != "yes"
	}
	if needsRestart {
		// This is a fresh-install setting, not a migration of running workloads.
		check := `if systemctl is-active --quiet docker.service; then
  ids=$(docker ps -aq) || exit 1
  if [ -n "$ids" ]; then echo "Docker logging configuration requires a fresh host without existing containers" >&2; exit 1; fi
fi`
		if err := s.exec.Run(ctx, s.ID(), check, nil, report); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if !matches {
		if err := os.MkdirAll(filepath.Dir(dockerDaemonConfig), 0o755); err != nil {
			return engine.ApplyResult{}, err
		}
		staged, err := os.CreateTemp(filepath.Dir(dockerDaemonConfig), ".eregion-daemon-*.json")
		if err != nil {
			return engine.ApplyResult{}, err
		}
		defer os.Remove(staged.Name())
		if _, err := staged.Write(desired); err != nil {
			staged.Close()
			return engine.ApplyResult{}, err
		}
		if err := staged.Close(); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := s.exec.Run(ctx, s.ID(), `set -euo pipefail
dockerd --validate --config-file "$STAGED" >/dev/null`, map[string]string{"STAGED": staged.Name()}, report); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := writeFile(dockerDaemonConfig, desired, 0o644, "root", "root"); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	script := `set -euo pipefail
systemctl daemon-reload
systemctl enable docker.service
if [ "$RESTART" = true ]; then systemctl restart docker.service; else systemctl start docker.service; fi
systemctl is-active --quiet docker.service
[ "$(docker info --format '{{.LoggingDriver}}')" = local ]`
	if err := s.exec.Run(ctx, s.ID(), script, map[string]string{"RESTART": fmt.Sprint(needsRestart)}, report); err != nil {
		if !matches {
			var restoreErr error
			if exists {
				restoreErr = writeFile(dockerDaemonConfig, raw, mode, "root", "root")
			} else {
				restoreErr = os.Remove(dockerDaemonConfig)
			}
			if restoreErr != nil {
				return engine.ApplyResult{}, fmt.Errorf("Docker setup failed and daemon configuration restoration failed: %w", restoreErr)
			}
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			recoveryCommand := "systemctl stop docker.service"
			if wasActive {
				recoveryCommand = "systemctl restart docker.service"
			}
			if recoveryErr := s.exec.Run(recovery, s.ID(), recoveryCommand, nil, report); recoveryErr != nil {
				return engine.ApplyResult{}, fmt.Errorf("Docker setup failed and daemon restart after restoration failed: %w", recoveryErr)
			}
		}
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"docker.service is enabled with bounded local container logging"}, Result: map[string]any{"service": "docker.service"}}, nil
}

func (s *dockerStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	return []string{"+ enable and start docker.service", "+ default container logging: local, 20m per file, 5 files, compression enabled"}, nil
}

func (s *dockerStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
