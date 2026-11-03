package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

// The worker runs as the unprivileged service account. Retention garbage
// collection must stop and start the registry unit, so this is the one narrow
// systemd action it is allowed to perform. The rule is evaluated by polkit for
// every request and grants no other unit, verb or user.
const (
	RegistryGCPolkitRuleDir   = "/etc/polkit-1/rules.d"
	RegistryGCPolkitRuleName  = "50-eregion-registry-gc.rules"
	registryGCPolkitRuleGroup = "polkitd"
)

type registryGCPolkitStep struct {
	cfg Config
	// dir is a test seam; production uses RegistryGCPolkitRuleDir.
	dir string
	// ownership is a test seam. Production leaves it nil and verifies that the
	// rule and its directory are owned by root.
	ownership func(os.FileInfo, string, string) (bool, error)
}

func newRegistryGCPolkitStep(cfg Config) *registryGCPolkitStep {
	return &registryGCPolkitStep{cfg: cfg, dir: RegistryGCPolkitRuleDir}
}

func (s *registryGCPolkitStep) ownedBy(info os.FileInfo, group string) (bool, error) {
	if s.ownership != nil {
		return s.ownership(info, "root", group)
	}
	return ownershipMatches(info, "root", group)
}

func (s *registryGCPolkitStep) ID() string { return "host-registry-gc-polkit-v1" }
func (s *registryGCPolkitStep) Describe() string {
	return "Allow the worker to start, stop and restart the registry for garbage collection"
}
func (s *registryGCPolkitStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *registryGCPolkitStep) rulePath() string {
	return filepath.Join(s.dir, RegistryGCPolkitRuleName)
}

func (s *registryGCPolkitStep) rule() []byte {
	user := s.cfg.Layout.User
	if user == "" {
		user = "eregion"
	}
	return []byte(fmt.Sprintf(`// managed by eregion: host-registry-gc-polkit-v1
// The unprivileged worker may only start, stop or restart the local registry
// so a retention run can garbage-collect unreferenced blobs. Every other unit
// and verb keeps the distribution's default authentication policy.
polkit.addRule(function(action, subject) {
    if (action.id != "org.freedesktop.systemd1.manage-units" || subject.user != %s) {
        return polkit.Result.NOT_HANDLED;
    }
    if (action.lookup("unit") != %s) {
        return polkit.Result.NOT_HANDLED;
    }
    var verb = action.lookup("verb");
    if (verb == "start" || verb == "stop" || verb == "restart") {
        return polkit.Result.YES;
    }
    return polkit.Result.NOT_HANDLED;
});
`, strconv.Quote(user), strconv.Quote(platform.RegistryUnit)))
}

func (s *registryGCPolkitStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Path string `json:"path"`
		Rule string `json:"rule"`
	}{s.rulePath(), contentSHA256(s.rule())})
}

func (s *registryGCPolkitStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	info, err := os.Lstat(s.dir)
	if err != nil || !info.IsDir() {
		return incomplete(s.dir + " does not exist")
	}
	if info.Mode().Perm() != 0o750 {
		return incomplete(fmt.Sprintf("%s mode %04o (want 0750)", s.dir, info.Mode().Perm()))
	}
	if ok, err := s.ownedBy(info, registryGCPolkitRuleGroup); err != nil || !ok {
		return incomplete(s.dir + " is not owned by root:" + registryGCPolkitRuleGroup)
	}
	fileInfo, err := os.Lstat(s.rulePath())
	if err != nil || !fileInfo.Mode().IsRegular() {
		return incomplete(s.rulePath() + " is missing")
	}
	ok, mode, err := sameFileContent(s.rulePath(), s.rule())
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok {
		return incomplete(s.rulePath() + " differs")
	}
	if mode.Perm() != 0o644 {
		return incomplete(fmt.Sprintf("%s mode %04o (want 0644)", s.rulePath(), mode.Perm()))
	}
	if ok, err := s.ownedBy(fileInfo, "root"); err != nil || !ok {
		return incomplete(s.rulePath() + " is not owned by root:root")
	}
	return complete("the worker may start, stop and restart " + platform.RegistryUnit + " through polkit")
}

func (s *registryGCPolkitStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	parent := filepath.Dir(s.dir)
	if err := ensureDir(parent, 0o755); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := chownIfRoot(parent, "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := ensureDir(s.dir, 0o750); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := chownIfRoot(s.dir, "root", registryGCPolkitRuleGroup); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(s.rulePath(), s.rule(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{
		Notes:  []string{"the worker may start, stop and restart " + platform.RegistryUnit + " through polkit"},
		Result: map[string]any{"path": s.rulePath(), "unit": platform.RegistryUnit},
	}, nil
}

func (s *registryGCPolkitStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	raw, _, _, err := fileState(s.rulePath())
	if err != nil {
		return nil, err
	}
	return simpleDiff(s.rulePath(), raw, s.rule()), nil
}

func (s *registryGCPolkitStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
