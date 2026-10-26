package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// runDomain applies a domain change: the domains table already holds it, so
// the routes file is rewritten from the serving deployments and Caddy loads it.
func (w *platformWorker) runDomain(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.DomainInput)
	if err := w.service.syncRoutes(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		kind, summary := "domain.added", "Routed "+input.Domain+" to "+input.App+"/"+input.Environment
		if normalized.Kind == platform.KindDomainRemove {
			kind, summary = "domain.removed", "Removed the route for "+input.Domain
		}
		return insertPlatformChange(ctx, tx, op, kind, input.App, input.Environment, summary, map[string]any{"domain": input.Domain}, nil)
	})
}

// syncRoutes rewrites the Caddy routes file from the domains table and each
// environment's serving deployment, then loads the Caddyfile through Caddy's
// admin endpoint. A config Caddy rejects puts the previous routes file back.
func (s *Service) syncRoutes(ctx context.Context) error {
	return s.writeRoutes(ctx, "", nil)
}

// switchRoutesForDeployment points one environment's domains at candidate
// upstreams while leaving every other environment on its serving deployment.
func (s *Service) switchRoutesForDeployment(ctx context.Context, env platform.Environment, upstreams []string) error {
	return s.writeRoutes(ctx, env.ID, upstreams)
}

func (s *Service) writeRoutes(ctx context.Context, overrideEnvironment platform.EnvironmentID, overrideUpstreams []string) error {
	rows, err := s.pool.Query(ctx, `SELECT d.name,e.id,coalesce(dep.instances,'[]'::jsonb)
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        LEFT JOIN eregion_platform_deployments dep ON dep.id=e.serving_deployment_id
        ORDER BY d.name`)
	if err != nil {
		return unavailable()
	}
	var routes []platform.Route
	for rows.Next() {
		var route platform.Route
		var environmentID platform.EnvironmentID
		var rawInstances []byte
		if err := rows.Scan(&route.Domain, &environmentID, &rawInstances); err != nil {
			rows.Close()
			return unavailable()
		}
		if environmentID == overrideEnvironment {
			route.Upstreams = append([]string(nil), overrideUpstreams...)
		} else if len(rawInstances) > 0 {
			var instances []platform.DeploymentInstance
			if err := json.Unmarshal(rawInstances, &instances); err != nil {
				rows.Close()
				return unavailable()
			}
			route.Upstreams = platform.WebUpstreams(instances)
		}
		routes = append(routes, route)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return unavailable()
	}
	previous, err := os.ReadFile(platform.CaddyRoutesPath)
	if err != nil {
		return contract.NewFault(contract.PreconditionFailed, "Caddy is not set up on the main node ("+err.Error()+")", "Run eregion server init with a public hostname.")
	}
	if err := os.WriteFile(platform.CaddyRoutesPath, platform.RenderRoutes(routes), 0o644); err != nil { //nolint:gosec // G306: Caddy reads it.
		return contract.NewFault(contract.PreconditionFailed, "could not write the Caddy routes", err.Error())
	}
	if err := loadCaddyfile(ctx); err != nil {
		_ = os.WriteFile(platform.CaddyRoutesPath, previous, 0o644) //nolint:gosec // G306: Caddy reads it.
		_ = loadCaddyfile(context.WithoutCancel(ctx))
		return err
	}
	return nil
}

// loadCaddyfile posts the Caddyfile to Caddy's admin API, which adapts it
// and resolves its imports itself.
func loadCaddyfile(ctx context.Context) error {
	raw, err := os.ReadFile(platform.CaddyfilePath)
	if err != nil {
		return contract.NewFault(contract.PreconditionFailed, "could not read "+platform.CaddyfilePath, err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, platform.CaddyAdmin+"/load", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return contract.NewFault(contract.Unavailable, "Caddy's admin endpoint does not answer", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return contract.NewFault(contract.PreconditionFailed, fmt.Sprintf("Caddy rejected the routes (%d)", resp.StatusCode), strings.TrimSpace(string(body)))
	}
	return nil
}

// verifyPublicRoute requests the primary domain through local Caddy. The URL
// names the real host so Caddy selects the site and presents the certificate,
// while the dial always goes to loopback. The route must answer 2xx/3xx.
func (s *Service) verifyPublicRoute(ctx context.Context, env platform.Environment, healthPath string) error {
	domain := env.PrimaryDomain
	if domain == "" {
		return contract.NewFault(contract.PreconditionFailed, "environment has no primary domain", "Add a primary domain before deploying.")
	}
	if healthPath == "" {
		healthPath = "/"
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: 3 * time.Second}
				return dialer.DialContext(ctx, "tcp", platform.CaddyHTTPS)
			},
			// This dials local Caddy, so it proves the route selected the right
			// site and returned a response. Public certificate validity is a
			// separate observation.
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: domain, InsecureSkipVerify: true}, //nolint:gosec // Loopback-only route verification; the URL host still selects the Caddy site.
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.Now().Add(30 * time.Second)
	last := "no response"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+healthPath, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return contract.NewFault(contract.PreconditionFailed, "the public route did not return a successful response", "Check DNS, the Caddy route, and the web process health; last result: "+last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
