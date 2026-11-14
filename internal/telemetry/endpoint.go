package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/platform"
)

// EndpointSchema identifies the non-secret endpoint description.
const EndpointSchema = "eregion/telemetry-endpoint/v2"

// RotationSchema identifies the non-secret rotation result.
const RotationSchema = "eregion/telemetry-rotation/v2"

// Endpoint metadata states. They describe local configuration, never public
// DNS, certificate issuance, route activation or reachability.
const (
	EndpointStateUnconfigured      = "unconfigured"
	EndpointStatePendingCredential = "pending_credential"
	EndpointStateConfigured        = "configured"
	// EndpointStateNeedsApply means a managed gateway file with a credential
	// exists but is not the current loopback render (for example an unapplied
	// older WireGuard layout). The public URL is desired, not active.
	EndpointStateNeedsApply = "needs_apply"
)

// EndpointInfo is the non-secret description of the public authenticated
// telemetry endpoint. It names the desired public HTTPS URLs and the loopback
// gateway, never a password or hash. It does not claim that DNS, TLS or the
// public route has been observed.
type EndpointInfo struct {
	Schema         string `json:"schema"`
	Node           string `json:"node"`
	Hostname       string `json:"hostname,omitempty"`
	State          string `json:"state"`
	Listen         string `json:"listen"`
	MetricsPath    string `json:"metrics_path"`
	DiscoveryPath  string `json:"discovery_path"`
	Access         string `json:"access"`
	Public         bool   `json:"public"`
	Authentication string `json:"authentication"`
	// Credential is true when the node's dedicated telemetry Caddyfile exists
	// and contains a valid bcrypt hash for the metrics principal.
	Credential   bool   `json:"credential_configured"`
	MetricsURL   string `json:"metrics_url,omitempty"`
	DiscoveryURL string `json:"discovery_url,omitempty"`
	Upstream     string `json:"upstream"`
	Collection   string `json:"collection"`
	Note         string `json:"note"`
}

// PublicMetricsURL is the desired authenticated public metrics URL.
func PublicMetricsURL(hostname string) (string, error) {
	return publicEndpointURL(hostname, MetricsPath)
}

// PublicDiscoveryURL is the desired authenticated public discovery URL.
func PublicDiscoveryURL(hostname string) (string, error) {
	return publicEndpointURL(hostname, DiscoveryPath)
}

func publicEndpointURL(hostname, path string) (string, error) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "", fmt.Errorf("the node has no public hostname; the telemetry endpoint is unconfigured")
	}
	if net.ParseIP(hostname) != nil {
		return "", fmt.Errorf("the telemetry hostname must be a public DNS name, not an IP address")
	}
	if err := platform.ValidateDomain(hostname); err != nil {
		return "", fmt.Errorf("invalid telemetry hostname: %w", err)
	}
	return "https://" + hostname + path, nil
}

