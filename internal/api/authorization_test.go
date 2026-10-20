package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestActionGrantsBoundKindAndTarget(t *testing.T) {
	principal := Principal{Actor: contract.Actor{Kind: "ci", Subject: "deploy-ci"}, ActionGrants: []ActionGrant{
		{Kind: "platform.deploy", Target: "env/api/production"},
		{Kind: "platform.release.create", Target: "env/api/*"},
	}}
	for _, tc := range []struct {
		kind, target string
		allowed      bool
	}{
		{"platform.deploy", "env/api/production", true},
		{"platform.deploy", "env/api/staging", false},
		{"platform.deploy", "env/other/production", false},
		{"platform.release.create", "env/api/staging", true},
		{"platform.release.create", "env/api/staging/nested", false},
		{"platform.node.register", "platform", false},
		{"platform.resource.destroy", "env/api/production", false},
	} {
		err := authorizePrincipalAction(principal, tc.kind, tc.target)
		if (err == nil) != tc.allowed {
			t.Fatalf("%s %s: allowed=%t err=%v", tc.kind, tc.target, tc.allowed, err)
		}
	}
}

func TestAutomationCannotInheritAdministrativeAccess(t *testing.T) {
	for _, kind := range []string{"ci", "webhook"} {
		principal := Principal{Actor: contract.Actor{Kind: kind, Subject: "automation"}}
		if err := authorizePrincipalAction(principal, "platform.deploy", "env/api/production"); contract.Classify(err).Code != contract.Forbidden {
			t.Fatalf("unscoped %s gained platform access", kind)
		}
		if err := authorizePrincipalAction(principal, "diagnostic.test", "control-plane"); err != nil {
			t.Fatalf("diagnostic compatibility: %v", err)
		}
	}
	operator := Principal{Actor: contract.Actor{Kind: "operator", Subject: "admin"}}
	if err := authorizePrincipalAction(operator, "platform.node.register", "platform"); err != nil {
		t.Fatal(err)
	}
}

func TestRejectAmbiguousGrants(t *testing.T) {
	for _, grant := range []ActionGrant{
		{Kind: "platform.*", Target: "env/api/production"},
		{Kind: "platform.deploy", Target: "*"},
		{Kind: "platform.deploy", Target: "env/*/production"},
		{Kind: "platform.deploy", Target: "env/api/prod*"},
		{Kind: "platform.deploy", Target: "env/api/../production"},
		{Kind: "platform.deploy", Target: "env/api/production\n"},
	} {
		if validateGrants([]ActionGrant{grant}) == nil {
			t.Fatalf("accepted invalid grant: %+v", grant)
		}
	}
}

func TestScopedInventoryFailsClosed(t *testing.T) {
	token := strings.Repeat("s", 32)
	digest := sha256.Sum256([]byte(token))
	principal := Principal{
		Actor: contract.Actor{Kind: "ci", Subject: "deploy-ci"}, TokenSHA256: hex.EncodeToString(digest[:]),
		Scopes:       []string{"operations:read", "operations:submit"},
		ActionGrants: []ActionGrant{{Kind: "platform.deploy", Target: "env/api/production"}},
	}
	auth, err := NewAuth([]Principal{principal})
	if err != nil {
		t.Fatal(err)
	}
	// NewAuth must not retain caller-owned mutable grants.
	principal.ActionGrants[0].Target = "env/other/production"
	for _, tc := range []struct {
		collection, app, environment string
		allowed                      bool
	}{
		{"releases", "api", "production", true},
		{"environments", "api", "production", true},
		{"releases", "api", "staging", false},
		{"releases", "other", "production", false},
		{"releases", "api", "", false},
		{"summary", "api", "production", false},
		{"nodes", "", "", false},
		{"secrets", "api", "production", false},
	} {
		err := auth.authorizeInventory("Bearer "+token, platform.InventoryQuery{Collection: tc.collection, App: tc.app, Environment: tc.environment})
		if (err == nil) != tc.allowed {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}
