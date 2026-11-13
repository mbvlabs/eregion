package controlplane

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func TestDeploymentWriteValidation(t *testing.T) {
	for _, tc := range []struct {
		action platform.DeploymentAction
		refs   string
		valid  bool
	}{
		{platform.ActionDeploy, "", true},
		{platform.ActionRollback, `{}`, true},
		{platform.ActionRestart, `{"TOKEN":1}`, true},
		{"invalid", `{}`, false},
		{platform.ActionDeploy, `[]`, false},
		{platform.ActionDeploy, `null`, false},
		{platform.ActionDeploy, `{`, false},
	} {
		if err := validateDeploymentWrite(tc.action, []byte(tc.refs)); (err == nil) != tc.valid {
			t.Fatalf("action=%q refs=%q: %v", tc.action, tc.refs, err)
		}
	}
}

func TestDomainObservationWriteValidation(t *testing.T) {
	if err := validateDomainObservationWrite(platform.DNSOK, platform.CertificateValid, platform.PublicOK, 200); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{-1, 99, 600} {
		if err := validateDomainObservationWrite(platform.DNSOK, platform.CertificateValid, platform.PublicOK, status); err == nil {
			t.Fatalf("accepted HTTP status %d", status)
		}
	}
	if err := validateDomainObservationWrite("bogus", platform.CertificateValid, platform.PublicOK, 0); err == nil {
		t.Fatal("accepted invalid DNS state")
	}
}
