package workspaceui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// MetricsSource is the small data-source boundary for terminal graphs. The
// telemetry node implementation lives outside this package; the workspace only
// accepts this interface and never imports a telemetry client directly. A nil
// source is the honest "no telemetry node configured" state.
type MetricsSource interface {
	Query(ctx context.Context, query MetricsQuery) (MetricsSeries, error)
}

// MetricsQuery identifies the observed subject, metric, and window. It carries
// no credential; the source owns authentication to the telemetry node.
type MetricsQuery struct {
	Node        string
	App         string
	Environment string
	Metric      string
	Window      time.Duration
}

// MetricsSample is one timestamped value. Values are in the series unit.
type MetricsSample struct {
	At    time.Time
	Value float64
}

// MetricsSeries is a bounded observation with its provenance and freshness.
// The presentation shows Source, Unit, and Window so a graph never hides where
// its data came from.
type MetricsSeries struct {
	Source     string
	Metric     string
	Unit       string
	Window     time.Duration
	ObservedAt time.Time
	Stale      bool
	Samples    []MetricsSample
}

type metricsView struct {
	key     string
	series  MetricsSeries
	errText string
}

func (m Model) metricsQuery(item Item) (MetricsQuery, bool) {
	if m.opts.Metrics == nil {
		return MetricsQuery{}, false
	}
	switch m.tab {
	case Fleet:
		if item.Data["name"] == "" {
			return MetricsQuery{}, false
		}
		return MetricsQuery{Node: item.Data["name"], Metric: "node.cpu.utilization", Window: 5 * time.Minute}, true
	case Envs:
		if item.Data["app"] == "" || item.Data["environment"] == "" {
			return MetricsQuery{}, false
		}
		return MetricsQuery{
			Node: item.Data["node"], App: item.Data["app"], Environment: item.Data["environment"],
			Metric: "http.request.duration.p95", Window: 5 * time.Minute,
		}, true
	default:
		return MetricsQuery{}, false
	}
}

// metricsCommand is issued when focused details open. It never blocks Update.
func (m Model) metricsCommand(item Item) tea.Cmd {
	source := m.opts.Metrics
	query, ok := m.metricsQuery(item)
	if source == nil || !ok {
		return nil
	}
	key := item.Key
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		series, err := source.Query(ctx, query)
		return metricsMsg{key: key, series: series, err: err}
	}
}

func (m *Model) requestMetrics(item Item) tea.Cmd {
	if _, ok := m.metricsQuery(item); !ok {
		m.metrics = nil
		m.metricsLoading = false
		return nil
	}
	m.metricsLoading = true
	m.metrics = nil
	return m.metricsCommand(item)
}

func (m Model) metricsText(width int) []string {
	if m.opts.Metrics == nil {
		return []string{m.dim().Render("Telemetry: no telemetry node is configured. No metric samples are available; operation history is still available.")}
	}
	if m.metricsLoading || m.metrics == nil {
		return []string{m.dim().Render("Telemetry: waiting for the configured data source.")}
	}
	view := m.metrics
	if view.errText != "" {
		return []string{m.warnStyle().Render("Telemetry unavailable: " + view.errText)}
	}
	series := view.series
	if len(series.Samples) == 0 {
		source := series.Source
		if source == "" {
			source = "configured data source"
		}
		return []string{m.dim().Render("Telemetry: " + source + " returned no samples for this window; no graph is drawn.")}
	}
	unit := series.Unit
	if unit == "" {
		unit = "unit not reported"
	}
	source := series.Source
	if source == "" {
		source = "source not reported"
	}
	window := series.Window
	if window <= 0 {
		window = 5 * time.Minute
	}
	metric := series.Metric
	if metric == "" {
		metric = "metric"
	}
	last := series.Samples[len(series.Samples)-1]
	freshness := "freshness unknown"
	if !series.ObservedAt.IsZero() {
		age := m.now.Sub(series.ObservedAt)
		if age < 0 {
			age = 0
		}
		freshness = "last sample " + shortDuration(age) + " ago"
		if series.Stale {
			freshness = "stale · " + freshness
		}
	}
	lines := []string{
		m.infoStyle().Render(sparkline(series.Samples, m.opts.ASCII, width)),
		m.dim().Render(fmt.Sprintf("%s · %.4g %s · %s", metric, last.Value, unit, source)),
		m.dim().Render(fmt.Sprintf("%s window · %s · %d samples", shortDuration(window), freshness, len(series.Samples))),
	}
	return lines
}

// sparkline renders measured samples as block (or ASCII) glyphs. It does not
// smooth, extrapolate, or label an unavailable metric as zero.
func sparkline(samples []MetricsSample, ascii bool, width int) string {
	if len(samples) == 0 {
		return ""
	}
	if width > 0 && len(samples) > width {
		samples = samples[len(samples)-width:]
	}
	minimum, maximum := samples[0].Value, samples[0].Value
	for _, sample := range samples {
		minimum = math.Min(minimum, sample.Value)
		maximum = math.Max(maximum, sample.Value)
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	if ascii {
		blocks = []rune(".:-=+*#@")
	}
	var out strings.Builder
	for _, sample := range samples {
		index := 0
		if maximum > minimum {
			index = int(math.Round((sample.Value - minimum) / (maximum - minimum) * float64(len(blocks)-1)))
		}
		index = max(0, min(len(blocks)-1, index))
		out.WriteRune(blocks[index])
	}
	return out.String()
}
