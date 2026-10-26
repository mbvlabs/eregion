package host

import (
	"slices"
	"testing"
)

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
