package controlplane

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// runDomain applies a domain change: the domains table already holds it, so
// the routes file is rewritten from the table and Caddy loads it.
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

// syncRoutes rewrites the Caddy routes file from the domains table and loads
// the Caddyfile through Caddy's admin endpoint. A config Caddy rejects puts
// the previous routes file back.
func (s *Service) syncRoutes(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT d.name,e.host_port FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id ORDER BY d.name`)
	if err != nil {
		return unavailable()
	}
	var routes []platform.Route
	for rows.Next() {
		var r platform.Route
		if err := rows.Scan(&r.Domain, &r.HostPort); err != nil {
			rows.Close()
			return unavailable()
		}
		routes = append(routes, r)
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
