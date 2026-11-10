package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

func TestPinnedCaddyArtifactsAndInputs(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		url, checksum, ok := caddyArtifact(arch)
		if !ok || !strings.HasSuffix(url, "/caddy_2.11.4_linux_"+arch+".deb") {
			t.Fatalf("%s: %s", arch, url)
		}
		if decoded, err := hex.DecodeString(checksum); err != nil || len(decoded) != sha256.Size {
			t.Fatalf("%s: invalid checksum %q", arch, checksum)
		}
		step := &ingressStep{cfg: Config{Arch: arch, Hostname: "main.example.com"}}
		raw, err := step.Inputs()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{CaddyVersion, arch, url, checksum} {
			if !strings.Contains(string(raw), want) {
				t.Fatalf("inputs lack %q: %s", want, raw)
			}
		}
		if step.ID() != "host-ingress-v3" {
			t.Fatal("changed installation must use a new host step version")
		}
	}
	if _, _, ok := caddyArtifact("386"); ok {
		t.Fatal("unsupported architecture accepted")
	}
	step := &ingressStep{cfg: Config{Arch: "386"}}
	if _, err := step.Inputs(); !errors.Is(err, engine.ErrPermanent) {
		t.Fatalf("unsupported inputs: %v", err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); !errors.Is(err, engine.ErrPermanent) {
		t.Fatalf("unsupported apply: %v", err)
	}
}

type caddyVersionExec struct {
	version string
	err     error
}

func (e caddyVersionExec) Output(context.Context, string) (string, error) { return e.version, e.err }
func (e caddyVersionExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	panic("a read-only Caddy check must not apply")
}

func TestIngressCheckRejectsAnUnpinnedOrMissingCaddy(t *testing.T) {
	for _, e := range []caddyVersionExec{{version: "2.6.2"}, {version: "v2.11.40"}, {err: errors.New("not installed")}} {
		step := &ingressStep{cfg: Config{Arch: "amd64", Hostname: "main.example.com"}, exec: e}
		check, err := step.Check(context.Background(), engine.Runtime{})
		if err != nil || check.Complete || !strings.Contains(check.Detail, CaddyVersion) {
			t.Fatalf("version %q: check=%+v err=%v", e.version, check, err)
		}
	}
}

type caddyStateExec struct {
	version string
	state   string
	account string
}

func (e caddyStateExec) Output(_ context.Context, script string) (string, error) {
	if strings.Contains(script, "caddy version") {
		return e.version, nil
	}
	if strings.Contains(script, "getent group caddy") {
		if e.account == "" {
			return "account:ok", nil
		}
		return e.account, nil
	}
	return e.state, nil
}

func (e caddyStateExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	panic("preflight refusal must not apply")
}

func TestHostnameLessIngressRejectsUnmanagedCaddyState(t *testing.T) {
	for name, state := range map[string]string{
		"enabled public service": "caddy.service:enabled",
		"active public service":  "caddy.service:active",
		"active API service":     "caddy-api.service:active",
	} {
		t.Run(name, func(t *testing.T) {
			step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: caddyStateExec{version: "v" + CaddyVersion, state: state}, configPath: filepath.Join(t.TempDir(), "Caddyfile")}
			check, err := step.Check(context.Background(), engine.Runtime{})
			if err != nil || check.Complete || !strings.Contains(check.Detail, "refusing to manage existing Caddy state") {
				t.Fatalf("check = %+v err=%v", check, err)
			}
		})
	}
}

type caddyApplyExec struct {
	scripts []string
}

func (e *caddyApplyExec) Output(_ context.Context, script string) (string, error) {
	if strings.Contains(script, "caddy version") {
		return "v" + CaddyVersion, nil
	}
	return "", nil
}

func (e *caddyApplyExec) Run(_ context.Context, _ string, script string, _ map[string]string, _ engine.Reporter) error {
	e.scripts = append(e.scripts, script)
	return nil
}

func TestHostnameLessIngressInstallsCaddyWithoutWritingAPublicConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "Caddyfile")
	exec := &caddyApplyExec{}
	step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: exec, configPath: configPath}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(exec.scripts) != 3 {
		t.Fatalf("scripts = %v", exec.scripts)
	}
	for _, script := range exec.scripts {
		if strings.Contains(script, "writePublicFile") {
			t.Fatalf("unexpected script = %s", script)
		}
	}
	if !strings.Contains(strings.Join(exec.scripts, "\n"), "rm -f /etc/caddy/Caddyfile") {
		t.Fatalf("fresh apply did not neutralize the package default: %v", exec.scripts)
	}
	if !strings.Contains(strings.Join(exec.scripts, "\n"), "useradd --system --gid caddy") {
		t.Fatalf("fresh apply did not ensure the Caddy account: %v", exec.scripts)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("hostname-less apply wrote a public Caddyfile: %v", err)
	}
}

