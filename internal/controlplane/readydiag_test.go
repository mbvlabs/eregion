package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	if len(report.stages[0].queryDetails) != 1 || report.stages[0].queryDetails[0].ordinal != 1 ||
		report.stages[0].queryDetails[0].elapsed != 2*time.Second || !report.stages[0].queryDetails[0].failed {
		t.Fatalf("query ordinal report = %+v", report.stages[0].queryDetails)
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

type readinessTestTimeout struct{}

func (readinessTestTimeout) Error() string   { return "canary timeout" }
func (readinessTestTimeout) Timeout() bool   { return true }
func (readinessTestTimeout) Temporary() bool { return true }

func TestReadinessErrorClassification(t *testing.T) {
	pgError := &pgconn.PgError{Code: "57014", Message: "canary query detail"}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "wrapped canceled", err: fmt.Errorf("canary: %w", context.Canceled), want: readinessErrorCanceled},
		{name: "wrapped deadline", err: fmt.Errorf("canary: %w", context.DeadlineExceeded), want: readinessErrorDeadline},
		{name: "postgres", err: fmt.Errorf("canary: %w", pgError), want: readinessErrorPostgres},
		{name: "fault-wrapped postgres", err: unavailable(pgError), want: readinessErrorPostgres},
		{name: "network timeout", err: fmt.Errorf("canary: %w", readinessTestTimeout{}), want: readinessErrorNetworkTimeout},
		{name: "network operation", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("canary reset")}, want: readinessErrorNetworkIO},
		{name: "eof", err: fmt.Errorf("canary: %w", io.EOF), want: readinessErrorNetworkIO},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: readinessErrorNetworkIO},
		{name: "closed pipe", err: io.ErrClosedPipe, want: readinessErrorNetworkIO},
		{name: "other", err: errors.New("canary detail"), want: readinessErrorOther},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyReadinessError(test.err); got != test.want {
				t.Fatalf("classification = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReadinessContextState(t *testing.T) {
	active := context.Background()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, deadlineCancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer deadlineCancel()

	for _, test := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "active", ctx: active, want: readinessContextActive},
		{name: "canceled", ctx: canceled, want: readinessContextCanceled},
		{name: "deadline", ctx: deadline, want: readinessContextDeadline},
		{name: "nil", ctx: nil, want: readinessContextActive},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := readinessContextState(test.ctx); got != test.want {
				t.Fatalf("context state = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReadinessContextDoesNotMaskActualError(t *testing.T) {
	pgError := &pgconn.PgError{Code: "57014", Message: "canary query detail"}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, deadlineCancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer deadlineCancel()

	for _, test := range []struct {
		name        string
		ctx         context.Context
		err         error
		wantClass   string
		wantContext string
	}{
		{name: "canceled context with postgres error", ctx: canceled, err: pgError, wantClass: readinessErrorPostgres, wantContext: readinessContextCanceled},
		{name: "active context with deadline error", ctx: context.Background(), err: context.DeadlineExceeded, wantClass: readinessErrorDeadline, wantContext: readinessContextActive},
		{name: "deadline context with io error", ctx: deadline, err: io.EOF, wantClass: readinessErrorNetworkIO, wantContext: readinessContextDeadline},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace := newReadyTrace(nil)
			trace.finishContext(test.ctx, readyPoolSnapshot{}, test.err)
			report := trace.report()
			if report.errorClass != test.wantClass || report.contextState != test.wantContext {
				t.Fatalf("report class/context = %q/%q, want %q/%q", report.errorClass, report.contextState, test.wantClass, test.wantContext)
			}
			if !report.failed {
				t.Fatal("a non-nil error did not mark the report failed")
			}
		})
	}
}

func TestReadinessTracePerStageQueryOrdinalsAreBounded(t *testing.T) {
	trace := newReadyTrace(nil)
	stage := trace.addStage("river_versions")
	for i := 1; i <= maxReadyStageQueryDetails+2; i++ {
		trace.recordQuery(stage, time.Duration(i)*time.Millisecond, i == 5)
	}
	report := trace.report()

	if len(report.stages) != 1 || len(report.stages[0].queryDetails) != maxReadyStageQueryDetails {
		t.Fatalf("bounded query details = %+v", report.stages)
	}
	if report.stages[0].omittedQueries != 2 || report.stages[0].queries != maxReadyStageQueryDetails+2 {
		t.Fatalf("query cap accounting = %+v", report.stages[0])
	}
	if detail := report.stages[0].queryDetails[4]; detail.ordinal != 5 || !detail.failed || detail.elapsed != 5*time.Millisecond {
		t.Fatalf("failed query ordinal = %+v", detail)
	}
	summary := report.stageSummary()
	for _, want := range []string{"river_versions:queries=10", "q1=1ms/ok", "q5=5ms/failed", "more=2"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("stage summary missing %q: %s", want, summary)
		}
	}
}

func TestReadinessFailureSummaryAttributionAndRedaction(t *testing.T) {
	const canary = "postgres://user:super-secret@db.example/private?token=canary"
	now := time.Unix(1350, 0)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	readyLog := newReadinessLog(logger, func() time.Time { return now }, time.Second, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan := func(context.Context, *pgxpool.Pool) (MigrationPlan, error) {
		return MigrationPlan{}, fmt.Errorf("%s: %w", canary, unavailable(&pgconn.PgError{Code: "57014", Message: canary}))
	}
	err := runReady(ctx, nil, readyConfig{
		log:     readyLog,
		now:     func() time.Time { return now },
		runtime: ReadinessRuntimeAPI,
		phase:   ReadinessPhaseRequest,
	}, plan)
	if err == nil {
		t.Fatal("readiness returned no failure")
	}
	output := logs.String()
	for _, want := range []string{
		"control-plane readiness failed",
		"runtime=api", "phase=request", "context_state=canceled", "error_class=postgres",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("failure summary missing %q: %s", want, output)
		}
	}
	for _, secret := range []string{canary, "postgres://", "super-secret", "canary query detail"} {
		if strings.Contains(output, secret) {
			t.Fatalf("failure summary leaked %q: %s", secret, output)
		}
	}
}

func TestReadinessAttributionNormalizesUnknown(t *testing.T) {
	config := readyConfig{}
	WithReadinessAttribution("not-a-runtime", "not-a-phase")(&config)
	if got := normalizeReadinessRuntime(config.runtime); got != ReadinessRuntimeUnknown {
		t.Fatalf("runtime normalized to %q, want unknown", got)
	}
	if got := normalizeReadinessPhase(config.phase); got != ReadinessPhaseUnknown {
		t.Fatalf("phase normalized to %q, want unknown", got)
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

func TestReadinessRefusesNewerRiverSchema(t *testing.T) {
	// ExistingVersions rejects an unknown newer version before this guard; the
	// explicit plan check remains covered independently.
	err := checkRiverPlan(MigrationPlan{Current: 2, Required: 2, RiverCurrent: 8, RiverRequired: 7})
	if contract.Classify(err).Code != contract.PreconditionFailed || !strings.Contains(err.Error(), "River schema is newer") {
		t.Fatalf("newer River schema not refused: %v", err)
	}
	if err := checkRiverPlan(MigrationPlan{Current: 2, Required: 2, RiverCurrent: 7, RiverRequired: 7}); err != nil {
		t.Fatalf("current River schema was refused: %v", err)
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
		"runtime=unknown", "phase=unknown", "context_state=active",
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
	if !strings.Contains(lines[1], "failed_stage=schema_table") || !strings.Contains(lines[1], "error_class=other") {
		t.Fatalf("failure attribution missing: %s", lines[1])
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
