package platform

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// DefaultGitAllowedHosts is the fail-closed allowlist for HTTPS git build
// sources. Deployments with a self-hosted forge set EREGION_GIT_ALLOWED_HOSTS
// explicitly; an environment value replaces (never widens) this list.
var DefaultGitAllowedHosts = []string{
	"github.com", "gitlab.com", "codeberg.org", "bitbucket.org", "git.sr.ht",
}

// GitAllowedHosts returns the configured hostname allowlist.
func GitAllowedHosts() []string {
	raw := strings.TrimSpace(os.Getenv("EREGION_GIT_ALLOWED_HOSTS"))
	if raw == "" {
		return append([]string(nil), DefaultGitAllowedHosts...)
	}
	var hosts []string
	for _, value := range strings.Split(raw, ",") {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimSuffix(value, ".")
		if value == "" || strings.ContainsAny(value, "/:@ \t\r\n") {
			continue
		}
		hosts = append(hosts, value)
	}
	if len(hosts) == 0 {
		return append([]string(nil), DefaultGitAllowedHosts...)
	}
	return hosts
}

// GitHubRepository returns owner/repository for an https://github.com URL.
// The webhook is GitHub-specific; another forge has no signed push delivery.
func GitHubRepository(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || strings.ToLower(strings.TrimSuffix(parsed.Hostname(), ".")) != "github.com" {
		return "", false
	}
	path := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return strings.ToLower(parts[0] + "/" + parts[1]), true
}

// ValidateGitSourceURL accepts only credential-free HTTPS repository URLs on an
// explicitly allowed hostname. IP literals, loopback/local names, non-443 ports,
// credentials, queries, and fragments are refused before git runs. Git is also
// invoked with redirects disabled, so an allowed host cannot bounce the clone to
// an internal service.
func ValidateGitSourceURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("use an https:// repository URL without credentials, query, or fragment")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return fmt.Errorf("git build sources must use the default HTTPS port")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || net.ParseIP(host) != nil {
		return fmt.Errorf("git build sources require a hostname, not an IP address")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("git build sources cannot target a loopback or internal name")
	}
	for _, allowed := range GitAllowedHosts() {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("git host %q is not in EREGION_GIT_ALLOWED_HOSTS", host)
}

// CheckGitSourceResolution re-checks the host at build time. A default public
// forge must resolve only to public addresses, which blocks DNS rebinding to an
// internal service. An operator-added host may resolve to a private address
// (self-hosted forges), but loopback, link-local, unspecified, and multicast
// answers are always refused.
func CheckGitSourceResolution(ctx context.Context, raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid git source URL")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil || len(addresses) == 0 {
		return fmt.Errorf("could not resolve the git build host")
	}
	explicit := true
	for _, known := range DefaultGitAllowedHosts {
		if host == known || strings.HasSuffix(host, "."+known) {
			explicit = false
			break
		}
	}
	for _, address := range addresses {
		ip := address.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("git build host resolves to a non-routable internal address")
		}
		if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return fmt.Errorf("git build host resolves to a carrier-grade NAT address")
		}
		if !explicit && ip.IsPrivate() {
			return fmt.Errorf("git build host resolves to a private address")
		}
	}
	return nil
}

// DefaultBuildSourceRoots is the only default directory from which a directory
// source build may read. A deployment may configure additional roots explicitly;
// there is no fallback to the server's whole filesystem.
const DefaultBuildSourceRoot = "/srv/eregion/sources"

// BuildSourceRoots returns the configured directory-build allowlist. The value
// is a path list, so a launcher can add a repository example directory without
// widening the default to /home or /etc.
func BuildSourceRoots() []string {
	raw := os.Getenv("EREGION_BUILD_SOURCE_ROOTS")
	if strings.TrimSpace(raw) == "" {
		return []string{DefaultBuildSourceRoot}
	}
	var roots []string
	for _, value := range filepath.SplitList(raw) {
		value = strings.TrimSpace(value)
		if value != "" && filepath.IsAbs(value) {
			roots = append(roots, value)
		}
	}
	if len(roots) == 0 {
		return []string{DefaultBuildSourceRoot}
	}
	return roots
}

// ResolveBuildSource resolves symlinks and requires the real path to be at or
// below one of the real configured roots. It rejects path traversal, symlink
// escapes, relative paths, and non-directories. Call it at submission and again
// immediately before the node step, because the filesystem can change.
func ResolveBuildSource(path string, roots []string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
		return "", contract.NewFault(contract.ValidationFailed, "directory source path must be absolute", "Use an absolute directory beneath a configured build source root.")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", contract.NewFault(contract.ValidationFailed, "directory source path is not readable", "Use an existing directory beneath a configured build source root.")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", contract.NewFault(contract.ValidationFailed, "directory source path is not a directory", "Point source.path at a readable directory.")
	}
	if len(roots) == 0 {
		roots = BuildSourceRoots()
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			continue
		}
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		realRoot = filepath.Clean(realRoot)
		if real == realRoot || strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
			return real, nil
		}
	}
	return "", contract.NewFault(contract.Forbidden, "directory source is outside the configured build roots", "Set EREGION_BUILD_SOURCE_ROOTS to a deliberate, narrow source directory; arbitrary server paths are not buildable.")
}
