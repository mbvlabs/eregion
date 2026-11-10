package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceStoreDefaultPathAndFormat(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", "")
	t.Setenv("EREGION_CONTEXT", "")
	t.Setenv("EREGION_CONTEXT_FILE", "")
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := DefaultWorkspacePath()
	if err != nil || path != filepath.Join(root, "eregion", "workspaces.json") {
		t.Fatalf("default path = %q, %v", path, err)
	}
	store, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"workspaces"`) || strings.Contains(string(raw), `"contexts"`) {
		t.Fatal("store did not use the workspace schema")
	}
}

func TestWorkspaceStoreRefusesOldContextsField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.json")
	if err := os.WriteFile(path, []byte(`{"schema":2,"contexts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspaceStore(path); err == nil {
		t.Fatal("old contexts schema accepted")
	}
}
