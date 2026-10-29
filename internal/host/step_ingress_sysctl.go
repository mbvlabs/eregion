package host

import (
	"context"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

const (
	// IngressSysctlPath persists the ingress kernel settings across reboots.
	IngressSysctlPath = "/etc/sysctl.d/60-eregion-ingress.conf"
	tcpMigrateReqPath = "/proc/sys/net/ipv4/tcp_migrate_req"
)

var ingressSysctlConf = []byte(`# managed by eregion
# Caddy binds a new SO_REUSEPORT listener on every config load and closes the
# old one. Without migration the kernel resets connections still queued on the
# closed socket, so a blue/green route switch fails in-flight TLS handshakes.
net.ipv4.tcp_migrate_req = 1
`)

// ingressSysctlStep keeps Caddy reloads, and so every traffic switch (D17),
// from resetting queued connections. It is a separate step so enabling it does
// not reinstall or restart Caddy on an existing node.
type ingressSysctlStep struct {
	confPath string
	procPath string
}

func newIngressSysctlStep() *ingressSysctlStep {
	return &ingressSysctlStep{confPath: IngressSysctlPath, procPath: tcpMigrateReqPath}
}

func (s *ingressSysctlStep) ID() string { return "host-ingress-sysctl-v1" }
func (s *ingressSysctlStep) Describe() string {
	return "Migrate queued TCP connections between Caddy listeners during reloads"
}
func (s *ingressSysctlStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *ingressSysctlStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Path string `json:"path"`
		Conf string `json:"conf"`
	}{s.confPath, contentSHA256(ingressSysctlConf)})
}

func (s *ingressSysctlStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	ok, _, err := sameFileContent(s.confPath, ingressSysctlConf)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok {
		return incomplete(s.confPath + " is missing or differs")
	}
	raw, err := os.ReadFile(s.procPath)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if strings.TrimSpace(string(raw)) != "1" {
		return incomplete("net.ipv4.tcp_migrate_req is not 1 in the running kernel")
	}
	return complete("net.ipv4.tcp_migrate_req = 1")
}

func (s *ingressSysctlStep) Apply(context.Context, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	if err := writePublicFile(s.confPath, ingressSysctlConf, "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	// The running kernel takes the value now; the file restores it at boot.
	if err := os.WriteFile(s.procPath, []byte("1\n"), 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"net.ipv4.tcp_migrate_req = 1"}, Result: map[string]any{
		"path": s.confPath, "tcp_migrate_req": 1,
	}}, nil
}

func (s *ingressSysctlStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	raw, _, _, err := fileState(s.confPath)
	if err != nil {
		return nil, err
	}
	return append(simpleDiff(s.confPath, raw, ingressSysctlConf), "+ set net.ipv4.tcp_migrate_req = 1"), nil
}

func (s *ingressSysctlStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
