// Package registrymeta performs bounded, read-only OCI/Docker registry metadata
// resolution shared by artifact admission and node publish verification. It
// never uses ambient Docker credentials, never follows redirects, and never
// treats a manifest digest as a config digest.
package registrymeta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	ManifestBodyLimit = 4 << 20
	TokenBodyLimit    = 64 << 10
	ConfigBodyLimit   = 8 << 20
	RequestTimeout    = 20 * time.Second
)

// ErrIndexNotAllowed marks an image index on a path whose contract is exactly
// one published platform manifest.
var ErrIndexNotAllowed = errors.New("image index refused by the single-platform publish contract")

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ManifestAccept is the bounded set of manifest media types a resolver accepts.
var ManifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Client resolves one registry repository reference. Base is the registry
// origin with scheme, such as https://registry.example.com or http://127.0.0.1:5000.
type Client struct {
	HTTPClient    *http.Client
	Base          string
	AnonymousAuth bool
	// RedirectHTTPClient performs config-blob redirect GETs. Production leaves
	// it nil, which builds a public-only client (no loopback privilege and
	// normal TLS roots). Tests may supply a client with a test CA and connector
	// while redirect validation and the public destination policy still apply.
	RedirectHTTPClient *http.Client
	// LookupIP is a production-shaped DNS seam for the public redirect client.
	// Nil uses net.DefaultResolver; every returned address must still pass the
	// public destination policy.
	LookupIP func(context.Context, string) ([]net.IPAddr, error)
}

// Options bounds one resolution. Architecture selects the index child when
// AllowIndex is true. A false AllowIndex refuses any index outright, which is
// the Pack publication contract.
type Options struct {
	Architecture string
	AllowIndex   bool
}

// Resolution is the immutable identity of one schema-2 image manifest. Manifest
// and Config are distinct digests; Runtime IDs are Docker observations and are
// deliberately not part of this result.
type Resolution struct {
	ManifestDigest string
	ConfigDigest   string
	MediaType      string
}

// NewHTTPClient returns a client with no environment proxy, bounded timeouts,
// no redirects and a dial-time destination policy. External clients allow only
// public IPs; the explicitly isolated private-registry client allows only
// loopback addresses. DNS results are pinned for the dial.
func NewHTTPClient(allowLoopback bool) *http.Client {
	return NewHTTPClientWithLookup(allowLoopback, nil)
}

// NewHTTPClientWithLookup builds the same bounded client with an explicit DNS
// lookup seam. Production passes nil; every resolved address still passes the
// loopback/public policy and the dial is pinned to a validated address.
func NewHTTPClientWithLookup(
	allowLoopback bool,
	lookup func(context.Context, string) ([]net.IPAddr, error),
) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialContext(allowLoopback, lookup),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
	}
	return &http.Client{
		Timeout:   RequestTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectRefused
		},
	}
}

var errRedirectRefused = errors.New("registry redirect refused")

var blockedDestinations = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "fc00::/7",
	"fe80::/10", "ff00::/8", "2001:db8::/32", "2002::/16", "64:ff9b::/96",
)

func mustPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}

func dialContext(
	allowLoopback bool,
	lookup func(context.Context, string) ([]net.IPAddr, error),
) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("registry destination is invalid: %w", err)
		}
		if host == "" || port == "" {
			return nil, errors.New("registry destination is incomplete")
		}
		addresses, err := resolveDestination(ctx, lookup, host, allowLoopback)
		if err != nil {
			return nil, err
		}
		return dialDestination(ctx, dialer, network, port, addresses)
	}
}

func resolveDestination(
	ctx context.Context,
	lookup func(context.Context, string) ([]net.IPAddr, error),
	host string,
	allowLoopback bool,
) ([]net.IPAddr, error) {
	addresses, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("registry destination could not be resolved: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("registry destination resolved to no address")
	}
	for _, candidate := range addresses {
		if !destinationAllowed(candidate.IP, allowLoopback) {
			return nil, fmt.Errorf(
				"registry destination %s is not an allowed public address",
				candidate.IP,
			)
		}
	}
	return addresses, nil
}