type caddyFailingInstallExec struct {
	scripts []string
	runs    int
}

func (e *caddyFailingInstallExec) Output(_ context.Context, script string) (string, error) {
	if strings.Contains(script, "caddy version") {
		return "v" + CaddyVersion, nil
	}
	return "", nil
}

func (e *caddyFailingInstallExec) Run(_ context.Context, _ string, script string, _ map[string]string, _ engine.Reporter) error {
	e.runs++
	e.scripts = append(e.scripts, script)
	if e.runs == 1 {
		return errors.New("apt failed")
	}
	return nil
}

func TestHostnameLessIngressCleansUpAFailedFreshInstall(t *testing.T) {
	exec := &caddyFailingInstallExec{}
	step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: exec, configPath: filepath.Join(t.TempDir(), "Caddyfile")}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err == nil || !strings.Contains(err.Error(), "apt failed") {
		t.Fatalf("apply = %v", err)
	}
	if len(exec.scripts) != 2 || !strings.Contains(exec.scripts[1], "rm -f /etc/caddy/Caddyfile") {
		t.Fatalf("failed install was not neutralized: %v", exec.scripts)
	}
}

func TestHostnameLessIngressRejectsExistingCaddyfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(path, []byte("example.com { respond ok }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: caddyStateExec{version: "v" + CaddyVersion}, configPath: path}
	if check, err := step.Check(context.Background(), engine.Runtime{}); err != nil || check.Complete {
		t.Fatalf("check = %+v err=%v", check, err)
	}
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("apply = %v, want precondition_failed", err)
	}
}

func TestHostnameLessIngressAcceptsFreshPinnedCaddy(t *testing.T) {
	step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: caddyStateExec{version: "v" + CaddyVersion}, configPath: filepath.Join(t.TempDir(), "Caddyfile")}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || !check.Complete || !strings.Contains(check.Detail, "no public Caddy config or service") {
		t.Fatalf("check = %+v err=%v", check, err)
	}
}

func TestHostnameLessIngressRequiresTheExactCaddyPath(t *testing.T) {
	step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: caddyExactPathExec{}, configPath: filepath.Join(t.TempDir(), "Caddyfile")}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("exact path was not accepted: check=%+v err=%v", check, err)
	}
}

type caddyExactPathExec struct{}

func (caddyExactPathExec) Output(_ context.Context, script string) (string, error) {
	switch {
	case script == CaddyBinaryPath+" version":
		return "v" + CaddyVersion, nil
	case strings.Contains(script, "getent group caddy"):
		return "account:ok", nil
	default:
		return "", nil
	}
}

func (caddyExactPathExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	panic("a read-only check must not apply")
}

func TestHostnameLessIngressRequiresTheCaddyAccount(t *testing.T) {
	step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: caddyStateExec{version: "v" + CaddyVersion, account: "account:missing"}, configPath: filepath.Join(t.TempDir(), "Caddyfile")}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete || !strings.Contains(check.Detail, "account") {
		t.Fatalf("check = %+v err=%v", check, err)
	}
}

func TestHostnameLessIngressRefusesBeforeInstallingOverUnmanagedCaddy(t *testing.T) {
	step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: caddyStateExec{version: "v" + CaddyVersion, state: "caddy.service:active"}, configPath: filepath.Join(t.TempDir(), "Caddyfile")}
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("apply = %v, want precondition_failed", err)
	}
}

func TestPinnedCaddyVersionIsExact(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"v2.11.4 h1:buildhash\n", true},
		{"2.6.2\n", false}, {"v2.11.40", false}, {"v2.11.3", false}, {"", false},
	} {
		if got := pinnedCaddyVersion(tc.output); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.output, got, tc.want)
		}
	}
}

