package platform

import (
	"fmt"
	"sort"
	"strings"
)

// Caddy on the main node: the Caddyfile is owned by bootstrap and imports the
// routes file, which the worker owns and rewrites from the domains table.
const (
	CaddyfilePath   = "/etc/caddy/Caddyfile"
	CaddyRoutesDir  = "/etc/caddy/eregion.d"
	CaddyRoutesPath = CaddyRoutesDir + "/routes.caddy"
	// CaddyAdmin is Caddy's loopback admin endpoint; the worker loads the
	// Caddyfile through it, so it needs no root.
	CaddyAdmin = "http://127.0.0.1:2019"
)

// Route is one public hostname and the loopback port it proxies to.
type Route struct {
	Domain   string
	HostPort int
}

// RenderRoutes is the routes file: one site block per domain, sorted.
func RenderRoutes(routes []Route) []byte {
	sort.Slice(routes, func(i, j int) bool { return routes[i].Domain < routes[j].Domain })
	var b strings.Builder
	b.WriteString("# managed by eregion: rewritten on every domain change\n")
	for _, r := range routes {
		fmt.Fprintf(&b, "\n%s {\n\treverse_proxy 127.0.0.1:%d\n}\n", r.Domain, r.HostPort)
	}
	return []byte(b.String())
}
