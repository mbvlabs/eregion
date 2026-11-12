package controlplane

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
)

func TestDefinitionAdmissionNonceAndDigestValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{strings.Repeat("ab", 32), true}, {"", false}, {strings.Repeat("AB", 32), false},
		{strings.Repeat("z", 64), false}, {strings.Repeat("a", 63), false},
	} {
		if got := definitionHex256(tc.value); got != tc.valid {
			t.Fatalf("definitionHex256(%q) = %v", tc.value, got)
		}
	}
}

func TestDefinitionAdmissionRemovalConfirmation(t *testing.T) {
	p := api.DefinitionPlan{App: "api", Environment: "production", ConfirmTarget: "api/production", Domains: []api.DefinitionDomainChange{{Action: "remove", Name: "old.example.com"}}}
	for _, value := range []string{"", "api", "old.example.com", "api/staging"} {
		if err := definitionConfirm(p, value); contract.Classify(err).Code != contract.InputRequired {
			t.Fatalf("confirmation %q: %v", value, err)
		}
	}
	if err := definitionConfirm(p, "api/production"); err != nil {
		t.Fatal(err)
	}
	p.Domains[0].Action = "update"
	p.ConfirmTarget = ""
	if err := definitionConfirm(p, ""); err != nil {
		t.Fatal(err)
	}
	p.Domains[0].Action = "remove" // forged or incomplete plan still fails closed
	if err := definitionConfirm(p, ""); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("domain fallback: %v", err)
	}
}

func TestDefinitionAdmissionRequestHash(t *testing.T) {
	actor := contract.Actor{Kind: "operator", Subject: "one"}
	input := api.DefinitionApplyInput{Plan: api.DefinitionPlanInput{Definition: "[automation]\nauto_deploy = false\n"}, ApprovedDigest: strings.Repeat("a", 64), IdempotencyKey: "key"}
	base, err := definitionRequestHash(actor, "env/api/production", input)
	if err != nil {
		t.Fatal(err)
	}
	other := input
	other.Plan.Definition = "[automation]\n"
	changed, _ := definitionRequestHash(actor, "env/api/production", other)
	if changed == base {
		t.Fatal("automation presence was not bound")
	}
	actor.Subject = "two"
	changed, _ = definitionRequestHash(actor, "env/api/production", input)
	if changed == base {
		t.Fatal("actor was not bound")
	}
	actor.Subject = "one"
	changed, _ = definitionRequestHash(actor, "env/api/staging", input)
	if changed == base {
		t.Fatal("target was not bound")
	}
}
