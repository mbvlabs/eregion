package telemetry

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Authenticated telemetry endpoint constants. The public ingress terminates
// HTTPS on the main hostname and proxies two exact paths to a loopback-only
// authenticated gateway; node_exporter itself stays on loopback.
const (
	// Port is the loopback gateway's TCP port.
	Port = "9101"
	// GatewayHost and GatewayListenAddress are the only gateway listener.
	GatewayHost          = "127.0.0.1"
	GatewayListenAddress = GatewayHost + ":" + Port
	// MetricsPath is the public HTTPS path the gateway proxies to
	// node_exporter's /metrics.
	MetricsPath       = "/metrics/node"
	APIMetricsPath    = "/metrics/api"
	WorkerMetricsPath = "/metrics/worker"
	// DiscoveryPath serves the public HTTPS Prometheus HTTP service-discovery
	// document.
	DiscoveryPath = "/metrics/targets.json"
	// GatewayMetricsPath is the gateway-local upstream path for MetricsPath.
	GatewayMetricsPath       = "/node"
	GatewayAPIMetricsPath    = "/api"
	GatewayWorkerMetricsPath = "/worker"
	// GatewayDiscoveryPath is the gateway-local path for DiscoveryPath.
	GatewayDiscoveryPath = "/targets.json"
	// PublicPort is the HTTPS port advertised by service discovery.
	PublicPort = "443"
	// Username is the only basic-auth principal the endpoint accepts.
	Username = "metrics"
	// PasswordPlaceholder is used by ordinary `metrics config` output. The
	// real password is written only to an explicit owner-only file or the
	// protected server handoff.
	PasswordPlaceholder = "<metrics-password>"
	// MetricsBcryptCost matches vaire's endpoint hash cost.
	MetricsBcryptCost = 8
)

// Credential is the operator-side endpoint credential, as vaire keeps it in
// the workspace credential directory. The node receives only its bcrypt hash.
type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// NewCredential returns a fresh 256-bit basic-auth credential.
func NewCredential() (Credential, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Credential{}, err
	}
	return Credential{
		Username: Username,
		Password: base64.RawURLEncoding.EncodeToString(raw[:]),
	}, nil
}

// Validate checks a credential before it is written or shown.
func (c Credential) Validate() error {
	if c.Username != Username {
		return fmt.Errorf("the telemetry credential username must be %q", Username)
	}
	if len(c.Password) < 32 || len(c.Password) > 256 {
		return fmt.Errorf("the telemetry credential password has an invalid length")
	}
	for _, r := range c.Password {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("the telemetry credential password contains an invalid character")
	}
	return nil
}

// HashPassword returns the bcrypt hash stored on the node. Only this value
// travels in a node RPC secret.
func HashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", fmt.Errorf("the telemetry password is empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), MetricsBcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword checks a password against a stored bcrypt hash.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ValidHash reports whether value is a well-formed bcrypt hash.
func ValidHash(value string) bool {
	_, err := bcrypt.Cost([]byte(value))
	return err == nil
}
