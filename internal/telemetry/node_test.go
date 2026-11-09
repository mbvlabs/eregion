package telemetry

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseNodeExporterComputesRealValuesAndOmitsPseudoFilesystems(t *testing.T) {
	firstAt := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Second)
	snapshot, err := ParseNodeExporter(
		readFixture(t, "node_exporter_first.prom"),
		readFixture(t, "node_exporter_second.prom"),
		firstAt, secondAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Node != "main.example" || snapshot.Hostname != "main.example" || snapshot.Kernel != "6.8.0-1-amd64" || snapshot.Machine != "x86_64" || snapshot.OS != "Linux" {
		t.Fatalf("identity = %+v", snapshot)
	}
	if snapshot.ExporterVersion != "1.11.1" {
		t.Fatalf("exporter version = %q", snapshot.ExporterVersion)
	}
	if snapshot.ObservedAt == nil || !snapshot.ObservedAt.Equal(time.Unix(1700000010, 0).UTC()) {
		t.Fatalf("observed at = %v", snapshot.ObservedAt)
	}
	if snapshot.CPU == nil || snapshot.CPU.UtilizationPercent == nil {
		t.Fatalf("cpu = %+v", snapshot.CPU)
	}
	if got, want := *snapshot.CPU.UtilizationPercent, 28.205128205128204; math.Abs(got-want) > 0.0001 {
		t.Fatalf("cpu utilization = %v, want %v", got, want)
	}
	if snapshot.CPU.LogicalCPUs != 2 || math.Abs(snapshot.CPU.WindowSeconds-10) > 0.001 {
		t.Fatalf("cpu details = %+v", snapshot.CPU)
	}
	if snapshot.Memory == nil || snapshot.Memory.TotalBytes != 8589934592 || snapshot.Memory.AvailableBytes != 5368709120 || math.Abs(snapshot.Memory.UsedPercent-37.5) > 0.001 {
		t.Fatalf("memory = %+v", snapshot.Memory)
	}
	if snapshot.Load == nil || snapshot.Load.Load1 != 0.7 || snapshot.Load.Load5 != 0.5 || snapshot.Load.Load15 != 0.4 {
		t.Fatalf("load = %+v", snapshot.Load)
	}
	if snapshot.UptimeSeconds == nil || *snapshot.UptimeSeconds != 100010 {
		t.Fatalf("uptime = %v", snapshot.UptimeSeconds)
	}
	if len(snapshot.Filesystems) != 2 {
		t.Fatalf("filesystems = %+v, want root and /boot", snapshot.Filesystems)
	}
	if snapshot.Filesystems[0].Mountpoint != "/" || snapshot.Filesystems[1].Mountpoint != "/boot" {
		t.Fatalf("filesystem order = %+v", snapshot.Filesystems)
	}
	for _, filesystem := range snapshot.Filesystems {
		if strings.Contains(filesystem.Mountpoint, "/run") || filesystem.Filesystem == "overlay" || filesystem.Filesystem == "tmpfs" {
			t.Fatalf("pseudo filesystem leaked: %+v", filesystem)
		}
	}
	if len(snapshot.Issues) != 0 {
		t.Fatalf("healthy fixture issues = %+v", snapshot.Issues)
	}
}

