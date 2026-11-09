package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/mbvlabs/eregion/internal/workspaceui"
)

type fakeNodeMetricsBackend struct {
	Backend
	snapshot telemetry.NodeSnapshot
	err      error
	calls    int
}

func (f *fakeNodeMetricsBackend) NodeMetrics(context.Context) (telemetry.NodeSnapshot, error) {
	f.calls++
	return f.snapshot, f.err
}

func TestWorkspaceNodeMetricsMapsOneCurrentSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cpu := 12.5
	uptime := 3600.0
	age := 10.0
	backend := &fakeNodeMetricsBackend{snapshot: telemetry.NodeSnapshot{
		Schema: telemetry.NodeMetricsSchema, Node: "main", Hostname: "main.example",
		FetchedAt: now, ObservedAt: &now, Freshness: telemetry.Freshness{State: telemetry.FreshnessFresh, AgeSeconds: &age, ThresholdSeconds: 30},
		Scrape:        telemetry.ScrapeInfo{Endpoint: "http://127.0.0.1:9100/metrics", Listen: "127.0.0.1:9100", Access: "loopback"},
		CPU:           &telemetry.CPUStats{UtilizationPercent: &cpu, LogicalCPUs: 4, WindowSeconds: 1},
		Memory:        &telemetry.MemoryStats{TotalBytes: 8 << 30, AvailableBytes: 6 << 30, UsedBytes: 2 << 30, UsedPercent: 25},
		Load:          &telemetry.LoadStats{Load1: 0.5, Load5: 0.4, Load15: 0.3},
		UptimeSeconds: &uptime,
		Filesystems: []telemetry.FilesystemStats{{
			Device: "/dev/sda1", Filesystem: "ext4", Mountpoint: "/",
			TotalBytes: 100 << 30, AvailableBytes: 40 << 30, UsedBytes: 60 << 30, UsedPercent: 60,
		}},
	}}
	source := (&app{}).workspaceMetricsSource(backend)
	if source == nil {
		t.Fatal("node metrics backend produced no workspace source")
	}
	series, err := source.Query(context.Background(), workspaceui.MetricsQuery{Node: "main", Metric: "node.cpu.utilization"})
	if err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 || len(series.Samples) != 1 || series.Samples[0].Value != 12.5 {
		t.Fatalf("series = %+v calls=%d", series, backend.calls)
	}
	if !series.ObservedAt.Equal(now) || series.Stale || series.Window != 0 {
		t.Fatalf("freshness/window = %+v", series)
	}
	if !series.AgeKnown || series.AgeAtReceipt != 10*time.Second || series.StaleAfter != 30*time.Second || series.ReceivedAt.IsZero() {
		t.Fatalf("node freshness age/threshold was not carried: %+v", series)
	}
	if !strings.Contains(series.Source, "node_exporter on main") || !strings.Contains(series.Note, "No history") {
		t.Fatalf("source/note = %q / %q", series.Source, series.Note)
	}
	labels := map[string]string{}
	for _, detail := range series.Details {
		labels[detail.Label] = detail.Value
	}
	for _, want := range []string{"CPU", "Memory", "Load", "Uptime", "Filesystem /", "Source"} {
		if labels[want] == "" {
			t.Fatalf("missing detail %q in %+v", want, series.Details)
		}
	}
	if strings.Contains(series.Details[len(series.Details)-1].Value, "password") || strings.Contains(series.Details[len(series.Details)-1].Value, "token") {
		t.Fatalf("scrape details leaked a credential: %q", series.Details[len(series.Details)-1].Value)
	}
}

func TestWorkspaceNodeMetricsReportsMissingCPUWithoutInventingSamples(t *testing.T) {
	now := time.Now().UTC()
	backend := &fakeNodeMetricsBackend{snapshot: telemetry.NodeSnapshot{
		Schema: telemetry.NodeMetricsSchema, Node: "main", FetchedAt: now.Add(2 * time.Second), ObservedAt: nil,
		Freshness: telemetry.Freshness{State: telemetry.FreshnessUnknown},
		Scrape:    telemetry.ScrapeInfo{Endpoint: "http://127.0.0.1:9100/metrics", Access: "loopback"},
		Issues:    []telemetry.Issue{{Field: "cpu", Reason: "node_cpu_seconds_total is missing"}},
	}}
	source := (&app{}).workspaceMetricsSource(backend)
	series, err := source.Query(context.Background(), workspaceui.MetricsQuery{Node: "main", Metric: "node.cpu.utilization"})
	if err != nil {
		t.Fatal(err)
	}
	if len(series.Samples) != 0 {
		t.Fatalf("missing CPU produced samples: %+v", series.Samples)
	}
	if !series.ObservedAt.IsZero() {
		t.Fatalf("unknown exporter time was replaced with collection time: %v", series.ObservedAt)
	}
	if series.Freshness != telemetry.FreshnessUnknown || series.Stale {
		t.Fatalf("explicit freshness was lost: %+v", series)
	}
	if !strings.Contains(series.Note, "node_cpu_seconds_total is missing") || !strings.Contains(series.Note, "Exporter time is unknown") || !strings.Contains(series.Note, "No history") {
		t.Fatalf("note = %q", series.Note)
	}
}

func TestWorkspaceNodeMetricsRejectsAnotherNodesSample(t *testing.T) {
	now := time.Now().UTC()
	cpu := 12.5
	backend := &fakeNodeMetricsBackend{snapshot: telemetry.NodeSnapshot{
		Schema: telemetry.NodeMetricsSchema, Node: "main", FetchedAt: now, ObservedAt: &now,
		Freshness: telemetry.Freshness{State: telemetry.FreshnessFresh},
		Scrape:    telemetry.ScrapeInfo{Endpoint: "http://127.0.0.1:9100/metrics", Access: "loopback"},
		CPU:       &telemetry.CPUStats{UtilizationPercent: &cpu, LogicalCPUs: 4, WindowSeconds: 1},
	}}
	source := (&app{}).workspaceMetricsSource(backend)
	series, err := source.Query(context.Background(), workspaceui.MetricsQuery{Node: "builder-2", Metric: "node.cpu.utilization"})
	if err == nil {
		t.Fatalf("another node's sample was relabeled: %+v", series)
	}
	if len(series.Samples) != 0 || backend.calls != 1 {
		t.Fatalf("mismatch returned a fake series: %+v calls=%d", series, backend.calls)
	}
}

type plainTestBackend struct{ Backend }

func TestWorkspaceMetricsSourceIsAbsentForAPlainBackend(t *testing.T) {
	if source := (&app{}).workspaceMetricsSource(&plainTestBackend{}); source != nil {
		t.Fatalf("plain backend produced a metrics source: %+v", source)
	}
}
