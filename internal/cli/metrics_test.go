package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

type metricsCLIBackend struct {
	Backend
	snapshot telemetry.NodeSnapshot
	err      error
	calls    int
}

func (f *metricsCLIBackend) NodeMetrics(context.Context) (telemetry.NodeSnapshot, error) {
	f.calls++
	return f.snapshot, f.err
}

func runMetricsCLI(t *testing.T, backend *metricsCLIBackend, args ...string) (int, string, string) {
	t.Helper()
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	return exit, stdout.String(), stderr.String()
}

func testMetricsSnapshot() telemetry.NodeSnapshot {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cpu := 12.5
	uptime := 100010.0
	age := 2.0
	return telemetry.NodeSnapshot{
		Schema: telemetry.NodeMetricsSchema, Node: "main", Hostname: "main.example",
		OS: "Linux", Kernel: "6.8.0-1-amd64", Machine: "x86_64", ExporterVersion: "1.11.1",
		FetchedAt: now, ObservedAt: &now,
		Freshness: telemetry.Freshness{State: telemetry.FreshnessFresh, AgeSeconds: &age, ThresholdSeconds: 30},
		Scrape: telemetry.ScrapeInfo{
			Endpoint: "http://10.78.0.1:9100/metrics", Listen: "10.78.0.1:9100", Access: "wireguard",
			Public: false, Authentication: "none", Collection: "operator_node_rpc",
			Note: "read by this operator command",
		},
		CPU:           &telemetry.CPUStats{UtilizationPercent: &cpu, LogicalCPUs: 4, WindowSeconds: 1, TotalSeconds: 1, IdleSeconds: 0.875},
		Memory:        &telemetry.MemoryStats{TotalBytes: 8 << 30, AvailableBytes: 6 << 30, UsedBytes: 2 << 30, UsedPercent: 25},
		Load:          &telemetry.LoadStats{Load1: 0.5, Load5: 0.4, Load15: 0.3},
		UptimeSeconds: &uptime,
		Filesystems: []telemetry.FilesystemStats{{
			Device: "/dev/sda1", Filesystem: "ext4", Mountpoint: "/",
			TotalBytes: 100 << 30, AvailableBytes: 40 << 30, UsedBytes: 60 << 30, UsedPercent: 60,
		}},
	}
}

func TestMetricsShowPlainAndJSONReadOneSnapshot(t *testing.T) {
	backend := &metricsCLIBackend{snapshot: testMetricsSnapshot()}
	exit, stdout, stderr := runMetricsCLI(t, backend, "metrics", "show", "--json")
	if exit != 0 || stderr != "" {
		t.Fatalf("json exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("envelope = %+v", envelope)
	}
	raw, _ := json.Marshal(envelope.Data)
	var snapshot telemetry.NodeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Node != "main" || snapshot.CPU == nil || snapshot.CPU.UtilizationPercent == nil || *snapshot.CPU.UtilizationPercent != 12.5 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Scrape.Public || snapshot.Scrape.Access != "wireguard" || snapshot.Scrape.Authentication != "none" {
		t.Fatalf("scrape details = %+v", snapshot.Scrape)
	}

	exit, stdout, stderr = runMetricsCLI(t, backend, "metrics", "show")
	if exit != 0 || stderr != "" {
		t.Fatalf("plain exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	for _, want := range []string{"Node metrics main", "main.example", "12.5% of 4 logical CPUs", "WireGuard only", "no public endpoint", "25.0%", "0.50 0.40 0.30", "1d 3h 46m", "/ (ext4)", "sample 2s old", "no samples are stored"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("plain output missing %q:\n%s", want, stdout)
		}
	}
	if backend.calls != 2 {
		t.Fatalf("backend calls = %d, want one per invocation", backend.calls)
	}
}

func TestMetricsShowUnavailableValuesAreNotZero(t *testing.T) {
	snapshot := testMetricsSnapshot()
	snapshot.CPU = nil
	snapshot.Memory = nil
	snapshot.Load = nil
	snapshot.UptimeSeconds = nil
	snapshot.Filesystems = nil
	snapshot.Issues = []telemetry.Issue{
		{Field: "cpu", Reason: "node_cpu_seconds_total is missing"},
		{Field: "memory", Reason: "memory counters are missing"},
		{Field: "load", Reason: "load counters are missing"},
		{Field: "uptime", Reason: "boot time is missing"},
		{Field: "filesystem", Reason: "no filesystem pair was found"},
	}
	backend := &metricsCLIBackend{snapshot: snapshot}
	exit, stdout, stderr := runMetricsCLI(t, backend, "metrics", "show")
	if exit != 0 || stderr != "" {
		t.Fatalf("exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	for _, want := range []string{"cpu        unavailable: node_cpu_seconds_total is missing", "memory     unavailable", "filesystem unavailable", "missing"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("plain output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "0.0% of") || strings.Contains(stdout, "0 B used") {
		t.Fatalf("unavailable metrics were rendered as zero:\n%s", stdout)
	}
}

func TestMetricsShowUsesTheNodeRPCMethod(t *testing.T) {
	transport := &fakeNodeTransport{
		version: rpcVersionResult{Version: buildinfo.Version, Commit: buildinfo.Commit, Arch: "amd64"},
		metrics: testMetricsSnapshot(),
	}
	client := &nodeClient{remote: transport}
	snapshot, err := client.NodeMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Node != "main" || !transport.called(methodMetricsShow) {
		t.Fatalf("snapshot=%+v methods=%v", snapshot, transport.methods)
	}
}

func TestMetricsShowRejectsInvalidNodeData(t *testing.T) {
	transport := &fakeNodeTransport{
		version: rpcVersionResult{Version: buildinfo.Version, Commit: buildinfo.Commit, Arch: "amd64"},
		metrics: map[string]string{"node": "main"},
	}
	client := &nodeClient{remote: transport}
	_, err := client.NodeMetrics(context.Background())
	if err == nil || contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("invalid metrics = %v", err)
	}
}
