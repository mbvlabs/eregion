package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	// A readiness request is only logged once it is slow or fails. The
	// interval bounds how many such summaries one process emits.
	defaultReadinessSlow     = time.Second
	defaultReadinessInterval = 10 * time.Second

	// Only the first queries in a stage retain ordinal timing. The cap keeps a
	// report small even if a driver or schema step issues an unexpected batch.
	maxReadyStageQueryDetails = 8
)

// ReadinessRuntime identifies the process that owns a readiness check. Unknown
// is the safe default: a new call site must opt in rather than be mislabelled
// as an API request.
type ReadinessRuntime string

const (
	ReadinessRuntimeAPI     ReadinessRuntime = "api"
	ReadinessRuntimeWorker  ReadinessRuntime = "worker"
	ReadinessRuntimeNodeRPC ReadinessRuntime = "node_rpc"
	ReadinessRuntimeUnknown ReadinessRuntime = "unknown"
)

// ReadinessPhase distinguishes process startup from a request-triggered check.
type ReadinessPhase string

const (
	ReadinessPhaseStartup ReadinessPhase = "startup"
	ReadinessPhaseRequest ReadinessPhase = "request"
	ReadinessPhaseUnknown ReadinessPhase = "unknown"
)

const (
	readinessErrorCanceled       = "canceled"
	readinessErrorDeadline       = "deadline"
	readinessErrorPostgres       = "postgres"
	readinessErrorNetworkTimeout = "network_timeout"
	readinessErrorNetworkIO      = "network_io"
	readinessErrorOther          = "other"

	readinessContextActive   = "active"
	readinessContextCanceled = "canceled"
	readinessContextDeadline = "deadline"
)

func normalizeReadinessRuntime(runtime ReadinessRuntime) ReadinessRuntime {
	switch runtime {
	case ReadinessRuntimeAPI, ReadinessRuntimeWorker, ReadinessRuntimeNodeRPC:
		return runtime
	default:
		return ReadinessRuntimeUnknown
	}
}

func normalizeReadinessPhase(phase ReadinessPhase) ReadinessPhase {
	switch phase {
	case ReadinessPhaseStartup, ReadinessPhaseRequest:
		return phase
	default:
		return ReadinessPhaseUnknown
	}
}

// classifyReadinessError reduces an error chain to a fixed diagnostic token.
// It inspects the actual error, not the context state: a canceled request that
// also received a PostgreSQL error is reported as postgres, with the separate
// context_state saying canceled. No error text is retained.
func classifyReadinessError(err error) string {
	if err == nil {
		return ""
	}
	// Fixed priority: cancellation, deadline, server error, network timeout,
	// network I/O, then an explicit other.
	if errors.Is(err, context.Canceled) {
		return readinessErrorCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return readinessErrorDeadline
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return readinessErrorPostgres
	}
	if pgconn.Timeout(err) {
		return readinessErrorNetworkTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return readinessErrorNetworkTimeout
		}
		return readinessErrorNetworkIO
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return readinessErrorNetworkIO
	}
	return readinessErrorOther
}

func readinessContextState(ctx context.Context) string {
	if ctx == nil {
		return readinessContextActive
	}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return readinessContextCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return readinessContextDeadline
	default:
		return readinessContextActive
	}
}

type (
	readyTraceKey   struct{}
	readyStageKey   struct{}
	readyAcquireKey struct{}
	readyQueryKey   struct{}
)

// readinessTracer is attached to the pgx pool before it is created. It records
// only work whose context carries a readiness trace, so ordinary pool users pay
// for nothing but a context lookup. It never reads SQL text, arguments, DSNs or
// driver errors into a report.
type readinessTracer struct {
	leases sync.Map // *pgx.Conn -> *readyTrace
}

// Acquire timing covers the whole pool.Acquire call, including an idle
// connection ping or reconnect. It is not a pure saturation-wait measurement.
func (t *readinessTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	trace := readyTraceFrom(ctx)
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, readyAcquireKey{}, trace.now())
}

