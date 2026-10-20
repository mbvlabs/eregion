package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
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
		"eregion ui --interactive --tab fleet")
	meta.Audience = []string{"human"}
	meta.Output = "result"
	meta.Mutating = true
	meta.DryRunReason = "The workspace previews every action with the platform dry-run API before explicit submission; the command itself only starts the interface."
	meta.WhenToUse = []string{
		"Explore fleet, app, environment, release, deployment, resource, and backup inventory with freshness and focused details.",
		"Preview and submit an app, environment, image release, deploy, rollback, node check, resource destruction, or backup action; every action shows its noninteractive platform submit equivalent.",
		"Use ':' for command discovery, '/' to filter, and the documented destructive confirmation before submission.",
		"Requires --interactive and a real terminal on stdin and stdout. Machine output never starts the interface.",
	}
	meta.Examples = []string{
		"eregion ui --interactive",
		"eregion ui --interactive --tab apps --ascii --no-color --reduced-motion",
		"eregion ui --interactive --tab deployments",
		"eregion ui --interactive --tab fleet --telemetry-url http://10.77.0.10",
	}
	meta.Environment = append(meta.Environment, telemetryEnvironment...)
	return []spec{{
		use:  "ui",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().Bool("interactive", false, "Open the workspace in this terminal; required")
			cmd.Flags().String("tab", "fleet", "Initial workspace: fleet, apps, envs, releases, deployments, resources, or backups")
			cmd.Flags().String("api-url", "", "API origin URL (defaults to EREGION_API_URL or loopback)")
			cmd.Flags().Bool("ascii", false, "Use ASCII glyphs instead of Unicode")
			cmd.Flags().Bool("no-color", false, "Disable colour (NO_COLOR is also honoured)")
			cmd.Flags().Bool("reduced-motion", false, "Replace spinners with a static loading label (EREGION_REDUCED_MOTION=1 is also honoured)")
			addTelemetrySourceFlags(cmd)
		},
		run: a.workspaceUI,
	}}
}

func (a *app) workspaceUI(cmd *cobra.Command, _ []string) (result, error) {
	interactive, _ := cmd.Flags().GetBool("interactive")
	if !interactive {
		return result{}, contract.NewFault(contract.Usage, "the workspace must be requested explicitly", "Run eregion ui --interactive on a terminal. Noninteractive clients use the platform submit commands.")
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	if jsonMode || outputMode != "plain" {
		return result{}, contract.NewFault(contract.InvalidOutputMode, "the interactive workspace cannot produce machine output", "Use plain output and run eregion ui --interactive, or use the platform submit commands with --json.")
	}
	streams := followConfig(cmd)
	if streams == nil || !isTerminal(streams.in) || !isTerminal(streams.out) {
		return result{}, contract.NewFault(contract.Usage, "the interactive workspace requires a terminal", "Run eregion ui --interactive from a TTY; prompts and full-screen input are never used on a pipe.")
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
	metrics, err := a.workspaceMetricsSource(cmd)
	if err != nil {
		return result{}, err
	}
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
	endpoint, _ := cmd.Flags().GetString("api-url")
	if endpoint == "" {
		endpoint = os.Getenv("EREGION_API_URL")
	}
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

// workspacePlatformClient is the platform surface documented in the platform
// contract. Asserting it at runtime keeps the workspace command compilable
// against older API clients while refusing to invent a second HTTP path.
type workspacePlatformClient interface {
	PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error)
	SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool) (json.RawMessage, error)
}

type workspaceLiveAPI struct {
	client   *api.Client
	platform workspacePlatformClient
}

func newWorkspaceAPI(client *api.Client) (workspaceui.API, error) {
	platform, ok := any(client).(workspacePlatformClient)
	if !ok {
		return nil, contract.NewFault(contract.Internal, "this API client has no platform surface", "Upgrade the binary; the interactive workspace never bypasses api.Client.")
	}
	return workspaceLiveAPI{client: client, platform: platform}, nil
}

func (w workspaceLiveAPI) PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error) {
	return w.platform.PlatformInventory(ctx, collection, filters)
}

