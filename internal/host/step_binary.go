package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/engine"
)

type binarySpec struct {
	Source string
	Dest   string
}

type binaryStep struct {
	cfg  Config
	role RoleConfig
}

func (s *binaryStep) ID() string                      { return "host-binary-v1" }
func (s *binaryStep) Describe() string                { return "Install the pinned eregion executable" }
func (s *binaryStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *binaryStep) binaries() ([]binarySpec, error) {
	// Every role runs the same binary: the control plane serves the API and
	// workers from it, and node roles answer "eregion node-agent serve".
	source, err := s.cfg.EffectiveBinaryPath()
	if err != nil {
		return nil, err
	}
	return []binarySpec{{Source: source, Dest: filepath.Join(s.cfg.Layout.BinDir, "eregion")}}, nil
}

func (s *binaryStep) Inputs() ([]byte, error) {
	specs, err := s.binaries()
	if err != nil {
		return nil, err
	}
	type entry struct {
		Source string `json:"source"`
		Dest   string `json:"dest"`
		Hash   string `json:"hash"`
	}
	var entries []entry
	for _, spec := range specs {
		hash, err := fileSHA256(spec.Source)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{spec.Source, spec.Dest, hash})
	}
	return canonicalInput(entries)
}

func (s *binaryStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	specs, err := s.binaries()
	if err != nil {
		return engine.CheckResult{}, err
	}
	for _, spec := range specs {
		want, err := fileSHA256(spec.Source)
		if err != nil {
			return engine.CheckResult{}, err
		}
		got, mode, err := fileSHA256Mode(spec.Dest)
		if err != nil {
			return engine.CheckResult{}, err
		}
		if got == "" {
			return incomplete("missing executable " + spec.Dest)
		}
		if got != want {
			return incomplete("executable " + spec.Dest + " differs from the source binary")
		}
		if mode&0o111 == 0 {
			return incomplete("executable " + spec.Dest + " is not executable")
		}
	}
	return complete("installed binaries match the supplied sources")
}

func (s *binaryStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	specs, err := s.binaries()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	var installed []string
	for _, spec := range specs {
		raw, err := os.ReadFile(spec.Source) //nolint:gosec // G304: operator-provided binary.
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if err := writeFile(spec.Dest, raw, 0o755, "root", "root"); err != nil {
			return engine.ApplyResult{}, err
		}
		installed = append(installed, spec.Dest)
	}
	return engine.ApplyResult{Notes: []string{"installed " + join(installed, ", ")}, Result: map[string]any{"installed": installed}}, nil
}

func (s *binaryStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	specs, err := s.binaries()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, spec := range specs {
		hash, err := fileSHA256(spec.Source)
		if err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("+ install %s -> %s (sha256 %s)", spec.Source, spec.Dest, hash))
	}
	return out, nil
}

func (s *binaryStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}

func fileSHA256Mode(path string) (string, os.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	hash, err := fileSHA256(path)
	return hash, info.Mode().Perm(), err
}

func join(values []string, separator string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += separator
		}
		result += value
	}
	return result
}