func (t *readinessTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	trace := readyTraceFrom(ctx)
	if data.Conn != nil {
		if trace == nil || data.Err != nil {
			t.leases.Delete(data.Conn)
		} else {
			t.leases.Store(data.Conn, trace)
			trace.recordLease()
		}
	}
	if trace == nil {
		return
	}
	if started, ok := ctx.Value(readyAcquireKey{}).(time.Time); ok {
		trace.recordAcquire(trace.now().Sub(started), data.Err != nil)
		return
	}
	trace.recordAcquire(0, data.Err != nil)
}

// TraceRelease attributes a released connection to the readiness request that
// acquired it. A release without a tracked acquisition is ordinary pool use.
func (t *readinessTracer) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	if data.Conn == nil {
		return
	}
	if tracked, ok := t.leases.LoadAndDelete(data.Conn); ok {
		tracked.(*readyTrace).recordRelease()
	}
}

func (t *readinessTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	trace := readyTraceFrom(ctx)
	if trace == nil {
		return ctx
	}
	stage, _ := ctx.Value(readyStageKey{}).(*readyStage)
	return context.WithValue(ctx, readyQueryKey{}, readyQueryTrace{started: trace.now(), stage: stage})
}

func (t *readinessTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	trace := readyTraceFrom(ctx)
	if trace == nil {
		return
	}
	query, _ := ctx.Value(readyQueryKey{}).(readyQueryTrace)
	trace.recordQuery(query.stage, trace.now().Sub(query.started), data.Err != nil)
}

type readyQueryTrace struct {
	started time.Time
	stage   *readyStage
}

func readyTraceFrom(ctx context.Context) *readyTrace {
	trace, _ := ctx.Value(readyTraceKey{}).(*readyTrace)
	return trace
}

type readyQuery struct {
	ordinal int
	elapsed time.Duration
	failed  bool
}

type readyStage struct {
	name           string
	started        time.Time
	elapsed        time.Duration
	queries        int
	queryTime      time.Duration
	queryDetails   []readyQuery
	omittedQueries int
	failed         bool
	ended          bool
}

type readyTrace struct {
	now     func() time.Time
	runtime ReadinessRuntime
	phase   ReadinessPhase

	mu           sync.Mutex
	started      time.Time
	finishedAt   time.Time
	stages       []*readyStage
	acquires     int
	acquireTime  time.Duration
	acquireErrs  int
	queries      int
	queryTime    time.Duration
	queryErrs    int
	leases       int
	released     int
	poolBefore   readyPoolSnapshot
	poolAfter    readyPoolSnapshot
	failed       bool
	errorClass   string
	contextState string
}

func newReadyTrace(now func() time.Time) *readyTrace {
	if now == nil {
		now = time.Now
	}
	return &readyTrace{
		now:          now,
		started:      now(),
		runtime:      ReadinessRuntimeUnknown,
		phase:        ReadinessPhaseUnknown,
		contextState: readinessContextActive,
	}
}

// beginReadyStage labels the queries made inside one top-level readiness step.
// When no readiness trace is in the context, it is a no-op.
func beginReadyStage(ctx context.Context, name string) (context.Context, func(error)) {
	trace := readyTraceFrom(ctx)
	if trace == nil {
		return ctx, func(error) {}
	}
	stage := trace.addStage(name)
	return context.WithValue(ctx, readyStageKey{}, stage), func(err error) {
		trace.endStage(stage, err)
	}
}

func (t *readyTrace) addStage(name string) *readyStage {
	stage := &readyStage{name: name, started: t.now()}
	t.mu.Lock()
	t.stages = append(t.stages, stage)
	t.mu.Unlock()
	return stage
}

func (t *readyTrace) endStage(stage *readyStage, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if stage == nil || stage.ended {
		return
	}
	stage.ended = true
	stage.elapsed = t.now().Sub(stage.started)
	stage.failed = err != nil
}

