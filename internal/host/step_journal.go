package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/engine"
)

const journalGroup = "systemd-journal"

// journalAccessStep makes the unprivileged admin account a member of the
// journal group so `journalctl` can read the eregion units' journal without
// sudo. It is a separate versioned step so an existing node converges on apply
// without re-running the account or access setup.
type journalAccessStep struct {
	cfg  Config
	exec Exec
}

func (s *journalAccessStep) ID() string { return "host-journal-access-v1" }
func (s *journalAccessStep) Describe() string {
	return "Allow the admin user to read the systemd journal"
}
func (s *journalAccessStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *journalAccessStep) adminUser() string {
	if s.cfg.AdminUser != "" {
		return s.cfg.AdminUser
	}
	// An intent recorded before the admin field existed still converges to the
	// default server-init account. Validate rejects anything but a plain name.
	return access.AdminUser
}

func (s *journalAccessStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		User  string `json:"user"`
		Group string `json:"group"`
	}{s.adminUser(), journalGroup})
}

func (s *journalAccessStep) checkScript() string {
	user := shellQuote(s.adminUser())
	return fmt.Sprintf(`if ! getent passwd %[1]s >/dev/null; then echo user:missing; elif id -nG %[1]s 2>/dev/null | tr ' ' '\n' | grep -qx %[2]s; then echo member:ok; else echo member:missing; fi`, user, journalGroup)
}

func (s *journalAccessStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, s.checkScript())
	if err != nil {
		return engine.CheckResult{}, err
	}
	switch strings.TrimSpace(output) {
	case "member:ok":
		return complete(s.adminUser() + " can read the systemd journal")
	case "user:missing":
		return complete("the admin account " + s.adminUser() + " does not exist; no journal access change is needed")
	default:
		return incomplete(s.adminUser() + " is not in the " + journalGroup + " group")
	}
}

func (s *journalAccessStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	user := shellQuote(s.adminUser())
	script := fmt.Sprintf(`set -euo pipefail
if ! getent passwd %[1]s >/dev/null; then
  echo "the admin account is absent; no journal access change is needed"
  exit 0
fi
if ! getent group %[2]s >/dev/null; then groupadd --system %[2]s; fi
if ! id -nG %[1]s | tr ' ' '\n' | grep -qx %[2]s; then usermod -aG %[2]s %[1]s; fi`, user, journalGroup)
	if err := s.exec.Run(ctx, s.ID(), script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{
		Notes:  []string{"the admin account can read the systemd journal"},
		Result: map[string]any{"user": s.adminUser(), "group": journalGroup},
	}, nil
}

func (s *journalAccessStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	return []string{"+ add " + s.adminUser() + " to the " + journalGroup + " group"}, nil
}

func (s *journalAccessStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
