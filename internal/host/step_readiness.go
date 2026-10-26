package host

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
)

// Probe checks the local API liveness and public readiness endpoints.
type Probe interface {
	Health(context.Context, Config) error
}

type httpProbe struct {
	Client *http.Client
}

func (p httpProbe) Health(ctx context.Context, cfg Config) error {
	client := p.Client
	if client == nil {
		client = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				// This is a local liveness probe against the listener
				// bootstrap just configured. It is not a trust decision for
				// remote clients; remote clients still require a valid
				// certificate through api.NewClient.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	base, err := probeBase(cfg)
	if err != nil {
		return err
	}
	health, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	if err := doProbe(client, health); err != nil {
		return fmt.Errorf("API liveness check failed: %w", err)
	}
	ready, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
	if err != nil {
		return err
	}
	if err := doProbe(client, ready); err != nil {
		return fmt.Errorf("API readiness check failed: %w", err)
	}
	return nil
}

func doProbe(client *http.Client, request *http.Request) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

func probeBase(cfg Config) (string, error) {
	host, port, err := net.SplitHostPort(cfg.APIListen)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("the API listen address must be a literal IP")
	}
	switch {
	case ip.IsUnspecified() && ip.To4() != nil:
		host = "127.0.0.1"
	case ip.IsUnspecified():
		host = "::1"
	}
	scheme := "http"
	if cfg.APITLSCert != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

type readinessStep struct {
	cfg   Config
	exec  Exec
	probe Probe
}

func (s *readinessStep) ID() string { return "host-readiness-v1" }
func (s *readinessStep) Describe() string {
	return "Verify the API liveness and public database readiness endpoints"
}
func (s *readinessStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *readinessStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Listen string `json:"listen"`
		TLS    bool   `json:"tls"`
	}{s.cfg.APIListen, s.cfg.APITLSCert != ""})
}

func (s *readinessStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	output, err := checkOutput(ctx, engine.Runtime{}, s.exec, `if systemctl is-active --quiet eregion-api.service; then echo api:ok; else echo api:missing; fi
if systemctl is-active --quiet eregion-worker.service; then echo worker:ok; else echo worker:missing; fi`)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if strings.Contains(output, ":missing") {
		return incomplete("control-plane services are not both active: " + output)
	}
	if s.probe == nil {
		return engine.CheckResult{}, fmt.Errorf("no readiness probe is configured")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := s.probe.Health(probeCtx, s.cfg); err != nil {
		return incomplete("services are active but not ready: " + err.Error())
	}
	return complete("API liveness and public /readyz succeeded")
}

func (s *readinessStep) Apply(ctx context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	if s.probe == nil {
		return engine.ApplyResult{}, fmt.Errorf("no readiness probe is configured")
	}
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		last = s.probe.Health(probeCtx, s.cfg)
		cancel()
		if last == nil {
			return engine.ApplyResult{Notes: []string{"API liveness and public /readyz succeeded"}, Result: map[string]any{"ready": true}}, nil
		}
		select {
		case <-ctx.Done():
			return engine.ApplyResult{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return engine.ApplyResult{}, contractError("control plane did not become ready: " + safeError(last))
}

func (s *readinessStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	return []string{"+ require both services active", "+ require GET /healthz and public GET /readyz to succeed"}, nil
}

func (s *readinessStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
