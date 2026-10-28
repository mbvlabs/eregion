package platform

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
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

// gitHostLookup resolves a git source hostname. Tests replace it to exercise
// the address-refusal rules without network access.
var gitHostLookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
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
	addresses, err := gitHostLookup(resolveCtx, host)
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

// ResolveGitRef resolves a branch, tag, or full commit SHA to the exact commit
// a build must use. It runs before the submission transaction opens, so a
// branch that moves afterwards cannot change what is built. The read credential
// reaches git only through the askpass child environment, never argv.
func ResolveGitRef(ctx context.Context, rawURL, ref string, credential []byte) (string, error) {
	if err := ValidateGitSourceURL(rawURL); err != nil {
		return "", contract.NewFault(contract.ValidationFailed, "the git source URL is not allowed", err.Error())
	}
	ref = strings.TrimSpace(ref)
	if IsGitCommit(ref) {
		return ref, nil
	}
	if abbreviatedGitSHA(ref) {
		return "", contract.NewFault(contract.ValidationFailed, "the git ref is an abbreviated commit", "Use the full 40-character commit SHA, or a branch or tag name.")
	}
	if ref != "" && !validGitRef(ref) {
		return "", contract.NewFault(contract.ValidationFailed, "invalid git ref", "Use a branch or tag of 1–128 letters, digits, dot, underscore, slash, or hyphen.")
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// The submission-time lookup must refuse the same internal addresses as the
	// build. A hostname that resolves to loopback, link-local, a carrier-grade
	// NAT address or (for a public forge) a private address never reaches git.
	if err := CheckGitSourceResolution(resolveCtx, rawURL); err != nil {
		return "", contract.NewFault(contract.PreconditionFailed, "the git build host is refused", err.Error()+"; use a public HTTPS host allowed by EREGION_GIT_ALLOWED_HOSTS, or fix its DNS answers.")
	}

	work, err := os.MkdirTemp("", "eregion-git-ref-*")
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "could not prepare the git ref check", "Check the node's temporary directory and retry.")
	}
	defer os.RemoveAll(work) //nolint:errcheck // best-effort cleanup of a private temp tree.
	isolation, err := buildIsolation(work)
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "could not prepare the git ref check", "Check the node's temporary directory and retry.")
	}
	env, err := gitCredentialEnvironment(isolation, work, string(credential))
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "could not prepare the git askpass helper", "Check the node's temporary directory and retry.")
	}
	args := []string{"ls-remote", rawURL}
	if ref == "" {
		args = append(args, "HEAD")
	} else {
		args = append(args, "refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
	}
	command := exec.CommandContext(resolveCtx, "git", args...)
	command.Env = env
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err != nil {
		if resolveCtx.Err() != nil {
			return "", contract.NewFault(contract.Unavailable, "the git repository did not answer within 30 seconds", "Check the repository URL and network, then retry.")
		}
		hint := "Check the repository URL and the read credential, then retry."
		if detail := redactGitStderr(stderr.String(), credential); detail != "" {
			hint = detail
		}
		return "", contract.NewFault(contract.Unavailable, "the git repository could not be reached", hint)
	}
	return matchGitRef(ref, string(stdout))
}

// redactGitStderr keeps the first stderr line as a hint while guaranteeing
// that a credential echoed by a remote never reaches an error or an event.
func redactGitStderr(stderr string, credential []byte) string {
	detail := firstLine(stderr)
	if detail == "" || len(credential) == 0 {
		return detail
	}
	return strings.ReplaceAll(detail, string(credential), "***")
}

// IsGitCommit reports a full lowercase git commit SHA (SHA-1 or SHA-256).
func IsGitCommit(value string) bool { return shaPattern.MatchString(value) }

// matchGitRef picks the one commit a branch or tag names in git ls-remote
// output. An annotated tag uses its peeled commit. A name that is both a branch
// and a tag is ambiguous and refused rather than guessed.
func matchGitRef(ref, output string) (string, error) {
	var head, branch, tag, peeled string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !shaPattern.MatchString(fields[0]) {
			continue
		}
		switch fields[1] {
		case "HEAD":
			head = fields[0]
		case "refs/heads/" + ref:
			branch = fields[0]
		case "refs/tags/" + ref:
			tag = fields[0]
		case "refs/tags/" + ref + "^{}":
			peeled = fields[0]
		}
	}
	if ref == "" {
		if head == "" {
			return "", contract.NewFault(contract.PreconditionFailed, "the repository's default branch did not resolve to a commit", "Set an explicit ref on the environment source, then retry.")
		}
		return head, nil
	}
	if branch != "" && (tag != "" || peeled != "") {
		return "", contract.NewFault(contract.ValidationFailed, "the git ref "+ref+" is both a branch and a tag", "Use the full commit SHA, or rename one of them so the ref is unambiguous.")
	}
	switch {
	case branch != "":
		return branch, nil
	case peeled != "":
		return peeled, nil
	case tag != "":
		return tag, nil
	default:
		return "", contract.NewFault(contract.PreconditionFailed, "the git ref "+ref+" was not found", "Use a branch or tag that exists in the repository, or the full commit SHA.")
	}
}

// abbreviatedGitSHA reports a value that can only be meant as a shortened
// commit. A full 40- or 64-character SHA is handled by ResolveGitRef itself.
func abbreviatedGitSHA(ref string) bool {
	if len(ref) < 4 || len(ref) == 40 || len(ref) == 64 {
		return false
	}
	for _, r := range ref {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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
