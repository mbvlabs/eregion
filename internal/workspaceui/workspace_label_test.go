package workspaceui

import (
	"strings"
	"testing"
)

func TestHeaderShowsActualNodeAndWorkspace(t *testing.T) {
	model := New(Options{
		Host: "admin@10.78.0.1", Node: "main", Workspace: "thelab",
		WorkspaceKnown: true, WorkspaceConfigured: true,
		Width: 100, Height: 30, NoColor: true,
	})
	rendered := model.render()
	for _, want := range []string{"workspace thelab", "node main", "Fleet"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("header lacks %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(strings.Split(rendered, "\n")[0], "admin@10.78.0.1") {
		t.Fatalf("header still labels the route instead of the node:\n%s", rendered)
	}
}

func TestHeaderDistinguishesUnconfiguredFromUnknownWorkspace(t *testing.T) {
	unconfigured := New(Options{
		Node: "main", WorkspaceKnown: true, WorkspaceConfigured: false,
		Width: 100, Height: 30, NoColor: true,
	}).render()
	if !strings.Contains(unconfigured, "workspace not configured") {
		t.Fatalf("unconfigured header:\n%s", unconfigured)
	}
	unknown := New(Options{
		Node: "main", WorkspaceKnown: false,
		Width: 100, Height: 30, NoColor: true,
	}).render()
	if !strings.Contains(unknown, "workspace unknown") || strings.Contains(unknown, "workspace not configured") {
		t.Fatalf("unknown header:\n%s", unknown)
	}
}
