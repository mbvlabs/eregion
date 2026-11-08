package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/spf13/cobra"
)

// nodeMetricsClient is the small read boundary for node-local metrics. It is
// optional on Backend so an injected test backend can serve only the commands
// it exercises; a real saved context always returns *nodeClient.
type nodeMetricsClient interface {
	NodeMetrics(context.Context) (telemetry.NodeSnapshot, error)
}

func (a *app) metricsSpecs() []spec {
	parentMeta := operationMeta("Read a current node-local host observation",
		"eregion metrics show")
	parentMeta.WhenToUse = []string{
		"Read CPU, memory, filesystem, load and uptime from the node's WireGuard-bound node_exporter through SSH.",
		"Use --json for the same snapshot in a structured envelope.",
		"There is no metrics history, PromQL, telemetry server or database: this is one current sample collected for this command.",
	}
	parentMeta.Next = []string{"eregion doctor", "eregion server status --json"}

	showMeta := operationMeta("Show the current node_exporter snapshot over the operator channel",
		"eregion metrics show --json")
	showMeta.WhenToUse = []string{
		"Read one current CPU, memory, filesystem, load and uptime snapshot from the node's local node_exporter.",
		"The exporter is bound to the WireGuard address; the operator command reaches it through SSH in WireGuard. No public endpoint, token or separate scraper identity exists.",
		"Do not use this as a time-series or historical query. No samples are stored server-side and no external backend is read.",
	}
	showMeta.Examples = []string{"eregion metrics show", "eregion metrics show --json"}
	showMeta.Next = []string{"eregion doctor", "eregion server status --json"}
	showMeta.OutputModes = []string{"plain", "json"}

	return []spec{{
		use:  "metrics",
		meta: parentMeta,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{{
			use:  "show",
			meta: showMeta,
			run:  a.metricsShow,
		}},
	}}
}

func (a *app) metricsShow(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	metrics, ok := client.(nodeMetricsClient)
	if !ok {
		return result{}, contract.NewFault(contract.Internal, "this client cannot read node metrics", "Use a saved context over SSH inside WireGuard, not an injected client.")
	}
	snapshot, err := metrics.NodeMetrics(cmd.Context())
	if err != nil {
		return result{}, err
	}
	return result{data: snapshot, plain: nodeMetricsText(snapshot)}, nil
}

// NodeMetrics is the read-only node RPC used by `metrics show` and the
// workspace's Fleet detail. It is deliberately independent of PostgreSQL.
func (n *nodeClient) NodeMetrics(ctx context.Context) (telemetry.NodeSnapshot, error) {
	raw, err := n.call(ctx, methodMetricsShow, struct{}{}, rpcCallOptions{})
	if err != nil {
		return telemetry.NodeSnapshot{}, err
	}
	var snapshot telemetry.NodeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Validate() != nil {
		return telemetry.NodeSnapshot{}, invalidNodeData("node metrics")
	}
	return snapshot, nil
}

