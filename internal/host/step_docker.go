package host

import (
	"context"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

type dockerStep struct {
	exec Exec
}

func (s *dockerStep) ID() string                      { return "host-docker-v1" }
func (s *dockerStep) Describe() string                { return "Enable the Docker container runtime" }
func (s *dockerStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *dockerStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Service string `json:"service"`
	}{"docker.service"})
}

func (s *dockerStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	script := `if systemctl is-enabled docker.service >/dev/null 2>&1; then echo enabled:ok; else echo enabled:missing; fi
if systemctl is-active docker.service >/dev/null 2>&1; then echo active:ok; else echo active:missing; fi
if docker info >/dev/null 2>&1; then echo docker:ok; else echo docker:missing; fi`
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
	return complete("docker.service is enabled, active, and answers docker info")
}

func (s *dockerStep) Apply(ctx context.Context, runtime engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	script := "set -euo pipefail\nsystemctl daemon-reload\nsystemctl enable --now docker.service\nsystemctl is-active --quiet docker.service\ndocker info >/dev/null"
	if err := s.exec.Run(ctx, s.ID(), script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"docker.service is enabled and active"}, Result: map[string]any{"service": "docker.service"}}, nil
}

func (s *dockerStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	return []string{"+ enable and start docker.service"}, nil
}

func (s *dockerStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