func (w workspaceLiveAPI) SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool) (json.RawMessage, error) {
	return w.platform.SubmitAction(ctx, kind, target, input, idempotencyKey, dryRun)
}

func (w workspaceLiveAPI) Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	return w.client.Get(ctx, id)
}

func (w workspaceLiveAPI) Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	return w.client.Events(ctx, cursor, limit)
}

// workspaceMetricsSource adapts the same telemetry read configuration used by
// `logs` and `metrics` to the workspace graph boundary. No telemetry
// configuration is the explicit nil source, so the UI renders "no telemetry
// node" instead of an empty graph.
func (a *app) workspaceMetricsSource(cmd *cobra.Command) (workspaceui.MetricsSource, error) {
	if strings.TrimSpace(flagOrEnvironment(cmd, "telemetry-url", "EREGION_TELEMETRY_URL")) == "" &&
		strings.TrimSpace(flagOrEnvironment(cmd, "logs-url", "EREGION_TELEMETRY_LOGS_URL")) == "" &&
		strings.TrimSpace(flagOrEnvironment(cmd, "metrics-url", "EREGION_TELEMETRY_METRICS_URL")) == "" {
		return nil, nil
	}
	source, err := a.telemetryDataSource(cmd)
	if err != nil {
		return nil, err
	}
	return workspaceMetrics{source: source, now: time.Now}, nil
}

type workspaceMetrics struct {
	source telemetry.DataSource
	now    func() time.Time
}

func (w workspaceMetrics) Query(ctx context.Context, query workspaceui.MetricsQuery) (workspaceui.MetricsSeries, error) {
	window := query.Window
	if window <= 0 {
		window = 5 * time.Minute
	}
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	end := now().UTC()
	expression, unit := workspaceMetricExpression(query.Metric)
	source := query.App
	if source == "" {
		source = query.Node
	}
	if source == "" {
		source = "workspace"
	}
	result, err := w.source.Metrics(ctx, telemetry.MetricQuery{
		Window: telemetry.Window{Start: end.Add(-window), End: end},
		Source: source, Expression: expression, Unit: unit,
	})
	if err != nil {
		return workspaceui.MetricsSeries{}, err
	}
	if result.State == telemetry.StateAbsent {
		return workspaceui.MetricsSeries{Source: result.Source, Metric: query.Metric, Unit: result.Unit, Window: window}, nil
	}
	if result.State == telemetry.StateError {
		return workspaceui.MetricsSeries{}, contract.NewFault(contract.Unavailable, "telemetry metrics query failed", result.Message)
	}
	series := workspaceui.MetricsSeries{
		Source: result.Source, Metric: query.Metric, Unit: result.Unit, Window: window,
		ObservedAt: result.ObservedAt, Stale: result.State == telemetry.StateStale,
	}
	if series.Unit == "" {
		series.Unit = unit
	}
	if len(result.Series) > 0 {
		for _, sample := range result.Series[0].Samples {
			series.Samples = append(series.Samples, workspaceui.MetricsSample{At: sample.Timestamp, Value: sample.Value})
		}
	}
	if len(series.Samples) > 0 {
		last := series.Samples[len(series.Samples)-1].At
		if series.ObservedAt.IsZero() || last.After(series.ObservedAt) {
			series.ObservedAt = last
		}
	}
	if !series.ObservedAt.IsZero() && end.Sub(series.ObservedAt) > window {
		series.Stale = true
	}
	return series, nil
}

// workspaceMetricExpression maps the workspace's stable observation names to
// explicit PromQL. An unknown name is passed through as an expression: the
// caller can ask for a real metric without the UI inventing a unit.
func workspaceMetricExpression(metric string) (string, string) {
	switch metric {
	case "node.cpu.utilization":
		return `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))`, "percent"
	case "http.request.duration.p95":
		return `histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))`, "seconds"
	default:
		return metric, ""
	}
}
