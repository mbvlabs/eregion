package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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
		kind, summary := "domain.added", "Saved the binding for "+input.Domain+" on "+input.App+"/"+input.Environment+"; route and observations are reported separately."
		if normalized.Kind == platform.KindDomainRemove {
			kind, summary = "domain.removed", "Removed the binding and route for "+input.Domain
		}
		return insertPlatformChange(ctx, tx, op, kind, input.App, input.Environment, summary, map[string]any{"domain": input.Domain}, nil)
	})
}

// caddyRoutesLockKey serializes every writer of the one routes file, including
// domain changes and deployment upstream switches. It is a PostgreSQL advisory
// lock, not a per-process mutex, so API and worker processes cooperate.
const caddyRoutesLockKey int64 = 0x6361646479726f75 // "caddyrou"

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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable()
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, caddyRoutesLockKey); err != nil {
		return unavailable()
	}
	if overrideEnvironment != "" {
		// A deployment switch changes this environment's desired upstreams, so
		// its domains get a new route revision before the write is attempted.
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains SET desired_revision=desired_revision+1 WHERE environment_id=$1`, overrideEnvironment); err != nil {
			return unavailable()
		}
	}
	rows, err := tx.Query(ctx, desiredRoutesQuery, deploymentRouteStep, deploymentPublicStep, deploymentRestoreRouteStep)
	if err != nil {
		return unavailable()
	}
	var routes []platform.Route
	var routed, unrouted []string
	for rows.Next() {
		var route platform.Route
		var environmentID platform.EnvironmentID
		var rawInstances []byte
		if err := rows.Scan(&route.Domain, &environmentID, &route.Primary, &rawInstances); err != nil {
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
		if len(route.Upstreams) == 0 {
			unrouted = append(unrouted, route.Domain)
		} else {
			routed = append(routed, route.Domain)
		}
		routes = append(routes, route)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return unavailable()
	}
	content := platform.RenderRoutes(routes)
	now := time.Now().UTC()
	previous, err := os.ReadFile(platform.CaddyRoutesPath)
	if err != nil {
		fault := contract.NewFault(contract.PreconditionFailed, "Caddy is not set up on the main node ("+err.Error()+")", "Run eregion server init with a public hostname.")
		return s.finishRouteWrite(ctx, tx, routed, unrouted, fault, now)
	}
	if len(routed) > 0 {
		if err := validateCaddyRoutes(ctx, content); err != nil {
			return s.finishRouteWrite(ctx, tx, routed, unrouted, err, now)
		}
	}
	if !bytes.Equal(previous, content) {
		if err := platform.WriteFileAtomic(platform.CaddyRoutesPath, content, 0o644); err != nil {
			fault := contract.NewFault(contract.PreconditionFailed, "could not write the Caddy routes", err.Error())
			return s.finishRouteWrite(ctx, tx, routed, unrouted, fault, now)
		}
		if err := loadCaddyfile(ctx); err != nil {
			restoreErr := platform.WriteFileAtomic(platform.CaddyRoutesPath, previous, 0o644)
			_ = loadCaddyfile(context.WithoutCancel(ctx))
			if restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore previous routes: %w", restoreErr))
			}
			return s.finishRouteWrite(ctx, tx, routed, unrouted, err, now)
		}
	}
	return s.finishRouteWrite(ctx, tx, routed, unrouted, nil, now)
}

// desiredRoutesQuery is shared by every route writer. Between the candidate
// switch and recordServing, the serving pointer still names the previous
// deployment. Rendering only that pointer would undo an in-flight switch (and
// remove a first deploy's site) during public verification. The step records
// and saved instances identify the active routing target without declaring it
// verified/serving. Failure, cancellation or restoration selects serving again.
// Include a running switch: its file write and step acknowledgement are not one
// transaction, and another writer can run between them.
const desiredRoutesQuery = `SELECT d.name,e.id,d.is_primary,
        coalesce(candidate.instances,dep.instances,'[]'::jsonb)
    FROM eregion_platform_domains d
    JOIN eregion_platform_environments e ON e.id=d.environment_id
    LEFT JOIN eregion_platform_deployments dep ON dep.id=e.serving_deployment_id
    LEFT JOIN LATERAL (
        SELECT active.instances
        FROM eregion_platform_deployments active
        JOIN eregion_operations op ON op.id=active.operation_id
        JOIN eregion_operation_steps switched ON switched.operation_id=op.id AND switched.step=$1
        WHERE active.environment_id=e.id AND active.state='running'
          AND op.state='running' AND switched.state IN ('running','completed')
          AND NOT EXISTS (
              SELECT 1 FROM eregion_operation_steps failed
              WHERE failed.operation_id=op.id AND failed.step=$2 AND failed.state='failed')
          AND NOT EXISTS (
              SELECT 1 FROM eregion_operation_steps restoring
              WHERE restoring.operation_id=op.id AND restoring.step=$3)
        ORDER BY active.created_at DESC,active.id DESC LIMIT 1
    ) candidate ON true
    ORDER BY d.name`

// finishRouteWrite records the applied or failed route attempt and commits the
// lock transaction. The file and the rows never claim more than the reload did.
func (s *Service) finishRouteWrite(ctx context.Context, tx pgx.Tx, routed, unrouted []string, routeErr error, now time.Time) error {
	if len(routed) > 0 {
		if routeErr == nil {
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains
                SET route_state='applied',route_error='',applied_revision=desired_revision,
                    route_applied_at=coalesce(route_applied_at,$2),route_observed_at=$2,observed_at=$2
                WHERE name=ANY($1)`, routed, now.UTC()); err != nil {
				return unavailable()
			}
		} else if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains
            SET route_state='failed',route_error=$2,route_observed_at=$3,observed_at=$3
            WHERE name=ANY($1)`, routed, boundedRouteError(routeErr.Error()), now.UTC()); err != nil {
			return unavailable()
		}
	}
	if len(unrouted) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains
            SET route_state='pending',route_error=$2,route_observed_at=$3,observed_at=$3
            WHERE name=ANY($1)`, unrouted, "no serving web upstream yet", now.UTC()); err != nil {
			return unavailable()
		}
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		if routeErr != nil {
			return errors.Join(routeErr, commitErr)
		}
		return commitErr
	}
	return routeErr
}

