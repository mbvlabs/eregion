package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

type dirSpec struct {
	Path  string
	Mode  os.FileMode
	Owner string
	Group string
}

type directoriesStep struct {
	cfg  Config
	role RoleConfig
}

func (s *directoriesStep) ID() string { return "host-directories-v1" }
func (s *directoriesStep) Describe() string {
	return "Create private bootstrap, node, configuration, and registry directories"
}
func (s *directoriesStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *directoriesStep) nodeUser() string {
	if !s.role.NodeAgent {
		return ""
	}
	return s.cfg.Layout.NodeUser
}

func (s *directoriesStep) nodeGroup() string {
	if !s.role.NodeAgent {
		return ""
	}
	return s.cfg.Layout.NodeGroup
}

func (s *directoriesStep) nodeHome() string {
	if s.nodeUser() == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.cfg.Layout.DataDir), s.nodeUser())
}

func (s *directoriesStep) specs() []dirSpec {
	owner, group := s.cfg.Layout.User, s.cfg.Layout.Group
	if s.role.NodeAgent {
		owner, group = s.nodeUser(), s.nodeGroup()
	}
	specs := []dirSpec{
		{Path: s.cfg.Layout.StateDir, Mode: 0o700, Owner: "root", Group: "root"},
		{Path: s.cfg.Layout.DataDir, Mode: 0o750, Owner: owner, Group: group},
	}
	if s.cfg.Layout.NodeStateDir != "" {
		owner, group := s.cfg.Layout.User, s.cfg.Layout.Group
		if s.nodeUser() != "" {
			owner, group = s.nodeUser(), s.nodeGroup()
		}
		specs = append(specs, dirSpec{Path: s.cfg.Layout.NodeStateDir, Mode: 0o700, Owner: owner, Group: group})
	}
	if s.cfg.Layout.ConfigDir != "" {
		group := s.cfg.Layout.Group
		if s.role.NodeAgent {
			group = s.nodeGroup()
		}
		if group == "" {
			group = "root"
		}
		specs = append(specs, dirSpec{Path: s.cfg.Layout.ConfigDir, Mode: 0o750, Owner: "root", Group: group})
	}
	if s.role.Registry {
		specs = append(specs, dirSpec{Path: s.cfg.Layout.RegistryDir, Mode: 0o700, Owner: "root", Group: "root"})
	}
	if home := s.nodeHome(); home != "" {
		specs = append(specs, dirSpec{Path: home, Mode: 0o700, Owner: s.nodeUser(), Group: s.nodeGroup()})
	}
	return specs
}

func (s *directoriesStep) Inputs() ([]byte, error) {
	type entry struct {
		Path  string `json:"path"`
		Mode  uint32 `json:"mode"`
		Owner string `json:"owner"`
		Group string `json:"group"`
	}
	var entries []entry
	for _, spec := range s.specs() {
		entries = append(entries, entry{spec.Path, uint32(spec.Mode), spec.Owner, spec.Group})
	}
	return canonicalInput(entries)
}

func (s *directoriesStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	var missing, wrong []string
	for _, spec := range s.specs() {
		info, err := os.Lstat(spec.Path)
		if err != nil || !info.IsDir() {
			missing = append(missing, spec.Path)
			continue
		}
		if info.Mode().Perm() != spec.Mode {
			wrong = append(wrong, fmt.Sprintf("%s mode %04o (want %04o)", spec.Path, info.Mode().Perm(), spec.Mode))
		}
		ok, err := ownershipMatches(info, spec.Owner, spec.Group)
		if err != nil || !ok {
			wrong = append(wrong, spec.Path+" ownership")
		}
	}
	if len(missing) > 0 || len(wrong) > 0 {
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "missing "+strings.Join(missing, ", "))
		}
		if len(wrong) > 0 {
			parts = append(parts, "wrong "+strings.Join(wrong, ", "))
		}
		return incomplete(strings.Join(parts, "; "))
	}
	return complete("directories exist with the requested private modes")
}

func (s *directoriesStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	var notes []string
	for _, spec := range s.specs() {
		if err := ensureDir(spec.Path, spec.Mode); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := chownIfRoot(spec.Path, spec.Owner, spec.Group); err != nil {
			return engine.ApplyResult{}, err
		}
		notes = append(notes, spec.Path)
	}
	// Bootstrap creates the shared lock before accounts exist. Leave it usable
	// by the actual node execution identity once account creation has finished.
	owner, group := s.cfg.Layout.User, s.cfg.Layout.Group
	if s.role.NodeAgent {
		owner, group = s.nodeUser(), s.nodeGroup()
	}
	lock := filepath.Join(s.cfg.Layout.NodeStateDir, "host.lock")
	if _, err := os.Lstat(lock); err == nil {
		if err := chownIfRoot(lock, owner, group); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	return engine.ApplyResult{Notes: notes, Result: map[string]any{"created": notes}}, nil
}

func (s *directoriesStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	var out []string
	for _, spec := range s.specs() {
		info, err := os.Stat(spec.Path)
		if err != nil || !info.IsDir() {
			out = append(out, "+ directory "+spec.Path)
			continue
		}
		if info.Mode().Perm() != spec.Mode {
			out = append(out, fmt.Sprintf("~ mode %s: %04o -> %04o", spec.Path, info.Mode().Perm(), spec.Mode))
		}
	}
	return out, nil
}

func (s *directoriesStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
