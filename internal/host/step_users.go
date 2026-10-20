package host

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

// accountSpec is the complete ownership/membership manifest for one account.
// The node account is the actual SSH execution identity, so it must be a Docker
// member wherever the runtime role can run application containers.
type accountSpec struct {
	Group  string `json:"group"`
	User   string `json:"user"`
	Home   string `json:"home"`
	Shell  string `json:"shell"`
	Docker bool   `json:"docker"`
}

type usersStep struct {
	cfg  Config
	exec Exec
	role RoleConfig
}

func (s *usersStep) nodeUser() string {
	if !s.role.NodeAgent {
		return ""
	}
	return s.cfg.Layout.NodeUser
}

func (s *usersStep) nodeGroup() string {
	if !s.role.NodeAgent {
		return ""
	}
	return s.cfg.Layout.NodeGroup
}

func (s *usersStep) ID() string                      { return "host-users-v1" }
func (s *usersStep) Describe() string                { return "Create the unprivileged service/node accounts" }
func (s *usersStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *usersStep) nodeHome() string {
	if s.nodeUser() == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.cfg.Layout.DataDir), s.nodeUser())
}

// accounts is the manifest used by both the check/apply scripts and tests. It
// deliberately includes the node execution account, not only the service
// account that runs the API/worker units.
func (s *usersStep) accounts() []accountSpec {
	var accounts []accountSpec
	if s.cfg.Layout.User != "" {
		accounts = append(accounts, accountSpec{
			Group: s.cfg.Layout.Group, User: s.cfg.Layout.User, Home: s.cfg.Layout.DataDir,
			Shell: "/usr/sbin/nologin", Docker: s.role.Runtime,
		})
	}
	if s.nodeUser() != "" {
		accounts = append(accounts, accountSpec{
			Group: s.nodeGroup(), User: s.nodeUser(), Home: s.nodeHome(),
			Shell: "/bin/bash", Docker: s.role.Runtime,
		})
	}
	return accounts
}

func (s *usersStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Accounts []accountSpec `json:"accounts"`
		Runtime  bool          `json:"runtime"`
	}{s.accounts(), s.role.Runtime})
}

func (s *usersStep) checkScript() string {
	var lines []string
	for _, account := range s.accounts() {
		if account.Group != "" {
			lines = append(lines, fmt.Sprintf("if getent group %s >/dev/null; then echo group:%s:ok; else echo group:%s:missing; fi", account.Group, account.Group, account.Group))
		}
		if account.User != "" {
			lines = append(lines, fmt.Sprintf("if getent passwd %s >/dev/null; then echo passwd:%s:ok; else echo passwd:%s:missing; fi", account.User, account.User, account.User))
		}
		if s.role.Runtime && account.User != "" {
			lines = append(lines, fmt.Sprintf("if id -nG %s 2>/dev/null | tr ' ' '\\n' | grep -qx docker; then echo member:%s:docker:ok; else echo member:%s:docker:missing; fi", account.User, account.User, account.User))
		}
	}
	return strings.Join(lines, "\n")
}

func (s *usersStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, s.checkScript())
	if err != nil {
		return engine.CheckResult{}, err
	}
	var missing []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasSuffix(line, ":missing") {
			line = strings.TrimSuffix(line, ":missing")
			parts := strings.Split(line, ":")
			switch len(parts) {
			case 2:
				missing = append(missing, parts[0]+" "+parts[1])
			case 3:
				missing = append(missing, parts[1]+" docker membership")
			default:
				missing = append(missing, line)
			}
		}
	}
	if len(missing) > 0 {
		return incomplete("missing accounts or membership: " + strings.Join(missing, ", "))
	}
	return complete("service and node accounts exist with the required docker membership")
}

func (s *usersStep) Apply(ctx context.Context, runtime engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if err := s.exec.Run(ctx, s.ID(), s.applyScript(), nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	notes := []string{"service and node accounts are present"}
	if s.role.Runtime {
		for _, account := range s.accounts() {
			if account.Docker {
				notes = append(notes, account.User+" is in the docker group")
			}
		}
	}
	return engine.ApplyResult{Notes: notes, Result: map[string]any{"accounts": s.accountNames()}}, nil
}

func (s *usersStep) accountNames() []string {
	var names []string
	for _, account := range s.accounts() {
		if account.User != "" {
			names = append(names, account.User)
		}
	}
	return names
}

func (s *usersStep) applyScript() string {
	var lines []string
	lines = append(lines, "set -euo pipefail")
	for _, account := range s.accounts() {
		if account.Group == "" || account.User == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf(`if ! getent group %s >/dev/null; then groupadd --system %s; fi`, account.Group, account.Group))
		lines = append(lines, fmt.Sprintf(`if ! getent passwd %s >/dev/null; then useradd --system --gid %s --home-dir %s --no-create-home --shell %s %s; fi`, account.User, account.Group, account.Home, account.Shell, account.User))
	}
	if s.role.Runtime {
		var dockerUsers []string
		for _, account := range s.accounts() {
			if account.Docker && account.User != "" {
				dockerUsers = append(dockerUsers, account.User)
			}
		}
		if len(dockerUsers) > 0 {
			lines = append(lines, `if ! getent group docker >/dev/null; then echo "the docker group is missing; install docker.io before configuring host roles" >&2; exit 1; fi`)
			for _, user := range dockerUsers {
				lines = append(lines, fmt.Sprintf(`if ! id -nG %s | tr ' ' '\n' | grep -qx docker; then usermod -aG docker %s; fi`, user, user))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (s *usersStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
