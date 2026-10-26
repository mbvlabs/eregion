package host

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

// CaddyfilePath is the distribution package's config, owned by eregion.
const CaddyfilePath = platform.CaddyfilePath

// PublicAPIPaths are the only API paths Caddy exposes on the main node's
// hostname: readiness and the GitHub webhook, which verifies its own HMAC
// signature. Everything else of the API stays on loopback, reached by the CLI
// through SSH over WireGuard.
var PublicAPIPaths = []string{"/readyz", "/v1/webhooks/github"}

// ValidateHostname checks a DNS name such as main.example.com.
func ValidateHostname(name string) error { return platform.ValidateDomain(name) }

// ingressStep installs Caddy on the main node: an ACME certificate for the
// hostname and a reverse proxy for the public API paths only.
type ingressStep struct {
	cfg  Config
	exec Exec
}

func (s *ingressStep) ID() string { return "host-ingress-v1" }
func (s *ingressStep) Describe() string {
	return "Install Caddy for " + s.cfg.Hostname + " with an ACME certificate"
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
	return canonicalInput(struct {
		Hostname  string `json:"hostname"`
		Caddyfile string `json:"caddyfile"`
	}{s.cfg.Hostname, contentSHA256(s.caddyfile())})
}

func (s *ingressStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
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
	return complete("Caddy serves " + s.cfg.Hostname)
}

func (s *ingressStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	install := "set -euo pipefail\nexport DEBIAN_FRONTEND=noninteractive\nif ! dpkg-query -W -f='${db:Status-Abbrev}' caddy 2>/dev/null | grep -q '^ii'; then apt-get update -qq; apt-get install -y --no-install-recommends caddy; fi"
	if err := s.exec.Run(ctx, s.ID(), install, nil, report); err != nil {
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
systemctl reload-or-restart caddy.service
systemctl is-active --quiet caddy.service`
	if err := s.exec.Run(ctx, s.ID(), script, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"Caddy serves " + s.cfg.Hostname}, Result: map[string]any{"hostname": s.cfg.Hostname, "paths": PublicAPIPaths}}, nil
}

func (s *ingressStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	raw, _, _, err := fileState(CaddyfilePath)
	if err != nil {
		return nil, err
	}
	return append([]string{"+ apt package caddy"}, simpleDiff(CaddyfilePath, raw, s.caddyfile())...), nil
}

func (s *ingressStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
