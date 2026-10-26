package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestDeployTokenScopeAndExpiry(t *testing.T) {
	token := strings.Repeat("t", 43)
	sum := sha256.Sum256([]byte(token))
	expires := time.Now().Add(time.Hour)
	auth, err := NewAuth([]Principal{{
		Actor:       contract.Actor{Kind: "ci", Subject: "deploy-api-production"},
		TokenSHA256: hex.EncodeToString(sum[:]),
		Scopes:      []string{"operations:submit", "operations:read"},
		ActionGrants: []ActionGrant{
			{Kind: "platform.deploy", Target: "env/api/production"},
		},
		Name:      "deploy-api-production",
		ExpiresAt: &expires,
	}})
	if err != nil {
		t.Fatal(err)
	}
	header := "Bearer " + token
	if _, err := auth.authenticateAction(header, "operations:submit", "platform.deploy", "env/api/production"); err != nil {
		t.Fatalf("authorized target refused: %v", err)
	}
	if _, err := auth.authenticateAction(header, "operations:submit", "platform.deploy", "env/api/staging"); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("wrong environment: %v", err)
	}
	if _, err := auth.authenticateAction(header, "operations:submit", "platform.release.create", "env/api/production"); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("wrong action: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	auth.principals[sum] = Principal{
		Actor: contract.Actor{Kind: "ci", Subject: "expired"}, TokenSHA256: hex.EncodeToString(sum[:]),
		Scopes: []string{"operations:submit"}, ActionGrants: []ActionGrant{{Kind: "platform.deploy", Target: "env/api/production"}},
		ExpiresAt: &past,
	}
	if _, err := auth.authenticateAction(header, "operations:submit", "platform.deploy", "env/api/production"); contract.Classify(err).Code != contract.Unauthenticated {
		t.Fatalf("expired token: %v", err)
	}
}

func TestPushSignatureAndBranch(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	secret := []byte("0123456789abcdef0123456789abcdef")
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	header := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !validGitHubSignature(secret, body, header) {
		t.Fatal("valid signature rejected")
	}
	if validGitHubSignature(secret, []byte(`{"ref":"refs/heads/other"}`), header) {
		t.Fatal("signature accepted for another body")
	}
	if !PushBranchMatches("main", "main") || !PushBranchMatches("refs/heads/main", "main") {
		t.Fatal("bound branch did not match")
	}
	if PushBranchMatches("main", "release") || PushBranchMatches("", "main") {
		t.Fatal("unbound branch matched")
	}
}
