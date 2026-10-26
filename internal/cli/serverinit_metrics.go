package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/host"
)

// checkNodeExporter is the init-side observation of D5: node_exporter answers
// on the node's WireGuard address and nowhere else. The host step installs and
// enables it; the access setup starts it once wg0 exists.
func (r *serverInit) checkNodeExporter(ctx context.Context, route *host.ContextSSH) string {
	if route == nil || route.WireGuard == nil {
		return "unknown"
	}
	task := r.start("node-exporter", "Checking node_exporter on the WireGuard address")
	dial, err := wireGuardDial(route.WireGuard)
	if err != nil {
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, "node_exporter could not be checked: "+err.Error())
		return "unknown"
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dial},
	}
	url := "http://" + net.JoinHostPort(access.NodeAddress, "9100") + "/metrics"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		task.end(ui.EndWarn, "")
		return "unknown"
	}
	response, err := client.Do(request)
	if err != nil {
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, "node_exporter does not answer on "+url+" over WireGuard; check node-exporter.service on the server")
		return "unreachable"
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "node_exporter_build_info") {
		task.end(ui.EndWarn, "")
		r.warnings = append(r.warnings, "node_exporter answered without its metrics on "+url)
		return "unhealthy"
	}
	task.end(ui.EndOK, url)
	return "reachable on the WireGuard address"
}
