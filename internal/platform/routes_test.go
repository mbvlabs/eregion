package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderRoutesBalancesWebReplicas(t *testing.T) {
	routes := []Route{
		{Domain: "b.example.com", Upstreams: []string{"127.0.0.1:40002"}},
		{Domain: "a.example.com", Upstreams: []string{"127.0.0.1:40003", "127.0.0.1:40001"}},
		{Domain: "empty.example.com"},
	}
	rendered := string(RenderRoutes(routes))
	if strings.Contains(rendered, "empty.example.com") {
		t.Fatalf("route without an upstream was rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "reverse_proxy 127.0.0.1:40001 127.0.0.1:40003") {
		t.Fatalf("web replicas are not load balanced:\n%s", rendered)
	}
	if strings.Index(rendered, "a.example.com") > strings.Index(rendered, "b.example.com") {
		t.Fatalf("routes are not sorted:\n%s", rendered)
	}
}

func TestRenderRoutesIncludesPrimaryAndAliases(t *testing.T) {
	routes := []Route{
		{Domain: "alias.example.com", Upstreams: []string{"127.0.0.1:40001"}},
		{Domain: "primary.example.com", Primary: true, Upstreams: []string{"127.0.0.1:40001"}},
	}
	rendered := string(RenderRoutes(routes))
	for _, domain := range []string{"primary.example.com", "alias.example.com"} {
		if !strings.Contains(rendered, domain+" {") {
			t.Fatalf("route %s is missing:\n%s", domain, rendered)
		}
	}
}

func TestWriteFileAtomicReplacesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.caddy")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" {
		t.Fatalf("content=%q", content)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "routes.caddy" {
		t.Fatalf("temporary file was not cleaned up: %v", entries)
	}
}

func TestWebUpstreamsUsesServingWebPorts(t *testing.T) {
	upstreams := WebUpstreams([]DeploymentInstance{
		{Kind: "web", HostPort: 40002},
		{Kind: "worker"},
		{Kind: "service", HostPort: 40003},
		{Kind: "web", HostPort: 40002},
		{Kind: "web", HostPort: 0},
	})
	if len(upstreams) != 1 || upstreams[0] != "127.0.0.1:40002" {
		t.Fatalf("upstreams=%v", upstreams)
	}
}
