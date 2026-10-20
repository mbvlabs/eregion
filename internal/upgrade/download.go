package upgrade

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Downloader fetches signed manifests and artifacts. It refuses redirects,
// proxies, plaintext HTTP, credentials in URLs, and non-public destinations.
// The unexported loopback switch exists only for tests.
type Downloader struct {
	client        *http.Client
	allowedHosts  []string
	allowLoopback bool
	maxArtifact   int64
}

// NewDownloader builds a strict downloader. allowedHosts, when nonempty,
// restricts every request to those hostnames (case-insensitive, exact). It
// reads no proxy environment variables.
func NewDownloader(allowedHosts []string) *Downloader {
	hosts := make([]string, 0, len(allowedHosts))
	for _, host := range allowedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			hosts = append(hosts, host)
		}
	}
	d := &Downloader{allowedHosts: hosts, maxArtifact: MaxArtifactBytes}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.dialContext,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	d.client = &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return d
}

// ValidateArtifactURL enforces the public HTTPS artifact URL policy. It is
// applied when a manifest is validated, before any request is considered.
func ValidateArtifactURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "release artifact URL is malformed", "Publish an absolute HTTPS URL without credentials or a fragment.")
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return contract.NewFault(contract.ValidationFailed, "release artifact URL is not a safe HTTPS URL", "Publish an absolute HTTPS URL without credentials, query credentials, fragments, or redirects.")
	}
	host := parsed.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return contract.NewFault(contract.ValidationFailed, "release artifact URL points at loopback", "Publish a public HTTPS object URL; loopback and private destinations are refused.")
	}
	if ip := net.ParseIP(host); ip != nil && !publicIP(ip) {
		return contract.NewFault(contract.ValidationFailed, "release artifact URL points at a private or special address", "Publish a public HTTPS object URL; loopback and private destinations are refused.")
	}
	return nil
}

func (d *Downloader) validateURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "download URL is malformed", "Use an absolute URL published in the signed manifest.")
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || parsed.Host == "" {
		return nil, contract.NewFault(contract.ValidationFailed, "download URL is unsafe", "Refuse URLs with credentials, fragments, or no host.")
	}
	loopback := isLoopbackHost(parsed.Hostname())
	if parsed.Scheme != "https" && !(d.allowLoopback && parsed.Scheme == "http" && loopback) {
		return nil, contract.NewFault(contract.ValidationFailed, "download URL must use HTTPS", "Plaintext downloads are refused; use a public HTTPS object URL.")
	}
	if loopback && !d.allowLoopback {
		return nil, contract.NewFault(contract.ValidationFailed, "download URL points at loopback", "Loopback and private destinations are refused.")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && !d.allowedIP(ip) {
		return nil, contract.NewFault(contract.ValidationFailed, "download URL points at a private or special address", "Loopback and private destinations are refused.")
	}
	if len(d.allowedHosts) > 0 {
		host := strings.ToLower(parsed.Hostname())
		allowed := false
		for _, candidate := range d.allowedHosts {
			if host == candidate {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, contract.NewFault(contract.ValidationFailed, "download host is not in the configured allowlist", "Add the release host to EREGION_UPGRADE_ALLOWED_HOSTS or publish artifacts there.")
		}
	}
	return parsed, nil
}

// Fetch reads at most limit bytes. It returns an error for a redirect rather
// than following it, so an attacker cannot move a signed URL to another host.
func (d *Downloader) Fetch(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	if limit <= 0 || limit > MaxManifestBytes {
		limit = MaxManifestBytes
	}
	body, err := d.open(ctx, rawURL, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "download could not be read", "Retry the request; no partial manifest is used.")
	}
	if int64(len(raw)) > limit {
		return nil, contract.NewFault(contract.PreconditionFailed, "download exceeds the size bound", "Refuse the object; a manifest or artifact is larger than this release allows.")
	}
	return raw, nil
}

// FetchManifest fetches a signed manifest with the manifest size bound.
func (d *Downloader) FetchManifest(ctx context.Context, rawURL string) ([]byte, error) {
	return d.Fetch(ctx, rawURL, MaxManifestBytes)
}

func (d *Downloader) open(ctx context.Context, rawURL string, limit int64) (io.ReadCloser, error) {
	if _, err := d.validateURL(rawURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "download request could not be built", "Use the signed manifest URL unchanged.")
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "download failed", "Check network access to the release host and retry; the installation was not changed.")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		_ = response.Body.Close()
		return nil, contract.NewFault(contract.PreconditionFailed, "download redirect was refused", "Publish a direct object URL; redirects are never followed.")
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, contract.NewFault(contract.Unavailable, fmt.Sprintf("download returned HTTP %d", response.StatusCode), "Check that the signed artifact still exists at its published URL.")
	}
	if response.ContentLength > limit {
		_ = response.Body.Close()
		return nil, contract.NewFault(contract.PreconditionFailed, "download exceeds the size bound", "Refuse the object; it is larger than its signed size.")
	}
	return response.Body, nil
}

