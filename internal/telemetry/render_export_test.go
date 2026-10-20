package telemetry

import (
	"strings"
	"testing"
)

func TestExternalExportRendersReferencesNotCredentials(t *testing.T) {
	cfg := validConfig(t)
	cfg.ExternalExport = &ExternalExport{
		Endpoint:  "https://otlp.example.com:4318/otlp",
		TokenFile: "/etc/eregion/telemetry/external-token",
		TLSCAFile: "/etc/eregion/telemetry/external-ca.crt",
	}
	prepared, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ExternalExport == nil || prepared.ExternalExport.Protocol != "otlp_http" {
		t.Fatalf("external export defaults were not applied: %+v", prepared.ExternalExport)
	}
	plan, err := RenderPlan(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var collector, compose string
	for _, file := range plan.Files {
		switch {
		case strings.HasSuffix(file.Path, "collector/config.yaml"):
			collector = string(file.Data)
		case strings.HasSuffix(file.Path, "compose.yaml"):
			compose = string(file.Data)
		}
	}
	for _, want := range []string{
		"otlphttp/external:", "endpoint: https://otlp.example.com:4318/otlp",
		"bearertokenauth/export", "filename: /etc/otelcol/external-token",
		"ca_file: /etc/otelcol/external-ca.crt",
		"exporters: [prometheusremotewrite, otlphttp/external]",
		"exporters: [otlphttp/logs, otlphttp/external]",
		"exporters: [otlp/tempo, otlphttp/external]",
	} {
		if !strings.Contains(collector, want) {
			t.Fatalf("collector config missing %q:\n%s", want, collector)
		}
	}
	for _, want := range []string{
		`"/etc/eregion/telemetry/external-token:/etc/otelcol/external-token:ro"`,
		`"/etc/eregion/telemetry/external-ca.crt:/etc/otelcol/external-ca.crt:ro"`,
	} {
		if !strings.Contains(compose, want) {
			t.Fatalf("compose missing external file reference %q:\n%s", want, compose)
		}
	}
	for _, file := range plan.Files {
		if strings.Contains(string(file.Data), "Bearer ") && !file.Secret {
			t.Fatalf("external credential material was inlined in %s", file.Path)
		}
	}
	for _, command := range plan.Commands {
		joined := command.Name + " " + strings.Join(command.Args, " ")
		if strings.Contains(joined, "external-token") && !strings.Contains(joined, "compose") {
			t.Fatalf("external credential path leaked onto a non-compose command: %s", joined)
		}
	}
}

func TestRenderPlanProvisionsAlertingDefaults(t *testing.T) {
	cfg := validConfig(t)
	prepared, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderPlan(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var alerts, provider, datasources, compose string
	for _, file := range plan.Files {
		switch {
		case strings.HasSuffix(file.Path, "alerting/eregion-alerts.yaml"):
			alerts = string(file.Data)
		case strings.HasSuffix(file.Path, "dashboards/provider.yaml"):
			provider = string(file.Data)
		case strings.HasSuffix(file.Path, "datasources/datasources.yaml"):
			datasources = string(file.Data)
		case strings.HasSuffix(file.Path, "compose.yaml"):
			compose = string(file.Data)
		}
	}
	if alerts == "" {
		t.Fatal("no provisioned Grafana alert rule file was rendered")
	}
	for _, want := range []string{"apiVersion: 1", "uid: eregion-telemetry-disk-low", "datasourceUid: VictoriaMetrics", "noDataState: NoData"} {
		if !strings.Contains(alerts, want) {
			t.Fatalf("alert rule file missing %q:\n%s", want, alerts)
		}
	}
	if !strings.Contains(provider, "path: /var/lib/grafana/dashboards") {
		t.Fatal("dashboard provider is not provisioned")
	}
	if !strings.Contains(datasources, "uid: VictoriaMetrics") {
		t.Fatal("datasource UID does not match the alert rule reference")
	}
	if !strings.Contains(compose, "./grafana/provisioning:/etc/grafana/provisioning:ro") {
		t.Fatal("Grafana provisioning is not mounted into the container")
	}
}

func TestExternalExportValidation(t *testing.T) {
	bad := []ExternalExport{
		{},
		{Endpoint: "ftp://example.com"},
		{Endpoint: "https://user:pass@example.com:4318"},
		{Endpoint: "http://203.0.113.5:4318"},
		{Endpoint: "https://example.com:4318", Protocol: "zipkin"},
		{Endpoint: "https://example.com:4318", TokenFile: "relative/token"},
		{Endpoint: "https://example.com:4318", TLSCAFile: "/etc/../secret"},
		{Endpoint: "https://example.com:4317/v1", Protocol: "otlp_grpc"},
	}
	for _, value := range bad {
		if _, err := value.Validate(); err == nil {
			t.Fatalf("unsafe external export accepted: %+v", value)
		}
	}
	good := []ExternalExport{
		{Endpoint: "https://otlp.example.com:4318"},
		{Endpoint: "https://otlp.example.com:4317", Protocol: "otlp_grpc"},
		{Endpoint: "http://10.77.0.10:4318"},
	}
	for _, value := range good {
		if _, err := value.Validate(); err != nil {
			t.Fatalf("valid external export rejected: %+v: %v", value, err)
		}
	}
}

func TestExternalGRPCExportUsesTheGRPCExporter(t *testing.T) {
	cfg := validConfig(t)
	cfg.ExternalExport = &ExternalExport{Endpoint: "https://otlp.example.com:4317", Protocol: "otlp_grpc"}
	prepared, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderPlan(prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range plan.Files {
		if strings.HasSuffix(file.Path, "collector/config.yaml") && !strings.Contains(string(file.Data), "otlp/external:") {
			t.Fatal("gRPC external exporter was not rendered")
		}
	}
}
