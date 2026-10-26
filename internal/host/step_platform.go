package host

import (
	"context"
	"fmt"

	"github.com/mbvlabs/eregion/internal/engine"
)

type platformStep struct {
	cfg Config
}

func (s *platformStep) ID() string { return "host-platform-v1" }
func (s *platformStep) Describe() string {
	return "Verify a supported Debian/Ubuntu release and architecture"
}
func (s *platformStep) CancelBoundary() engine.Boundary { return engine.BoundaryBeforeApply }

func (s *platformStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Path string `json:"path"`
		Arch string `json:"arch"`
		Role Role   `json:"role"`
	}{Path: s.cfg.PlatformPath, Arch: s.cfg.Arch, Role: s.cfg.Role})
}

func (s *platformStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	platform, err := DetectPlatform(s.cfg.PlatformPath, s.cfg.Arch)
	if err != nil {
		return engine.CheckResult{}, fmt.Errorf("%w: %v", engine.ErrPermanent, err)
	}
	if !platform.Supported() {
		return engine.CheckResult{}, fmt.Errorf("%w: %s is not in the verified matrix (Debian 13, Ubuntu 26.04; amd64/arm64)", engine.ErrPermanent, platform)
	}
	return complete("supported: " + platform.String())
}

func (s *platformStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	return engine.ApplyResult{}, fmt.Errorf("%w: platform verification is read-only", engine.ErrPermanent)
}

func (s *platformStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
