// Package processmetrics exposes low-cost, process-local observations.
package processmetrics

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	APIAddress    = "127.0.0.1:9102"
	WorkerAddress = "127.0.0.1:9103"
)

type Metrics struct {
	Registry              *prometheus.Registry
	Requests              *prometheus.CounterVec
	Inflight              *prometheus.GaugeVec
	HTTPDuration          *prometheus.HistogramVec
	Attempts              *prometheus.CounterVec
	AttemptDuration       *prometheus.HistogramVec
	ActionAttempts        *prometheus.CounterVec
	ActionAttemptDuration *prometheus.HistogramVec
	Retries               *prometheus.CounterVec
	CapacityWaits         prometheus.Counter
	CapacityWaiting       prometheus.Gauge
	CapacityWaitDuration  prometheus.Histogram
}

func New(pool *pgxpool.Pool) *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry()}
	m.Requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "eregion_http_requests_total", Help: "Completed HTTP requests."}, []string{"route", "method", "status"})
	m.Inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "eregion_http_inflight_requests", Help: "HTTP requests currently being handled."}, []string{"route", "method"})
	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "eregion_http_request_duration_seconds", Help: "HTTP request duration."}, []string{"route", "method", "status"})
	m.Attempts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "eregion_worker_attempts_total", Help: "River work function attempts by returned outcome; not durable operation status."}, []string{"job", "action", "outcome"})
	m.AttemptDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "eregion_worker_attempt_duration_seconds", Help: "River work function attempt duration."}, []string{"job", "action", "outcome"})
	m.ActionAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "eregion_action_attempts_total", Help: "Observed settled action attempts by domain outcome; not authoritative durable state."}, []string{"action", "outcome"})
	m.ActionAttemptDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "eregion_action_attempt_duration_seconds", Help: "Observed settled action attempt duration."}, []string{"action", "outcome"})
	m.Retries = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "eregion_worker_retry_attempts_total", Help: "River attempts with attempt number greater than one."}, []string{"job", "action"})
	m.CapacityWaits = prometheus.NewCounter(prometheus.CounterOpts{Name: "eregion_worker_build_capacity_waits_total", Help: "Build slot acquisitions that encountered full capacity."})
	m.CapacityWaiting = prometheus.NewGauge(prometheus.GaugeOpts{Name: "eregion_worker_build_capacity_waiting", Help: "Build slot acquisitions currently waiting for capacity."})
	m.CapacityWaitDuration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "eregion_worker_build_capacity_wait_duration_seconds", Help: "Time spent waiting for build capacity, including interrupted waits."})
	m.Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), m.Requests, m.Inflight, m.HTTPDuration, m.Attempts, m.AttemptDuration, m.ActionAttempts, m.ActionAttemptDuration, m.Retries, m.CapacityWaits, m.CapacityWaiting, m.CapacityWaitDuration)
	if pool != nil {
		m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "eregion_db_pool_acquired_connections", Help: "Connections currently acquired from the pool."}, func() float64 { return float64(pool.Stat().AcquiredConns()) }))
		m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "eregion_db_pool_idle_connections", Help: "Idle connections in the pool."}, func() float64 { return float64(pool.Stat().IdleConns()) }))
		m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "eregion_db_pool_total_connections", Help: "Connections currently in the pool."}, func() float64 { return float64(pool.Stat().TotalConns()) }))
		m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "eregion_db_pool_max_connections", Help: "Configured maximum connections in the pool."}, func() float64 { return float64(pool.Stat().MaxConns()) }))
	}
	return m
}

// ObserveActionAttempt is called after an action attempt's domain settlement.
// Unrecognised kinds and outcomes are collapsed rather than used as labels.
func (m *Metrics) ObserveActionAttempt(kind, outcome string, duration time.Duration) {
	if kind != operations.TestKind && kind != platform.KindDefinitionApply && !platform.IsKind(kind) {
		kind = "other"
	}
	switch outcome {
	case "succeeded", "failed", "cancelled", "retry", "retrying", "snoozed", "interrupted":
	default:
		outcome = "other"
	}
	m.ActionAttempts.WithLabelValues(kind, outcome).Inc()
	m.ActionAttemptDuration.WithLabelValues(kind, outcome).Observe(duration.Seconds())
}

type contextKey struct{}

// WithWorker attaches the process registry to a real River work invocation.
func WithWorker(ctx context.Context, m *Metrics) context.Context {
	return context.WithValue(ctx, contextKey{}, m)
}

// BeginBuildCapacityWait records a wait only after capacity is actually full.
// The returned function records its duration, including cancellation or timeout.
func BeginBuildCapacityWait(ctx context.Context) func() {
	m, _ := ctx.Value(contextKey{}).(*Metrics)
	if m == nil {
		return func() {}
	}
	m.CapacityWaits.Inc()
	m.CapacityWaiting.Inc()
	started := time.Now()
	return func() {
		m.CapacityWaiting.Dec()
		m.CapacityWaitDuration.Observe(time.Since(started).Seconds())
	}
}

// Start binds only loopback and never prevents the service from running.
func (m *Metrics) Start(address string, logger *slog.Logger) func() {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		logger.Warn("metrics listener unavailable", "address", address, "error", err)
		return func() {}
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Warn("metrics listener stopped", "address", address, "error", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	}
}
