package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

func clearTelemetryEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"EREGION_TELEMETRY_URL", "EREGION_TELEMETRY_LOGS_URL", "EREGION_TELEMETRY_METRICS_URL",
		"EREGION_TELEMETRY_LOGS_TOKEN_FILE", "EREGION_TELEMETRY_METRICS_TOKEN_FILE",
		"EREGION_TELEMETRY_LOGS_TOKEN", "EREGION_TELEMETRY_METRICS_TOKEN",
	} {
		t.Setenv(name, "")
	}
}

type telemetryServer struct {
	logs    string
	metrics string
	message string
	token   string
	now     time.Time
	calls   []string
}

func newTelemetryServer(t *testing.T) (*telemetryServer, *httptest.Server) {
	t.Helper()
	state := &telemetryServer{now: time.Now().UTC()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.calls = append(state.calls, r.URL.Path)
		switch r.URL.Path {
		case "/select/logsql/query":
			if state.token != "" && r.Header.Get("Authorization") != "Bearer "+state.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, state.logs)
		case "/api/v1/query_range":
			if state.token != "" && r.Header.Get("Authorization") != "Bearer "+state.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, state.metrics)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return state, server
}

func telemetryTestEnvironment(t *testing.T, server *httptest.Server, token string) {
	t.Helper()
	clearTelemetryEnvironment(t)
	t.Setenv("EREGION_TELEMETRY_LOGS_URL", server.URL)
	t.Setenv("EREGION_TELEMETRY_METRICS_URL", server.URL)
	t.Setenv("EREGION_TELEMETRY_LOGS_TOKEN", token)
	t.Setenv("EREGION_TELEMETRY_METRICS_TOKEN", token)
}

func logsServerBody(message string) string {
	encoded, _ := json.Marshal(message)
	return fmt.Sprintf("{\"_time\":%q,\"_msg\":%s,\"_stream\":\"{host=\\\"app-1\\\"}\",\"pod\":\"api-1\"}\n",
		time.Now().UTC().Format(time.RFC3339Nano), encoded)
}

