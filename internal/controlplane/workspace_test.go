package controlplane

import (
	"regexp"
	"strings"
	"testing"
)

func TestWorkspaceNameAndIDValidation(t *testing.T) {
	for _, name := range []string{"thelab", "a", "alpha-7", "team-01"} {
		if err := ValidateWorkspaceName(name); err != nil {
			t.Fatalf("valid workspace name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "Thelab", "-thelab", "thelab-", "the_lab", strings.Repeat("a", 65), "the lab"} {
		if err := ValidateWorkspaceName(name); err == nil {
			t.Fatalf("invalid workspace name %q was accepted", name)
		}
	}
	if err := ValidateWorkspaceID("ws_0123456789abcdef01234567"); err != nil {
		t.Fatalf("valid workspace ID: %v", err)
	}
	for _, id := range []string{"", "ws_", "ws_0123", "WS_0123456789abcdef01234567", "ws_0123456789abcdef0123456g"} {
		if err := ValidateWorkspaceID(id); err == nil {
			t.Fatalf("invalid workspace ID %q was accepted", id)
		}
	}
}

func TestNewWorkspaceIDIsPortableAndRandom(t *testing.T) {
	ids := map[string]bool{}
	for i := 0; i < 32; i++ {
		id, err := NewWorkspaceID()
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateWorkspaceID(id); err != nil {
			t.Fatalf("generated ID %q: %v", id, err)
		}
		if ids[id] {
			t.Fatalf("duplicate generated ID %q", id)
		}
		ids[id] = true
	}
}

func TestWorkspaceMigrationIsForwardOnlyAndDoesNotSeedAnIdentity(t *testing.T) {
	if !strings.Contains(strings.ToLower(migration028), "create table eregion_workspace") {
		t.Fatal("migration 028 does not create the workspace table")
	}
	if !strings.Contains(strings.ToLower(migration028), "immutable") {
		t.Fatal("migration 028 does not protect the identity from updates")
	}
	var statements []string
	for _, line := range strings.Split(migration028, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		statements = append(statements, line)
	}
	if regexp.MustCompile(`(?im)^\s*(insert|update|delete)\b`).MatchString(strings.Join(statements, "\n")) {
		t.Fatal("migration 028 seeds, changes or deletes identity rows; an alpha.6 database must stay unconfigured")
	}
	if m, ok := migrationByVersion(28); !ok || m.name != "028_workspace_identity.sql" {
		t.Fatal("migration 028 is not registered")
	}
}
