package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
		step := &ingressStep{cfg: Config{Arch: "amd64"}, exec: e}
		check, err := step.Check(context.Background(), engine.Runtime{})
		if err != nil || check.Complete || !strings.Contains(check.Detail, CaddyVersion) {
			t.Fatalf("version %q: check=%+v err=%v", e.version, check, err)
		}
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
