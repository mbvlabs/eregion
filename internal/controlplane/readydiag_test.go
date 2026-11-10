package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

func TestReadinessTraceAcquireQueryAndLeaseRelease(t *testing.T) {
	now := time.Unix(1000, 0)
	trace := newReadyTrace(func() time.Time { return now })
	tracer := &readinessTracer{}
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	ctx, endStage := beginReadyStage(ctx, "schema_migrations")

	acquireCtx := tracer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
	now = now.Add(250 * time.Millisecond)
	conn := &pgx.Conn{}
	tracer.TraceAcquireEnd(acquireCtx, nil, pgxpool.TraceAcquireEndData{Conn: conn})

	queryCtx := tracer.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{
		SQL:  "SELECT secret FROM private_table",
		Args: []any{"postgres://user:do-not-print@db.example/secret"},
	})
	now = now.Add(2 * time.Second) // Includes row lifetime until Close.
	tracer.TraceQueryEnd(queryCtx, conn, pgx.TraceQueryEndData{Err: errors.New("driver detail do-not-print")})
	tracer.TraceRelease(nil, pgxpool.TraceReleaseData{Conn: conn})
	endStage(nil)
	now = now.Add(100 * time.Millisecond)
	trace.poolBefore = readyPoolSnapshot{idle: 2, total: 2, max: 12}
	trace.finish(readyPoolSnapshot{acquired: 1, total: 2, max: 12}, false)
	report := trace.report()

	if report.acquires != 1 || report.acquireTime != 250*time.Millisecond || report.acquireErrs != 0 {
		t.Fatalf("acquire report = %+v", report)
	}
	if report.queries != 1 || report.queryTime != 2*time.Second || report.queryErrs != 1 {
		t.Fatalf("query report = %+v", report)
	}
	if len(report.stages) != 1 || report.stages[0].name != "schema_migrations" ||
		report.stages[0].queries != 1 || report.stages[0].elapsed != 2250*time.Millisecond {
		t.Fatalf("stage report = %+v", report.stages)
	}
	if report.leases != 1 || report.released != 1 {
		t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
	}
	if report.total != 2350*time.Millisecond {
		t.Fatalf("total = %v", report.total)
	}
	dump := fmt.Sprintf("%+v", report)
	for _, secret := range []string{"SELECT secret", "do-not-print", "postgres://"} {
		if strings.Contains(dump, secret) {
			t.Fatalf("report leaked %q: %s", secret, dump)
		}
	}
}

func TestReadinessTraceAcquireFailure(t *testing.T) {
	now := time.Unix(1100, 0)
	trace := newReadyTrace(func() time.Time { return now })
	tracer := &readinessTracer{}
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	ctx, endStage := beginReadyStage(ctx, "river_versions")

	acquireCtx := tracer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
	now = now.Add(75 * time.Millisecond)
	tracer.TraceAcquireEnd(acquireCtx, nil, pgxpool.TraceAcquireEndData{Err: errors.New("dial postgres://user:do-not-print@db")})
	endStage(context.DeadlineExceeded)
	trace.finish(readyPoolSnapshot{}, true)
	report := trace.report()

	if report.acquires != 1 || report.acquireTime != 75*time.Millisecond || report.acquireErrs != 1 {
		t.Fatalf("acquire failure report = %+v", report)
	}
	if report.failedStage != "river_versions" || !report.failed {
		t.Fatalf("failed stage = %+v", report)
	}
	if dump := fmt.Sprintf("%+v", report); strings.Contains(dump, "postgres://") || strings.Contains(dump, "do-not-print") {
		t.Fatalf("report leaked a driver error: %s", dump)
	}
}