func dialDestination(
	ctx context.Context,
	dialer *net.Dialer,
	network, port string,
	addresses []net.IPAddr,
) (net.Conn, error) {
	var lastErr error
	for _, candidate := range addresses {
		conn, dialErr := dialer.DialContext(
			ctx,
			network,
			net.JoinHostPort(candidate.IP.String(), port),
		)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("registry destination is unreachable")
	}
	return nil, lastErr
}

func destinationAllowed(ip net.IP, allowLoopback bool) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if allowLoopback {
		return addr.IsLoopback()
	}
	if addr.IsLoopback() {
		return false
	}
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedDestinations {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// Resolve reads the manifest (and its selected child), verifies every byte
// bound against its digest, then fetches and hashes the config blob before
// accepting the platform metadata. The returned ManifestDigest and
// ConfigDigest are always distinct identities.
func (c *Client) Resolve(
	ctx context.Context,
	repository, reference string,
	opts Options,
) (Resolution, error) {
	if c == nil || c.HTTPClient == nil || strings.TrimSpace(c.Base) == "" {
		return Resolution{}, contract.NewFault(
			contract.Internal,
			"the registry resolver is not configured",
			"Report the command and binary version.",
		)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	manifest, mediaType, manifestDigest, err := c.resolveManifest(
		resolveCtx,
		repository,
		reference,
		opts,
	)
	if err != nil {
		return Resolution{}, err
	}
	config, err := manifestConfigDigest(manifest, mediaType)
	if err != nil {
		return Resolution{}, err
	}
	if err := c.verifyConfig(resolveCtx, repository, config, opts.Architecture); err != nil {
		return Resolution{}, err
	}
	if manifestDigest == config {
		return Resolution{}, contract.NewFault(
			contract.PreconditionFailed,
			"the registry returned the manifest digest as the image config digest",
			"Do not accept an image whose config identity is not distinct.",
		)
	}
	return Resolution{
		ManifestDigest: manifestDigest,
		ConfigDigest:   config,
		MediaType:      mediaType,
	}, nil
}

func (c *Client) resolveManifest(
	ctx context.Context,
	repository, reference string,
	opts Options,
) (distributionManifest, string, string, error) {
	body, mediaType, err := c.getManifest(ctx, repository, reference)
	if err != nil {
		return distributionManifest{}, "", "", err
	}
	manifestDigest := "sha256:" + sha256Hex(body)
	if IsDigest(reference) && manifestDigest != strings.ToLower(strings.TrimSpace(reference)) {
		return distributionManifest{}, "", "", contract.NewFault(
			contract.PreconditionFailed,
			"the registry returned different manifest bytes than the requested digest",
			"Do not admit an artifact that does not match its requested digest; retry against the registry.",
		)
	}
	var manifest distributionManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return distributionManifest{}, "", "", contract.NewFault(
			contract.PreconditionFailed,
			"the registry returned an unreadable manifest",
			"Use a schema-2 image manifest.",
		)
	}
	if !isManifestList(mediaType, manifest) {
		return manifest, mediaType, manifestDigest, nil
	}
	return c.resolveIndexChild(ctx, repository, manifest, opts)
}

func (c *Client) resolveIndexChild(
	ctx context.Context,
	repository string,
	manifest distributionManifest,
	opts Options,
) (distributionManifest, string, string, error) {
	if !opts.AllowIndex {
		fault := contract.NewFault(
			contract.PreconditionFailed,
			"the registry returned an image index",
			"The bounded single-platform publish contract requires one platform manifest; publish a single-platform image.",
		)
		fault.Cause = ErrIndexNotAllowed
		return distributionManifest{}, "", "", fault
	}
	digest := selectManifestForArch(manifest, opts.Architecture)
	if digest == "" {
		return distributionManifest{}, "", "", contract.NewFault(
			contract.PreconditionFailed,
			"the image index has no manifest for the target node architecture",
			"Use a single-platform image or an index that contains the node's architecture.",
		)
	}
	body, mediaType, err := c.getManifest(ctx, repository, digest)
	if err != nil {
		return distributionManifest{}, "", "", err
	}
	childDigest := "sha256:" + sha256Hex(body)
	if childDigest != digest {
		return distributionManifest{}, "", "", contract.NewFault(
			contract.PreconditionFailed,
			"the image index child manifest does not match its descriptor digest",
			"Do not admit a child manifest that differs from the index descriptor; retry against the registry.",
		)
	}
	var child distributionManifest
	if err := json.Unmarshal(body, &child); err != nil {
		return distributionManifest{}, "", "", contract.NewFault(
			contract.PreconditionFailed,
			"the registry returned an unreadable child manifest",
			"Use a schema-2 image manifest.",
		)
	}
	return child, mediaType, childDigest, nil
}

func manifestConfigDigest(manifest distributionManifest, mediaType string) (string, error) {
	if manifest.SchemaVersion != 2 || isManifestList(mediaType, manifest) {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the registry did not return a schema-2 image manifest",
			"Use a single-platform schema-2 image.",
		)
	}
	config := strings.ToLower(strings.TrimSpace(manifest.Config.Digest))
	if !IsDigest(config) {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the image manifest has no sha256 config digest",
			"Use a schema-2 image manifest.",
		)
	}
	return config, nil
}

