package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestDomainRemovalConfirmationNamesHostname(t *testing.T) {
	if err := ValidateDomainConfirmation("hello.example.com", "hello.example.com"); err != nil {
		t.Fatalf("matching hostname: %v", err)
	}
	if err := ValidateDomainConfirmation("hello.example.com", ""); contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("empty token code = %v", contract.Classify(err).Code)
	}
	if err := ValidateDomainConfirmation("hello.example.com", "other.example.com"); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("wrong hostname code = %v", contract.Classify(err).Code)
	}
}

func TestEnvironmentDomainsUsesInventoryObservationAndReady(t *testing.T) {
	env := Environment{App: "api", Name: "production"}
	inventory := []DomainBinding{
		{
			Domain: "other.example.com", App: "api", Environment: "staging",
			RouteState: RouteApplied, DNSState: DNSOK, CertificateState: CertificateValid, PublicState: PublicOK,
			DesiredRevision: 1, AppliedRevision: 1,
		},
		{
			Domain: "api.example.com", App: "api", Environment: "production", Primary: true,
			RouteState: RouteApplied, DNSState: DNSOK, CertificateState: CertificateValid, PublicState: PublicOK,
			DesiredRevision: 2, AppliedRevision: 2,
		},
		{
			Domain: "pending.example.com", App: "api", Environment: "production",
			RouteState: RoutePending, DNSState: DNSPending, CertificateState: CertificatePending, PublicState: PublicPending,
			DesiredRevision: 1, AppliedRevision: 0,
		},
	}
	domains := EnvironmentDomains(env, inventory)
	if len(domains) != 2 {
		t.Fatalf("domains = %+v", domains)
	}
	if domains[0].Domain != "api.example.com" || !domains[0].Ready {
		t.Fatalf("ready observed domain = %+v", domains[0])
	}
	if domains[1].Domain != "pending.example.com" || domains[1].Ready {
		t.Fatalf("pending domain = %+v", domains[1])
	}
	if domains[0].RouteState != RouteApplied || domains[0].DNSState != DNSOK {
		t.Fatalf("inventory state not preserved: %+v", domains[0])
	}
	raw, err := json.Marshal(domains[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"route_state":"applied"`, `"dns_state":"ok"`, `"certificate_state":"valid"`, `"public_state":"ok"`, `"ready":true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("status JSON missing %s: %s", want, raw)
		}
	}
}
