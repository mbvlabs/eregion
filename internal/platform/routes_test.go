package platform

import (
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
