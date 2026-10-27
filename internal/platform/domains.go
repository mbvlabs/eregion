package platform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Domain bindings keep four separate facts: the desired hostname, the Caddy
// route, the DNS record, the certificate and the public response. None of the
// observations changes the desired binding.
type RouteState string

const (
	RoutePending RouteState = "pending"
	RouteApplied RouteState = "applied"
	RouteFailed  RouteState = "failed"
)

type DNSState string

const (
	DNSPending DNSState = "pending"
	DNSOK      DNSState = "ok"
	DNSWrong   DNSState = "wrong"
	DNSProxied DNSState = "proxied"
)

type CertificateState string

const (
	CertificatePending CertificateState = "pending"
	CertificateValid   CertificateState = "valid"
	CertificateFailed  CertificateState = "failed"
)

type PublicState string

const (
	PublicPending PublicState = "pending"
	PublicOK      PublicState = "ok"
	PublicFailed  PublicState = "failed"
)

// DomainBinding is the list/status view of one desired hostname plus its
// applied route revision and the independent observations.
type DomainBinding struct {
	Domain      string `json:"domain"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Primary     bool   `json:"primary"`

	DesiredRevision int64      `json:"desired_revision"`
	AppliedRevision int64      `json:"applied_revision"`
	RouteState      RouteState `json:"route_state"`
	RouteError      string     `json:"route_error,omitempty"`
	RouteAppliedAt  *time.Time `json:"route_applied_at,omitempty"`
	RouteObservedAt *time.Time `json:"route_observed_at,omitempty"`
	Upstreams       []string   `json:"upstreams"`

	ExpectedAddresses []string   `json:"expected_addresses"`
	DNSState          DNSState   `json:"dns_state"`
	DNSAddresses      []string   `json:"dns_addresses"`
	DNSError          string     `json:"dns_error,omitempty"`
	DNSObservedAt     *time.Time `json:"dns_observed_at,omitempty"`

	CertificateState      CertificateState `json:"certificate_state"`
	CertificateNotAfter   *time.Time       `json:"certificate_not_after,omitempty"`
	CertificateError      string           `json:"certificate_error,omitempty"`
	CertificateObservedAt *time.Time       `json:"certificate_observed_at,omitempty"`

	PublicState      PublicState `json:"public_state"`
	PublicStatus     int         `json:"public_status,omitempty"`
	PublicError      string      `json:"public_error,omitempty"`
	PublicObservedAt *time.Time  `json:"public_observed_at,omitempty"`

	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// Ready is true only when the route, DNS, certificate and public response all
// observed success. An accepted binding or a written route is not enough. A
// Cloudflare-proxied record counts as DNS success: the proxy reaches the node,
// and the certificate and public checks run against the node's own Caddy.
func (d DomainBinding) Ready() bool {
	return d.RouteState == RouteApplied && d.AppliedRevision >= d.DesiredRevision &&
		DNSReachesNode(d.DNSState) && d.CertificateState == CertificateValid && d.PublicState == PublicOK
}

// DNSReachesNode reports whether public traffic for the record reaches the
// main node: directly, or through a Cloudflare proxy.
func DNSReachesNode(state DNSState) bool { return state == DNSOK || state == DNSProxied }

// PublicResolvers are queried directly over UDP so a local cache cannot hide a
// missing or proxied record.
var PublicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// DNSAnswers is one resolver's A/AAAA answer.
type DNSAnswers struct {
	Resolver string   `json:"resolver"`
	A        []string `json:"a,omitempty"`
	AAAA     []string `json:"aaaa,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// DNSObservation is every resolver's answer and the merged address set.
type DNSObservation struct {
	Resolvers []DNSAnswers `json:"resolvers"`
	Addresses []string     `json:"addresses"`
	Error     string       `json:"error,omitempty"`
}

// LookupPublicDNS asks each public resolver for A and AAAA records over DNS/UDP.
func LookupPublicDNS(ctx context.Context, host string) DNSObservation {
	observation := DNSObservation{Addresses: []string{}}
	seen := map[string]bool{}
	var failures []string
	for _, server := range PublicResolvers {
		answers := lookupResolver(ctx, server, host)
		observation.Resolvers = append(observation.Resolvers, answers)
		if answers.Error != "" {
			failures = append(failures, answers.Error)
		}
		for _, address := range append(append([]string(nil), answers.A...), answers.AAAA...) {
			if !seen[address] {
				seen[address] = true
				observation.Addresses = append(observation.Addresses, address)
			}
		}
	}
	sort.Strings(observation.Addresses)
	if len(observation.Addresses) == 0 && len(failures) > 0 {
		observation.Error = strings.Join(failures, "; ")
	}
	return observation
}

func lookupResolver(ctx context.Context, server, host string) DNSAnswers {
	out := DNSAnswers{Resolver: strings.TrimSuffix(server, ":53")}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "udp", server)
		},
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if ips, err := resolver.LookupNetIP(lookupCtx, "ip4", host); err == nil {
		for _, ip := range ips {
			out.A = appendUnique(out.A, ip.Unmap().String())
		}
	} else {
		out.Error = dnsLookupError(err)
	}
	if ips, err := resolver.LookupNetIP(lookupCtx, "ip6", host); err == nil {
		for _, ip := range ips {
			if !ip.Is4In6() {
				out.AAAA = appendUnique(out.AAAA, ip.String())
			}
		}
	}
	sort.Strings(out.A)
	sort.Strings(out.AAAA)
	return out
}

