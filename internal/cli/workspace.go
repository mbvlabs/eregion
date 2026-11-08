package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/mbvlabs/eregion/internal/workspaceui"
	"github.com/spf13/cobra"
)

// workspaceSpecs returns the interactive workspace commands. They are kept in
// their own list so the command tree can attach them without pulling Charm into
// the noninteractive command path.
func (a *app) workspaceSpecs() []spec {
	meta := operationMeta("Browse and act on the platform in an interactive terminal",
		"eregion ui --tab fleet")
	meta.Audience = []string{"human"}
	meta.Output = "result"
	meta.Mutating = true
	meta.DryRunReason = "The workspace previews every action with the platform dry-run API before explicit submission; the command itself only starts the interface."
	meta.WhenToUse = []string{
		"Explore fleet, app, environment, release, and deployment inventory with freshness and focused details.",
		"Preview and submit an app, environment, image release, deploy, or rollback action; every action shows its exact noninteractive platform command equivalent.",
		"Use ':' for command discovery, '/' to filter, and the documented destructive confirmation before submission.",
		"Requires a real terminal on stdin and stdout. --json, --output json, and piped input or output are refused; the platform commands provide machine output.",
	}
	meta.Examples = []string{
		"eregion ui",
		"eregion ui --tab apps --ascii --no-color --reduced-motion",
		"eregion ui --tab deployments",
		"eregion ui --tab fleet",
	}
	return []spec{{
		use:  "ui",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("tab", "fleet", "Initial workspace: fleet, apps, envs, releases, or deployments")
			cmd.Flags().Bool("ascii", false, "Use ASCII glyphs instead of Unicode")
			cmd.Flags().Bool("no-color", false, "Disable colour (NO_COLOR is also honoured)")
			cmd.Flags().Bool("reduced-motion", false, "Replace spinners with a static loading label (EREGION_REDUCED_MOTION=1 is also honoured)")
		},
		run: a.workspaceUI,
	}}
}

func (a *app) workspaceUI(cmd *cobra.Command, _ []string) (result, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	if jsonMode || outputMode != "plain" {
		return result{}, contract.NewFault(contract.InvalidOutputMode, "the interactive workspace cannot produce machine output", "Use plain output and run eregion ui, or use the platform commands with --json.")
	}
	streams := followConfig(cmd)
	if streams == nil || !isTerminal(streams.in) || !isTerminal(streams.out) {
		return result{}, contract.NewFault(contract.Usage, "the interactive workspace requires a terminal", "Run eregion ui from a TTY; prompts and full-screen input are never used on a pipe.")
	}
	tabName, _ := cmd.Flags().GetString("tab")
	tab, err := workspaceui.ParseTab(tabName)
	if err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	workspaceAPI, err := newWorkspaceAPI(client)
	if err != nil {
		return result{}, err
	}
	ascii, _ := cmd.Flags().GetBool("ascii")
	noColor, _ := cmd.Flags().GetBool("no-color")
	reduced, _ := cmd.Flags().GetBool("reduced-motion")
	metrics := a.workspaceMetricsSource(client)
	summary, err := workspaceui.Run(cmd.Context(), workspaceui.Options{
		API:           workspaceAPI,
		Metrics:       metrics,
		Host:          workspaceHost(cmd),
		Secrets:       a.secrets,
		ASCII:         ascii || strings.EqualFold(os.Getenv("TERM"), "dumb"),
		NoColor:       noColor || os.Getenv("NO_COLOR") != "",
		ReducedMotion: reduced || os.Getenv("EREGION_REDUCED_MOTION") == "1",
		InitialTab:    tab,
		In:            streams.in,
		Out:           streams.out,
	})
	if err != nil {
		return result{}, err
	}
	operations := make([]string, 0, len(summary.Operations))
	for _, id := range summary.Operations {
		operations = append(operations, string(id))
	}
	var plain strings.Builder
	fmt.Fprintf(&plain, "Detached from the workspace UI (%s).\n", summary.Tab)
	if len(operations) == 0 {
		plain.WriteString("No operations were submitted. Inventory reads and any earlier operations are unchanged.\n")
	} else {
		plain.WriteString("Submitted operations continue without this interface:\n")
		for _, id := range operations {
			fmt.Fprintf(&plain, "  %s\n", id)
		}
		fmt.Fprintf(&plain, "\nResume follow: eregion operations follow %s --interactive\n", operations[len(operations)-1])
		fmt.Fprintf(&plain, "Read once:     eregion operations get %s --json\n", operations[len(operations)-1])
	}
	plain.WriteString("Detaching never cancels an operation. Noninteractive equivalents are shown before every submission.")
	return result{
		data: map[string]any{
			"detached":   summary.Detached,
			"tab":        summary.Tab,
			"operations": operations,
		},
		plain: plain.String(),
	}, nil
}

func workspaceHost(cmd *cobra.Command) string {
	current, err := selectedContext(cmd)
	if err != nil || current.SSH == nil {
		return ""
	}
	return current.SSH.User + "@" + current.SSH.Address
}

type workspaceLiveAPI struct {
	client Backend
}

func newWorkspaceAPI(client Backend) (workspaceui.API, error) {
	return workspaceLiveAPI{client: client}, nil
}