// Only stub commands run: no downloads, apt, systemd or real host changes.
// Use the real sha256sum to prove a corrupt package never reaches apt.
func TestCaddyInstallerVerifiesBeforeInstallingAndChecksResult(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("executes a Debian/Ubuntu host installer using Linux checksum tools; artifact and version checks run on every platform")
	}
	for _, tc := range []struct {
		name, installed, after        string
		corrupt, wantInstall, wantErr bool
	}{
		{name: "fresh", after: "v2.11.4", wantInstall: true},
		{name: "old distribution package", installed: "2.6.2", after: "v2.11.4", wantInstall: true},
		{name: "already pinned", installed: "v2.11.4", after: "v2.11.4"},
		{name: "corrupt download", installed: "2.6.2", after: "v2.11.4", corrupt: true, wantErr: true},
		{name: "wrong installed version", installed: "2.6.2", after: "v2.11.3", wantInstall: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "version")
			log := filepath.Join(dir, "apt.log")
			put(t, state, tc.installed, 0o600)
			put(t, filepath.Join(dir, "caddy"), "#!/bin/sh\ncat \"$TEST_VERSION_FILE\"\n", 0o700)
			put(t, filepath.Join(dir, "dpkg"), "#!/bin/sh\necho amd64\n", 0o700)
			put(t, filepath.Join(dir, "curl"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
 if [ "$1" = --output ]; then shift; printf 'fixture package' > "$1"; exit 0; fi
 shift
done
exit 1
`, 0o700)
			put(t, filepath.Join(dir, "apt-get"), `#!/bin/sh
printf '%s\n' "$*" > "$TEST_APT_LOG"
printf '%s' "$TEST_AFTER" > "$TEST_VERSION_FILE"
`, 0o700)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CADDY_BIN", filepath.Join(dir, "caddy"))
			t.Setenv("TEST_VERSION_FILE", state)
			t.Setenv("TEST_APT_LOG", log)
			t.Setenv("TEST_AFTER", tc.after)
			t.Setenv("CADDY_VERSION", CaddyVersion)
			t.Setenv("CADDY_ARCH", "amd64")
			t.Setenv("CADDY_URL", "https://example.invalid/caddy.deb")
			sum := sha256.Sum256([]byte("fixture package"))
			checksum := hex.EncodeToString(sum[:])
			if tc.corrupt {
				checksum = strings.Repeat("0", 64)
			}
			t.Setenv("CADDY_SHA256", checksum)
			output, err := exec.Command("bash", "-c", installCaddyScript).CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v output=%s", err, output)
			}
			raw, err := os.ReadFile(log)
			installed := err == nil
			if installed != tc.wantInstall {
				t.Fatalf("apt called=%v want=%v output=%s", installed, tc.wantInstall, output)
			}
			if installed && !strings.Contains(string(raw), "--force-confold") {
				t.Fatal("install does not preserve existing config")
			}
		})
	}
}

// caddyPathMatch mirrors Caddy 2.11's path matcher for the pattern forms used in
// PublicAPIPaths: a single trailing * is a prefix match, otherwise path.Match,
// where each * is one segment.
func caddyPathMatch(pattern, requestPath string) bool {
	if strings.Count(pattern, "*") == 1 && strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(requestPath, strings.TrimSuffix(pattern, "*"))
	}
	matched, _ := path.Match(pattern, requestPath)
	return matched
}

func TestPublicAPIPathsReachTheHandlers(t *testing.T) {
	matches := func(requestPath string) bool {
		for _, pattern := range PublicAPIPaths {
			if caddyPathMatch(pattern, requestPath) {
				return true
			}
		}
		return false
	}
	for _, public := range []string{
		"/readyz",
		"/v1/environments/api/production/deployments",
		"/v1/environments/api/production/deployments/op_0123456789abcdef0123456789abcdef",
		"/v1/webhooks/github/api/production",
	} {
		if !matches(public) {
			t.Errorf("%s is not exposed by Caddy", public)
		}
	}
	for _, private := range []string{
		"/", "/v1/operations", "/v1/secrets", "/v1/secrets/x", "/v1/apps",
		"/v1/environments/api/production", "/v1/environments/api/deployments",
		"/private/x", "/metrics",
	} {
		if matches(private) {
			t.Errorf("%s must stay on loopback", private)
		}
	}
}

// A Caddyfile change on the pinned binary must reload, so a host upgrade does
// not drop every site's connections; a changed binary must restart.
func TestCaddyActivationRestartsOnlyForANewBinary(t *testing.T) {
	reload := caddyActivateScript("/etc/caddy/Caddyfile", false)
	if !strings.Contains(reload, "systemctl reload-or-restart caddy.service") || strings.Contains(reload, "systemctl restart caddy.service") {
		t.Fatalf("pinned binary does not reload:\n%s", reload)
	}
	restart := caddyActivateScript("/etc/caddy/Caddyfile", true)
	if !strings.Contains(restart, "systemctl restart caddy.service") || strings.Contains(restart, "reload-or-restart") {
		t.Fatalf("new binary does not restart:\n%s", restart)
	}
	for _, script := range []string{reload, restart} {
		if !strings.HasPrefix(script, "set -euo pipefail\n"+CaddyBinaryPath+" validate --config '/etc/caddy/Caddyfile' ") ||
			!strings.HasSuffix(script, "systemctl is-active --quiet caddy.service") {
			t.Fatalf("activation must validate first and verify last:\n%s", script)
		}
	}
}

func TestPublicTelemetryPathsAreExactAndUnrelatedMetricsPathsStayDenied(t *testing.T) {
	matches := func(requestPath string) bool {
		for _, pattern := range []string{telemetry.MetricsPath, telemetry.DiscoveryPath} {
			if caddyPathMatch(pattern, requestPath) {
				return true
			}
		}
		return false
	}
	for _, public := range []string{"/metrics/node", "/metrics/targets.json"} {
		if !matches(public) {
			t.Errorf("%s is not owned by the public telemetry proxy", public)
		}
	}
	for _, private := range []string{"/metrics", "/metrics/", "/metrics/node/", "/metrics/node/extra", "/metrics/targets.json/extra", "/metrics/other", "/node", "/targets.json"} {
		if matches(private) {
			t.Errorf("%s must not match a public telemetry route", private)
		}
	}
}

func TestIngressCaddyfileOwnsOnlyExactTelemetryPathsWithExplicitGatewayHost(t *testing.T) {
	step := &ingressStep{cfg: Config{Hostname: "main.example.com", APIListen: "127.0.0.1:8080"}}
	site := string(step.caddyfile())
	for _, want := range []string{
		"import /etc/caddy/eregion.d/*.caddy",
		"@api path /readyz",
		"handle /metrics/node {",
		"rewrite * /node",
		"handle /metrics/targets.json {",
		"rewrite * /targets.json",
		"reverse_proxy 127.0.0.1:9101",
		"header_up Host 127.0.0.1:9101",
		"dial_timeout 2s",
		"handle {\n\t\trespond 404",
	} {
		if !strings.Contains(site, want) {
			t.Fatalf("public Caddyfile missing %q:\n%s", want, site)
		}
	}
	for _, unwanted := range []string{"handle /metrics/*", "path /metrics", "header_up -Authorization", "header_up -Cookie", "10.78.0.1", "127.0.0.1:9100", "eregion-telemetry"} {
		if strings.Contains(site, unwanted) {
			t.Fatalf("public Caddyfile contains %q:\n%s", unwanted, site)
		}
	}
}

func TestIngressCaddyfileAdaptsWithPinnedCaddy(t *testing.T) {
	bin := os.Getenv("EREGION_CADDY_BIN")
	if bin == "" {
		t.Skip("set EREGION_CADDY_BIN to a pinned Caddy binary to adapt the generated public Caddyfile")
	}
	dir := t.TempDir()
	routesDir := filepath.Join(dir, "eregion.d")
	if err := os.MkdirAll(routesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A secret-free local application route; never touch the real
	// /etc/caddy/eregion.d files.
	fixture := filepath.Join(routesDir, "app.caddy")
	if err := os.WriteFile(fixture, []byte("app.example.com {\n\treverse_proxy 127.0.0.1:40001\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	step := &ingressStep{cfg: Config{Hostname: "main.example.com", APIListen: "127.0.0.1:8080"}}
	configPath := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(configPath, step.caddyfileForRoutesDir(routesDir), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "adapt", "--config", configPath, "--adapter", "caddyfile")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("public Caddyfile rejected by %s adapt: %v\n%s", bin, err, output)
	}
	for _, want := range []string{"main.example.com", "/metrics/node", "/metrics/targets.json", "127.0.0.1:9101", "app.example.com"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("adapted public Caddyfile output is missing %q:\n%s", want, output)
		}
	}
}

type caddyRollbackExec struct {
	scripts          []string
	activations      int
	firstErr         error
	cancel           context.CancelFunc
	recoveryCtx      context.Context
	recoveryLive     bool
	recoveryDeadline bool
	recoveryValue    any
}

func (e *caddyRollbackExec) Output(_ context.Context, script string) (string, error) {
	if strings.Contains(script, "caddy version") {
		return "v" + CaddyVersion, nil
	}
	return "", nil
}

func (e *caddyRollbackExec) Run(ctx context.Context, _ string, script string, _ map[string]string, _ engine.Reporter) error {
	e.scripts = append(e.scripts, script)
	if strings.Contains(script, "systemctl") {
		e.activations++
		if e.activations == 1 {
			if e.cancel != nil {
				e.cancel()
			}
			if e.firstErr != nil {
				return e.firstErr
			}
			return errors.New("activation rejected")
		}
		e.recoveryCtx = ctx
		e.recoveryLive = ctx.Err() == nil
		_, e.recoveryDeadline = ctx.Deadline()
		e.recoveryValue = ctx.Value(testContextKey("recovery"))
	}
	return nil
}

type testContextKey string

func TestIngressApplyRollsBackOnlyTheRootCaddyfileUnderBothUMasks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		umask int
	}{
		{name: "umask 077", umask: 0o077},
		{name: "umask 022", umask: 0o022},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withUmaskTest(tc.umask, func() {
				dir := t.TempDir()
				configPath := filepath.Join(dir, "Caddyfile")
				if err := os.WriteFile(configPath, []byte("old.example.com { respond ok }\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				// os.WriteFile applies the process umask. The fixture is meant to
				// represent an existing 0644 root config, so set it explicitly.
				if err := os.Chmod(configPath, 0o644); err != nil {
					t.Fatal(err)
				}
				exec := &caddyRollbackExec{}
				cfg := DefaultConfig(RoleMain, "main")
				cfg.Hostname = "main.example.com"
				step := &ingressStep{cfg: cfg, exec: exec, configPath: configPath, lockPath: filepath.Join(dir, "ingress.lock")}
				_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
				if err == nil || !strings.Contains(err.Error(), "activation rejected") {
					t.Fatalf("apply = %v, want the activation failure", err)
				}
				raw, mode, exists, err := fileState(configPath)
				if err != nil || !exists || mode != 0o644 || string(raw) != "old.example.com { respond ok }\n" {
					t.Fatalf("previous root Caddyfile was not restored: %q mode=%04o exists=%t err=%v", raw, mode, exists, err)
				}
				if len(exec.scripts) < 3 || !strings.Contains(strings.Join(exec.scripts, "\n"), "caddy validate") {
					t.Fatalf("activation/rollback scripts = %v", exec.scripts)
				}
				if exec.activations != 2 {
					t.Fatalf("failed activation was not followed by exactly one rollback activation: %d", exec.activations)
				}
			})
		})
	}
}

func TestIngressRollbackUsesLiveRecoveryContextAfterCancellation(t *testing.T) {
	for name, tc := range map[string]struct {
		firstErr  error
		cancel    bool
		wantCode  contract.Code
		wantError error
	}{
		"cancelled": {
			firstErr: context.Canceled, cancel: true,
			wantCode: contract.Cancelled, wantError: context.Canceled,
		},
		"deadline": {
			firstErr: context.DeadlineExceeded,
			wantCode: contract.Unavailable, wantError: context.DeadlineExceeded,
		},
		"engine interrupted with canceled caller": {
			firstErr: engine.ErrInterrupted, cancel: true,
			wantCode: contract.Cancelled, wantError: context.Canceled,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "Caddyfile")
			previous := []byte("old.example.com { respond ok }\n")
			if err := os.WriteFile(configPath, previous, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(configPath, 0o644); err != nil {
				t.Fatal(err)
			}
			exec := &caddyRollbackExec{firstErr: tc.firstErr}
			ctx := context.WithValue(context.Background(), testContextKey("recovery"), "preserved")
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				exec.cancel = cancel
			}
			cfg := DefaultConfig(RoleMain, "main")
			cfg.Hostname = "main.example.com"
			step := &ingressStep{cfg: cfg, exec: exec, configPath: configPath, lockPath: filepath.Join(dir, "ingress.lock")}
			_, err := step.Apply(ctx, engine.Runtime{}, nil)
			if !errors.Is(err, tc.wantError) || contract.Classify(err).Code != tc.wantCode {
				t.Fatalf("apply error = %v class=%s, want %s", err, contract.Classify(err).Code, tc.wantCode)
			}
			if exec.recoveryCtx == nil || !exec.recoveryLive {
				t.Fatalf("recovery ran with a cancelled context: %+v", exec.recoveryCtx)
			}
			if !exec.recoveryDeadline {
				t.Fatal("recovery context has no finite deadline")
			}
			if exec.recoveryValue != "preserved" {
				t.Fatal("recovery context lost the original values")
			}
			raw, err := os.ReadFile(configPath)
			if err != nil || !bytes.Equal(raw, previous) {
				t.Fatalf("previous root Caddyfile was not restored: %q err=%v", raw, err)
			}
		})
	}
}