func metricsServerBody() string {
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"host":"app-1"},"values":[[%d,"1"],[%d,"4"],[%d,"2"]]}]}}`,
		time.Now().Add(-2*time.Minute).Unix(), time.Now().Add(-time.Minute).Unix(), time.Now().Unix())
}

func TestLogsSearchJSONAndPlain(t *testing.T) {
	state, server := newTelemetryServer(t)
	state.token = "read-token-value"
	state.logs = logsServerBody("hello from the app")
	telemetryTestEnvironment(t, server, state.token)

	code, stdout, stderr := invoke(t, "logs", "search", "--source", "*", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK || envelope.Command != "eregion logs search" {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	raw, _ := json.Marshal(envelope.Data)
	var logged telemetry.LogResult
	if err := json.Unmarshal(raw, &logged); err != nil {
		t.Fatal(err)
	}
	if logged.State != telemetry.StateOK || len(logged.Entries) != 1 || logged.Entries[0].Message != "hello from the app" {
		t.Fatalf("unexpected log result: %s", raw)
	}
	if logged.Source != "*" || logged.Unit != "entries" || logged.Entries[0].Fields["pod"] != "api-1" {
		t.Fatalf("source, unit, or fields missing: %+v", logged)
	}

	code, stdout, stderr = invoke(t, "logs", "search", "--source", "*", "--since", "15m")
	if code != 0 || stderr != "" {
		t.Fatalf("plain code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"state: ok", "source: *", "unit: entries", "hello from the app", "pod=api-1"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("plain output missing %q:\n%s", want, stdout)
		}
	}
	if strings.ContainsRune(stdout, '\x1b') {
		t.Fatal("plain output contains terminal escapes")
	}
}

func TestLogsSearchSanitizesUntrustedText(t *testing.T) {
	state, server := newTelemetryServer(t)
	state.logs = logsServerBody("\x1b[31mred\x07")
	telemetryTestEnvironment(t, server, "")

	code, stdout, stderr := invoke(t, "logs", "search", "--source", "*")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.ContainsAny(stdout, "\x1b\x07") {
		t.Fatalf("untrusted terminal control reached plain output: %q", stdout)
	}
	if !strings.Contains(stdout, `\u001b`) || !strings.Contains(stdout, `\u0007`) {
		t.Fatalf("control characters were not made visible: %q", stdout)
	}
}

func TestLogsSearchNoTelemetryNodeIsAnExplicitState(t *testing.T) {
	clearTelemetryEnvironment(t)
	code, stdout, stderr := invoke(t, "logs", "search", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("absent telemetry is not an error: %+v", envelope.Error)
	}
	raw, _ := json.Marshal(envelope.Data)
	var logged telemetry.LogResult
	if err := json.Unmarshal(raw, &logged); err != nil {
		t.Fatal(err)
	}
	if logged.State != telemetry.StateAbsent || len(logged.Entries) != 0 || logged.Message == "" {
		t.Fatalf("absent state was not explicit: %s", raw)
	}
}

func TestLogsTailOnceEmitsNDJSON(t *testing.T) {
	state, server := newTelemetryServer(t)
	state.logs = logsServerBody("tail line")
	telemetryTestEnvironment(t, server, "")

	code, stdout, stderr := invoke(t, "logs", "tail", "--source", "*", "--once", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one log and one result record, got %d:\n%s", len(lines), stdout)
	}
	var logRecord struct {
		Schema string              `json:"schema"`
		Type   string              `json:"type"`
		Log    *telemetry.LogEntry `json:"log"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &logRecord); err != nil || logRecord.Type != "log" || logRecord.Log == nil || logRecord.Log.Message != "tail line" {
		t.Fatalf("invalid log record: %s %v", lines[0], err)
	}
	var resultRecord struct {
		Type   string             `json:"type"`
		Result *contract.Envelope `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resultRecord); err != nil || resultRecord.Type != "result" || resultRecord.Result == nil || !resultRecord.Result.OK {
		t.Fatalf("missing terminal result: %s %v", lines[1], err)
	}
}

func TestLogsTailNDJSONFlagWorksInPlainMode(t *testing.T) {
	state, server := newTelemetryServer(t)
	state.logs = logsServerBody("ndjson line")
	telemetryTestEnvironment(t, server, "")

	code, stdout, stderr := invoke(t, "logs", "tail", "--source", "*", "--once", "--ndjson")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	records := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(records) != 2 {
		t.Fatalf("expected NDJSON records, got %q", stdout)
	}
	var record output.Record
	if err := json.Unmarshal([]byte(records[0]), &record); err != nil || record.Schema != contract.Schema || record.Type != "log" {
		t.Fatalf("invalid NDJSON record: %s %v", records[0], err)
	}
}

func TestLogsTailSupportsOutputNDJSON(t *testing.T) {
	state, server := newTelemetryServer(t)
	state.logs = logsServerBody("output ndjson line")
	telemetryTestEnvironment(t, server, "")

	code, stdout, stderr := invoke(t, "logs", "tail", "--source", "*", "--once", "--output", "ndjson")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	records := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(records) != 2 {
		t.Fatalf("expected NDJSON records, got %q", stdout)
	}
	for _, line := range records {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid NDJSON record %q: %v", line, err)
		}
	}
}

func TestLogsTailNoDataIsNotAFailure(t *testing.T) {
	state, server := newTelemetryServer(t)
	state.logs = ""
	telemetryTestEnvironment(t, server, "")

	code, stdout, stderr := invoke(t, "logs", "tail", "--source", "*", "--once")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "No log entries matched") {
		t.Fatalf("no-data state was not explicit: %q", stdout)
	}
}

func TestMetricsQueryJSONAndGraph(t *testing.T) {
	state, server := newTelemetryServer(t)
	state.metrics = metricsServerBody()
	telemetryTestEnvironment(t, server, "")

	code, stdout, stderr := invoke(t, "metrics", "query", "--source", "app-1", "--expression", "up", "--unit", "count", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	raw, _ := json.Marshal(envelope.Data)
	var metrics telemetry.MetricResult
	if err := json.Unmarshal(raw, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.State != telemetry.StateOK || metrics.Unit != "count" || len(metrics.Series) != 1 || len(metrics.Series[0].Samples) != 3 {
		t.Fatalf("unexpected metric result: %s", raw)
	}

	code, stdout, stderr = invoke(t, "metrics", "query", "--source", "app-1", "--expression", "up", "--unit", "count", "--graph")
	if code != 0 || stderr != "" {
		t.Fatalf("graph code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "series {host=app-1}") || !strings.ContainsAny(stdout, "▁▂▃▄▅▆▇█") {
		t.Fatalf("sparkline was not rendered:\n%s", stdout)
	}
}

func TestTelemetryBackendFailureIsUnavailable(t *testing.T) {
	clearTelemetryEnvironment(t)
	t.Setenv("EREGION_TELEMETRY_LOGS_URL", "http://127.0.0.1:1")
	t.Setenv("EREGION_TELEMETRY_METRICS_URL", "http://127.0.0.1:2")
	code, stdout, stderr := invoke(t, "logs", "search", "--source", "*", "--json")
	if code != 6 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	envelope := decodeOne(t, stdout)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.Unavailable {
		t.Fatalf("unreachable backend was not unavailable: %+v", envelope)
	}
}