func (c *Client) verifyConfig(ctx context.Context, repository, digest, architecture string) error {
	body, err := c.getBlob(ctx, repository, digest)
	if err != nil {
		return err
	}
	if got := "sha256:" + sha256Hex(body); got != digest {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the image config blob does not match its manifest digest",
			"Do not admit an image whose config bytes differ from the authenticated digest.",
		)
	}
	if strings.TrimSpace(architecture) == "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the target image architecture is unavailable",
			"Resolve an artifact for an explicit Linux node architecture.",
		)
	}
	var config struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the image config blob is unreadable",
			"Use a standard schema-2 image config.",
		)
	}
	if config.OS != "linux" || config.Architecture != architecture {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the image config platform does not match the target node",
			"Require an explicit linux/<node architecture> image config; an absent or different platform is refused.",
		)
	}
	return nil
}

type distributionManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

func isManifestList(mediaType string, manifest distributionManifest) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if strings.HasPrefix(mediaType, "application/vnd.oci.image.index.v1+json") ||
		strings.HasPrefix(mediaType, "application/vnd.docker.distribution.manifest.list.v2+json") {
		return true
	}
	return manifest.SchemaVersion == 2 && len(manifest.Manifests) > 0 &&
		manifest.Config.Digest == ""
}

func selectManifestForArch(manifest distributionManifest, arch string) string {
	for _, candidate := range manifest.Manifests {
		if candidate.Platform.OS == "linux" && candidate.Platform.Architecture == arch {
			digest := strings.ToLower(strings.TrimSpace(candidate.Digest))
			if IsDigest(digest) {
				return digest
			}
		}
	}
	return ""
}