func (t *readyTrace) recordAcquire(elapsed time.Duration, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.acquires++
	t.acquireTime += elapsed
	if failed {
		t.acquireErrs++
	}
}

func (t *readyTrace) recordQuery(stage *readyStage, elapsed time.Duration, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.queries++
	t.queryTime += elapsed
	if stage != nil {
		stage.queries++
		stage.queryTime += elapsed
		if len(stage.queryDetails) < maxReadyStageQueryDetails {
			stage.queryDetails = append(stage.queryDetails, readyQuery{
				ordinal: stage.queries,
				elapsed: elapsed,
				failed:  failed,
			})
		} else {
			stage.omittedQueries++
		}
	}
	if failed {
		t.queryErrs++
	}
}

func (t *readyTrace) recordLease() {
	t.mu.Lock()
	t.leases++
	t.mu.Unlock()
}

func (t *readyTrace) recordRelease() {
	t.mu.Lock()
	t.released++
	t.mu.Unlock()
}

func (t *readyTrace) finish(after readyPoolSnapshot, failed bool) {
	errorClass := ""
	if failed {
		errorClass = readinessErrorOther
	}
	t.finishResult(after, failed, errorClass, readinessContextActive)
}

func (t *readyTrace) finishContext(ctx context.Context, after readyPoolSnapshot, err error) {
	t.finishResult(after, err != nil, classifyReadinessError(err), readinessContextState(ctx))
}

func (t *readyTrace) finishResult(after readyPoolSnapshot, failed bool, errorClass, contextState string) {
	if failed && errorClass == "" {
		errorClass = readinessErrorOther
	}
	if contextState == "" {
		contextState = readinessContextActive
	}
	t.mu.Lock()
	t.finishedAt = t.now()
	t.poolAfter = after
	t.failed = failed
	t.errorClass = errorClass
	t.contextState = contextState
	t.mu.Unlock()
}

type readyQueryReport struct {
	ordinal int
	elapsed time.Duration
	failed  bool
}

type readyStageReport struct {
	name           string
	elapsed        time.Duration
	queries        int
	queryTime      time.Duration
	queryDetails   []readyQueryReport
	omittedQueries int
	failed         bool
}

type readyReport struct {
	total        time.Duration
	stages       []readyStageReport
	acquires     int
	acquireTime  time.Duration
	acquireErrs  int
	queries      int
	queryTime    time.Duration
	queryErrs    int
	leases       int
	released     int
	poolBefore   readyPoolSnapshot
	poolAfter    readyPoolSnapshot
	failed       bool
	failedStage  string
	errorClass   string
	contextState string
	runtime      ReadinessRuntime
	phase        ReadinessPhase
}

func (t *readyTrace) report() readyReport {
	t.mu.Lock()
	defer t.mu.Unlock()
	finishedAt := t.finishedAt
	if finishedAt.IsZero() {
		finishedAt = t.now()
	}
	report := readyReport{
		total:        finishedAt.Sub(t.started),
		acquires:     t.acquires,
		acquireTime:  t.acquireTime,
		acquireErrs:  t.acquireErrs,
		queries:      t.queries,
		queryTime:    t.queryTime,
		queryErrs:    t.queryErrs,
		leases:       t.leases,
		released:     t.released,
		poolBefore:   t.poolBefore,
		poolAfter:    t.poolAfter,
		failed:       t.failed,
		errorClass:   t.errorClass,
		contextState: t.contextState,
		runtime:      normalizeReadinessRuntime(t.runtime),
		phase:        normalizeReadinessPhase(t.phase),
	}
	if report.failed && report.errorClass == "" {
		report.errorClass = readinessErrorOther
	}
	if report.contextState == "" {
		report.contextState = readinessContextActive
	}
	for _, stage := range t.stages {
		stageReport := readyStageReport{
			name:           stage.name,
			elapsed:        stage.elapsed,
			queries:        stage.queries,
			queryTime:      stage.queryTime,
			omittedQueries: stage.omittedQueries,
			failed:         stage.failed,
		}
		for _, query := range stage.queryDetails {
			stageReport.queryDetails = append(stageReport.queryDetails, readyQueryReport{
				ordinal: query.ordinal,
				elapsed: query.elapsed,
				failed:  query.failed,
			})
		}
		report.stages = append(report.stages, stageReport)
		if stage.failed && report.failedStage == "" {
			report.failedStage = stage.name
		}
	}
	return report
}

