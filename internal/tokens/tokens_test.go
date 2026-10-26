package tokens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateScopesExpiresAndStoresOnlyTheHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	now := time.Unix(1800000000, 0).UTC()
	issued, err := Create(path, CreateRequest{App: "api", Environment: "production", TTL: 2 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Token, Prefix) {
		t.Fatalf("token = %q", issued.Token)
	}
	if issued.Metadata.App != "api" || issued.Metadata.Environment != "production" {
		t.Fatalf("metadata = %+v", issued.Metadata)
	}
	if len(issued.Metadata.ActionGrants) != 1 || issued.Metadata.ActionGrants[0].Target != "env/api/production" {
		t.Fatalf("grants = %+v", issued.Metadata.ActionGrants)
	}
	if !issued.Metadata.ExpiresAt.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("expiry = %v", issued.Metadata.ExpiresAt)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), issued.Token) {
		t.Fatal("the auth file contains the plaintext token")
	}
	list, err := List(path)
	if err != nil || len(list) != 1 || list[0].Name != issued.Metadata.Name {
		t.Fatalf("list = %+v, %v", list, err)
	}
	removed, err := Revoke(path, issued.Metadata.Name)
	if err != nil || removed.Name != issued.Metadata.Name {
		t.Fatalf("revoke = %+v, %v", removed, err)
	}
	if list, _ := List(path); len(list) != 0 {
		t.Fatalf("token survived revocation: %+v", list)
	}
}