func (c *Client) getManifest(
	ctx context.Context,
	repository, reference string,
) ([]byte, string, error) {
	target := c.Base + "/v2/" + escapeRepository(
		repository,
	) + "/manifests/" + url.PathEscape(
		reference,
	)
	response, err := c.get(ctx, target)
	if err != nil {
		return nil, "", err
	}
	defer closeResponse(response)
	if response.StatusCode == http.StatusUnauthorized && c.AnonymousAuth {
		token, tokenErr := c.requestAnonymousToken(ctx, response.Header.Get("WWW-Authenticate"))
		if tokenErr != nil {
			return nil, "", tokenErr
		}
		response, err = c.getWithToken(ctx, target, token)
		if err != nil {
			return nil, "", err
		}
		defer closeResponse(response)
	}
	switch response.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(response.Body, ManifestBodyLimit+1))
		if err != nil {
			return nil, "", contract.NewFault(
				contract.Unavailable,
				"the registry manifest read failed",
				"Retry after the registry is reachable.",
			)
		}
		if len(body) > ManifestBodyLimit {
			return nil, "", contract.NewFault(
				contract.PreconditionFailed,
				"the registry manifest is larger than the bounded read",
				"Use a normal image manifest under 4 MiB.",
			)
		}
		return body, response.Header.Get("Content-Type"), nil
	case http.StatusNotFound:
		return nil, "", contract.NewFault(
			contract.NotFound,
			"the registry has no manifest for the selected artifact",
			"Push the image, or choose a tag or digest that exists.",
		)
	default:
		return nil, "", contract.NewFault(
			contract.Unavailable,
			"the registry answered the manifest lookup with "+response.Status,
			"Retry after the registry is reachable.",
		)
	}
}

func (c *Client) getBlob(ctx context.Context, repository, digest string) ([]byte, error) {
	target := c.Base + "/v2/" + escapeRepository(repository) + "/blobs/" + url.PathEscape(digest)
	response, err := c.requestBlob(ctx, noFollowClient(c.HTTPClient), target) //nolint:bodyclose // readBlob closes it on every return path; bodyclose cannot follow the hand-off
	if err != nil {
		return nil, err
	}
	return readBlob(ctx, noFollowClient(c.redirectClient()), target, response)
}

func (c *Client) requestBlob(
	ctx context.Context,
	client *http.Client,
	target string,
) (*http.Response, error) {
	initial, err := blobRequest(ctx, target, "")
	if err != nil {
		return nil, err
	}
	response, err := client.Do(initial)
	if err != nil {
		return nil, registryRequestFault(err)
	}
	if response.StatusCode != http.StatusUnauthorized || !c.AnonymousAuth {
		return response, nil
	}
	token, tokenErr := c.requestAnonymousToken(ctx, response.Header.Get("WWW-Authenticate"))
	closeResponse(response)
	if tokenErr != nil {
		return nil, tokenErr
	}
	retry, err := blobRequest(ctx, target, token)
	if err != nil {
		return nil, err
	}
	retried, err := client.Do(retry)
	if err != nil {
		return nil, registryRequestFault(err)
	}
	return retried, nil
}

type blobRedirect struct {
	target   string
	response *http.Response
}

func readBlob(
	ctx context.Context,
	redirectClient *http.Client,
	target string,
	response *http.Response,
) ([]byte, error) {
	for redirects := 0; ; redirects++ {
		switch response.StatusCode {
		case http.StatusOK:
			return readBlobBody(response)
		case http.StatusMovedPermanently,
			http.StatusFound,
			http.StatusSeeOther,
			http.StatusTemporaryRedirect,
			http.StatusPermanentRedirect:
			next, err := nextBlobRedirect(ctx, redirectClient, target, response, redirects)
			if err != nil {
				return nil, err
			}
			target = next.target
			response = next.response
		case http.StatusNotFound:
			closeResponse(response)
			return nil, contract.NewFault(
				contract.PreconditionFailed,
				"the image manifest names a config blob the registry does not have",
				"Do not admit an incomplete artifact; push the image again.",
			)
		default:
			status := response.Status
			closeResponse(response)
			return nil, contract.NewFault(
				contract.Unavailable,
				"the registry answered the config read with "+status+"; retry after the registry is reachable.",
				"Retry after the registry is reachable.",
			)
		}
	}
}