func TestRunReadyCancellationReportsGenericFailure(t *testing.T) {
	started := make(chan struct{})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	clock := func() time.Time { return time.Unix(1200, 0) }
	readyLog := newReadinessLog(logger, clock, time.Second, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plan := func(ctx context.Context, _ *pgxpool.Pool) (MigrationPlan, error) {
		close(started)
		<-ctx.Done()
		return MigrationPlan{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		done <- runReady(ctx, nil, readyConfig{log: readyLog, now: clock}, plan)
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readiness did not return after cancellation")
	}
	output := logs.String()
	if !strings.Contains(output, "control-plane readiness failed") {
		t.Fatalf("missing bounded failure summary: %s", output)
	}
	for _, detail := range []string{"context canceled", "deadline exceeded", tokenLikeValue} {
		if strings.Contains(output, detail) {
			t.Fatalf("readiness log leaked %q: %s", detail, output)
		}
	}
}

func TestReadyStillChecksMigrationVersions(t *testing.T) {
	err := runReady(context.Background(), nil, readyConfig{}, func(context.Context, *pgxpool.Pool) (MigrationPlan, error) {
		return MigrationPlan{Current: 1, Required: 2, RiverCurrent: 7, RiverRequired: 7}, nil
	})
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("pending migrations not refused: %v", err)
	}
}

func TestReadinessLogRateLimitAndSuppression(t *testing.T) {
	now := time.Unix(1300, 0)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	readyLog := newReadinessLog(logger, func() time.Time { return now }, time.Second, 10*time.Second)

	slow := readyReport{
		total:       1500 * time.Millisecond,
		stages:      []readyStageReport{{name: "river_versions", elapsed: 1400 * time.Millisecond, queries: 3, queryTime: 1400 * time.Millisecond}},
		acquires:    4,
		acquireTime: 300 * time.Millisecond,
		queries:     3,
		queryTime:   1400 * time.Millisecond,
		poolBefore:  readyPoolSnapshot{idle: 12, total: 12, max: 12},
		poolAfter:   readyPoolSnapshot{acquired: 1, idle: 11, total: 12, max: 12},
	}
	readyLog.report(slow)
	readyLog.report(readyReport{total: 1500 * time.Millisecond})
	now = now.Add(5 * time.Second)
	readyLog.report(readyReport{failed: true, total: 20 * time.Millisecond, failedStage: "schema_table"})
	lines := nonEmptyLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("expected one rate-limited summary, got %d: %s", len(lines), logs.String())
	}
	for _, want := range []string{
		"total_ms=1500", "river_versions:queries=3,ms=1400",
		"acquires=4", "acquire_ms=300", "queries=3", "query_ms=1400",
		`pool_before="acquired=0 idle=12 total=12 max=12`, `pool_after="acquired=1 idle=11 total=12 max=12`,
	} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("summary missing %q: %s", want, lines[0])
		}
	}
	now = now.Add(5 * time.Second)
	readyLog.report(readyReport{failed: true, total: 20 * time.Millisecond, failedStage: "schema_table"})
	lines = nonEmptyLines(logs.String())
	if len(lines) != 2 {
		t.Fatalf("expected a second summary after the interval, got %d: %s", len(lines), logs.String())
	}
	if !strings.Contains(lines[1], "suppressed=2") {
		t.Fatalf("suppression count missing: %s", lines[1])
	}
	if !strings.Contains(lines[1], "failed_stage=schema_table") {
		t.Fatalf("failure stage missing: %s", lines[1])
	}
	readyLog.report(readyReport{total: 50 * time.Millisecond})
	if got := len(nonEmptyLines(logs.String())); got != 2 {
		t.Fatalf("a fast successful check was logged: %d lines", got)
	}
	for _, secret := range []string{"postgres://", "SELECT ", "password"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("readiness log leaked %q: %s", secret, logs.String())
		}
	}
}

func TestReadinessTracerIgnoresUntracedPoolUse(t *testing.T) {
	tracer := &readinessTracer{}
	ctx := tracer.TraceAcquireStart(context.Background(), nil, pgxpool.TraceAcquireStartData{})
	conn := &pgx.Conn{}
	tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Conn: conn})
	tracer.TraceRelease(nil, pgxpool.TraceReleaseData{Conn: conn})
	if readyTraceFrom(ctx) != nil {
		t.Fatal("untraced pool use acquired a readiness trace")
	}
}

const tokenLikeValue = "0123456789abcdef"

func nonEmptyLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
