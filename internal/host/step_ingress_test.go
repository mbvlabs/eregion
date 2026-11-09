package host

import (
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
		if step.ID() != "host-ingress-v2" {
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
	reload := caddyActivateScript(false)
	if !strings.Contains(reload, "systemctl reload-or-restart caddy.service") || strings.Contains(reload, "systemctl restart caddy.service") {
		t.Fatalf("pinned binary does not reload:\n%s", reload)
	}
	restart := caddyActivateScript(true)
	if !strings.Contains(restart, "systemctl restart caddy.service") || strings.Contains(restart, "reload-or-restart") {
		t.Fatalf("new binary does not restart:\n%s", restart)
	}
	for _, script := range []string{reload, restart} {
		if !strings.HasPrefix(script, "set -euo pipefail\n"+CaddyBinaryPath+" validate ") || !strings.HasSuffix(script, "systemctl is-active --quiet caddy.service") {
			t.Fatalf("activation must validate first and verify last:\n%s", script)
		}
	}
}
