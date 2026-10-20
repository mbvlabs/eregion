package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPDataSourceMetricsStates(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	mode := "ok"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		switch mode {
		case "error":
			http.Error(w, "backend secret detail", http.StatusInternalServerError)
		case "malformed":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[["bad","1"]]}]}}`))
		case "empty":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		case "stale":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"host":"app-1"},"values":[[%d,"4"]]}]}}`, now.Add(-time.Hour).Unix())))
		default:
			_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"host":"app-1"},"values":[[%d,"4"]]}]}}`, now.Add(-time.Minute).Unix())))
		}
	}))
	defer server.Close()
	source, err := NewHTTPDataSource(HTTPConfig{
		MetricsURL: server.URL, LogsURL: server.URL, StaleAfter: 5 * time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	query := MetricQuery{
		Window: Window{Start: now.Add(-2 * time.Hour), End: now}, Source: "app-1",
		Expression: "sum(up)", Unit: "count", Step: time.Minute,
	}
	result, err := source.Metrics(context.Background(), query)
	if err != nil || result.State != StateOK || len(result.Series) != 1 || result.ObservedAt.IsZero() {
		t.Fatalf("ok metrics = %+v, %v", result, err)
	}
	if result.Unit != "count" || result.Source != "app-1" {
		t.Fatal("source or unit was not preserved")
	}
	mode = "stale"
	result, err = source.Metrics(context.Background(), query)
	if err != nil || result.State != StateStale || len(result.Series) != 1 {
		t.Fatalf("stale metrics = %+v, %v", result, err)
	}
	mode = "empty"
	result, err = source.Metrics(context.Background(), query)
	if err != nil || result.State != StateNoData || len(result.Series) != 0 {
		t.Fatalf("empty metrics = %+v, %v", result, err)
	}
	mode = "error"
	result, err = source.Metrics(context.Background(), query)
	if err == nil || result.State != StateError || strings.Contains(result.Message, "secret") {
		t.Fatalf("error metrics = %+v, %v", result, err)
	}
	mode = "malformed"
	result, err = source.Metrics(context.Background(), query)
	if err == nil || result.State != StateError {
		t.Fatalf("malformed metrics = %+v, %v", result, err)
	}
	if _, err := source.Metrics(context.Background(), MetricQuery{Window: query.Window, Source: "app", Expression: "up"}); err == nil {
		t.Fatal("metrics query without a unit was accepted")
	}
}

func TestHTTPDataSourceLogsStatesAndFields(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	mode := "ok"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("query") == "" || r.URL.Query().Get("start") == "" || r.URL.Query().Get("end") == "" {
			http.Error(w, "missing params", http.StatusBadRequest)
			return
		}
		switch mode {
		case "empty":
			return
		case "stale":
			_, _ = w.Write([]byte(`{"_time":"2026-10-20T10:00:00Z","_msg":"old","_stream":"{host=\"app-1\"}"}` + "\n"))
		case "malformed":
			_, _ = w.Write([]byte(`{"_time":"not-a-time","_msg":"x"}`))
		default:
			_, _ = w.Write([]byte(`{"_time":"2026-10-20T11:59:00Z","_msg":"hello","_stream":"{host=\"app-1\"}","level":"info","count":2}` + "\n"))
		}
	}))
	defer server.Close()
	source, err := NewHTTPDataSource(HTTPConfig{
		MetricsURL: server.URL, LogsURL: server.URL, StaleAfter: 5 * time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	query := LogQuery{Window: Window{Start: now.Add(-time.Hour), End: now}, Source: `{host="app-1"}`, Limit: 10}
	result, err := source.Logs(context.Background(), query)
	if err != nil || result.State != StateOK || len(result.Entries) != 1 {
		t.Fatalf("ok logs = %+v, %v", result, err)
	}
	if result.Entries[0].Message != "hello" || result.Entries[0].Source != `{host="app-1"}` || result.Entries[0].Fields["level"] != "info" || result.Entries[0].Fields["count"] != "2" {
		t.Fatalf("unexpected log entry: %+v", result.Entries[0])
	}
	if result.Unit != "entries" {
		t.Fatal("logs did not report a unit")
	}
	mode = "stale"
	result, err = source.Logs(context.Background(), query)
	if err != nil || result.State != StateStale {
		t.Fatalf("stale logs = %+v, %v", result, err)
	}
	mode = "empty"
	result, err = source.Logs(context.Background(), query)
	if err != nil || result.State != StateNoData {
		t.Fatalf("empty logs = %+v, %v", result, err)
	}
	mode = "malformed"
	result, err = source.Logs(context.Background(), query)
	if err == nil || result.State != StateError {
		t.Fatalf("malformed logs = %+v, %v", result, err)
	}
}

func TestHTTPDataSourceRejectsPublicAndQueryURLs(t *testing.T) {
	for _, endpoint := range []string{
		"http://203.0.113.1:8428", "https://user:pass@example.com", "https://example.com/path",
		"https://example.com?token=x", "ftp://example.com",
	} {
		if _, err := NewHTTPDataSource(HTTPConfig{MetricsURL: endpoint, LogsURL: "http://127.0.0.1:9428"}); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
}
