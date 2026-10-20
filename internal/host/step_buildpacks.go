package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

type buildpacksStep struct {
	cfg        Config
	exec       Exec
	downloader Downloader
}

func (s *buildpacksStep) ID() string { return "host-buildpacks-v1" }
func (s *buildpacksStep) Describe() string {
	return "Install the pinned Cloud Native Buildpacks pack CLI"
}
func (s *buildpacksStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *buildpacksStep) arch() string {
	if s.cfg.Arch != "" {
		return s.cfg.Arch
	}
	return runtime.GOARCH
}

func (s *buildpacksStep) packPath() string { return filepath.Join(s.cfg.Layout.BinDir, "pack") }

func (s *buildpacksStep) Inputs() ([]byte, error) {
	version, url, checksum, ok := PackArtifact(s.arch())
	if !ok {
		return nil, fmt.Errorf("%w: no pinned pack artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	return canonicalInput(struct {
		Version  string `json:"version"`
		URL      string `json:"url"`
		Checksum string `json:"checksum"`
		Arch     string `json:"arch"`
		Path     string `json:"path"`
	}{version, url, checksum, s.arch(), s.packPath()})
}

func (s *buildpacksStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	version, _, checksum, ok := PackArtifact(s.arch())
	if !ok {
		return engine.CheckResult{}, fmt.Errorf("%w: no pinned pack artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	info, err := os.Stat(s.packPath())
	if err != nil || !info.Mode().IsRegular() {
		return incomplete("pack is not installed; pinned version " + version + " checksum " + checksum)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return incomplete("pack exists but is not executable")
	}
	output, err := s.exec.Output(ctx, shellQuote(s.packPath())+" --version")
	if err != nil {
		return incomplete("pack is installed but did not report its version")
	}
	if !strings.Contains(output, version) {
		return incomplete("pack version does not match the pinned " + version)
	}
	return complete("pack " + version + " reports the pinned version (archive checksum is verified on installation)")
}

func (s *buildpacksStep) Apply(ctx context.Context, runtime engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	version, url, checksum, ok := PackArtifact(s.arch())
	if !ok {
		return engine.ApplyResult{}, fmt.Errorf("%w: no pinned pack artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	if s.downloader == nil {
		return engine.ApplyResult{}, fmt.Errorf("no artifact downloader is configured")
	}
	archive, err := s.downloader.Get(ctx, url, 64<<20, checksum)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	binary, err := extractPack(archive, 128<<20)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFile(s.packPath(), binary, 0o755, "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"installed pack " + version + " from the pinned archive"}, Result: map[string]any{
		"version": version, "sha256": checksum, "path": s.packPath(),
	}}, nil
}

func (s *buildpacksStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	version, url, checksum, _ := PackArtifact(s.arch())
	return []string{
		"+ download " + url,
		"+ verify sha256 " + checksum,
		"+ install pack " + version + " at " + s.packPath(),
	}, nil
}

func (s *buildpacksStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
