package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/processmetrics"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Exercise the actual HTTP API handler and exporter over running listeners.
// These requests require neither a database nor credentials.
func TestIntegrationProcessMetricsRunningAPI(t *testing.T) {
	m := processmetrics.New(nil)
	service := httptest.NewServer(api.Instrument(api.NewHandler(nil, nil, nil), m))
	defer service.Close()
	exporter := httptest.NewServer(promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	defer exporter.Close()

	// An occupied exporter port must not prevent the running API from serving.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	stop := m.Start(occupied.Addr().String(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()

	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := free.Addr().String()
	_ = free.Close()
	stopExporter := m.Start(address, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response, err := http.Get("http://" + address + "/metrics")
	if err != nil {
		stopExporter()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		stopExporter()
		t.Fatalf("exporter status %d", response.StatusCode)
	}
	stopExporter()
	if listener, err := net.Listen("tcp", address); err != nil {
		t.Fatalf("exporter did not release listener: %v", err)
	} else {
		_ = listener.Close()
	}

	for _, request := range []struct {
		path   string
		status int
	}{
		{"/healthz", http.StatusOK},
		{"/v1/operations/private-identifier", http.StatusUnauthorized},
	} {
		resp, err := http.Get(service.URL + request.path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != request.status {
			t.Fatalf("%s: status %d", request.path, resp.StatusCode)
		}
	}
	resp, err := http.Get(exporter.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	assertContainsEach(t, body, []string{
		`eregion_http_requests_total{method="GET",route="health",status="200"} 1`,
		`eregion_http_requests_total{method="GET",route="api",status="401"} 1`,
		`eregion_http_request_duration_seconds_count{method="GET",route="api",status="401"} 1`,
		`eregion_http_inflight_requests{method="GET",route="api"} 0`,
		`go_goroutines`,
	})
	if strings.Contains(body, "private-identifier") {
		t.Fatal("raw request path leaked into metrics")
	}
	targets, err := telemetry.TargetsJSON("main", "node.example.com")
	if err != nil {
		t.Fatal(err)
	}
	assertContainsEach(t, string(targets), []string{telemetry.MetricsPath, telemetry.APIMetricsPath, telemetry.WorkerMetricsPath})
}

func assertContainsEach(t *testing.T, body string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Uses the repository's isolated PostgreSQL fixture and an actual River
// worker. The definition service creates a child and therefore snoozes while
// the child is queued; the diagnostic deliberately fails its first attempt.
func TestIntegrationProcessMetricsRiverPostgres(t *testing.T) {
	pool, _, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("definition plan: can_apply=%v error=%v", plan.CanApply, err)
	}
	input.PlanNonce = plan.PlanNonce
	if _, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "metrics-definition", Confirm: "api/production",
	}); err != nil {
		t.Fatal(err)
	}

	m := processmetrics.New(pool)
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 1, TestOnly: true, Transport: newScriptedDeployTransport(),
		NodeStateDir: t.TempDir(), LocalNodeName: "local", Metrics: m,
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	op, err := run.Service.Submit(ctx, submission("metrics-retry", `{"steps":1,"step_delay_ms":0,"fail_until_attempt":1}`))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a persisted input that can no longer be normalized. The real
	// platform worker settles it as failed but returns nil to River.
	failed := platformSubmitAs(t, run.Service, definitionIntegrationActor, platform.KindAppCreate, "platform", "metrics-terminal-failure", map[string]any{"name": "metrics-failure"})
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET input='{}'::jsonb WHERE id=$1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	exporter := httptest.NewServer(promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	t.Cleanup(exporter.Close)
	startWorkers(t, run)
	var scrape string
	await(t, 30*time.Second, func() bool {
		response, err := http.Get(exporter.URL)
		if err != nil {
			return false
		}
		raw, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return false
		}
		scrape = string(raw)
		return metricsWorkerStateSettled(ctx, pool, run.Service, op, failed, scrape)
	})
	assertContainsEach(t, scrape, []string{
		`eregion_worker_attempt_duration_seconds_count{action="none",job="diagnostic_test",outcome="error"} 1`,
		"eregion_action_attempt_duration_seconds_count{action=\"platform.app.create\",outcome=\"failed\"} 1\n",
		`eregion_db_pool_total_connections `,
		`eregion_db_pool_acquired_connections `,
		`eregion_db_pool_max_connections `,
	})
}

func metricsWorkerStateSettled(
	ctx context.Context,
	pool *pgxpool.Pool,
	service *Service,
	op, failed contract.Operation,
	scrape string,
) bool {
	current, err := service.Get(ctx, testActor, op.ID)
	failure, failureErr := service.Get(ctx, definitionIntegrationActor, failed.ID)
	var riverState string
	riverErr := pool.QueryRow(ctx, `SELECT state FROM river_job WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, failed.ID).Scan(&riverState)
	return err == nil && current.State == contract.Succeeded &&
		failureErr == nil && failure.State == contract.Failed &&
		riverErr == nil && riverState == "completed" &&
		strings.Contains(scrape, `eregion_worker_attempts_total{action="platform.definition.apply",job="platform_action",outcome="snoozed"}`) &&
		strings.Contains(scrape, `eregion_worker_attempts_total{action="none",job="diagnostic_test",outcome="error"} 1`) &&
		strings.Contains(scrape, `eregion_worker_attempts_total{action="none",job="diagnostic_test",outcome="success"} 1`) &&
		strings.Contains(scrape, `eregion_worker_retry_attempts_total{action="none",job="diagnostic_test"} 1`) &&
		strings.Contains(scrape, `eregion_worker_attempts_total{action="platform.app.create",job="platform_action",outcome="success"}`) &&
		strings.Contains(scrape, "eregion_action_attempts_total{action=\"platform.app.create\",outcome=\"failed\"} 1\n")
}

func TestIntegrationProcessMetricsBuildCapacityPostgres(t *testing.T) {
	pool, service, _ := definitionIntegrationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET build_concurrency=1 WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	release, err := service.acquireBuildSlot(ctx, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	m := processmetrics.New(pool)
	waiting, done := acquireBuildSlotInBackground(ctx, service, m)
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("build capacity was not observed as full")
	}
	exporter := httptest.NewServer(promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	defer exporter.Close()
	if body := scrapeProcessMetrics(t, exporter.URL); !strings.Contains(body, "eregion_worker_build_capacity_waiting 1") || !strings.Contains(body, "eregion_worker_build_capacity_waits_total 1") {
		t.Fatalf("in-progress capacity wait not exposed")
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("build capacity wait did not finish")
	}
	if body := scrapeProcessMetrics(t, exporter.URL); !strings.Contains(body, "eregion_worker_build_capacity_waiting 0") || !strings.Contains(body, "eregion_worker_build_capacity_wait_duration_seconds_count 1") {
		t.Fatal("completed capacity wait not exposed")
	}
	releaseAgain, err := service.acquireBuildSlot(ctx, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgain()
	cancelledContext, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	waitingAgain, cancelled := acquireBuildSlotInBackground(cancelledContext, service, m)
	select {
	case <-waitingAgain:
	case <-time.After(10 * time.Second):
		t.Fatal("second build capacity wait was not observed")
	}
	cancelWait()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled capacity wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled capacity wait did not finish")
	}
	if body := scrapeProcessMetrics(t, exporter.URL); !strings.Contains(body, "eregion_worker_build_capacity_waiting 0\n") ||
		!strings.Contains(body, "eregion_worker_build_capacity_waits_total 2\n") ||
		!strings.Contains(body, "eregion_worker_build_capacity_wait_duration_seconds_count 2\n") {
		t.Fatal("cancelled capacity wait did not balance its measurements")
	}
}

func scrapeProcessMetrics(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func acquireBuildSlotInBackground(ctx context.Context, service *Service, m *processmetrics.Metrics) (<-chan struct{}, <-chan error) {
	waiting := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		unlock, err := service.acquireBuildSlot(processmetrics.WithWorker(ctx, m), "local", func() { close(waiting) })
		if err == nil {
			unlock()
		}
		done <- err
	}()
	return waiting, done
}