type readyPoolSnapshot struct {
	acquired        int32
	idle            int32
	total           int32
	max             int32
	constructing    int32
	acquireCount    int64
	acquireDuration time.Duration
	emptyAcquires   int64
	canceledAcquire int64
}

// snapshotReadyPool is whole-pool state, not one request's usage. Acquire and
// query totals are the request-scoped attribution; these snapshots only frame
// the pool before and after the check.
func snapshotReadyPool(pool *pgxpool.Pool) readyPoolSnapshot {
	if pool == nil {
		return readyPoolSnapshot{}
	}
	stat := pool.Stat()
	return readyPoolSnapshot{
		acquired:        stat.AcquiredConns(),
		idle:            stat.IdleConns(),
		total:           stat.TotalConns(),
		max:             stat.MaxConns(),
		constructing:    stat.ConstructingConns(),
		acquireCount:    stat.AcquireCount(),
		acquireDuration: stat.AcquireDuration(),
		emptyAcquires:   stat.EmptyAcquireCount(),
		canceledAcquire: stat.CanceledAcquireCount(),
	}
}

func (s readyPoolSnapshot) String() string {
	return fmt.Sprintf("acquired=%d idle=%d total=%d max=%d constructing=%d acquire_count=%d acquire_ms=%d empty_acquire=%d canceled_acquire=%d",
		s.acquired, s.idle, s.total, s.max, s.constructing, s.acquireCount, s.acquireDuration.Milliseconds(), s.emptyAcquires, s.canceledAcquire)
}

// ReadinessLog rate-limits one bounded slow/failure summary per process. It
// logs only fixed timings, counts, stage names and attribution/error-class
// tokens, never SQL, arguments, DSNs or raw driver errors.
type ReadinessLog struct {
	logger   *slog.Logger
	now      func() time.Time
	slow     time.Duration
	interval time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func NewReadinessLog(logger *slog.Logger) *ReadinessLog {
	return &ReadinessLog{
		logger:   logger,
		now:      time.Now,
		slow:     defaultReadinessSlow,
		interval: defaultReadinessInterval,
	}
}

func newReadinessLog(logger *slog.Logger, now func() time.Time, slow, interval time.Duration) *ReadinessLog {
	if now == nil {
		now = time.Now
	}
	if slow <= 0 {
		slow = defaultReadinessSlow
	}
	if interval <= 0 {
		interval = defaultReadinessInterval
	}
	return &ReadinessLog{logger: logger, now: now, slow: slow, interval: interval}
}

func (l *ReadinessLog) report(report readyReport) {
	if l == nil || l.logger == nil {
		return
	}
	if !report.failed && report.total < l.slow {
		return
	}
	l.mu.Lock()
	now := l.now()
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.suppressed = 0
	l.last = now
	l.mu.Unlock()

	runtime := normalizeReadinessRuntime(report.runtime)
	phase := normalizeReadinessPhase(report.phase)
	contextState := report.contextState
	if contextState == "" {
		contextState = readinessContextActive
	}
	errorClass := report.errorClass
	if report.failed && errorClass == "" {
		errorClass = readinessErrorOther
	}
	attrs := []any{
		"runtime", string(runtime),
		"phase", string(phase),
		"context_state", contextState,
		"total_ms", report.total.Milliseconds(),
		"stages", report.stageSummary(),
		"acquires", report.acquires,
		"acquire_ms", report.acquireTime.Milliseconds(),
		"acquire_errors", report.acquireErrs,
		"queries", report.queries,
		"query_ms", report.queryTime.Milliseconds(),
		"query_errors", report.queryErrs,
		"pool_before", report.poolBefore.String(),
		"pool_after", report.poolAfter.String(),
	}
	if suppressed > 0 {
		attrs = append(attrs, "suppressed", suppressed)
	}
	if report.failed {
		attrs = append(attrs, "error_class", errorClass)
		if report.failedStage != "" {
			attrs = append(attrs, "failed_stage", report.failedStage)
		}
		l.logger.Warn("control-plane readiness failed", attrs...)
		return
	}
	l.logger.Warn("control-plane readiness slow", attrs...)
}

func (r readyReport) stageSummary() string {
	if len(r.stages) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(r.stages))
	for _, stage := range r.stages {
		part := fmt.Sprintf("%s:queries=%d,ms=%d", stage.name, stage.queries, stage.elapsed.Milliseconds())
		if details := stage.queryDetailSummary(); details != "" {
			part += "," + details
		}
		if stage.failed {
			part += ",failed"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ";")
}

func (s readyStageReport) queryDetailSummary() string {
	if len(s.queryDetails) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.queryDetails)+1)
	for _, query := range s.queryDetails {
		outcome := "ok"
		if query.failed {
			outcome = "failed"
		}
		parts = append(parts, fmt.Sprintf("q%d=%dms/%s", query.ordinal, query.elapsed.Milliseconds(), outcome))
	}
	if s.omittedQueries > 0 {
		parts = append(parts, fmt.Sprintf("more=%d", s.omittedQueries))
	}
	return strings.Join(parts, ",")
}

