package cli

import (
	"context"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/mbvlabs/eregion/internal/workspaceui"
	"github.com/spf13/cobra"
)

func clearTelemetryEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range telemetryEnvironment {
		t.Setenv(name, "")
	}
}

type fakeMetricsSource struct {
	logs    telemetry.LogResult
	metrics telemetry.MetricResult
	query   telemetry.MetricQuery
	err     error
}

func (f *fakeMetricsSource) Logs(context.Context, telemetry.LogQuery) (telemetry.LogResult, error) {
	return f.logs, f.err
}

func (f *fakeMetricsSource) Metrics(_ context.Context, query telemetry.MetricQuery) (telemetry.MetricResult, error) {
	f.query = query
	return f.metrics, f.err
}

func TestWorkspaceMetricsAdapterMapsSeriesAndHonestStates(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	source := &fakeMetricsSource{metrics: telemetry.MetricResult{
		State: telemetry.StateOK, Source: "app-1", Unit: "percent",
		Window: telemetry.Window{Start: now.Add(-5 * time.Minute), End: now},
		Series: []telemetry.MetricSeries{{Samples: []telemetry.MetricSample{
			{Timestamp: now.Add(-2 * time.Minute), Value: 12},
			{Timestamp: now.Add(-time.Minute), Value: 18},
		}}},
	}}
	adapter := workspaceMetrics{source: source, now: func() time.Time { return now }}
	series, err := adapter.Query(context.Background(), workspaceui.MetricsQuery{Node: "app-1", Metric: "node.cpu.utilization", Window: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(series.Samples) != 2 || series.Samples[1].Value != 18 || series.Unit != "percent" || series.Source != "app-1" {
		t.Fatalf("series = %+v", series)
	}
	if series.Stale || !series.ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("freshness = %+v", series)
	}
	if source.query.Expression == "" || source.query.Unit != "percent" || source.query.Source != "app-1" {
		t.Fatalf("telemetry query = %+v", source.query)
	}
	if !source.query.Window.End.Equal(now) || !source.query.Window.Start.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("window = %+v", source.query.Window)
	}

	source.metrics = telemetry.MetricResult{State: telemetry.StateAbsent, Source: "app-1", Unit: "percent"}
	series, err = adapter.Query(context.Background(), workspaceui.MetricsQuery{Node: "app-1", Metric: "node.cpu.utilization", Window: 5 * time.Minute})
	if err != nil || len(series.Samples) != 0 {
		t.Fatalf("absent series = %+v, %v", series, err)
	}

	source.metrics = telemetry.MetricResult{
		State: telemetry.StateStale, Source: "app-1", Unit: "percent",
		Series: []telemetry.MetricSeries{{Samples: []telemetry.MetricSample{{Timestamp: now.Add(-time.Hour), Value: 1}}}},
	}
	series, err = adapter.Query(context.Background(), workspaceui.MetricsQuery{Node: "app-1", Metric: "node.cpu.utilization", Window: 5 * time.Minute})
	if err != nil || !series.Stale {
		t.Fatalf("stale series = %+v, %v", series, err)
	}
}

func TestWorkspaceMetricsSourceIsNilWithoutConfiguration(t *testing.T) {
	clearTelemetryEnvironment(t)
	app := newApp()
	cmd := &cobra.Command{}
	addTelemetrySourceFlags(cmd)
	source, err := app.workspaceMetricsSource(cmd)
	if err != nil || source != nil {
		t.Fatalf("unconfigured workspace metrics = %+v, %v", source, err)
	}

	t.Setenv("EREGION_TELEMETRY_METRICS_URL", "http://127.0.0.1:8427")
	t.Setenv("EREGION_TELEMETRY_LOGS_URL", "http://127.0.0.1:9427")
	source, err = app.workspaceMetricsSource(cmd)
	if err != nil || source == nil {
		t.Fatalf("configured workspace metrics = %+v, %v", source, err)
	}
}
