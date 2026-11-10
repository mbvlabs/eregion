package host

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

// CaddyfilePath is the distribution package's config, owned by eregion.
const CaddyfilePath = platform.CaddyfilePath

// CaddyVersion pins the same upstream release as deploycrate-ce setup.
const CaddyVersion = "2.11.4"

// CaddyBinaryPath is the executable both the public service and the dedicated
// telemetry unit run. Never resolve Caddy through PATH here: a matching manual
// binary elsewhere must not satisfy this dependency.
const CaddyBinaryPath = "/usr/bin/caddy"

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
// over WireGuard. Caddy matches a pattern with an inner wildcard as a glob in
// which each * is one path segment, so <app>/<env> needs two.
var PublicAPIPaths = []string{
	"/readyz",
	"/v1/environments/*/*/deployments",
	"/v1/environments/*/*/deployments/*",
	"/v1/webhooks/github/*/*",
}

// ValidateHostname checks a DNS name such as main.example.com.
func ValidateHostname(name string) error { return platform.ValidateDomain(name) }

// ingressStep installs pinned Caddy on every control-plane node. With a
// hostname it serves the public API paths and ACME certificate; without one it
// only ensures the binary/account for the dedicated telemetry unit and refuses
// to touch any pre-existing Caddy configuration or service.
type ingressStep struct {
	cfg        Config
	exec       Exec
	configPath string
	lockPath   string
}

func (s *ingressStep) ID() string { return "host-ingress-v3" }
func (s *ingressStep) Describe() string {
	if strings.TrimSpace(s.cfg.Hostname) == "" {
		return "Install pinned Caddy " + CaddyVersion + " for the dedicated telemetry endpoint without a public hostname"
	}
	return "Install pinned Caddy " + CaddyVersion + " for " + s.cfg.Hostname + " with an ACME certificate"
}
func (s *ingressStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *ingressStep) caddyConfigPath() string {
	if s.configPath != "" {
		return s.configPath
	}
	return CaddyfilePath
}

func (s *ingressStep) ingressLockPath() string {
	if s.lockPath != "" {
		return s.lockPath
	}
	return platform.CaddyIngressLockPath
}

// telemetryProxyBlock is one exact public telemetry path. It rewrites to the
// gateway's fixed upstream path, names the loopback gateway Host explicitly
// (Caddy would otherwise preserve the public Host) and leaves Authorization in
// place for the gateway's basic authentication.
func telemetryProxyBlock(publicPath, gatewayPath string) string {
	return fmt.Sprintf(`	handle %s {
		rewrite * %s
		reverse_proxy %s {
			header_up Host %s
			transport http {
				dial_timeout 2s
				response_header_timeout 30s
			}
		}
	}
`, publicPath, gatewayPath, telemetry.GatewayListenAddress, telemetry.GatewayListenAddress)
}

func (s *ingressStep) caddyfile() []byte {
	return s.caddyfileForRoutesDir(platform.CaddyRoutesDir)
}

func (s *ingressStep) caddyfileForRoutesDir(routesDir string) []byte {
	if strings.TrimSpace(s.cfg.Hostname) == "" {
		return nil
	}
	return []byte(fmt.Sprintf(`# managed by eregion
import %s/*.caddy

%s {
	@api path %s
	handle @api {
		reverse_proxy %s
	}
%s%s	handle {
		respond 404
	}
}
`, routesDir, s.cfg.Hostname, strings.Join(PublicAPIPaths, " "), s.cfg.APIListen,
		telemetryProxyBlock(telemetry.MetricsPath, telemetry.GatewayMetricsPath),
		telemetryProxyBlock(telemetry.DiscoveryPath, telemetry.GatewayDiscoveryPath)))
}

// unmanagedCaddy returns a non-empty reason when a hostname-less control plane
// already has Caddy state that eregion did not install. Callers must not run
// the package install or mutate caddy.service while an unmanaged reason exists:
// apt's postinst can otherwise overwrite a Caddyfile or start a service.
func (s *ingressStep) unmanagedCaddy(ctx context.Context) (string, error) {
	if _, err := os.Lstat(s.caddyConfigPath()); err == nil {
		return s.caddyConfigPath() + " exists", nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, `if ! command -v systemctl >/dev/null 2>&1; then echo systemctl:missing; exit 0; fi
for unit in caddy.service caddy-api.service; do
  if systemctl is-enabled --quiet "$unit" >/dev/null 2>&1; then echo "$unit:enabled"; fi
  if systemctl is-active --quiet "$unit" >/dev/null 2>&1; then echo "$unit:active"; fi
done`)
	if err != nil {
		return "", err
	}
	output = strings.TrimSpace(output)
	switch output {
	case "":
		return "", nil
	case "systemctl:missing":
		return "systemd is unavailable", nil
	default:
		return strings.ReplaceAll(output, "\n", ", "), nil
	}
}