func TestParseNodeExporterNamesMissingValuesInsteadOfZeroes(t *testing.T) {
	missing := readFixture(t, "node_exporter_missing.prom")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(missing, missing, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU != nil || snapshot.Memory != nil || snapshot.Load != nil || snapshot.UptimeSeconds != nil {
		t.Fatalf("missing metrics were rendered as values: %+v", snapshot)
	}
	if len(snapshot.Filesystems) != 0 {
		t.Fatalf("missing filesystems = %+v", snapshot.Filesystems)
	}
	if snapshot.ObservedAt != nil || snapshot.Freshness.State != FreshnessUnknown {
		t.Fatalf("freshness = %+v observed=%v", snapshot.Freshness, snapshot.ObservedAt)
	}
	for _, field := range []string{"cpu", "memory", "load", "uptime", "filesystem", "timestamp"} {
		if !hasIssue(snapshot.Issues, field) {
			t.Fatalf("missing %q issue in %+v", field, snapshot.Issues)
		}
	}
}

func TestParseNodeExporterExcludesLegacyGuestCPUModes(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom") +
		"\nnode_cpu_seconds_total{cpu=\"0\",mode=\"guest\"} 100\n" +
		"node_cpu_seconds_total{cpu=\"0\",mode=\"guest_nice\"} 50\n"
	second := readFixture(t, "node_exporter_second.prom") +
		"\nnode_cpu_seconds_total{cpu=\"0\",mode=\"guest\"} 900\n" +
		"node_cpu_seconds_total{cpu=\"0\",mode=\"guest_nice\"} 150\n"
	now := time.Unix(1700000010, 0).UTC()
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU == nil || snapshot.CPU.UtilizationPercent == nil || math.Abs(*snapshot.CPU.UtilizationPercent-28.205128205128204) > 0.0001 {
		t.Fatalf("legacy guest CPU modes were double-counted = %+v", snapshot.CPU)
	}
}

func TestParseNodeExporterFullyBusyCPUIsOneHundredPercent(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := readFixture(t, "node_exporter_second.prom")
	second = strings.ReplaceAll(second, `node_cpu_seconds_total{cpu="0",mode="idle"} 1060`, `node_cpu_seconds_total{cpu="0",mode="idle"} 1000`)
	second = strings.ReplaceAll(second, `node_cpu_seconds_total{cpu="1",mode="idle"} 2080`, `node_cpu_seconds_total{cpu="1",mode="idle"} 2000`)
	now := time.Unix(1700000010, 0).UTC()
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU == nil || snapshot.CPU.UtilizationPercent == nil || *snapshot.CPU.UtilizationPercent != 100 {
		t.Fatalf("fully busy cpu = %+v", snapshot.CPU)
	}
}

func TestParseNodeExporterCounterResetIsUnavailable(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := strings.ReplaceAll(readFixture(t, "node_exporter_second.prom"), `node_cpu_seconds_total{cpu="0",mode="idle"} 1060`, `node_cpu_seconds_total{cpu="0",mode="idle"} 900`)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU != nil {
		t.Fatalf("cpu after reset = %+v, want unavailable", snapshot.CPU)
	}
	if !hasIssue(snapshot.Issues, "cpu") {
		t.Fatalf("cpu issue missing: %+v", snapshot.Issues)
	}
}

func TestParseNodeExporterRequiresIdleForEveryObservedCPU(t *testing.T) {
	first := strings.ReplaceAll(readFixture(t, "node_exporter_first.prom"), `node_cpu_seconds_total{cpu="1",mode="idle"} 2000`+"\n", "")
	second := strings.ReplaceAll(readFixture(t, "node_exporter_second.prom"), `node_cpu_seconds_total{cpu="1",mode="idle"} 2080`+"\n", "")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU != nil {
		t.Fatalf("cpu without one idle series = %+v, want unavailable", snapshot.CPU)
	}
	if !hasIssue(snapshot.Issues, "cpu") {
		t.Fatalf("cpu issue missing: %+v", snapshot.Issues)
	}
}

func TestParseNodeExporterRequiresEssentialCPUModes(t *testing.T) {
	first := strings.ReplaceAll(readFixture(t, "node_exporter_first.prom"), `node_cpu_seconds_total{cpu="1",mode="user"} 300`+"\n", "")
	first = strings.ReplaceAll(first, `node_cpu_seconds_total{cpu="1",mode="system"} 100`+"\n", "")
	second := strings.ReplaceAll(readFixture(t, "node_exporter_second.prom"), `node_cpu_seconds_total{cpu="1",mode="user"} 325`+"\n", "")
	second = strings.ReplaceAll(second, `node_cpu_seconds_total{cpu="1",mode="system"} 110`+"\n", "")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU != nil {
		t.Fatalf("cpu with partial essential modes = %+v, want unavailable", snapshot.CPU)
	}
	if !hasIssue(snapshot.Issues, "cpu") {
		t.Fatalf("cpu issue missing: %+v", snapshot.Issues)
	}
}

func TestParseNodeExporterRequiresFullLinuxCPUModes(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := readFixture(t, "node_exporter_second.prom")
	for _, mode := range []string{"nice", "iowait", "softirq", "steal"} {
		first = withoutCPUMode(first, mode)
		second = withoutCPUMode(second, mode)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU != nil {
		t.Fatalf("cpu with missing expected modes = %+v, want unavailable", snapshot.CPU)
	}
	if !hasIssue(snapshot.Issues, "cpu") {
		t.Fatalf("cpu issue missing: %+v", snapshot.Issues)
	}
}

func withoutCPUMode(text, mode string) string {
	var out strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `mode="`+mode+`"`) {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func TestParseNodeExporterRejectsIdleOnlyCPUFamily(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	first := "node_cpu_seconds_total{cpu=\"0\",mode=\"idle\"} 100\n"
	second := "node_cpu_seconds_total{cpu=\"0\",mode=\"idle\"} 200\n"
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU != nil {
		t.Fatalf("idle-only CPU = %+v, want unavailable", snapshot.CPU)
	}
	if !hasIssue(snapshot.Issues, "cpu") {
		t.Fatalf("cpu issue missing: %+v", snapshot.Issues)
	}
}

func TestParseNodeExporterUsesOnlySecondScrapeTimestamp(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := strings.ReplaceAll(readFixture(t, "node_exporter_second.prom"), "node_time_seconds 1.70000001e+09\n", "")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservedAt != nil {
		t.Fatalf("observed at borrowed the first scrape: %v", snapshot.ObservedAt)
	}
	if snapshot.Freshness.State != FreshnessUnknown {
		t.Fatalf("freshness = %+v, want unknown", snapshot.Freshness)
	}
	if !hasIssue(snapshot.Issues, "timestamp") {
		t.Fatalf("timestamp issue missing: %+v", snapshot.Issues)
	}
}

func TestParseNodeExporterMateriallyFutureTimestampIsUnknown(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := readFixture(t, "node_exporter_second.prom")
	now := time.Unix(1700000010, 0).UTC()
	future := now.Add(5 * time.Minute)
	first = strings.ReplaceAll(first, "node_time_seconds 1.7e+09", fmt.Sprintf("node_time_seconds %d", future.Unix()))
	second = strings.ReplaceAll(second, "node_time_seconds 1.70000001e+09", fmt.Sprintf("node_time_seconds %d", future.Unix()+10))
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservedAt != nil {
		t.Fatalf("future exporter timestamp was exposed as observed: %v", *snapshot.ObservedAt)
	}
	if snapshot.Freshness.State != FreshnessUnknown || snapshot.Freshness.AgeSeconds != nil {
		t.Fatalf("future freshness = %+v, want unknown with no age", snapshot.Freshness)
	}
	if !hasIssue(snapshot.Issues, "timestamp") {
		t.Fatalf("future timestamp issue missing: %+v", snapshot.Issues)
	}
}

func TestParseNodeExporterSmallFutureSkewIsClampedButNotMateriallyFuture(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := readFixture(t, "node_exporter_second.prom")
	now := time.Unix(1700000010, 0).UTC()
	near := now.Add(2 * time.Second)
	second = strings.ReplaceAll(second, "node_time_seconds 1.70000001e+09", fmt.Sprintf("node_time_seconds %d", near.Unix()))
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservedAt == nil || snapshot.Freshness.State != FreshnessFresh {
		t.Fatalf("small future skew = %+v observed=%v, want fresh", snapshot.Freshness, snapshot.ObservedAt)
	}
}

func TestParseNodeExporterRejectsMalformedRelevantSamples(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := readFixture(t, "node_exporter_second.prom")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, malformed := range []string{
		`node_cpu_seconds_total{cpu="0",mode="idle"} not-a-number`,
		`node_cpu_seconds_total{cpu="0"} 1`,
		`node_cpu_seconds_total{cpu="0",cpu="1",mode="idle"} 2`,
		`node_memory_MemTotal_bytes NaN`,
		`node_time_seconds 0`,
		`node_time_seconds 1 not-a-timestamp`,
		`node_time_seconds 1.7e+09 1234567890`,
		`node_cpu_seconds_total{cpu="0",mode="idle"} 1000 1234567890`,
		`node_filesystem_size_bytes{device="/dev/sda1"} 1`,
	} {
		_, err := ParseNodeExporter(first+"\n"+malformed+"\n", second+"\n"+malformed+"\n", now, now.Add(time.Second))
		if err == nil {
			t.Fatalf("malformed relevant sample was accepted: %q", malformed)
		}
	}
}

func TestParseNodeExporterRejectsDuplicateConsumedSeries(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom")
	second := readFixture(t, "node_exporter_second.prom")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for name, duplicate := range map[string]string{
		"cpu":            `node_cpu_seconds_total{cpu="0",mode="idle"} 1001`,
		"memory":         `node_memory_MemTotal_bytes 8.0e+09`,
		"load":           `node_load1 1.5`,
		"time":           `node_time_seconds 1.70000002e+09`,
		"boot time":      `node_boot_time_seconds 1.6999e+09`,
		"filesystem":     `node_filesystem_size_bytes{device="/dev/sda1",fstype="ext4",mountpoint="/"} 1`,
		"uname":          `node_uname_info{nodename="main.example",sysname="Linux",release="6.8",machine="x86_64"} 1`,
		"exporter build": `node_exporter_build_info{version="1.11.1"} 1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNodeExporter(first+"\n"+duplicate+"\n", second+"\n"+duplicate+"\n", now, now.Add(time.Second))
			if err == nil {
				t.Fatalf("duplicate consumed series was accepted: %q", duplicate)
			}
		})
	}
}

func TestParseNodeExporterIgnoresMalformedUnrelatedSamples(t *testing.T) {
	first := readFixture(t, "node_exporter_first.prom") + "\nnode_scrape_other_metric{broken\n"
	second := readFixture(t, "node_exporter_second.prom") + "\nnode_scrape_other_metric{broken\n"
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if _, err := ParseNodeExporter(first, second, now, now.Add(time.Second)); err != nil {
		t.Fatalf("unrelated malformed line rejected: %v", err)
	}
}

func TestParseNodeExporterRejectsNonExporterResponses(t *testing.T) {
	now := time.Now().UTC()
	if _, err := ParseNodeExporter("hello world", "hello world", now, now.Add(time.Second)); err == nil {
		t.Fatal("non-Prometheus text was accepted")
	}
	if _, err := ParseNodeExporter("", "", now, now.Add(time.Second)); err == nil {
		t.Fatal("empty text was accepted")
	}
}

func TestParsePrometheusLineHandlesEscapedLabels(t *testing.T) {
	name, labels, value, timestamped, ok := parsePrometheusLine(`node_filesystem_size_bytes{device="/dev/sda1",fstype="ext4",mountpoint="/mnt/my\\disk"} 4096`)
	if !ok || timestamped || name != "node_filesystem_size_bytes" || value != 4096 || labels["mountpoint"] != `/mnt/my\disk` {
		t.Fatalf("parsed = %q %+v %v timestamped=%v ok=%v", name, labels, value, timestamped, ok)
	}
	if _, _, _, _, ok := parsePrometheusLine(`node_filesystem_size_bytes{device="/dev/sda1",fstype="ext4"} 4096 123`); !ok {
		t.Fatal("valid timestamped sample was not parseable")
	}
}

func TestParsePrometheusLabelsAcceptsWhitespaceAndTrailingComma(t *testing.T) {
	for _, text := range []string{
		`cpu="0",`,
		` cpu = "0" , mode = "idle" , `,
		"\tcpu\t=\t\"0\"\t,\tmode\t=\t\"idle\"\t,\t",
		`cpu="0",mode="idle",`,
	} {
		labels, ok := parsePrometheusLabels(text)
		if !ok {
			t.Fatalf("valid labels rejected: %q", text)
		}
		if labels["cpu"] != "0" {
			t.Fatalf("labels = %+v for %q", labels, text)
		}
	}
	if name, labels, value, _, ok := parsePrometheusLine(`node_load1{ cpu = "0" , } 1.5`); !ok || name != "node_load1" || labels["cpu"] != "0" || value != 1.5 {
		t.Fatalf("valid label whitespace/trailing comma rejected: %q %+v %v %v", name, labels, value, ok)
	}
}

func TestParsePrometheusLabelsRejectsInvalidGrammar(t *testing.T) {
	for _, text := range []string{
		`cpu="0"\q`,
		`cpu="0" mode="idle"`,
		`cpu="0",,`,
		`,cpu="0"`,
		`cpu="0",mode="idle"extra="x"`,
		`cpu="0",mode="idle\q"`,
		`cpu="0",0mode="idle"`,
		`cpu="0",cpu="1"`,
		`cpu="0",mode="idle", }`,
	} {
		if _, ok := parsePrometheusLabels(text); ok {
			t.Fatalf("invalid labels accepted: %q", text)
		}
	}
}

func TestParseNodeExporterSanitizesExporterLabelValues(t *testing.T) {
	first := "node_uname_info{nodename=\"main\x1b[31m\",sysname=\"Linux\",release=\"6.8\",machine=\"x86_64\"} 1\n" +
		"node_exporter_build_info{version=\"1.0\x1b]0;evil\x07\"} 1\n" +
		"node_filesystem_size_bytes{device=\"/dev/sda1\x1b\",fstype=\"ext4\",mountpoint=\"/\x1b]0;evil\x07\"} 100\n" +
		"node_filesystem_avail_bytes{device=\"/dev/sda1\x1b\",fstype=\"ext4\",mountpoint=\"/\x1b]0;evil\x07\"} 50\n"
	second := strings.Replace(first, " 100\n", " 200\n", 1)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{snapshot.Node, snapshot.ExporterVersion, snapshot.Filesystems[0].Device, snapshot.Filesystems[0].Mountpoint} {
		if strings.ContainsAny(value, "\x1b\x07\n\r\t") {
			t.Fatalf("unsanitized exporter label value: %q", value)
		}
	}
	if !strings.Contains(snapshot.Filesystems[0].Mountpoint, "]0;evil") {
		t.Fatalf("sanitization removed ordinary text: %q", snapshot.Filesystems[0].Mountpoint)
	}
}

func TestParseNodeExporterSanitizesCPULabelInIssue(t *testing.T) {
	first := "node_cpu_seconds_total{cpu=\"0\x1b[31m\",mode=\"idle\"} 100\n" +
		"node_time_seconds 1000\n"
	second := "node_cpu_seconds_total{cpu=\"0\x1b[31m\",mode=\"idle\"} 110\n" +
		"node_time_seconds 1001\n"
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	snapshot, err := ParseNodeExporter(first, second, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CPU != nil {
		t.Fatalf("partial CPU family = %+v, want unavailable", snapshot.CPU)
	}
	for _, issue := range snapshot.Issues {
		if strings.ContainsAny(issue.Reason, "\x1b\x07\n\r\t") {
			t.Fatalf("unsanitized CPU label in issue: %q", issue.Reason)
		}
	}
}

func TestProbeLocalReportsCollectorHealth(t *testing.T) {
	body := strings.Join([]string{
		`node_exporter_build_info{branch="HEAD",goversion="go1.24.0",revision="abc",tags="unknown",version="1.11.1"} 1`,
		`node_scrape_collector_success{collector="systemd"} 1`,
		`node_scrape_collector_success{collector="textfile"} 1`,
		`node_scrape_collector_success{collector="cpu"} 1`,
		`node_textfile_scrape_error 0`,
	}, "\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	probe, err := ProbeLocal(context.Background(), server.URL+"/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if !probe.Reachable || probe.ExporterVersion != "1.11.1" {
		t.Fatalf("probe = %+v", probe)
	}
	if !probe.Collectors["systemd"] || !probe.Collectors["textfile"] {
		t.Fatalf("collectors = %+v", probe.Collectors)
	}
	if _, present := probe.Collectors["missing"]; present {
		t.Fatal("an unobserved collector was invented")
	}
	if probe.TextfileScrapeError == nil || *probe.TextfileScrapeError != 0 || probe.TextfileScrapeErrorAmbiguous {
		t.Fatalf("textfile scrape error = %v ambiguous=%t", probe.TextfileScrapeError, probe.TextfileScrapeErrorAmbiguous)
	}
	if len(probe.CollectorsAmbiguous) != 0 {
		t.Fatalf("collector ambiguity = %+v", probe.CollectorsAmbiguous)
	}
}

func TestProbeLocalRequiresUnambiguousCollectorSuccess(t *testing.T) {
	base := []string{
		`node_exporter_build_info{version="1.11.1"} 1`,
		`node_textfile_scrape_error 0`,
	}
	for name, tc := range map[string]struct {
		extra     []string
		ambiguous string
	}{
		"valid": {
			extra: []string{
				`node_scrape_collector_success{collector="textfile"} 1`,
				`node_scrape_collector_success{collector="systemd"} 1`,
			},
		},
		"duplicate textfile": {
			extra: []string{
				`node_scrape_collector_success{collector="textfile"} 1`,
				`node_scrape_collector_success{collector="textfile"} 1`,
				`node_scrape_collector_success{collector="systemd"} 1`,
			},
			ambiguous: "textfile",
		},
		"duplicate systemd conflicting": {
			extra: []string{
				`node_scrape_collector_success{collector="textfile"} 1`,
				`node_scrape_collector_success{collector="systemd"} 0`,
				`node_scrape_collector_success{collector="systemd"} 1`,
			},
			ambiguous: "systemd",
		},
		"timestamped textfile": {
			extra: []string{
				`node_scrape_collector_success{collector="textfile"} 1 1700000000`,
				`node_scrape_collector_success{collector="systemd"} 1`,
			},
			ambiguous: "*",
		},
		"malformed": {
			extra: []string{
				`node_scrape_collector_success{collector="textfile"} 1`,
				`node_scrape_collector_success{collector="systemd"`,
			},
			ambiguous: "*",
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Join(append(append([]string{}, base...), tc.extra...), "\n")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			probe, err := ProbeLocal(context.Background(), server.URL+"/metrics")
			if err != nil {
				t.Fatal(err)
			}
			if tc.ambiguous == "" {
				if len(probe.CollectorsAmbiguous) != 0 {
					t.Fatalf("collector ambiguity = %+v", probe.CollectorsAmbiguous)
				}
				return
			}
			if !probe.CollectorsAmbiguous[tc.ambiguous] {
				t.Fatalf("collector ambiguity = %+v, want %q", probe.CollectorsAmbiguous, tc.ambiguous)
			}
		})
	}
}

func TestProbeLocalRequiresUnambiguousTextfileScrapeError(t *testing.T) {
	base := []string{
		`node_exporter_build_info{version="1.11.1"} 1`,
		`node_scrape_collector_success{collector="systemd"} 1`,
		`node_scrape_collector_success{collector="textfile"} 1`,
	}
	for name, tc := range map[string]struct {
		extra     []string
		value     *float64
		ambiguous bool
	}{
		"missing":     {extra: nil, value: nil, ambiguous: false},
		"zero":        {extra: []string{"node_textfile_scrape_error 0"}, value: ptrFloat(0), ambiguous: false},
		"error":       {extra: []string{"node_textfile_scrape_error 1"}, value: ptrFloat(1), ambiguous: false},
		"duplicate":   {extra: []string{"node_textfile_scrape_error 0", "node_textfile_scrape_error 0"}, value: nil, ambiguous: true},
		"malformed":   {extra: []string{"node_textfile_scrape_error{broken"}, value: nil, ambiguous: true},
		"labelled":    {extra: []string{`node_textfile_scrape_error{path="/tmp/x"} 0`}, value: nil, ambiguous: true},
		"timestamped": {extra: []string{"node_textfile_scrape_error 0 1700000000"}, value: nil, ambiguous: true},
		"out of range": {
			extra: []string{"node_textfile_scrape_error 2"}, value: nil, ambiguous: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Join(append(append([]string{}, base...), tc.extra...), "\n")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			probe, err := ProbeLocal(context.Background(), server.URL+"/metrics")
			if err != nil {
				t.Fatal(err)
			}
			if probe.TextfileScrapeErrorAmbiguous != tc.ambiguous {
				t.Fatalf("ambiguous = %t, want %t; probe=%+v", probe.TextfileScrapeErrorAmbiguous, tc.ambiguous, probe)
			}
			switch {
			case tc.value == nil && probe.TextfileScrapeError != nil:
				t.Fatalf("textfile error = %v, want nil", *probe.TextfileScrapeError)
			case tc.value != nil && (probe.TextfileScrapeError == nil || *probe.TextfileScrapeError != *tc.value):
				t.Fatalf("textfile error = %v, want %v", probe.TextfileScrapeError, *tc.value)
			}
		})
	}
}

func ptrFloat(value float64) *float64 { return &value }

func TestProbeLocalRefusesAnUnrecognizedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello\n"))
	}))
	defer server.Close()
	if _, err := ProbeLocal(context.Background(), server.URL+"/metrics"); err == nil {
		t.Fatal("an unrelated HTTP service was accepted as node_exporter")
	}
}

func TestCollectNodeExporterReadsTwiceAndSetsScrapeDetails(t *testing.T) {
	firstBody := readFixture(t, "node_exporter_first.prom")
	secondBody := readFixture(t, "node_exporter_second.prom")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(firstBody))
			return
		}
		_, _ = w.Write([]byte(secondBody))
	}))
	defer server.Close()
	now := time.Unix(1700000010, 0).UTC()
	scrape := ScrapeInfo{
		Endpoint: server.URL + "/metrics", Listen: "127.0.0.1:9100", Access: "loopback",
		Authentication: "none", Collection: "operator_node_rpc", Note: "test",
	}
	snapshot, err := CollectNodeExporter(context.Background(), NodeExporterConfig{
		Endpoint: server.URL + "/metrics", Scrape: scrape,
		Now: func() time.Time { return now }, SampleInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want two bounded reads", requests.Load())
	}
	if snapshot.Scrape != scrape || snapshot.Freshness.State != FreshnessFresh || snapshot.Freshness.AgeSeconds == nil {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
}

func TestCollectNodeExporterDoesNotClampMateriallyFutureTimestampToFresh(t *testing.T) {
	now := time.Unix(1700000010, 0).UTC()
	future := now.Add(5 * time.Minute)
	firstBody := strings.ReplaceAll(readFixture(t, "node_exporter_first.prom"), "node_time_seconds 1.7e+09", fmt.Sprintf("node_time_seconds %d", future.Unix()))
	secondBody := strings.ReplaceAll(readFixture(t, "node_exporter_second.prom"), "node_time_seconds 1.70000001e+09", fmt.Sprintf("node_time_seconds %d", future.Unix()+10))
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			_, _ = w.Write([]byte(firstBody))
			return
		}
		_, _ = w.Write([]byte(secondBody))
	}))
	defer server.Close()
	snapshot, err := CollectNodeExporter(context.Background(), NodeExporterConfig{
		Endpoint:       server.URL + "/metrics",
		Scrape:         ScrapeInfo{Endpoint: server.URL + "/metrics", Access: "loopback"},
		Now:            func() time.Time { return now },
		SampleInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservedAt != nil || snapshot.Freshness.State != FreshnessUnknown || snapshot.Freshness.AgeSeconds != nil {
		t.Fatalf("collector clamped future timestamp to fresh: observed=%v freshness=%+v", snapshot.ObservedAt, snapshot.Freshness)
	}
}

func TestCollectNodeExporterRecordsCompletionTimestampAfterSecondRead(t *testing.T) {
	firstBody := readFixture(t, "node_exporter_first.prom")
	secondBody := readFixture(t, "node_exporter_second.prom")
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if reads.Add(1) == 1 {
			_, _ = w.Write([]byte(firstBody))
			return
		}
		_, _ = w.Write([]byte(secondBody))
	}))
	defer server.Close()
	start := time.Unix(1700000010, 0).UTC()
	completion := start.Add(3 * time.Second)
	var calls int
	snapshot, err := CollectNodeExporter(context.Background(), NodeExporterConfig{
		Endpoint: server.URL + "/metrics", Scrape: ScrapeInfo{Endpoint: server.URL + "/metrics", Access: "loopback"},
		Now: func() time.Time {
			calls++
			if calls == 1 {
				return start
			}
			return completion
		},
		SampleInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("clock calls = %d, want first start and one completion", calls)
	}
	if !snapshot.FetchedAt.Equal(completion) {
		t.Fatalf("fetched_at = %s, want completion %s", snapshot.FetchedAt, completion)
	}
}

func TestCollectNodeExporterReportsUnreachableAndOversizedReads(t *testing.T) {
	now := time.Now().UTC()
	_, err := CollectNodeExporter(context.Background(), NodeExporterConfig{
		Endpoint: "http://127.0.0.1:1/metrics", Now: func() time.Time { return now },
		SampleInterval: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("unreachable exporter was accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
	}))
	defer server.Close()
	_, err = CollectNodeExporter(context.Background(), NodeExporterConfig{
		Endpoint: server.URL + "/metrics", Now: func() time.Time { return now },
		SampleInterval: 100 * time.Millisecond, MaxBody: 4096,
	})
	if err == nil {
		t.Fatal("oversized exporter response was accepted")
	}
}

func hasIssue(issues []Issue, field string) bool {
	for _, issue := range issues {
		if issue.Field == field && strings.TrimSpace(issue.Reason) != "" {
			return true
		}
	}
	return false
}
