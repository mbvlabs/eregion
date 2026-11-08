package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelemetryFactsReadsDirectoryAndCollectors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "textfile")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Join([]string{
			`node_exporter_build_info{version="1.11.1"} 1`,
			`node_scrape_collector_success{collector="textfile"} 1`,
			`node_scrape_collector_success{collector="systemd"} 1`,
			`node_textfile_scrape_error 0`,
		}, "\n")))
	}))
	defer server.Close()
	facts := telemetryFacts(context.Background(), RoleMain, dir, server.URL+"/metrics")
	if !facts.Known || !facts.Textfile.Exists || facts.Textfile.Path != dir ||
		!facts.Parent.Exists || facts.ExporterVersion != "1.11.1" ||
		!facts.Collectors["textfile"] || !facts.Collectors["systemd"] ||
		facts.TextfileScrapeError == nil || *facts.TextfileScrapeError != 0 || facts.TextfileScrapeErrorAmbiguous {
		t.Fatalf("telemetry facts = %+v", facts)
	}
}

func TestTelemetryFactsKeepsAMissingDirectoryUnknownNotHealthy(t *testing.T) {
	facts := telemetryFacts(context.Background(), RoleMain, filepath.Join(t.TempDir(), "absent"), "http://127.0.0.1:1/metrics")
	if !facts.Known || facts.Textfile.Exists || facts.EndpointError == "" {
		t.Fatalf("telemetry facts = %+v", facts)
	}
}

func TestTelemetryFactsDoesNotApplyToApplicationRole(t *testing.T) {
	if facts := telemetryFacts(context.Background(), RoleApplication, NodeExporterTextfileDir, NodeExporterMetricsURL()); facts.Known {
		t.Fatalf("application role reported local telemetry: %+v", facts)
	}
}