func nextBlobRedirect(
	ctx context.Context,
	client *http.Client,
	target string,
	response *http.Response,
	redirects int,
) (blobRedirect, error) {
	location := response.Header.Get("Location")
	closeResponse(response)
	if redirects >= 3 {
		return blobRedirect{}, contract.NewFault(
			contract.PreconditionFailed,
			"the image config redirect limit was exceeded",
			"Use a registry whose config blob resolves within three redirects.",
		)
	}
	next, err := blobRedirectTarget(target, location)
	if err != nil {
		return blobRedirect{}, err
	}
	// Every redirect is a fresh, unauthenticated HTTPS GET. The original
	// private/loopback privilege, bearer token, cookies and headers are
	// deliberately not carried across origins.
	request, err := blobRequest(ctx, next, "")
	if err != nil {
		return blobRedirect{}, err
	}
	nextResponse, err := client.Do(request) //nolint:bodyclose // ownership transfers to readBlob, which closes it on every return path
	if err != nil {
		return blobRedirect{}, registryRequestFault(err)
	}
	return blobRedirect{target: next, response: nextResponse}, nil
}

func readBlobBody(response *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, ConfigBodyLimit+1))
	closeResponse(response)
	if err != nil {
		return nil, contract.NewFault(
			contract.Unavailable,
			"the image config read failed",
			"Retry after the registry is reachable.",
		)
	}
	if len(body) > ConfigBodyLimit {
		return nil, contract.NewFault(
			contract.PreconditionFailed,
			"the image config is larger than the bounded read",
			"Use a normal image config under 8 MiB.",
		)
	}
	return body, nil
}

func noFollowClient(client *http.Client) *http.Client {
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copied
}

func (c *Client) redirectClient() *http.Client {
	if c.RedirectHTTPClient != nil {
		return c.RedirectHTTPClient
	}
	return NewHTTPClientWithLookup(false, c.LookupIP)
}

func blobRequest(ctx context.Context, target, token string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, contract.NewFault(
			contract.ValidationFailed,
			"the image config reference is invalid",
			"Use a registry-provided HTTPS config URL.",
		)
	}
	request.Header.Set("Accept", ManifestAccept)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request, nil
}

func registryRequestFault(err error) error {
	if errors.Is(err, errRedirectRefused) {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the registry returned an unsupported redirect",
			"Registry manifest and token reads never follow redirects; only a bounded HTTPS config-blob redirect is supported.",
		)
	}
	return contract.NewFault(
		contract.Unavailable,
		"the registry is not reachable",
		"Retry after the registry endpoint is reachable; no Docker credentials are used.",
	)
}

func blobRedirectTarget(current, location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the image config redirect has no location",
			"Use a registry whose config blob resolves directly or through a bounded HTTPS redirect.",
		)
	}
	reference, err := url.Parse(location)
	if err != nil || reference.Opaque != "" {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the image config redirect is invalid",
			"Use a registry whose config blob redirects to an absolute HTTPS URL.",
		)
	}
	base, err := url.Parse(current)
	if err != nil {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the image config redirect base is invalid",
			"Use a registry whose config blob redirects to an absolute HTTPS URL.",
		)
	}
	resolved := base.ResolveReference(reference)
	if !strings.EqualFold(resolved.Scheme, "https") || resolved.Host == "" ||
		resolved.User != nil ||
		resolved.Fragment != "" {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the image config redirect is not a plain HTTPS URL",
			"Redirects must be HTTPS, without credentials or a fragment; downgrades and private endpoint reuse are refused.",
		)
	}
	return resolved.String(), nil
}

func (c *Client) get(ctx context.Context, target string) (*http.Response, error) {
	return c.getWithToken(ctx, target, "")
}