// DownloadVerified downloads url, enforces the exact signed size and SHA256,
// and stores it under dir with a content-addressed name. Existing files are
// re-verified before reuse.
func (d *Downloader) DownloadVerified(ctx context.Context, rawURL, wantSHA string, wantSize int64, dir string) (string, error) {
	if err := validateDigest(wantSHA, wantSize, d.maxArtifact); err != nil {
		return "", err
	}
	if err := ensureStateDir(dir); err != nil {
		return "", contract.NewFault(contract.Unavailable, "download directory is not usable", "Check the upgrade state directory permissions.")
	}
	destination := filepath.Join(dir, wantSHA+".artifact")
	if existing, err := verifyExisting(destination, wantSHA, wantSize); err == nil && existing {
		return destination, nil
	}
	body, err := d.open(ctx, rawURL, wantSize)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	if err := writeVerified(destination, body, wantSHA, wantSize); err != nil {
		return "", err
	}
	return destination, nil
}

// CopyVerifiedFile imports an artifact from an operator-supplied offline
// mirror. Size, digest, regular-file type, and destination bounds still apply.
func CopyVerifiedFile(source, wantSHA string, wantSize int64, dir string) (string, error) {
	if err := validateDigest(wantSHA, wantSize, MaxArtifactBytes); err != nil {
		return "", err
	}
	if err := ensureStateDir(dir); err != nil {
		return "", contract.NewFault(contract.Unavailable, "download directory is not usable", "Check the upgrade state directory permissions.")
	}
	destination := filepath.Join(dir, wantSHA+".artifact")
	if existing, err := verifyExisting(destination, wantSHA, wantSize); err == nil && existing {
		return destination, nil
	}
	sha, size, err := copyFileAtomic(destination, source, 0o600, wantSize)
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "offline artifact could not be read", "Check the artifact path and permissions; no partial artifact is used.")
	}
	if size != wantSize || sha != wantSHA {
		_ = os.Remove(destination)
		return "", contract.NewFault(contract.PreconditionFailed, "offline artifact does not match the signed size or SHA256", "Refuse the artifact; re-copy it from the release source.")
	}
	return destination, nil
}

func validateDigest(wantSHA string, wantSize, maximum int64) error {
	if wantSize <= 0 || wantSize > maximum {
		return contract.NewFault(contract.ValidationFailed, "artifact size is outside the download bound", "Use the size from the signed manifest.")
	}
	if len(wantSHA) != sha256.Size*2 {
		return contract.NewFault(contract.ValidationFailed, "artifact digest is malformed", "Use the SHA256 from the signed manifest.")
	}
	if _, err := hex.DecodeString(wantSHA); err != nil || strings.ToLower(wantSHA) != wantSHA {
		return contract.NewFault(contract.ValidationFailed, "artifact digest is malformed", "Use a lowercase hexadecimal SHA256.")
	}
	return nil
}

func verifyExisting(path, wantSHA string, wantSize int64) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || (wantSize > 0 && info.Size() != wantSize) {
		return false, fmt.Errorf("existing artifact does not match")
	}
	sha, _, _, err := fileSHA256(path, wantSize)
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare([]byte(sha), []byte(wantSHA)) != 1 {
		return false, fmt.Errorf("existing artifact does not match")
	}
	return true, nil
}

func writeVerified(destination string, body io.Reader, wantSHA string, wantSize int64) error {
	dir := filepath.Dir(destination)
	temp, err := os.CreateTemp(dir, ".artifact-*")
	if err != nil {
		return contract.NewFault(contract.Unavailable, "download could not be staged", "Check free space and permissions in the upgrade state directory.")
	}
	tempName := temp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tempName)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(body, wantSize+1))
	if err != nil {
		_ = temp.Close()
		return contract.NewFault(contract.Unavailable, "download was interrupted", "Retry; no partial artifact is installed.")
	}
	if written != wantSize {
		_ = temp.Close()
		return contract.NewFault(contract.PreconditionFailed, "download size does not match the signed manifest", "Refuse the artifact; the server returned different bytes.")
	}
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(hash.Sum(nil))), []byte(wantSHA)) != 1 {
		_ = temp.Close()
		return contract.NewFault(contract.PreconditionFailed, "download SHA256 does not match the signed manifest", "Refuse the artifact; it was tampered with or corrupted.")
	}
	if err := temp.Chmod(0o600); err != nil {
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
	if err := os.Rename(tempName, destination); err != nil {
		return contract.NewFault(contract.Unavailable, "verified artifact could not be stored", "Check permissions in the upgrade state directory.")
	}
	remove = false
	return syncDir(dir)
}

func (d *Downloader) allowedIP(ip net.IP) bool {
	if d.allowLoopback && ip.IsLoopback() {
		return true
	}
	return publicIP(ip)
}

func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsValid() {
		return false
	}
	for _, prefix := range unsafePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var unsafePrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (TEST-NET-1)
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (TEST-NET-2)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (TEST-NET-3)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (d *Downloader) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if ip := net.ParseIP(host); ip != nil {
		if !d.allowedIP(ip) {
			return nil, errors.New("refusing to connect to a non-public address")
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, candidate := range addresses {
		if !d.allowedIP(candidate.IP) {
			continue
		}
		connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if err == nil {
			return connection, nil
		}
		last = err
	}
	if last != nil {
		return nil, last
	}
	return nil, errors.New("refusing to connect: host resolves only to non-public addresses")
}
