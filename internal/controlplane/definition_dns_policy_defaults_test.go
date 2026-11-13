package controlplane

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
)

type policyTestRow func(...any) error

func (r policyTestRow) Scan(dest ...any) error { return r(dest...) }

func TestDefinitionDNSPolicyAbsentAndErrors(t *testing.T) {
	policy, err := scanDefinitionDNSPolicy(policyTestRow(func(...any) error { return pgx.ErrNoRows }))
	if err != nil || policy.Enabled || policy.Revision != 0 || policy.ProxyMode != "dns-only" || policy.Origins == nil {
		t.Fatalf("absent policy = %#v, %v", policy, err)
	}
	_, err = scanDefinitionDNSPolicy(policyTestRow(func(...any) error { return errors.New("missing table") }))
	if err == nil {
		t.Fatal("database error was treated as an absent policy")
	}
}

func TestDefinitionDNSPolicyRejectsCorruptPair(t *testing.T) {
	_, err := scanDefinitionDNSPolicy(policyTestRow(func(dest ...any) error {
		*dest[0].(*int64) = 1
		*dest[1].(*bool) = true
		provider := "cloudflare"
		*dest[2].(**string) = &provider
		*dest[6].(*string) = "dns-only"
		return nil
	}))
	if err == nil {
		t.Fatal("incomplete provider/zone pair accepted")
	}
}

func TestDefinitionDNSPolicyInputDefaultsAndValidation(t *testing.T) {
	if _, err := validateDefinitionDNSPolicyInput(api.DefinitionDNSPolicyInput{}); err != nil {
		t.Fatal(err)
	}
	for _, input := range []api.DefinitionDNSPolicyInput{
		{Enabled: true, Zone: "zone", ProxyMode: "dns-only"},
		{Enabled: true, Origins: []string{"8.8.8.8"}, ProxyMode: "dns-only"},
		{Zone: "zone"},
	} {
		if _, err := validateDefinitionDNSPolicyInput(input); err == nil {
			t.Fatalf("accepted incomplete input: %#v", input)
		}
	}
}