func (c *Client) getWithToken(ctx context.Context, target, token string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, contract.NewFault(
			contract.ValidationFailed,
			"the artifact reference is invalid",
			"Use registry/repository:tag or registry/repository@sha256:….",
		)
	}
	request.Header.Set("Accept", ManifestAccept)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		if errors.Is(err, errRedirectRefused) {
			return nil, contract.NewFault(
				contract.PreconditionFailed,
				"the registry returned a redirect",
				"Registry metadata reads never follow redirects; use the canonical registry endpoint.",
			)
		}
		return nil, contract.NewFault(
			contract.Unavailable,
			"the registry is not reachable",
			"Retry after the registry endpoint is reachable; no Docker credentials are used.",
		)
	}
	return response, nil
}

func (c *Client) requestAnonymousToken(ctx context.Context, challenge string) (string, error) {
	realm, params, ok := parseBearerChallenge(challenge)
	if !ok {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the external registry requires credentials",
			"Only public anonymous images are supported; private external registries are not mirrored or credentialed.",
		)
	}
	u, err := validateTokenRealm(realm)
	if err != nil {
		return "", err
	}
	query := u.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if scope := params["scope"]; scope != "" {
		query.Set("scope", scope)
	}
	u.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", contract.NewFault(
			contract.Unavailable,
			"the registry authentication request is invalid",
			"Retry with a public image.",
		)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return "", contract.NewFault(
			contract.Unavailable,
			"the registry token endpoint is not reachable",
			"Retry after the registry is reachable.",
		)
	}
	defer closeResponse(response)
	if response.StatusCode != http.StatusOK {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the external registry refused anonymous access",
			"Only public anonymous images are supported.",
		)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, TokenBodyLimit+1))
	if err != nil || len(body) > TokenBodyLimit {
		return "", contract.NewFault(
			contract.Unavailable,
			"the registry token response is unreadable",
			"Retry after the registry is reachable.",
		)
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &token) != nil {
		return "", contract.NewFault(
			contract.Unavailable,
			"the registry token response is unreadable",
			"Retry after the registry is reachable.",
		)
	}
	value := token.Token
	if value == "" {
		value = token.AccessToken
	}
	if value == "" {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the external registry did not issue an anonymous token",
			"Only public anonymous images are supported.",
		)
	}
	return value, nil
}

func parseBearerChallenge(header string) (string, map[string]string, bool) {
	value := strings.TrimSpace(header)
	if len(value) < 7 || !strings.EqualFold(value[:6], "Bearer") {
		return "", nil, false
	}
	realmIndex := strings.Index(value, `realm="`)
	if realmIndex < 0 {
		return "", nil, false
	}
	rest := value[realmIndex+7:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return "", nil, false
	}
	realm := rest[:end]
	params := map[string]string{}
	for _, field := range strings.Split(rest[end+1:], ",") {
		field = strings.TrimSpace(field)
		key, raw, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		params[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(raw), `"`)
	}
	return realm, params, true
}

func validateTokenRealm(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, contract.NewFault(
			contract.PreconditionFailed,
			"the external registry returned an invalid anonymous token realm",
			"Only a public HTTPS token realm is supported; private external credentials are not used.",
		)
	}
	host := u.Hostname()
	if host == "" {
		return nil, contract.NewFault(
			contract.PreconditionFailed,
			"the external registry returned an invalid anonymous token realm",
			"Only a public HTTPS token realm is supported.",
		)
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return nil, contract.NewFault(
			contract.PreconditionFailed,
			"the external registry requested a local token realm",
			"Only a public HTTPS token realm is supported.",
		)
	}
	if ip := net.ParseIP(host); ip != nil && !destinationAllowed(ip, false) {
		return nil, contract.NewFault(
			contract.PreconditionFailed,
			"the external registry requested a private token realm",
			"Only a public HTTPS token realm is supported.",
		)
	}
	return u, nil
}

func escapeRepository(repository string) string {
	parts := strings.Split(strings.Trim(repository, "/"), "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func closeResponse(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// IsDigest reports whether value is a sha256 content digest.
func IsDigest(value string) bool {
	return digestPattern.MatchString(strings.ToLower(strings.TrimSpace(value)))
}
