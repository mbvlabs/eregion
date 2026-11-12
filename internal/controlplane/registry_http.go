package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// errRegistryRedirectRefused is deliberately fatal: a registry metadata read
// never follows a redirect, so an origin cannot bounce the client to a private
// network, downgrade HTTPS, or forward an authorization header to another host.
var errRegistryRedirectRefused = errors.New("registry redirect refused")

var blockedRegistryDestinations = mustRegistryPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "fc00::/7",
	"fe80::/10", "ff00::/8", "2001:db8::/32", "2002::/16", "64:ff9b::/96",
)

func mustRegistryPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}

// newRegistryHTTPClient returns a client with no environment proxy, bounded
// timeouts, no redirects and a dial-time destination policy. External clients
// allow only public IPs; the explicitly isolated private-registry client allows
// only loopback addresses. DNS results are pinned for the dial, so a rebinding
// answer cannot change the connected address.
func newRegistryHTTPClient(allowLoopback bool) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           registryDialContext(allowLoopback),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
	}
	return &http.Client{
		Timeout:   artifactRequestTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRegistryRedirectRefused
		},
	}
}

func registryDialContext(allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("registry destination is invalid: %w", err)
		}
		if host == "" || port == "" {
			return nil, errors.New("registry destination is incomplete")
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("registry destination could not be resolved: %w", err)
		}
		if len(addresses) == 0 {
			return nil, errors.New("registry destination resolved to no address")
		}
		for _, candidate := range addresses {
			if !registryDestinationAllowed(candidate.IP, allowLoopback) {
				return nil, fmt.Errorf("registry destination %s is not an allowed public address", candidate.IP)
			}
		}
		var lastErr error
		for _, candidate := range addresses {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
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
}

func registryDestinationAllowed(ip net.IP, allowLoopback bool) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() {
		return allowLoopback
	}
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedRegistryDestinations {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// validateRegistryTokenRealm applies the same public-HTTPS contract to an
// anonymous token endpoint before it is dialed. The client's dial policy still
// checks the resolved IPs; this rejects malformed, downgraded and
// credential-bearing realms before any request is made.
func validateRegistryTokenRealm(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, contract.NewFault(contract.PreconditionFailed, "the external registry returned an invalid anonymous token realm", "Only a public HTTPS token realm is supported; private external credentials are not used.")
	}
	host := u.Hostname()
	if host == "" {
		return nil, contract.NewFault(contract.PreconditionFailed, "the external registry returned an invalid anonymous token realm", "Only a public HTTPS token realm is supported.")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return nil, contract.NewFault(contract.PreconditionFailed, "the external registry requested a local token realm", "Only a public HTTPS token realm is supported.")
	}
	if ip := net.ParseIP(host); ip != nil && !registryDestinationAllowed(ip, false) {
		return nil, contract.NewFault(contract.PreconditionFailed, "the external registry requested a private token realm", "Only a public HTTPS token realm is supported.")
	}
	return u, nil
}