// Validate refuses an endpoint description that would claim an unauthenticated
// or non-public URL, or reconstruct a target from the listener address.
func (e EndpointInfo) Validate() error {
	if e.Schema != EndpointSchema {
		return fmt.Errorf("unexpected telemetry endpoint schema %q", e.Schema)
	}
	if strings.TrimSpace(e.Node) == "" {
		return fmt.Errorf("telemetry endpoint identity is missing")
	}
	if e.Listen != GatewayListenAddress {
		return fmt.Errorf("telemetry gateway must listen on %s", GatewayListenAddress)
	}
	if e.MetricsPath != MetricsPath || e.DiscoveryPath != DiscoveryPath {
		return fmt.Errorf("telemetry endpoint paths are unexpected")
	}
	switch e.State {
	case EndpointStateUnconfigured:
		if strings.TrimSpace(e.Hostname) != "" || e.MetricsURL != "" || e.DiscoveryURL != "" ||
			e.Credential || e.Public {
			return fmt.Errorf("an unconfigured telemetry endpoint must not claim a hostname, URL or credential")
		}
		if e.Access != EndpointStateUnconfigured || e.Authentication != "none" {
			return fmt.Errorf("an unconfigured telemetry endpoint must not claim access")
		}
		return nil
	case EndpointStatePendingCredential, EndpointStateConfigured, EndpointStateNeedsApply:
	default:
		return fmt.Errorf("unexpected telemetry endpoint state %q", e.State)
	}
	if (e.State == EndpointStateConfigured || e.State == EndpointStateNeedsApply) && !e.Credential {
		return fmt.Errorf("a configured telemetry endpoint must report a credential")
	}
	if e.State == EndpointStatePendingCredential && e.Credential {
		return fmt.Errorf("a pending telemetry endpoint must not report a credential")
	}
	if e.Access != "public_https" || !e.Public || e.Authentication != "basic" {
		return fmt.Errorf("telemetry endpoint must be public HTTPS with basic authentication")
	}
	hostname := strings.TrimSpace(e.Hostname)
	metricsURL, err := PublicMetricsURL(hostname)
	if err != nil {
		return err
	}
	discoveryURL, err := PublicDiscoveryURL(hostname)
	if err != nil {
		return err
	}
	if e.MetricsURL != metricsURL || e.DiscoveryURL != discoveryURL {
		return fmt.Errorf("telemetry endpoint URLs are not the exact public hostname paths")
	}
	for _, value := range []string{e.MetricsURL, e.DiscoveryURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return fmt.Errorf("invalid telemetry endpoint URL")
		}
	}
	return nil
}

// EndpointStatus is one unauthenticated observation used by doctor. It never
// carries the credential and never treats an unauthenticated 200 as healthy.
type EndpointStatus struct {
	Status    int
	Reachable bool
	NoAuth    bool
	Error     string
}