// collectNodeMetrics is the node-side implementation. It scrapes the local
// exporter twice, computes the CPU counter delta and never opens a database.
func collectNodeMetrics(ctx context.Context) (telemetry.NodeSnapshot, error) {
	info := telemetry.ScrapeInfo{
		Endpoint:       host.NodeExporterMetricsURL(),
		Listen:         host.NodeExporterListenAddress(),
		Access:         "wireguard",
		Public:         false,
		Authentication: "none",
		Collection:     "operator_node_rpc",
		Note:           "Collected by this operator command over SSH inside WireGuard; the exporter has no public endpoint and no independent scraper identity.",
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snapshot, err := telemetry.CollectNodeExporter(ctx, telemetry.NodeExporterConfig{
		Endpoint:       info.Endpoint,
		Scrape:         info,
		Client:         nodeExporterHTTPClient(),
		SampleInterval: time.Second,
	})
	if err != nil {
		return telemetry.NodeSnapshot{}, err
	}
	if config, err := host.LoadNodeFile(host.DefaultNodeConfigPath()); err == nil {
		if strings.TrimSpace(config.Node) != "" {
			snapshot.Node = config.Node
		}
		if strings.TrimSpace(snapshot.Hostname) == "" && strings.TrimSpace(config.Hostname) != "" {
			snapshot.Hostname = config.Hostname
		}
	}
	if strings.TrimSpace(snapshot.Node) == "" {
		snapshot.Node = "local"
	}
	return snapshot, nil
}

func nodeExporterHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func nodeMetricsText(snapshot telemetry.NodeSnapshot) string {
	var out strings.Builder
	out.WriteString(ui.Header("Node metrics "+snapshot.Node, 72))
	field := func(name, value string) {
		fmt.Fprintf(&out, "\n  %s %s", ui.Dim(fmt.Sprintf("%-10s", name)), value)
	}
	if snapshot.Hostname != "" {
		field("host", snapshot.Hostname)
	}
	platform := strings.TrimSpace(strings.Join([]string{snapshot.OS, snapshot.Kernel, snapshot.Machine}, " "))
	if platform != "" {
		field("platform", platform)
	}
	version := snapshot.ExporterVersion
	if version == "" {
		version = "version unknown"
	}
	field("exporter", version+" · "+snapshot.Scrape.Endpoint)
	field("access", "WireGuard only · no public endpoint or credential · read by this operator command over SSH")
	field("sample", sampleText(snapshot))
	if snapshot.CPU == nil {
		field("cpu", "unavailable: "+issueReason(snapshot, "cpu"))
	} else if snapshot.CPU.UtilizationPercent == nil {
		field("cpu", "unavailable")
	} else {
		field("cpu", fmt.Sprintf("%.1f%% of %d logical CPUs over %.1fs of counters", *snapshot.CPU.UtilizationPercent, snapshot.CPU.LogicalCPUs, snapshot.CPU.WindowSeconds))
	}
	if snapshot.Memory == nil {
		field("memory", "unavailable: "+issueReason(snapshot, "memory"))
	} else {
		field("memory", fmt.Sprintf("%s used of %s (%.1f%%)", humanBytes(snapshot.Memory.UsedBytes), humanBytes(snapshot.Memory.TotalBytes), snapshot.Memory.UsedPercent))
	}
	if snapshot.Load == nil {
		field("load", "unavailable: "+issueReason(snapshot, "load"))
	} else {
		field("load", fmt.Sprintf("%.2f %.2f %.2f", snapshot.Load.Load1, snapshot.Load.Load5, snapshot.Load.Load15))
	}
	if snapshot.UptimeSeconds == nil {
		field("uptime", "unavailable: "+issueReason(snapshot, "uptime"))
	} else {
		field("uptime", uptimeText(int64(*snapshot.UptimeSeconds)))
	}
	if len(snapshot.Filesystems) == 0 {
		field("filesystem", "unavailable: "+issueReason(snapshot, "filesystem"))
	} else {
		for _, filesystem := range snapshot.Filesystems {
			field("filesystem", fmt.Sprintf("%s (%s) %s free of %s (%.0f%% free)", filesystem.Mountpoint, filesystem.Filesystem, humanBytes(filesystem.AvailableBytes), humanBytes(filesystem.TotalBytes), 100-filesystem.UsedPercent))
		}
	}
	if snapshot.Freshness.State != telemetry.FreshnessFresh || len(snapshot.Issues) > 0 {
		out.WriteString("\n")
		if snapshot.Freshness.State == telemetry.FreshnessStale {
			field("freshness", "stale: the newest exporter timestamp is older than the freshness threshold")
		}
		if snapshot.Freshness.State == telemetry.FreshnessUnknown {
			field("freshness", "unknown: node_time_seconds is missing")
		}
		for _, issue := range snapshot.Issues {
			fmt.Fprintf(&out, "\n  %s %s", ui.Dim(fmt.Sprintf("%-10s", "missing")), issue.Field+": "+issue.Reason)
		}
	}
	out.WriteString("\n\n")
	out.WriteString(ui.Note("This is one current snapshot; no samples are stored and there is no history or PromQL endpoint."))
	return out.String()
}

func sampleText(snapshot telemetry.NodeSnapshot) string {
	if snapshot.ObservedAt == nil {
		return "exporter time unknown · collected at " + snapshot.FetchedAt.Format(time.RFC3339)
	}
	if snapshot.Freshness.AgeSeconds == nil {
		return snapshot.ObservedAt.Format(time.RFC3339) + " · freshness unknown"
	}
	age := time.Duration(*snapshot.Freshness.AgeSeconds * float64(time.Second))
	return fmt.Sprintf("%s · %s", snapshot.ObservedAt.Format(time.RFC3339), shortSampleAge(age))
}

func shortSampleAge(age time.Duration) string {
	if age < time.Second {
		return "fresh (under 1s old)"
	}
	return "sample " + age.Truncate(100*time.Millisecond).String() + " old"
}

func issueReason(snapshot telemetry.NodeSnapshot, field string) string {
	for _, issue := range snapshot.Issues {
		if issue.Field == field {
			return issue.Reason
		}
	}
	return "the exporter did not report it"
}