// neutralizeFreshCaddy removes only package-created defaults on a hostname-less
// node. Preflight established that no Caddyfile and no enabled/active Caddy
// service existed, so this never disables an unrelated running service.
func (s *ingressStep) neutralizeFreshCaddy(ctx context.Context, report engine.Reporter) error {
	cleanup := `set -euo pipefail
rm -f /etc/caddy/Caddyfile
for unit in caddy.service caddy-api.service; do
  if systemctl is-enabled --quiet "$unit" >/dev/null 2>&1 || systemctl is-active --quiet "$unit" >/dev/null 2>&1; then
    systemctl disable --now "$unit" >/dev/null
  fi
done`
	return s.exec.Run(ctx, s.ID()+"-cleanup", cleanup, nil, report)
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

// caddyAccountCheckScript verifies the account the telemetry and public Caddy
// units actually run as. A matching binary elsewhere on PATH is insufficient.
func caddyAccountCheckScript() string {
	return `if getent group caddy >/dev/null 2>&1 && getent passwd caddy >/dev/null 2>&1; then echo account:ok; else echo account:missing; fi`
}

// caddyAccountScript creates the service account only when it is absent. It
// does not modify an existing Caddy configuration or service.
func caddyAccountScript() string {
	return `set -euo pipefail
if ! getent group caddy >/dev/null 2>&1; then
  groupadd --system caddy
fi
if ! getent passwd caddy >/dev/null 2>&1; then
  useradd --system --gid caddy --create-home --home-dir /var/lib/caddy --shell /usr/sbin/nologin --comment "Caddy web server" caddy
fi`
}

func (s *ingressStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	if strings.TrimSpace(s.cfg.Hostname) == "" {
		reason, err := s.unmanagedCaddy(ctx)
		if err != nil {
			return engine.CheckResult{}, err
		}
		if reason != "" {
			return incomplete("refusing to manage existing Caddy state: " + reason)
		}
	}
	if _, _, ok := caddyArtifact(s.arch()); !ok {
		return engine.CheckResult{}, fmt.Errorf("%w: no pinned Caddy artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	version, err := s.exec.Output(ctx, CaddyBinaryPath+" version")
	if err != nil || !pinnedCaddyVersion(version) {
		return incomplete("Caddy must report pinned version " + CaddyVersion + " at " + CaddyBinaryPath)
	}
	account, err := checkOutput(ctx, engine.Runtime{}, s.exec, caddyAccountCheckScript())
	if err != nil {
		return engine.CheckResult{}, err
	}
	if account != "account:ok" {
		return incomplete("Caddy service account caddy:caddy is missing")
	}
	if strings.TrimSpace(s.cfg.Hostname) == "" {
		return complete("Caddy " + CaddyVersion + " is installed for the dedicated telemetry endpoint; no public Caddy config or service")
	}
	ok, _, err := sameFileContent(s.caddyConfigPath(), s.caddyfile())
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
	hostnameLess := strings.TrimSpace(s.cfg.Hostname) == ""
	if hostnameLess {
		reason, err := s.unmanagedCaddy(ctx)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if reason != "" {
			fault := contract.NewFault(contract.PreconditionFailed,
				"refusing to install Caddy over unmanaged existing state: "+reason,
				"Remove or stop the existing Caddy configuration/service, or configure a public hostname so eregion can manage it.")
			fault.Details = map[string]any{"unmanaged_caddy": reason}
			return engine.ApplyResult{}, fault
		}
	}
	// Only a changed executable needs a restart, which drops every site's
	// connections; a Caddyfile change on the pinned version reloads.
	installed, err := s.exec.Output(ctx, CaddyBinaryPath+" version")
	restart := err != nil || !pinnedCaddyVersion(installed)
	if err := s.exec.Run(ctx, s.ID(), installCaddyScript, map[string]string{
		"CADDY_VERSION": CaddyVersion, "CADDY_ARCH": s.arch(), "CADDY_URL": url, "CADDY_SHA256": checksum, "CADDY_BIN": CaddyBinaryPath,
	}, report); err != nil {
		if hostnameLess {
			if cleanupErr := s.neutralizeFreshCaddy(ctx, report); cleanupErr != nil {
				return engine.ApplyResult{}, errors.Join(err, fmt.Errorf("neutralize the failed Caddy install: %w", cleanupErr))
			}
		}
		return engine.ApplyResult{}, err
	}
	if err := s.exec.Run(ctx, s.ID()+"-account", caddyAccountScript(), nil, report); err != nil {
		return engine.ApplyResult{}, fmt.Errorf("ensure the Caddy service account: %w", err)
	}
	if hostnameLess {
		// apt's postinst may create its default public Caddyfile and start
		// caddy.service. The state was absent at preflight, so neutralize only
		// the package's fresh defaults; never touch a pre-existing service.
		if err := s.neutralizeFreshCaddy(ctx, report); err != nil {
			return engine.ApplyResult{}, err
		}
		return engine.ApplyResult{Notes: []string{"Caddy " + CaddyVersion + " is installed for the dedicated telemetry endpoint; no public Caddy config or service"}, Result: map[string]any{
			"hostname": "", "paths": []string{}, "public": false, "version": CaddyVersion, "sha256": checksum,
		}}, nil
	}
	// The worker owns the routes file; it rewrites it on every domain change.
	routes := fmt.Sprintf(`set -euo pipefail
install -d -m 0755 -o %[1]s -g %[2]s %[3]s
[ -f %[4]s ] || install -m 0644 -o %[1]s -g %[2]s /dev/null %[4]s`, shellQuote(s.cfg.Layout.User), shellQuote(s.cfg.Layout.Group), platform.CaddyRoutesDir, platform.CaddyRoutesPath)
	if err := s.exec.Run(ctx, s.ID(), routes, nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	// Serialize the root publish/activation with the worker's app-fragment
	// publish/activation. The worker takes its PostgreSQL desired-route lock
	// first and then this local lock; host bootstrap takes only this local lock.
	if err := s.ensureIngressLock(); err != nil {
		return engine.ApplyResult{}, err
	}
	release, err := engine.AcquireFile(ctx, s.ingressLockPath(), engine.LockExclusive, 30*time.Second)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	defer release()
	previous, previousMode, existed, err := fileState(s.caddyConfigPath())
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(s.caddyConfigPath(), s.caddyfile(), "root", "root"); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := s.exec.Run(ctx, s.ID(), caddyActivateScript(s.caddyConfigPath(), restart), nil, report); err != nil {
		// Roll back only the root Caddyfile. The current app fragment belongs
		// to the worker and is deliberately never part of this snapshot.
		activationErr := interruptedError(ctx, err)
		recoveryCtx, cancel := managedRecoveryContext(ctx)
		restoreErr := s.restoreRootCaddyfile(recoveryCtx, previous, previousMode, existed, report)
		cancel()
		return engine.ApplyResult{}, joinRecoveryError(activationErr, restoreErr)
	}
	return engine.ApplyResult{Notes: []string{"Caddy " + CaddyVersion + " serves " + s.cfg.Hostname}, Result: map[string]any{
		"hostname": s.cfg.Hostname, "paths": PublicAPIAndTelemetryPaths(), "version": CaddyVersion, "sha256": checksum,
	}}, nil
}

// ensureIngressLock creates the shared local lock with permissions the worker
// account can use. It never replaces an existing lock inode, which would
// invalidate a lock another process already holds.
func (s *ingressStep) ensureIngressLock() error {
	path := s.ingressLockPath()
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o660) //nolint:gosec // G304: fixed node state path.
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	return chownIfRoot(path, s.cfg.Layout.User, s.cfg.Layout.Group)
}

// PublicAPIAndTelemetryPaths names the exact public paths the root Caddyfile
// owns. It excludes the worker's application routes and describes no broad
// /metrics proxy.
func PublicAPIAndTelemetryPaths() []string {
	paths := append([]string(nil), PublicAPIPaths...)
	return append(paths, telemetry.MetricsPath, telemetry.DiscoveryPath)
}

func (s *ingressStep) restoreRootCaddyfile(ctx context.Context, previous []byte, mode os.FileMode, existed bool, report engine.Reporter) error {
	path := s.caddyConfigPath()
	if existed {
		if err := writeFile(path, previous, mode, "root", "root"); err != nil {
			return fmt.Errorf("restore the previous public Caddyfile: %w", err)
		}
	} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove the failed public Caddyfile: %w", err)
	}
	if err := s.exec.Run(ctx, s.ID()+"-rollback", caddyActivateScript(path, false), nil, report); err != nil {
		return fmt.Errorf("reactivate the previous public Caddyfile: %w", interruptedError(ctx, err))
	}
	return nil
}

// caddyActivateScript validates the Caddyfile and activates it. A reload keeps
// the old executable in memory after a package upgrade, so a changed binary
// restarts; reload-or-restart also starts a stopped service.
func caddyActivateScript(configPath string, restart bool) string {
	activate := "systemctl reload-or-restart caddy.service"
	if restart {
		activate = "systemctl restart caddy.service"
	}
	return `set -euo pipefail
` + CaddyBinaryPath + ` validate --config ` + shellQuote(configPath) + ` --adapter caddyfile >/dev/null
systemctl enable caddy.service >/dev/null 2>&1
` + activate + `
systemctl is-active --quiet caddy.service`
}

func (s *ingressStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	url, checksum, ok := caddyArtifact(s.arch())
	if !ok {
		return nil, fmt.Errorf("%w: no pinned Caddy artifact for architecture %s", engine.ErrPermanent, s.arch())
	}
	lines := []string{"+ install Caddy " + CaddyVersion + " from " + url, "+ verify sha256 " + checksum}
	if strings.TrimSpace(s.cfg.Hostname) == "" {
		return append(lines, "+ refuse an existing /etc/caddy/Caddyfile or enabled/active Caddy service; keep a fresh package install without a public config"), nil
	}
	raw, _, _, err := fileState(s.caddyConfigPath())
	if err != nil {
		return nil, err
	}
	return append(lines, simpleDiff(s.caddyConfigPath(), raw, s.caddyfile())...), nil
}

func pinnedCaddyVersion(output string) bool {
	fields := strings.Fields(output)
	return len(fields) > 0 && fields[0] == "v"+CaddyVersion
}

func (s *ingressStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