func dnsLookupError(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "no record"
		case dnsErr.IsTimeout:
			return "timeout"
		}
	}
	return err.Error()
}

// ClassifyDNS compares the observed addresses with the main node's recorded
// addresses. Cloudflare proxy addresses are reported as proxied; any other
// mismatch is wrong and never becomes ready.
func ClassifyDNS(expected, observed []string) DNSState {
	want := addressSet(expected)
	if len(want) == 0 {
		return DNSPending
	}
	got := addressSet(observed)
	if len(got) == 0 {
		return DNSWrong
	}
	matches := true
	for address := range want {
		if !got[address] {
			matches = false
			break
		}
	}
	if matches {
		return DNSOK
	}
	for address := range got {
		if CloudflareProxied(address) {
			return DNSProxied
		}
	}
	return DNSWrong
}

func addressSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		addr, err := netip.ParseAddr(strings.TrimSpace(value))
		if err == nil {
			out[addr.Unmap().String()] = true
		}
	}
	return out
}

var cloudflareRanges = mustPrefixes(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

func mustPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}

// CloudflareProxied reports whether an answer belongs to Cloudflare's proxy
// ranges, which means the record is not DNS-only and cannot prove the app.
func CloudflareProxied(address string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range cloudflareRanges {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// CertificateObservation is the result of a TLS handshake through local Caddy.
type CertificateObservation struct {
	State    CertificateState `json:"state"`
	NotAfter *time.Time       `json:"not_after,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// ObserveCertificate handshakes with the hostname through address (normally
// local Caddy) with SNI, then verifies the presented chain and name against the
// system roots. A missing certificate stays pending; an invalid one fails.
func ObserveCertificate(ctx context.Context, host, address string) CertificateObservation {
	dialer := &net.Dialer{Timeout: 6 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return CertificateObservation{State: CertificatePending, Error: err.Error()}
	}
	defer raw.Close()
	conn := tls.Client(raw, &tls.Config{ //nolint:gosec // The chain is verified explicitly below so expiry and validity are both recorded.
		MinVersion: tls.VersionTLS12,
		ServerName: host,
		// Inspect the certificate even when verification fails so the
		// observation can record the expiry instead of inventing one.
		InsecureSkipVerify: true,
	})
	handshakeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		return CertificateObservation{State: certificateHandshakeState(err), Error: err.Error()}
	}
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return CertificateObservation{State: CertificatePending, Error: "no certificate was presented"}
	}
	leaf := state.PeerCertificates[0]
	notAfter := leaf.NotAfter.UTC()
	observation := CertificateObservation{NotAfter: &notAfter}
	roots, err := x509.SystemCertPool()
	if err != nil {
		observation.State, observation.Error = CertificateFailed, "system certificate roots are unavailable: "+err.Error()
		return observation
	}
	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, DNSName: host,
	}); err != nil {
		observation.State, observation.Error = CertificateFailed, err.Error()
		return observation
	}
	observation.State = CertificateValid
	return observation
}

func certificateHandshakeState(err error) CertificateState {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return CertificatePending
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return CertificatePending
	}
	return CertificateFailed
}

// PublicObservation is one HTTP response through local Caddy with the real
// hostname and SNI. Certificate validity is observed separately.
type PublicObservation struct {
	State  PublicState `json:"state"`
	Status int         `json:"status,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// ObservePublic requests path from the hostname through local Caddy.
func ObservePublic(ctx context.Context, host, path, address string) PublicObservation {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: 4 * time.Second}
				return dialer.DialContext(ctx, "tcp", address)
			},
			// The URL host selects the Caddy site; the dial stays local. The
			// certificate is verified by ObserveCertificate, not here.
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, InsecureSkipVerify: true}, //nolint:gosec // Loopback response probe; the chain is observed separately.
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return PublicObservation{State: PublicFailed, Error: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return PublicObservation{State: PublicFailed, Error: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	observation := PublicObservation{Status: resp.StatusCode}
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		observation.State = PublicOK
		return observation
	}
	observation.State = PublicFailed
	observation.Error = fmt.Sprintf("status %d", resp.StatusCode)
	return observation
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
