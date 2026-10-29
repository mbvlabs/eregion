package host

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

// CaddyfilePath is the distribution package's config, owned by eregion.
const CaddyfilePath = platform.CaddyfilePath

// CaddyVersion pins the same upstream release as deploycrate-ce setup.
const CaddyVersion = "2.11.4"

//go:embed scripts/install-caddy.sh
var installCaddyScript string

func caddyArtifact(arch string) (url, checksum string, ok bool) {
	switch arch {
	case "amd64":
		checksum = "c41708ffb4af9bc6d19f7d22a7a034804352a21ecc62e1d3dfe3d58e30b38a3e"
	case "arm64":
		checksum = "aeab2e38bf77a0162611a1703a5e16c09475b000d41f7edaa9337734d16642fd"
	default:
		return "", "", false
	}
	return "https://github.com/caddyserver/caddy/releases/download/v" + CaddyVersion + "/caddy_" + CaddyVersion + "_linux_" + arch + ".deb", checksum, true
}

func (s *ingressStep) arch() string {
	if s.cfg.Arch != "" {
		return s.cfg.Arch
	}
	return runtime.GOARCH
}

// PublicAPIPaths are the only API paths Caddy exposes on the main node's
// hostname: readiness, the per-environment CI deploy endpoint, and the
// per-environment GitHub webhook, which verifies its own HMAC signature.
// Everything else of the API stays on loopback, reached by the CLI through SSH
// over WireGuard.
var PublicAPIPaths = []string{
	"/readyz",
	"/v1/environments/*/deployments",
	"/v1/environments/*/deployments/*",
	"/v1/webhooks/github/*/*",
}

// ValidateHostname checks a DNS name such as main.example.com.
func ValidateHostname(name string) error { return platform.ValidateDomain(name) }

// ingressStep installs Caddy on the main node: an ACME certificate for the
// hostname and a reverse proxy for the public API paths only.
type ingressStep struct {
	cfg  Config
	exec Exec
}

func (s *ingressStep) ID() string { return "host-ingress-v2" }
func (s *ingressStep) Describe() string {
	return "Install pinned Caddy " + CaddyVersion + " for " + s.cfg.Hostname + " with an ACME certificate"
}
func (s *ingressStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *ingressStep) caddyfile() []byte {
	return []byte(fmt.Sprintf(`# managed by eregion
import %s/*.caddy

%s {
	@api path %s
	handle @api {
		reverse_proxy %s
	}
	handle {
		respond 404
	}
}
`, platform.CaddyRoutesDir, s.cfg.Hostname, strings.Join(PublicAPIPaths, " "), s.cfg.APIListen))
}

func (s *ingressStep) Inputs() ([]byte, error) {
	url, checksum, ok := caddyArtifact(s.arch())
	if !ok {
		return nil, fmt.Errorf("%w: no pinned Caddy artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	return canonicalInput(struct {
		Hostname  string `json:"hostname"`
		Caddyfile string `json:"caddyfile"`
		Version   string `json:"version"`
		Arch      string `json:"arch"`
		URL       string `json:"url"`
		Checksum  string `json:"sha256"`
	}{s.cfg.Hostname, contentSHA256(s.caddyfile()), CaddyVersion, s.arch(), url, checksum})
}

func (s *ingressStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	if _, _, ok := caddyArtifact(s.arch()); !ok {
		return engine.CheckResult{}, fmt.Errorf("%w: no pinned Caddy artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	version, err := s.exec.Output(ctx, "caddy version")
	if err != nil || !pinnedCaddyVersion(version) {
		return incomplete("Caddy must report pinned version " + CaddyVersion)
	}
	ok, _, err := sameFileContent(CaddyfilePath, s.caddyfile())
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok {
		return incomplete("Caddy is not installed or its Caddyfile differs")
	}
	if _, err := os.Stat(platform.CaddyRoutesPath); err != nil {
		return incomplete("the routes file " + platform.CaddyRoutesPath + " is missing")
	}
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, `if systemctl is-active caddy.service >/dev/null 2>&1; then echo active:ok; else echo active:missing; fi`)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if output != "active:ok" {
		return incomplete("caddy.service is not active")
	}
	return complete("Caddy " + CaddyVersion + " serves " + s.cfg.Hostname)
}

func (s *ingressStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	url, checksum, ok := caddyArtifact(s.arch())
	if !ok {
		return engine.ApplyResult{}, fmt.Errorf("%w: no pinned Caddy artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	if err := s.exec.Run(ctx, s.ID(), installCaddyScript, map[string]string{
		"CADDY_VERSION": CaddyVersion, "CADDY_ARCH": s.arch(), "CADDY_URL": url, "CADDY_SHA256": checksum,
	}, report); err != nil {
		return engine.ApplyResult{}, err
	}
	// The worker owns the routes file; it rewrites it on every domain change.
	routes := fmt.Sprintf(`set -euo pipefail
install -d -m 0755 -o %[1]s -g %[2]s %[3]s
[ -f %[4]s ] || install -m 0644 -o %[1]s -g %[2]s /dev/null %[4]s`, shellQuote(s.cfg.Layout.User), shellQuote(s.cfg.Layout.Group), platform.CaddyRoutesDir, platform.CaddyRoutesPath)
	if err := s.exec.Run(ctx, s.ID(), routes, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(CaddyfilePath, s.caddyfile(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	script := `set -euo pipefail
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null
systemctl enable caddy.service >/dev/null 2>&1
# A reload keeps the old executable in memory after a package upgrade.
systemctl restart caddy.service
systemctl is-active --quiet caddy.service`
	if err := s.exec.Run(ctx, s.ID(), script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"Caddy " + CaddyVersion + " serves " + s.cfg.Hostname}, Result: map[string]any{
		"hostname": s.cfg.Hostname, "paths": PublicAPIPaths, "version": CaddyVersion, "sha256": checksum,
	}}, nil
}

func (s *ingressStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	raw, _, _, err := fileState(CaddyfilePath)
	if err != nil {
		return nil, err
	}
	url, checksum, ok := caddyArtifact(s.arch())
	if !ok {
		return nil, fmt.Errorf("%w: no pinned Caddy artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	return append([]string{"+ install Caddy " + CaddyVersion + " from " + url, "+ verify sha256 " + checksum}, simpleDiff(CaddyfilePath, raw, s.caddyfile())...), nil
}

func pinnedCaddyVersion(output string) bool {
	fields := strings.Fields(output)
	return len(fields) > 0 && fields[0] == "v"+CaddyVersion
}

func (s *ingressStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
