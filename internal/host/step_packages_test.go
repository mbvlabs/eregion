package host

import (
	"slices"
	"testing"
)

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