type readyConfig struct {
	log     *ReadinessLog
	now     func() time.Time
	runtime ReadinessRuntime
	phase   ReadinessPhase
}

// ReadyOption configures the diagnostics around one readiness check. Public
// readiness output stays generic; diagnostics go only to the supplied logger.
type ReadyOption func(*readyConfig)

func WithReadinessLog(log *ReadinessLog) ReadyOption {
	return func(config *readyConfig) { config.log = log }
}

// WithReadinessAttribution names the actual source call site. Omit it only
// when the runtime or phase is genuinely unknown; unknown is reported as such
// rather than guessed as an API request.
func WithReadinessAttribution(runtime ReadinessRuntime, phase ReadinessPhase) ReadyOption {
	return func(config *readyConfig) {
		config.runtime = runtime
		config.phase = phase
	}
}

// Ready remains the migration/schema check. The tracing is context-scoped:
// requests without a readiness trace are untouched.
func Ready(ctx context.Context, pool *pgxpool.Pool, opts ...ReadyOption) error {
	config := readyConfig{now: time.Now, runtime: ReadinessRuntimeUnknown, phase: ReadinessPhaseUnknown}
	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}
	return runReady(ctx, pool, config, PlanMigrations)
}

func runReady(ctx context.Context, pool *pgxpool.Pool, config readyConfig, plan func(context.Context, *pgxpool.Pool) (MigrationPlan, error)) error {
	if config.now == nil {
		config.now = time.Now
	}
	trace := newReadyTrace(config.now)
	trace.runtime = normalizeReadinessRuntime(config.runtime)
	trace.phase = normalizeReadinessPhase(config.phase)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx = context.WithValue(ctx, readyTraceKey{}, trace)
	migrationPlan, err := plan(ctx, pool)
	if err == nil && (migrationPlan.Current != migrationPlan.Required || migrationPlan.RiverCurrent != migrationPlan.RiverRequired) {
		err = contract.NewFault(contract.PreconditionFailed, "database migrations are pending", "Run eregion node migrate before starting API or workers.")
	}
	trace.finishContext(ctx, snapshotReadyPool(pool), err)
	report := trace.report()
	if config.log != nil {
		config.log.report(report)
	}
	return err
}
