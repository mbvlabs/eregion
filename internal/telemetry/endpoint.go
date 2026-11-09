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
)

// EndpointSchema identifies the non-secret endpoint description.
const EndpointSchema = "eregion/telemetry-endpoint/v1"

// RotationSchema identifies the non-secret rotation result.
const RotationSchema = "eregion/telemetry-rotation/v1"

// EndpointInfo is the non-secret description of the node's authenticated
// telemetry endpoint. It names paths, the WireGuard address and whether a
// credential exists, never a password or hash.
type EndpointInfo struct {
	Schema         string `json:"schema"`
	Node           string `json:"node"`
	Listen         string `json:"listen"`
	MetricsPath    string `json:"metrics_path"`
	DiscoveryPath  string `json:"discovery_path"`
	Access         string `json:"access"`
	Public         bool   `json:"public"`
	Authentication string `json:"authentication"`
	// Credential is true when the node's dedicated telemetry Caddyfile exists.
	// The endpoint is not served before the first rotation.
	Credential   bool   `json:"credential_configured"`
	MetricsURL   string `json:"metrics_url"`
	DiscoveryURL string `json:"discovery_url"`
	Upstream     string `json:"upstream"`
	Collection   string `json:"collection"`
	Note         string `json:"note"`
}

// EndpointListenAddress is the only endpoint listen address.
func EndpointListenAddress(nodeAddress string) string {
	return net.JoinHostPort(strings.TrimSpace(nodeAddress), Port)
}

// EndpointMetricsURL is the authenticated node metrics URL.
func EndpointMetricsURL(nodeAddress string) string {
	return "http://" + EndpointListenAddress(nodeAddress) + MetricsPath
}

// EndpointDiscoveryURL is the authenticated HTTP service-discovery URL.
func EndpointDiscoveryURL(nodeAddress string) string {
	return "http://" + EndpointListenAddress(nodeAddress) + DiscoveryPath
}

// Validate refuses an endpoint description that would claim a public or
// unauthenticated surface.
func (e EndpointInfo) Validate() error {
	if e.Schema != EndpointSchema {
		return fmt.Errorf("unexpected telemetry endpoint schema %q", e.Schema)
	}
	if strings.TrimSpace(e.Node) == "" || strings.TrimSpace(e.Listen) == "" {
		return fmt.Errorf("telemetry endpoint identity is missing")
	}
	if e.Access != "wireguard" || e.Authentication != "basic" {
		return fmt.Errorf("telemetry endpoint access must be WireGuard with basic authentication")
	}
	if e.Public {
		return fmt.Errorf("telemetry endpoint is marked public")
	}
	if e.MetricsPath != MetricsPath || e.DiscoveryPath != DiscoveryPath {
		return fmt.Errorf("telemetry endpoint paths are unexpected")
	}
	for _, value := range []string{e.MetricsURL, e.DiscoveryURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
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
	Username   string   `json:"username"`
	MetricsURL string   `json:"metrics_url"`
	Discovery  string   `json:"discovery_url"`
	Warnings   []string `json:"warnings,omitempty"`
}

// BuildEndpointInfo describes the endpoint without opening it. credential is
// true when the node has a dedicated telemetry Caddyfile with a basic-auth hash.
func BuildEndpointInfo(node, nodeAddress, upstream string, credential bool) EndpointInfo {
	listen := EndpointListenAddress(nodeAddress)
	return EndpointInfo{
		Schema:         EndpointSchema,
		Node:           strings.TrimSpace(node),
		Listen:         listen,
		MetricsPath:    MetricsPath,
		DiscoveryPath:  DiscoveryPath,
		Access:         "wireguard",
		Public:         false,
		Authentication: "basic",
		Credential:     credential,
		MetricsURL:     EndpointMetricsURL(nodeAddress),
		DiscoveryURL:   EndpointDiscoveryURL(nodeAddress),
		Upstream:       upstream,
		Collection:     "operator_or_external_scraper",
		Note:           "The endpoint listens only on the node's WireGuard address; Prometheus or Grafana Alloy must be a WireGuard peer. There is no public listener, central telemetry server or retained history on the node.",
	}
}

type discoveryTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// TargetsJSON returns the Prometheus HTTP service-discovery document (also
// understood by Grafana Alloy's discovery.http). It describes one scrape path
// at the authenticated endpoint.
func TargetsJSON(node, nodeAddress string) ([]byte, error) {
	node = strings.TrimSpace(node)
	if node == "" {
		return nil, fmt.Errorf("the telemetry target has no node name")
	}
	listen := EndpointListenAddress(nodeAddress)
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return nil, fmt.Errorf("invalid telemetry listen address: %w", err)
	}
	out := []discoveryTarget{{
		Targets: []string{listen},
		Labels: map[string]string{
			"__metrics_path__":  MetricsPath,
			"__scheme__":        "http",
			"eregion_node":      node,
			"eregion_component": "node",
			"eregion_access":    "wireguard",
			"eregion_public":    "false",
		},
	}}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// PrometheusConfig renders a scrape configuration that follows the endpoint's
// HTTP service discovery. password is either PasswordPlaceholder for ordinary
// output or the real credential in an explicit owner-only config file.
func PrometheusConfig(node, nodeAddress, username, password string) string {
	discovery := EndpointDiscoveryURL(nodeAddress)
	return fmt.Sprintf(`scrape_configs:
  - job_name: eregion-%s
    scheme: http
    basic_auth:
      username: %s
      password: %s
    http_sd_configs:
      - url: %s
        refresh_interval: 5m
        basic_auth:
          username: %s
          password: %s
`, safeConfigToken(node), username, password, discovery, username, password)
}

// AlloyConfig renders the Grafana Alloy equivalent. It assumes the operator
// configures a prometheus.remote_write.default receiver.
func AlloyConfig(node, nodeAddress, username, password string) string {
	discovery := EndpointDiscoveryURL(nodeAddress)
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
  scheme     = "http"
  basic_auth {
    username = "%s"
    password = "%s"
  }
  forward_to = [prometheus.remote_write.default.receiver]
}
`, id, discovery, username, password, id, id, username, password)
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
