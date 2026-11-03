package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

type packagesCheckExec struct {
	output string
}

func (e *packagesCheckExec) Output(context.Context, string) (string, error) { return e.output, nil }
func (e *packagesCheckExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	panic("a read-only package check must not apply")
}

type packagesApplyExec struct {
	t      *testing.T
	dir    string
	script string
}

func (e *packagesApplyExec) Output(context.Context, string) (string, error) {
	return "", nil
}

func (e *packagesApplyExec) Run(_ context.Context, _ string, script string, _ map[string]string, _ engine.Reporter) error {
	e.script = script
	command := exec.Command("bash", "-c", script) //nolint:gosec // G204: test fixture script from this package.
	command.Env = append(os.Environ(), "PATH="+e.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		e.t.Logf("package apply output: %s", output)
	}
	return err
}

func runPackagesApply(t *testing.T, step *packagesStep, present []string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "apt.log")
	put(t, filepath.Join(dir, "dpkg-query"), `#!/bin/sh
for last; do :; done
case " $PRESENT " in
  *" $last "*) printf 'ii ' ;;
  *) exit 1 ;;
esac
`, 0o700)
	put(t, filepath.Join(dir, "apt-get"), `#!/bin/sh
printf '%s\n' "$*" >> "$APT_LOG"
`, 0o700)
	t.Setenv("PRESENT", strings.Join(present, " "))
	t.Setenv("APT_LOG", log)
	step.exec = &packagesApplyExec{t: t, dir: dir}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(raw)
}

func TestPackagesApplyScriptInstallsOnlyMissingPackagesWithNoUpgrade(t *testing.T) {
	main, err := ConfigForRole(RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	step := &packagesStep{role: main}
	script := step.applyScript()
	for _, want := range []string{
		"for package in 'ca-certificates' 'passwd' 'curl' 'tar' 'docker.io' 'git' 'polkitd'",
		"missing+=(\"$package\")",
		`apt-get install -y --no-upgrade --no-install-recommends "${missing[@]}"`,
		"export DEBIAN_FRONTEND=noninteractive",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("package apply script lacks %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "apt-get update -qq") < strings.Index(script, `if [ ${#missing[@]} -eq 0 ]`) {
		t.Fatalf("apt-get update can run when nothing is missing:\n%s", script)
	}
}

func TestPackagesApplyInstallsOnlyMissingPackages(t *testing.T) {
	main, err := ConfigForRole(RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	all := (&packagesStep{role: main}).packages()
	for _, tc := range []struct {
		name    string
		present []string
		want    []string
	}{
		{
			name:    "existing node with docker installed",
			present: []string{"ca-certificates", "docker.io"},
			want:    []string{"passwd", "curl", "tar", "git", "polkitd"},
		},
		{name: "fresh install", want: all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := runPackagesApply(t, &packagesStep{role: main}, tc.present)
			if !strings.Contains(log, "update -qq") {
				t.Fatalf("apt-get update did not run: %q", log)
			}
			install := lineWithPrefix(log, "install ")
			if install == "" {
				t.Fatalf("apt-get install did not run: %q", log)
			}
			fields := strings.Fields(install)
			if !slices.Contains(fields, "--no-upgrade") || !slices.Contains(fields, "--no-install-recommends") {
				t.Fatalf("install options = %v", fields)
			}
			for _, name := range tc.present {
				if slices.Contains(fields, name) {
					t.Fatalf("an already-installed package %q was passed to apt-get: %v", name, fields)
				}
			}
			for _, name := range tc.want {
				if !slices.Contains(fields, name) {
					t.Fatalf("missing package %q was not passed to apt-get: %v", name, fields)
				}
			}
		})
	}
}

func TestPackagesApplyDoesNotTouchAptWhenEverythingIsPresent(t *testing.T) {
	main, err := ConfigForRole(RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	step := &packagesStep{role: main}
	if log := runPackagesApply(t, step, step.packages()); log != "" {
		t.Fatalf("apt-get ran with no missing packages: %q", log)
	}
}

func TestPackagesCheckPassesWhenAllArePresent(t *testing.T) {
	main, err := ConfigForRole(RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	step := &packagesStep{role: main}
	var lines []string
	for _, name := range step.packages() {
		lines = append(lines, name+":ok")
	}
	step.exec = &packagesCheckExec{output: strings.Join(lines, "\n")}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("all packages present: %+v, %v", check, err)
	}
	step.exec = &packagesCheckExec{output: strings.Join(lines[1:], "\n") + "\n" + step.packages()[0] + ":missing"}
	check, err = step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete || !strings.Contains(check.Detail, step.packages()[0]) {
		t.Fatalf("one package missing: %+v, %v", check, err)
	}
}

func lineWithPrefix(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func TestPackagesInstallPolicyKitOnTheRegistryRole(t *testing.T) {
	if got := (&packagesStep{}).ID(); got != "host-packages-v2" {
		t.Fatalf("adding polkitd changed the package contract; use a new step version, got %q", got)
	}
	main, err := ConfigForRole(RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	packages := (&packagesStep{role: main}).packages()
	if !slices.Contains(packages, "polkitd") {
		t.Fatalf("main packages %v lack polkitd", packages)
	}
	application, err := ConfigForRole(RoleApplication)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range (&packagesStep{role: application}).packages() {
		if name == "polkitd" {
			t.Fatal("the application role installs the polkit policy daemon it does not use")
		}
	}
}

func TestTelemetryComposePackageFollowsDistribution(t *testing.T) {
	role, err := ConfigForRole(RoleTelemetry)
	if err != nil {
		t.Fatal(err)
	}
	for distro, want := range map[string]string{"ubuntu": "docker-compose-v2", "debian": "docker-compose"} {
		packages := (&packagesStep{role: role, distro: distro}).packages()
		if !slices.Contains(packages, want) {
			t.Errorf("%s: packages %v lack %s", distro, packages, want)
		}
	}
	main, _ := ConfigForRole(RoleMain)
	for _, name := range (&packagesStep{role: main, distro: "ubuntu"}).packages() {
		if name == "docker-compose" || name == "docker-compose-v2" {
			t.Errorf("main role must not install %s", name)
		}
	}
}
