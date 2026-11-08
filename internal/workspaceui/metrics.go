package workspaceui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// MetricsSource is the small data-source boundary for the Fleet detail. The
// node-local implementation lives outside this package; the workspace only
// accepts this interface and never imports a telemetry client directly. A nil
// source is the honest "no node metrics source attached" state. No source keeps
// history: one current observation is returned and discarded.
type MetricsSource interface {
	Query(ctx context.Context, query MetricsQuery) (MetricsSeries, error)
}

// MetricsQuery identifies the observed subject, metric, and window. It carries
// no credential; the source owns the SSH/WireGuard read.
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
// its data came from. Details carry current non-graph facts (memory, load,
// uptime); Note states a limitation or why a value is unavailable.
type MetricsSeries struct {
	Source     string
	Metric     string
	Unit       string
	Window     time.Duration
	ObservedAt time.Time
	// Freshness is the source's explicit "fresh", "stale" or "unknown"
	// state. It is independent of whether CPU or another metric is available.
	Freshness string
	Stale     bool
	// AgeAtReceipt and StaleAfter come from the node's snapshot. When
	// ReceivedAt is set, presentation adds monotonic local elapsed time instead
	// of subtracting the node's wall clock from the operator's wall clock.
	AgeAtReceipt time.Duration
	AgeKnown     bool
	StaleAfter   time.Duration
	ReceivedAt   time.Time
	Samples      []MetricsSample
	Details      []Detail
	Note         string
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
	// Only the Fleet node detail has a real local source: node_exporter's
	// current host counters. There is no application request history, so the
	// workspace must not invent an Envs graph or a global PromQL query.
	if m.tab != Fleet {
		return MetricsQuery{}, false
	}
	if item.Data["name"] == "" {
		return MetricsQuery{}, false
	}
	return MetricsQuery{Node: item.Data["name"], Metric: "node.cpu.utilization"}, true
}

// metricsCommand is issued when focused details open. It never blocks Update.
// The captured generation fences an older response from overwriting a newer
// one for the same item.
func (m Model) metricsCommand(item Item) tea.Cmd {
	source := m.opts.Metrics
	query, ok := m.metricsQuery(item)
	if source == nil || !ok {
		return nil
	}
	key, generation := item.Key, m.metricsGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		series, err := source.Query(ctx, query)
		return metricsMsg{key: key, generation: generation, series: series, err: err}
	}
}

func (m *Model) requestMetrics(item Item) tea.Cmd {
	m.metricsGeneration++
	if _, ok := m.metricsQuery(item); !ok {
		m.metrics = nil
		m.metricsLoading = false
		return nil
	}
	m.metricsLoading = true
	m.metrics = nil
	return m.metricsCommand(item)
}

// clearMetrics invalidates any in-flight metrics response as well as the
// currently displayed view.
func (m *Model) clearMetrics() {
	m.metricsGeneration++
	m.metrics = nil
	m.metricsLoading = false
}

func (m Model) metricsText(width int) []string {
	if m.opts.Metrics == nil {
		return []string{m.dim().Render("Node metrics: no node metrics source is attached to this view. Operation history is still available.")}
	}
	if m.metricsLoading || m.metrics == nil {
		return []string{m.dim().Render("Node metrics: waiting for the current node_exporter sample.")}
	}
	view := m.metrics
	if view.errText != "" {
		return []string{m.warnStyle().Render("Node metrics unavailable: " + view.errText)}
	}
	series := view.series
	if len(series.Samples) == 0 {
		source := series.Source
		if source == "" {
			source = "the node metrics source"
		}
		reason := source + " returned no samples for this node; no graph is drawn."
		if series.Note != "" {
			reason = series.Note
		}
		return m.metricLines(series, []string{
			m.dim().Render("Node metrics: " + m.clean(reason)),
			m.dim().Render("freshness: " + m.metricsFreshnessText(series)),
		})
	}
	unit := series.Unit
	if unit == "" {
		unit = "unit not reported"
	}
	source := series.Source
	if source == "" {
		source = "source not reported"
	}
	metric := series.Metric
	if metric == "" {
		metric = "metric"
	}
	last := series.Samples[len(series.Samples)-1]
	freshness := m.metricsFreshnessText(series)
	var lines []string
	if len(series.Samples) == 1 {
		lines = append(lines, m.infoStyle().Render(fmt.Sprintf("Current %s: %.4g %s", metric, last.Value, unit)))
	} else {
		lines = append(lines, m.infoStyle().Render(sparkline(series.Samples, m.opts.ASCII, width)))
	}
	lines = append(lines, m.dim().Render(fmt.Sprintf("%s · %.4g %s · %s", metric, last.Value, unit, source)))
	if series.Window > 0 {
		lines = append(lines, m.dim().Render(fmt.Sprintf("%s window · %s · %d samples", shortDuration(series.Window), freshness, len(series.Samples))))
	} else {
		lines = append(lines, m.dim().Render(fmt.Sprintf("current sample · %s · no history retained", freshness)))
	}
	return m.metricLines(series, lines)
}

func (m Model) metricsFreshnessText(series MetricsSeries) string {
	age, ageKnown := m.metricAge(series)
	observed := ""
	if ageKnown {
		observed = "last sample " + shortDuration(age) + " ago"
	}
	effective := series.Freshness
	if effective != "unknown" && effective != "stale" {
		if ageKnown && series.StaleAfter > 0 && age > series.StaleAfter {
			effective = "stale"
		} else if effective == "" && series.Stale {
			effective = "stale"
		}
	}
	switch effective {
	case "fresh":
		if observed != "" {
			return observed
		}
		return "fresh · timestamp unavailable"
	case "stale":
		if observed != "" {
			return "stale · " + observed
		}
		return "stale · timestamp unavailable"
	case "unknown":
		return "freshness unknown"
	}
	// Sources that predate the explicit field retain the older behavior, but
	// never turn an explicit unknown into an age computed from collection time.
	if observed != "" {
		return observed
	}
	return "freshness unknown"
}

func (m Model) metricAge(series MetricsSeries) (time.Duration, bool) {
	if series.AgeKnown && !series.ReceivedAt.IsZero() {
		age := series.AgeAtReceipt + time.Since(series.ReceivedAt)
		if age < 0 {
			age = 0
		}
		return age, true
	}
	if !series.ObservedAt.IsZero() && !m.now.IsZero() {
		age := m.now.Sub(series.ObservedAt)
		if age < 0 {
			age = 0
		}
		return age, true
	}
	return 0, false
}

func (m Model) metricLines(series MetricsSeries, lines []string) []string {
	if series.Note != "" {
		lines = append(lines, m.dim().Render(m.clean(series.Note)))
	}
	for _, detail := range series.Details {
		value := m.clean(detail.Value)
		lines = append(lines, m.dim().Render(detail.Label+": ")+value)
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