func boundedRouteError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}

// validateCaddyRoutes adapts the new routes fragment before it replaces the
// file. Caddy's /adapt endpoint reports syntax errors without touching the
// running configuration.
func validateCaddyRoutes(ctx context.Context, content []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, platform.CaddyAdmin+"/adapt", bytes.NewReader(content))
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
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
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

// primaryCheckPath is the primary binding's observation path: its own
// --check-path, else the deployment's pinned revision health path (D29). The
// worker already loaded the pinned revision into env; never read the current
// revision here, which may be newer than the deployment being verified.
func (s *Service) primaryCheckPath(ctx context.Context, env platform.Environment) string {
	var explicit string
	err := s.pool.QueryRow(ctx, `SELECT coalesce(d.check_path,'')
        FROM eregion_platform_domains d
        WHERE d.environment_id=$1 AND d.name=$2 AND d.is_primary
        LIMIT 1`, env.ID, env.PrimaryDomain).Scan(&explicit)
	if err != nil {
		explicit = ""
	}
	return publicCheckPath(explicit, env)
}

// publicCheckPath is the pure path choice: explicit binding path, then the
// deployment's pinned revision health path, then the legacy environment path.
func publicCheckPath(explicit string, env platform.Environment) string {
	if explicit != "" {
		return explicit
	}
	if env.Revision != nil && env.Revision.Runtime.HealthPath != "" {
		return env.Revision.Runtime.HealthPath
	}
	if env.Health.Path != "" {
		return env.Health.Path
	}
	return "/"
}

func publicCheckMessage(host, path string, status int, probeErr error) string {
	target := "https://" + host + path
	if probeErr != nil {
		return "public check " + target + " → error: " + probeErr.Error()
	}
	return fmt.Sprintf("public check %s → %d", target, status)
}

// publicCheckWindow bounds public verification after the Caddy switch, as in
// deploycrate. A new domain's site first appears in Caddy at that switch, so
// the window must cover Caddy obtaining its first certificate; ACME issuance
// routinely takes longer than a web health timeout.
const publicCheckWindow = 2 * time.Minute

// verifyPublicRoute requests the primary domain through local Caddy. The URL
// names the real host so Caddy selects the site and presents the certificate,
// while the dial always goes to loopback. The route must answer 2xx/3xx.
func (s *Service) verifyPublicRoute(ctx context.Context, id contract.OperationID, attempt uint32, env platform.Environment, healthPath string) error {
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
	deadline := time.Now().Add(publicCheckWindow)
	last := "no response"
	lastEvent := ""
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+healthPath, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			message := publicCheckMessage(domain, healthPath, resp.StatusCode, nil)
			if message != lastEvent {
				s.appendWorkerEvent(ctx, id, attempt, deploymentPublicStep, message)
				lastEvent = message
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
			message := publicCheckMessage(domain, healthPath, 0, err)
			if message != lastEvent {
				s.appendWorkerEvent(ctx, id, attempt, deploymentPublicStep, message)
				lastEvent = message
			}
		}
		if time.Now().After(deadline) {
			message := "public check https://" + domain + healthPath + " → " + last
			if message != lastEvent {
				s.appendWorkerEvent(ctx, id, attempt, deploymentPublicStep, message)
			}
			return contract.NewFault(contract.PreconditionFailed, "the public route did not return a successful response", "Check DNS, the Caddy route, and the web process health; last result: "+last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