func (w workspaceLiveAPI) PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error) {
	return w.client.PlatformInventory(ctx, collection, filters)
}

func (w workspaceLiveAPI) SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, idempotencyKey, confirm string, dryRun bool) (json.RawMessage, error) {
	return w.client.SubmitAction(ctx, kind, target, input, idempotencyKey, confirm, dryRun)
}

func (w workspaceLiveAPI) Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	return w.client.Get(ctx, id)
}

func (w workspaceLiveAPI) Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	return w.client.Events(ctx, cursor, limit)
}

// workspaceMetricsSource attaches the current per-node snapshot to the Fleet
// detail. A backend without the node metrics method (an injected test client)
// gets no source, and the UI then says so instead of drawing a graph.
func (a *app) workspaceMetricsSource(client Backend) workspaceui.MetricsSource {
	metrics, ok := client.(nodeMetricsClient)
	if !ok {
		return nil
	}
	return workspaceNodeMetrics{client: metrics}
}

type workspaceNodeMetrics struct {
	client nodeMetricsClient
}

func (w workspaceNodeMetrics) Query(ctx context.Context, query workspaceui.MetricsQuery) (workspaceui.MetricsSeries, error) {
	snapshot, err := w.client.NodeMetrics(ctx)
	if err != nil {
		return workspaceui.MetricsSeries{}, err
	}
	if node := strings.TrimSpace(query.Node); node != "" && node != snapshot.Node {
		return workspaceui.MetricsSeries{}, contract.NewFault(contract.Unavailable,
			"node metrics are unavailable for node "+node,
			"The current source observes "+snapshot.Node+"; select that node instead of relabeling another node's sample.")
	}
	freshness := snapshot.Freshness.State
	if freshness == "" {
		freshness = telemetry.FreshnessUnknown
	}
	series := workspaceui.MetricsSeries{
		Source: "node_exporter on " + snapshot.Node,
		Metric: query.Metric, Unit: "percent",
		Freshness:  freshness,
		ReceivedAt: time.Now(),
		StaleAfter: time.Duration(snapshot.Freshness.ThresholdSeconds * float64(time.Second)),
	}
	if snapshot.Freshness.AgeSeconds != nil {
		series.AgeAtReceipt = time.Duration(*snapshot.Freshness.AgeSeconds * float64(time.Second))
		series.AgeKnown = true
	}
	notes := []string{"One current node_exporter sample. No history is stored."}
	if snapshot.ObservedAt != nil {
		series.ObservedAt = *snapshot.ObservedAt
	} else {
		notes = append(notes, "Exporter time is unknown; no sample timestamp is invented.")
	}
	series.Stale = freshness == telemetry.FreshnessStale
	if snapshot.CPU != nil && snapshot.CPU.UtilizationPercent != nil {
		series.Samples = []workspaceui.MetricsSample{{
			At: series.ObservedAt, Value: *snapshot.CPU.UtilizationPercent,
		}}
		series.Details = append(series.Details, workspaceui.Detail{
			Label: "CPU",
			Value: fmt.Sprintf("%.1f%% of %d logical CPUs over %.1fs of counters", *snapshot.CPU.UtilizationPercent, snapshot.CPU.LogicalCPUs, snapshot.CPU.WindowSeconds),
		})
	} else {
		notes = append(notes, "CPU utilization is unavailable: "+nodeMetricsIssue(snapshot, "cpu")+".")
	}
	if snapshot.Memory != nil {
		series.Details = append(series.Details, workspaceui.Detail{
			Label: "Memory",
			Value: fmt.Sprintf("%s used of %s (%.1f%%)", humanBytes(snapshot.Memory.UsedBytes), humanBytes(snapshot.Memory.TotalBytes), snapshot.Memory.UsedPercent),
		})
	}
	if snapshot.Load != nil {
		series.Details = append(series.Details, workspaceui.Detail{
			Label: "Load",
			Value: fmt.Sprintf("%.2f %.2f %.2f", snapshot.Load.Load1, snapshot.Load.Load5, snapshot.Load.Load15),
		})
	}
	if snapshot.UptimeSeconds != nil {
		series.Details = append(series.Details, workspaceui.Detail{Label: "Uptime", Value: uptimeText(int64(*snapshot.UptimeSeconds))})
	}
	for _, filesystem := range snapshot.Filesystems {
		series.Details = append(series.Details, workspaceui.Detail{
			Label: "Filesystem " + filesystem.Mountpoint,
			Value: fmt.Sprintf("%s free of %s (%.0f%% free)", humanBytes(filesystem.AvailableBytes), humanBytes(filesystem.TotalBytes), 100-filesystem.UsedPercent),
		})
	}
	series.Details = append(series.Details, workspaceui.Detail{
		Label: "Source",
		Value: snapshot.Scrape.Endpoint + " · WireGuard only; no public endpoint or credential",
	})
	series.Note = strings.Join(notes, " ")
	return series, nil
}

func nodeMetricsIssue(snapshot telemetry.NodeSnapshot, field string) string {
	for _, issue := range snapshot.Issues {
		if issue.Field == field {
			return issue.Reason
		}
	}
	return "the exporter did not report it"
}
