package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
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

func TestWorkspaceStoreRequiresSingleJSONDocument(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		valid        bool
	}{
		{"second object", "{}", false},
		{"garbage", "garbage", false},
		{"whitespace", " \n\t", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workspaces.json")
			raw := []byte(`{"schema":2,"current":"thelab","workspaces":[{"name":"thelab","workspace":{"id":"ws_779004d145462c0fd7f48868","name":"thelab"}}]}` + tc.suffix)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := LoadWorkspaceStore(path)
			if tc.valid {
				if err != nil || store == nil || store.Current != "thelab" {
					t.Fatalf("valid whitespace suffix refused: %v", err)
				}
			} else if store != nil || contract.Classify(err).Code != contract.PreconditionFailed {
				t.Fatalf("trailing data accepted: store=%v err=%v", store != nil, err)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, after) {
				t.Fatalf("store validation changed the file: %v", err)
			}
		})
	}
}
