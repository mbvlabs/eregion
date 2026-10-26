package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

type packagesStep struct {
	cfg  Config
	exec Exec
	role RoleConfig
	// distro is the os-release ID; package names differ between distributions.
	distro string
}

func (s *packagesStep) ID() string { return "host-packages-v1" }
func (s *packagesStep) Describe() string {
	return "Install the role's distribution packages (repository versions)"
}
func (s *packagesStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *packagesStep) packages() []string {
	packages := []string{"ca-certificates", "passwd"}
	if s.role.Runtime {
		packages = append(packages, "docker.io")
	}
	if s.role.Builder {
		// Builds in this increment fetch source with git and execute it on the
		// build host. Git is part of the build prerequisite, not optional.
		packages = append(packages, "git")
	}
	if s.role.NodeAgent {
		// Enrollment probes `wg`, and private connectivity renders WireGuard
		// configuration with the operator-installed helper.
		packages = append(packages, "wireguard-tools")
	}
	if s.role.Telemetry {
		// The telemetry role needs a Compose-capable host. Bootstrap does not
		// install a telemetry stack, collector, or storage.
		// Ubuntu ships Compose v2 as docker-compose-v2; Debian as docker-compose.
		if s.distro == "ubuntu" {
			packages = append(packages, "docker-compose-v2")
		} else {
			packages = append(packages, "docker-compose")
		}
	}
	return packages
}

func (s *packagesStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Packages []string `json:"packages"`
		Role     Role     `json:"role"`
	}{s.packages(), s.cfg.Role})
}

func (s *packagesStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	var lines []string
	for _, name := range s.packages() {
		lines = append(lines, fmt.Sprintf(`if dpkg-query -W -f='${db:Status-Abbrev}' %s 2>/dev/null | grep -q '^ii'; then echo %s:ok; else echo %s:missing; fi`, name, name, name))
	}
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, strings.Join(lines, "\n"))
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
		return incomplete("missing packages: " + strings.Join(missing, ", "))
	}
	return complete("installed packages: " + strings.Join(s.packages(), ", "))
}

func (s *packagesStep) Apply(ctx context.Context, runtime engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	script := "set -euo pipefail\nexport DEBIAN_FRONTEND=noninteractive\napt-get update -qq\napt-get install -y --no-install-recommends " + strings.Join(s.packages(), " ")
	if err := s.exec.Run(ctx, s.ID(), script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"installed " + strings.Join(s.packages(), ", ")}, Result: map[string]any{"packages": s.packages()}}, nil
}

func (s *packagesStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	var out []string
	for _, name := range s.packages() {
		out = append(out, "+ apt package "+name)
	}
	return out, nil
}

func (s *packagesStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
