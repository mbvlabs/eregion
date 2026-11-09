package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/host"
)

// checkTelemetryEndpoint is the init-side observation of D5: the loopback exporter
// is installed with its host step, and the WireGuard-facing endpoint must
// challenge an unauthenticated request. The client dials through WireGuard.
func (r *serverInit) checkTelemetryEndpoint(ctx context.Context, route *host.ContextSSH) string {
	if route == nil || route.WireGuard == nil {
		return "unknown"
	}
	task := r.start("telemetry", "Checking the authenticated telemetry endpoint over WireGuard")
	dial, err := wireGuardDial(route.WireGuard)
	if err != nil {
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, "the telemetry endpoint could not be checked: "+err.Error())
		return "unknown"
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dial},
	}
	url := host.TelemetryDiscoveryURL()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		task.end(ui.EndWarn, "")
		return "unknown"
	}
	response, err := client.Do(request)
	if err != nil {
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, "the telemetry endpoint does not answer on "+url+" over WireGuard; run eregion metrics rotate and check eregion-telemetry.service on the server")
		return "unreachable"
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	switch response.StatusCode {
	case http.StatusUnauthorized:
		task.end(ui.EndOK, "authenticated endpoint on "+url)
		return "authenticated endpoint reachable on the WireGuard address"
	case http.StatusOK:
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, "the telemetry endpoint answered discovery without authentication on "+url)
		return "unauthenticated"
	default:
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, fmt.Sprintf("the telemetry endpoint answered HTTP %d on %s", response.StatusCode, url))
		return "unhealthy"
	}
}
