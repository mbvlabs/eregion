package platform

import (
	"fmt"
	"os"
	"path/filepath"
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
	// CaddyHTTPS is where public-route verification connects locally. The Host
	// header and TLS SNI still name the real domain, so Caddy selects the site.
	CaddyHTTPS = "127.0.0.1:443"
)

// Route is one public hostname and the loopback upstreams it proxies to. Caddy
// loads balances a web process across every replica upstream.
type Route struct {
	Domain string
	// Primary marks the environment's primary hostname. Every binding,
	// including aliases, gets its own Caddy site.
	Primary   bool
	Upstreams []string
}

// WebUpstreams returns the loopback upstreams recorded for serving web
// instances, deduplicated and sorted for a stable routes file.
func WebUpstreams(instances []DeploymentInstance) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(instances))
	for _, instance := range instances {
		if instance.Kind != "web" || instance.HostPort < 1 || instance.HostPort > 65535 {
			continue
		}
		upstream := fmt.Sprintf("127.0.0.1:%d", instance.HostPort)
		if seen[upstream] {
			continue
		}
		seen[upstream] = true
		out = append(out, upstream)
	}
	sort.Strings(out)
	return out
}

// RenderRoutes is the routes file: one site block per domain, sorted. A domain
// with no serving upstream is omitted rather than pointed at a stale port.
func RenderRoutes(routes []Route) []byte {
	sorted := append([]Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })
	var b strings.Builder
	b.WriteString("# managed by eregion: rewritten on every deployment or domain change\n")
	for _, r := range sorted {
		upstreams := validUpstreams(r.Upstreams)
		if len(upstreams) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s {\n\treverse_proxy %s\n}\n", r.Domain, strings.Join(upstreams, " "))
	}
	return []byte(b.String())
}

// WriteFileAtomic replaces path with data without exposing a partially written
// file: write a sibling temp file, fsync it, rename it over path, then fsync the
// directory. Caddy only ever sees the previous or the complete new routes file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func validUpstreams(upstreams []string) []string {
	out := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		upstream = strings.TrimSpace(upstream)
		if upstream == "" || strings.ContainsAny(upstream, " \t\r\n\x00{}") {
			continue
		}
		out = append(out, upstream)
	}
	sort.Strings(out)
	return out
}
