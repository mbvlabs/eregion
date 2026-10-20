package telemetry

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// PlanFile is one rendered file. Secret files are always written 0600.
type PlanFile struct {
	Path   string
	Data   []byte
	Mode   fs.FileMode
	Secret bool
}

// Command is one argv vector. It is executed directly, never through a shell.
type Command struct {
	Name string
	Args []string
}

// Plan is a complete, inspectable render of an install or reconciliation.
type Plan struct {
	RootDir  string
	Files    []PlanFile
	Commands []Command
}

// RenderPlan renders the dedicated telemetry stack. It performs no I/O and
// contains no shell command. The token file is the only rendered file that
// contains ingest credentials.
func RenderPlan(prepared Prepared) (Plan, error) {
	cfg := prepared.Config
	if _, err := Prepare(cfg); err != nil {
		return Plan{}, err
	}
	root := cfg.RootDir
	stackDir := filepath.Join(root, "stack")
	tlsMaterial, err := loadOrCreateTLS(root, cfg.Node.PrivateAddress)
	if err != nil {
		return Plan{}, err
	}
	files := []PlanFile{
		{Path: filepath.Join(root, "tls", "ca.crt"), Data: tlsMaterial.CAPEM, Mode: 0644},
		{Path: filepath.Join(root, "tls", "server.crt"), Data: tlsMaterial.CertPEM, Mode: 0644},
		{Path: filepath.Join(root, "tls", "server.key"), Data: tlsMaterial.KeyPEM, Mode: 0600, Secret: true},
		{Path: filepath.Join(stackDir, "compose.yaml"), Data: []byte(renderCompose(cfg)), Mode: 0640},
		{Path: filepath.Join(stackDir, "collector", "config.yaml"), Data: []byte(renderCollector(cfg)), Mode: 0640},
		{Path: filepath.Join(stackDir, "collector", "ingest-tokens"), Data: []byte(renderTokens(cfg.Credentials)), Mode: 0600, Secret: true},
		{Path: filepath.Join(stackDir, "vmauth", "metrics.yaml"), Data: []byte(renderVMAuth(prepared.MetricsReadToken(), "http://victoriametrics:8428")), Mode: 0600, Secret: true},
		{Path: filepath.Join(stackDir, "vmauth", "logs.yaml"), Data: []byte(renderVMAuth(prepared.LogsReadToken(), "http://victorialogs:9428")), Mode: 0600, Secret: true},
		{Path: filepath.Join(root, "read-tokens", "metrics"), Data: []byte(prepared.MetricsReadToken() + "\n"), Mode: 0600, Secret: true},
		{Path: filepath.Join(root, "read-tokens", "logs"), Data: []byte(prepared.LogsReadToken() + "\n"), Mode: 0600, Secret: true},
		{Path: filepath.Join(stackDir, "grafana", "grafana.env"), Data: []byte(renderGrafanaEnv(cfg)), Mode: 0600, Secret: true},
		{Path: filepath.Join(stackDir, "grafana", "provisioning", "datasources", "datasources.yaml"), Data: []byte(renderDatasources(cfg, prepared.MetricsReadToken())), Mode: 0600, Secret: true},
		{Path: filepath.Join(stackDir, "grafana", "provisioning", "dashboards", "provider.yaml"), Data: []byte(renderDashboardProvider()), Mode: 0640},
		{Path: filepath.Join(stackDir, "grafana", "provisioning", "alerting", "eregion-alerts.yaml"), Data: []byte(renderAlertRules()), Mode: 0640},
		{Path: filepath.Join(stackDir, "grafana", "dashboards", "eregion-telemetry.json"), Data: []byte(renderDashboard(cfg)), Mode: 0640},
	}
	if cfg.EnableTraces {
		files = append(files, PlanFile{Path: filepath.Join(stackDir, "tempo", "tempo.yaml"), Data: []byte(renderTempo(cfg)), Mode: 0640})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	composePath := filepath.Join(stackDir, "compose.yaml")
	return Plan{
		RootDir: root,
		Files:   files,
		Commands: []Command{
			{Name: "docker", Args: []string{"compose", "--file", composePath, "pull"}},
			{Name: "docker", Args: []string{"compose", "--file", composePath, "up", "--detach", "--remove-orphans", "--wait"}},
		},
	}, nil
}

func renderCompose(cfg Config) string {
	ip := cfg.Node.PrivateAddress
	externalVolumes := ""
	if cfg.ExternalExport != nil {
		if cfg.ExternalExport.TokenFile != "" {
			externalVolumes += fmt.Sprintf("      - %q\n", cfg.ExternalExport.TokenFile+":/etc/otelcol/external-token:ro")
		}
		if cfg.ExternalExport.TLSCAFile != "" {
			externalVolumes += fmt.Sprintf("      - %q\n", cfg.ExternalExport.TLSCAFile+":/etc/otelcol/external-ca.crt:ro")
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `name: eregion-telemetry

services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib:%s
    command: ["--config=/etc/otelcol/config.yaml"]
    restart: unless-stopped
    volumes:
      - ./collector/config.yaml:/etc/otelcol/config.yaml:ro
      - ./collector/ingest-tokens:/etc/otelcol/ingest-tokens:ro
      - ../tls:/etc/otelcol/tls:ro
      - otel-state:/var/lib/otelcol
%s    ports:
      - "%s:4317:4317"
      - "%s:4318:4318"
    depends_on:
      victoriametrics:
        condition: service_started
      victorialogs:
        condition: service_started
    read_only: true
    tmpfs:
      - /tmp
    security_opt:
      - no-new-privileges:true

  victoriametrics:
    image: victoriametrics/victoria-metrics:%s
    command:
      - "-storageDataPath=/victoria-metrics-data"
      - "-retentionPeriod=%dd"
      - "-httpListenAddr=:8428"
      - "-selfScrapeInterval=15s"
    restart: unless-stopped
    volumes:
      - vm-data:/victoria-metrics-data
    security_opt:
      - no-new-privileges:true

  victorialogs:
    image: victoriametrics/victoria-logs:%s
    command:
      - "-storageDataPath=/victoria-logs-data"
      - "-retentionPeriod=%dd"
      - "-httpListenAddr=:9428"
    restart: unless-stopped
    volumes:
      - vl-data:/victoria-logs-data
    security_opt:
      - no-new-privileges:true

  vmauth-metrics:
    image: victoriametrics/vmauth:%s
    command:
      - "-auth.config=/etc/vmauth/auth.yaml"
      - "-httpListenAddr=:8427"
    restart: unless-stopped
    volumes:
      - ./vmauth/metrics.yaml:/etc/vmauth/auth.yaml:ro
    ports:
      - "%s:8427:8427"
    depends_on:
      victoriametrics:
        condition: service_started
    read_only: true
    security_opt:
      - no-new-privileges:true

  vmauth-logs:
    image: victoriametrics/vmauth:%s
    command:
      - "-auth.config=/etc/vmauth/auth.yaml"
      - "-httpListenAddr=:9427"
    restart: unless-stopped
    volumes:
      - ./vmauth/logs.yaml:/etc/vmauth/auth.yaml:ro
    ports:
      - "%s:9427:9427"
    depends_on:
      victorialogs:
        condition: service_started
    read_only: true
    security_opt:
      - no-new-privileges:true

  grafana:
    image: grafana/grafana:%s
    restart: unless-stopped
    env_file:
      - ./grafana/grafana.env
    environment:
      GF_USERS_ALLOW_SIGN_UP: "false"
      GF_AUTH_ANONYMOUS_ENABLED: "false"
      GF_SECURITY_DISABLE_GRAVATAR: "true"
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
      - ./grafana/dashboards:/var/lib/grafana/dashboards:ro
      - grafana-data:/var/lib/grafana
    ports:
      - "%s:3000:3000"
    depends_on:
      vmauth-metrics:
        condition: service_started
    security_opt:
      - no-new-privileges:true
`, cfg.Versions.Collector, externalVolumes, ip, ip, cfg.Versions.VictoriaMetrics, cfg.Retention.MetricsDays, cfg.Versions.VictoriaLogs, cfg.Retention.LogsDays, cfg.Versions.VictoriaMetrics, ip, cfg.Versions.VictoriaMetrics, ip, cfg.Versions.Grafana, ip)
	if cfg.EnableTraces {
		fmt.Fprintf(&b, `
  tempo:
    image: grafana/tempo:%s
    command: ["-config.file=/etc/tempo/tempo.yaml"]
    restart: unless-stopped
    volumes:
      - ./tempo/tempo.yaml:/etc/tempo/tempo.yaml:ro
      - tempo-data:/var/tempo
    security_opt:
      - no-new-privileges:true
`, cfg.Versions.Tempo)
	}
	b.WriteString(`
volumes:
  vm-data:
  vl-data:
  grafana-data:
  otel-state:
`)
	if cfg.EnableTraces {
		b.WriteString("  tempo-data:\n")
	}
	return b.String()
}

func renderTokens(credentials []IngestCredential) string {
	var b strings.Builder
	for _, credential := range credentials {
		fmt.Fprintf(&b, "%s node=%s\n", credential.Token, credential.Node)
	}
	return b.String()
}

func renderCollector(cfg Config) string {
	extensionList := "bearertokenauth/ingest, file_storage, health_check"
	externalExtension := ""
	externalExporter := ""
	metricExporters := "[prometheusremotewrite]"
	logExporters := "[otlphttp/logs]"
	traceExporters := "[otlp/tempo]"
	external := cfg.ExternalExport
	if external != nil {
		exporterName := "otlphttp/external"
		if external.Protocol == "otlp_grpc" {
			exporterName = "otlp/external"
		}
		tlsBlock := ""
		if external.TLSCAFile != "" {
			tlsBlock = "\n    tls:\n      ca_file: /etc/otelcol/external-ca.crt"
		}
		authBlock := ""
		if external.TokenFile != "" {
			externalExtension = "  bearertokenauth/export:\n    scheme: Bearer\n    filename: /etc/otelcol/external-token\n"
			extensionList += ", bearertokenauth/export"
			authBlock = "\n    auth:\n      authenticator: bearertokenauth/export"
		}
		externalExporter = fmt.Sprintf(`  %s:
    endpoint: %s%s%s
    retry_on_failure:
      enabled: true
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 10000
`, exporterName, external.Endpoint, tlsBlock, authBlock)
		metricExporters = "[prometheusremotewrite, " + exporterName + "]"
		logExporters = "[otlphttp/logs, " + exporterName + "]"
		traceExporters = "[otlp/tempo, " + exporterName + "]"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `extensions:
  bearertokenauth/ingest:
    scheme: Bearer
    filename: /etc/otelcol/ingest-tokens
%s  file_storage:
    directory: /var/lib/otelcol
  health_check:
    endpoint: 127.0.0.1:13133

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        max_recv_msg_size_mib: 16
        tls:
          cert_file: /etc/otelcol/tls/server.crt
          key_file: /etc/otelcol/tls/server.key
        auth:
          authenticator: bearertokenauth/ingest
      http:
        endpoint: 0.0.0.0:4318
        max_request_body_size: 16777216
        tls:
          cert_file: /etc/otelcol/tls/server.crt
          key_file: /etc/otelcol/tls/server.key
        auth:
          authenticator: bearertokenauth/ingest

processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
    spike_limit_mib: 128
  batch:
    timeout: 5s
    send_batch_size: 8192
    send_batch_max_size: 16384

exporters:
  prometheusremotewrite:
    endpoint: http://victoriametrics:8428/api/v1/write
    resource_to_telemetry_conversion:
      enabled: true
    retry_on_failure:
      enabled: true
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 10000
  otlphttp/logs:
    logs_endpoint: http://victorialogs:9428/insert/opentelemetry/v1/logs
    retry_on_failure:
      enabled: true
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 10000
%s`, externalExtension, externalExporter)
	if cfg.EnableTraces {
		b.WriteString(`  otlp/tempo:
    endpoint: tempo:4317
    tls:
      insecure: true
    retry_on_failure:
      enabled: true
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 10000
`)
	}
	fmt.Fprintf(&b, `
service:
  extensions: [%s]
  telemetry:
    metrics:
      address: 127.0.0.1:8888
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: %s
    logs:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: %s
`, extensionList, metricExporters, logExporters)
	if cfg.EnableTraces {
		fmt.Fprintf(&b, `    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: %s
`, traceExporters)
	}
	return b.String()
}

func renderTempo(cfg Config) string {
	return fmt.Sprintf(`server:
  http_listen_port: 3200
  grpc_listen_port: 9095

distributor:
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: 0.0.0.0:4317
        http:
          endpoint: 0.0.0.0:4318

storage:
  trace:
    backend: local
    local:
      path: /var/tempo/blocks
    wal:
      path: /var/tempo/wal

compactor:
  compaction:
    block_retention: %dh
`, cfg.Retention.TracesDays*24)
}

func renderGrafanaEnv(cfg Config) string {
	return fmt.Sprintf(`GF_SECURITY_ADMIN_USER=admin
GF_SECURITY_ADMIN_PASSWORD=%s
GF_SERVER_HTTP_ADDR=0.0.0.0
GF_SERVER_HTTP_PORT=3000
`, cfg.GrafanaAdminPassword)
}

func renderDatasources(cfg Config, metricsToken string) string {
	extra := ""
	if cfg.EnableTraces {
		extra = `  - name: Tempo
    type: tempo
    access: proxy
    url: http://tempo:3200
    isDefault: false
`
	}
	return fmt.Sprintf(`apiVersion: 1

datasources:
  - name: VictoriaMetrics
    uid: VictoriaMetrics
    type: prometheus
    access: proxy
    url: http://vmauth-metrics:8427
    isDefault: true
    jsonData:
      httpMethod: POST
      manageAlerts: false
      httpHeaderName1: Authorization
    secureJsonData:
      httpHeaderValue1: Bearer %s
%s`, metricsToken, extra)
}

func renderVMAuth(token, target string) string {
	return fmt.Sprintf(`users:
  - bearer_token: %s
    url_prefix: %s
`, token, target)
}

func renderDashboardProvider() string {
	return `apiVersion: 1

providers:
  - name: eregion
    orgId: 1
    folder: eregion
    type: file
    disableDeletion: true
    editable: false
    options:
      path: /var/lib/grafana/dashboards
`
}

// renderAlertRules provisions one upstream-format Grafana alert rule file. The
// rules watch VictoriaMetrics self-monitoring data that the stack actually
// produces; they do not assume an application already exports a metric.
func renderAlertRules() string {
	return `apiVersion: 1

groups:
  - orgId: 1
    name: eregion-telemetry
    folder: eregion
    interval: 1m
    rules:
      - uid: eregion-telemetry-disk-low
        title: Telemetry storage has less than 5 GiB free
        condition: A
        data:
          - refId: A
            relativeTimeRange:
              from: 600
              to: 0
            datasourceUid: VictoriaMetrics
            model:
              editorMode: code
              expr: vm_free_disk_space_bytes < 5e9
              instant: false
              intervalMs: 60000
              maxDataPoints: 43200
              range: true
              refId: A
        noDataState: NoData
        execErrState: Error
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: Telemetry node storage is below the 5 GiB safety margin.
          description: Free space reported by VictoriaMetrics has stayed below 5 GiB for ten minutes. Retention or disk size needs operator attention.
      - uid: eregion-telemetry-scrape-missing
        title: Telemetry storage self-monitoring is absent
        condition: A
        data:
          - refId: A
            relativeTimeRange:
              from: 600
              to: 0
            datasourceUid: VictoriaMetrics
            model:
              editorMode: code
              expr: absent(vm_app_start_time_seconds)
              instant: false
              intervalMs: 60000
              maxDataPoints: 43200
              range: true
              refId: A
        noDataState: Alerting
        execErrState: Error
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: VictoriaMetrics self-monitoring data is absent.
          description: The telemetry storage backend has not reported its own metrics for five minutes; this is not proof that application telemetry is healthy.
`
}

// renderDashboard intentionally uses VictoriaMetrics self-monitoring metrics, not
// invented application metrics. Absent data is rendered by Grafana as no data.
func renderDashboard(cfg Config) string {
	return `{
  "annotations": {"list": []},
  "editable": false,
  "graphTooltip": 0,
  "panels": [
    {
      "datasource": {"type": "prometheus", "uid": "VictoriaMetrics"},
      "fieldConfig": {"defaults": {"unit": "percent"}, "overrides": []},
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0},
      "id": 1,
      "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "single"}},
      "targets": [{"expr": "100 * (1 - vm_free_disk_space_bytes / vm_free_disk_space_bytes + 0)", "refId": "A"}],
      "title": "Telemetry node disk availability",
      "type": "timeseries"
    },
    {
      "datasource": {"type": "prometheus", "uid": "VictoriaMetrics"},
      "fieldConfig": {"defaults": {"unit": "bytes"}, "overrides": []},
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 0},
      "id": 2,
      "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "single"}},
      "targets": [{"expr": "vm_free_disk_space_bytes", "refId": "A"}],
      "title": "Telemetry node free disk space",
      "type": "timeseries"
    }
  ],
  "refresh": "30s",
  "schemaVersion": 39,
  "tags": ["eregion", "telemetry"],
  "templating": {"list": []},
  "time": {"from": "now-6h", "to": "now"},
  "timezone": "utc",
  "title": "eregion telemetry",
  "uid": "eregion-telemetry",
  "version": 1
}`
}

// RenderAgentPlan renders the collector agent for one application or builder
// node. Its token file is 0600 and is the only rendered secret.
func RenderAgentPlan(prepared PreparedAgent) (Plan, error) {
	cfg := prepared.AgentConfig
	if _, err := PrepareAgent(cfg); err != nil {
		return Plan{}, err
	}
	root := cfg.RootDir
	stackDir := filepath.Join(root, "stack")
	files := []PlanFile{
		{Path: filepath.Join(stackDir, "compose.yaml"), Data: []byte(renderAgentCompose(cfg)), Mode: 0640},
		{Path: filepath.Join(stackDir, "collector", "config.yaml"), Data: []byte(renderAgentCollector(cfg)), Mode: 0640},
		{Path: filepath.Join(stackDir, "tls", "ca.crt"), Data: cfg.TLSCAPEM, Mode: 0644},
		{Path: filepath.Join(stackDir, "collector", "ingest-token"), Data: []byte(cfg.Token + "\n"), Mode: 0600, Secret: true},
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	composePath := filepath.Join(stackDir, "compose.yaml")
	return Plan{
		RootDir: root,
		Files:   files,
		Commands: []Command{
			{Name: "docker", Args: []string{"compose", "--file", composePath, "pull"}},
			{Name: "docker", Args: []string{"compose", "--file", composePath, "up", "--detach", "--remove-orphans", "--wait"}},
		},
	}, nil
}

func renderAgentCompose(cfg AgentConfig) string {
	return fmt.Sprintf(`name: eregion-otel-agent

services:
  otel-agent:
    image: otel/opentelemetry-collector-contrib:%s
    command: ["--config=/etc/otelcol/config.yaml"]
    restart: unless-stopped
    volumes:
      - ./collector/config.yaml:/etc/otelcol/config.yaml:ro
      - ./collector/ingest-token:/etc/otelcol/ingest-token:ro
      - ./tls:/etc/otelcol/tls:ro
      - /var/lib/docker/containers:/var/lib/docker/containers:ro
      - /var/log:/var/log:ro
      - /:/hostfs:ro
      - agent-state:/var/lib/otelcol
    ports:
      - "%s:4317:4317"
      - "%s:4318:4318"
    read_only: true
    tmpfs:
      - /tmp
    security_opt:
      - no-new-privileges:true

volumes:
  agent-state:
`, cfg.Versions.Collector, cfg.Node.PrivateAddress, cfg.Node.PrivateAddress)
}

func renderAgentCollector(cfg AgentConfig) string {
	var includes strings.Builder
	for _, path := range cfg.LogPaths {
		fmt.Fprintf(&includes, "        - %q\n", path)
	}
	return fmt.Sprintf(`extensions:
  bearertokenauth/telemetry:
    scheme: Bearer
    filename: /etc/otelcol/ingest-token
  file_storage:
    directory: /var/lib/otelcol
  health_check:
    endpoint: 127.0.0.1:13133

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
  hostmetrics:
    root_path: /hostfs
    collection_interval: 30s
    scrapers:
      cpu:
      disk:
      filesystem:
      load:
      memory:
      network:
  filelog:
    include:
%s    start_at: end
    include_file_path: true
    operators:
      - type: container
        id: container-parser

processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 256
    spike_limit_mib: 64
  batch:
    timeout: 5s
    send_batch_size: 4096
  resourcedetection:
    detectors: [env, system]
    system:
      hostname_sources: [os]
    override: false

exporters:
  otlp/telemetry:
    endpoint: %s
    tls:
      ca_file: /etc/otelcol/tls/ca.crt
    auth:
      authenticator: bearertokenauth/telemetry
    retry_on_failure:
      enabled: true
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 10000

service:
  extensions: [bearertokenauth/telemetry, file_storage, health_check]
  telemetry:
    metrics:
      address: 127.0.0.1:8888
  pipelines:
    metrics:
      receivers: [otlp, hostmetrics]
      processors: [memory_limiter, resourcedetection, batch]
      exporters: [otlp/telemetry]
    logs:
      receivers: [otlp, filelog]
      processors: [memory_limiter, resourcedetection, batch]
      exporters: [otlp/telemetry]
    traces:
      receivers: [otlp]
      processors: [memory_limiter, resourcedetection, batch]
      exporters: [otlp/telemetry]
`, includes.String(), cfg.TelemetryAddress)
}

// DataSourceEndpoints are the private read endpoints the control plane or CLI
// may query. They intentionally have no public bind.
func (p Prepared) DataSourceEndpoints() map[string]string {
	return map[string]string{
		"metrics": netJoin(p.Node.PrivateAddress, "8427"),
		"logs":    netJoin(p.Node.PrivateAddress, "9427"),
		"grafana": netJoin(p.Node.PrivateAddress, "3000"),
	}
}

func netJoin(host, port string) string {
	return host + ":" + port
}
