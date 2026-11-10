package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

// checkTelemetryEndpoint is the init-side observation of D5: the loopback
// exporter is installed with its host step, and the public HTTPS discovery
// path must challenge an unauthenticated request. It observes the public URL,
// not a WireGuard-only or loopback target.
func (r *serverInit) checkTelemetryEndpoint(ctx context.Context) string {
	url, err := telemetry.PublicDiscoveryURL(r.answers.Hostname)
	if err != nil {
		r.warnings = append(r.warnings, "the telemetry endpoint is unconfigured because the node has no public hostname")
		return "unconfigured"
	}
	task := r.start("telemetry", "Checking the authenticated telemetry endpoint over public HTTPS")
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		task.end(ui.EndWarn, "")
		return "unknown"
	}
	response, err := client.Do(request)
	if err != nil {
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, "the telemetry endpoint does not answer on "+url+"; run eregion metrics rotate and check public ingress and eregion-telemetry.service on the server")
		return "unreachable"
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	switch response.StatusCode {
	case http.StatusUnauthorized:
		task.end(ui.EndOK, "authenticated endpoint on "+url)
		return "authenticated endpoint reachable on public HTTPS"
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
