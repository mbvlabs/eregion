package workspaceui

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceGoldenRenders(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
		tab    rune
		raw    string
		keys   string
		ascii  bool
	}{
		{name: "fleet-wide", width: 120, height: 24, raw: nodesJSON},
		{name: "resources-narrow", width: 44, height: 16, tab: '6', raw: resourcesJSON},
		{name: "releases-standard", width: 80, height: 24, tab: '4', raw: releasesJSON},
		{name: "palette-standard", width: 100, height: 34, raw: nodesJSON, keys: ":backup"},
		{name: "fleet-ascii", width: 90, height: 20, raw: nodesJSON, ascii: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			m, api, _ := testArgs(t)
			api.inventory = func(_ context.Context, collection string, _ url.Values) (json.RawMessage, error) {
				return json.RawMessage(test.raw), nil
			}
			if test.tab != 0 {
				next, command := stepCmd(t, m, press(test.tab))
				m = applyCommand(t, next, command)
			}
			m = load(t, m, test.raw)
			m.width, m.height = test.width, test.height
			m.opts.ASCII = test.ascii
			for _, key := range test.keys {
				m = step(t, m, press(key))
			}
			rendered := render(m)
			if strings.ContainsRune(rendered, '\x1b') {
				t.Fatalf("golden render contains ANSI:\n%q", rendered)
			}
			for _, line := range strings.Split(rendered, "\n") {
				if len([]rune(line)) > test.width {
					t.Fatalf("line exceeds width %d: %q", test.width, line)
				}
			}
			checkWorkspaceGolden(t, test.name, rendered)
		})
	}
}

func checkWorkspaceGolden(t *testing.T, name, rendered string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run UPDATE_GOLDEN=1 go test ./internal/workspaceui): %v", path, err)
	}
	if string(want) != rendered {
		t.Fatalf("golden mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s", name, want, rendered)
	}
}