// ProbeEndpoint requests the discovery path without a credential. A healthy
// authenticated endpoint answers 401; a 200 is an unauthenticated exposure.
func ProbeEndpoint(ctx context.Context, discoveryURL string, client *http.Client) EndpointStatus {
	if client == nil {
		client = &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return EndpointStatus{Error: err.Error()}
	}
	response, err := client.Do(request)
	if err != nil {
		return EndpointStatus{Error: err.Error()}
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	return EndpointStatus{
		Status:    response.StatusCode,
		Reachable: true,
		NoAuth:    response.StatusCode == http.StatusOK,
	}
}

// Rotation is the non-secret result of replacing the endpoint credential.
type Rotation struct {
	Schema     string   `json:"schema"`
	Node       string   `json:"node"`
	Hostname   string   `json:"hostname"`
	Username   string   `json:"username"`
	MetricsURL string   `json:"metrics_url"`
	Discovery  string   `json:"discovery_url"`
	Warnings   []string `json:"warnings,omitempty"`
}

// BuildEndpointInfo describes the endpoint without opening it. credential is
// true when the node has a dedicated loopback gateway Caddyfile with a
// valid basic-auth hash. A missing hostname is explicitly unconfigured: there
// is no WireGuard or listener-address fallback.
func BuildEndpointInfo(node, hostname, upstream string, credential bool) (EndpointInfo, error) {
	node = strings.TrimSpace(node)
	if node == "" {
		return EndpointInfo{}, fmt.Errorf("the telemetry endpoint has no node name")
	}
	hostname = strings.TrimSpace(hostname)
	info := EndpointInfo{
		Schema:         EndpointSchema,
		Node:           node,
		Hostname:       hostname,
		Listen:         GatewayListenAddress,
		MetricsPath:    MetricsPath,
		DiscoveryPath:  DiscoveryPath,
		Upstream:       upstream,
		Collection:     "operator_or_external_scraper",
		State:          EndpointStateUnconfigured,
		Access:         EndpointStateUnconfigured,
		Authentication: "none",
	}
	if hostname == "" {
		info.Note = "The node has no public hostname, so the telemetry endpoint is unconfigured. There is no WireGuard or loopback-address fallback target."
		return info, nil
	}
	metricsURL, err := PublicMetricsURL(hostname)
	if err != nil {
		return EndpointInfo{}, err
	}
	discoveryURL, err := PublicDiscoveryURL(hostname)
	if err != nil {
		return EndpointInfo{}, err
	}
	info.MetricsURL = metricsURL
	info.DiscoveryURL = discoveryURL
	info.Access = "public_https"
	info.Public = true
	info.Authentication = "basic"
	info.Credential = credential
	info.State = EndpointStatePendingCredential
	if credential {
		info.State = EndpointStateConfigured
	}
	info.Note = "The URLs are the desired public HTTPS ingress paths; the raw exporter and the authenticated gateway stay on loopback. Configuration does not assert DNS, certificate issuance, route activation or reachability."
	return info, nil
}

type discoveryTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// TargetsJSON returns the Prometheus HTTP service-discovery document (also
// understood by Grafana Alloy's discovery.http). It advertises the main public
// hostname on HTTPS only; it never derives a target from the loopback gateway,
// the WireGuard address or a request host.
func TargetsJSON(node, hostname string) ([]byte, error) {
	node = strings.TrimSpace(node)
	if node == "" {
		return nil, fmt.Errorf("the telemetry target has no node name")
	}
	if _, err := PublicMetricsURL(hostname); err != nil {
		return nil, err
	}
	target := strings.TrimSpace(hostname) + ":" + PublicPort
	if _, _, err := net.SplitHostPort(target); err != nil {
		return nil, fmt.Errorf("invalid telemetry target hostname: %w", err)
	}
	var out []discoveryTarget
	for _, component := range []struct{ name, path string }{
		{"node", MetricsPath}, {"api", APIMetricsPath}, {"worker", WorkerMetricsPath},
	} {
		out = append(out, discoveryTarget{Targets: []string{target}, Labels: map[string]string{
			"__metrics_path__": component.path, "__scheme__": "https",
			"eregion_node": node, "eregion_component": component.name,
			"eregion_access": "public_https", "eregion_public": "true",
		}})
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// PrometheusConfig renders a scrape configuration that follows the endpoint's
// public HTTPS HTTP service discovery. password is either PasswordPlaceholder
// for ordinary output or the real credential in an explicit owner-only config
// file. TLS verification is left at the default system trust store.
func PrometheusConfig(node, hostname, username, password string) (string, error) {
	discovery, err := PublicDiscoveryURL(hostname)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`scrape_configs:
  - job_name: eregion-%s
    scheme: https
    basic_auth:
      username: %s
      password: %s
    http_sd_configs:
      - url: %s
        refresh_interval: 5m
        basic_auth:
          username: %s
          password: %s
    # TLS verification is left at the system trust store default.
`, safeConfigToken(node), username, password, discovery, username, password), nil
}

// AlloyConfig renders the Grafana Alloy equivalent. It assumes the operator
// configures a prometheus.remote_write.default receiver. TLS verification is
// left at the default system trust store.
func AlloyConfig(node, hostname, username, password string) (string, error) {
	discovery, err := PublicDiscoveryURL(hostname)
	if err != nil {
		return "", err
	}
	id := safeConfigToken(node)
	return fmt.Sprintf(`discovery.http "eregion_%s" {
  url              = "%s"
  refresh_interval = "5m"
  basic_auth {
    username = "%s"
    password = "%s"
  }
}

prometheus.scrape "eregion_%s" {
  targets    = discovery.http.eregion_%s.targets
  scheme     = "https"
  basic_auth {
    username = "%s"
    password = "%s"
  }
  forward_to = [prometheus.remote_write.default.receiver]
  // TLS verification is left at the system trust store default.
}
`, id, discovery, username, password, id, id, username, password), nil
}

func safeConfigToken(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "node"
	}
	return b.String()
}
