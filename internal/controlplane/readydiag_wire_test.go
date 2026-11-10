package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

// This file exercises the readiness tracer through a real pgxpool connected to
// a loopback, in-process PostgreSQL wire fake. It does not use an ambient
// database, Docker, or a production connection, and it is not live-database
// qualification: the fake speaks only the startup and simple-query messages
// these tests need.

// fakeGate is an explicit release point for a fake query or handshake. Cleanup
// can call Release repeatedly without panicking.
type fakeGate struct {
	once sync.Once
	ch   chan struct{}
}

func newFakeGate() *fakeGate {
	return &fakeGate{ch: make(chan struct{})}
}

func (g *fakeGate) Release() {
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.ch) })
}

func (g *fakeGate) Wait(stop <-chan struct{}) error {
	if g == nil {
		return nil
	}
	select {
	case <-g.ch:
		return nil
	case <-stop:
		return errors.New("fake PostgreSQL server stopping")
	}
}

type fakePlanState struct {
	schemaTableExists bool
	schemaVersions    []int64
	riverTableExists  bool
	riverLineColumn   bool
	riverVersions     []int64
	riverFail         bool
}

type fakePGServer struct {
	t  *testing.T
	ln net.Listener

	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	mu        sync.Mutex
	gates     map[string]*fakeGate
	conns     map[net.Conn]struct{}
	plan      fakePlanState
	started   chan string
	accepted  chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32
}

func newFakePGServer(t *testing.T) *fakePGServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakePGServer{
		t:        t,
		ln:       ln,
		stop:     make(chan struct{}),
		gates:    make(map[string]*fakeGate),
		conns:    make(map[net.Conn]struct{}),
		started:  make(chan string, 16),
		accepted: make(chan struct{}, 16),
	}
	server.wg.Add(1)
	go server.accept()
	t.Cleanup(server.Close)
	return server
}

// Stop closes the listener, stop channel and every active server connection,
// but does not wait for handlers. Tests call it before closing a pool so row
// draining and idle handlers cannot outlive the client.
func (s *fakePGServer) Stop() {
	s.once.Do(func() {
		close(s.stop)
		_ = s.ln.Close()
		s.CloseConnections()
	})
}

// CloseConnections forcibly closes current server-side sockets. It is the
// bounded backstop for cleanup paths that must not wait on a stalled handler.
func (s *fakePGServer) CloseConnections() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *fakePGServer) Close() {
	s.Stop()
	s.wg.Wait()
}

func (s *fakePGServer) dsn() string {
	host, port, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		s.t.Fatal(err)
	}
	return fmt.Sprintf("postgres://%s/eregion?sslmode=disable", net.JoinHostPort(host, port))
}

func (s *fakePGServer) setGate(name string, gate *fakeGate) {
	s.mu.Lock()
	s.gates[name] = gate
	s.mu.Unlock()
}

func (s *fakePGServer) gate(name string) *fakeGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gates[name]
}

func (s *fakePGServer) setPlanState(state fakePlanState) {
	s.mu.Lock()
	state.schemaVersions = append([]int64(nil), state.schemaVersions...)
	state.riverVersions = append([]int64(nil), state.riverVersions...)
	s.plan = state
	s.mu.Unlock()
}

func (s *fakePGServer) planState() fakePlanState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.plan
	state.schemaVersions = append([]int64(nil), state.schemaVersions...)
	state.riverVersions = append([]int64(nil), state.riverVersions...)
	return state
}

func (s *fakePGServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				_ = conn.Close()
			}()
			s.serve(conn)
		}()
	}
}

func (s *fakePGServer) serve(conn net.Conn) {
	backend := pgproto3.NewBackend(conn, conn)
	startup, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if _, ok := startup.(*pgproto3.StartupMessage); !ok {
		return
	}
	select {
	case s.accepted <- struct{}{}:
	default:
	}
	if err := s.gate("establish").Wait(s.stop); err != nil {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	for _, status := range [][2]string{
		{"server_version", "17.0"},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"TimeZone", "UTC"},
		{"integer_datetimes", "on"},
		{"standard_conforming_strings", "on"},
		{"application_name", "eregion"},
		{"is_superuser", "on"},
	} {
		backend.Send(&pgproto3.ParameterStatus{Name: status[0], Value: status[1]})
	}
	backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return
	}
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		switch message := message.(type) {
		case *pgproto3.Query:
			if err := s.handleQuery(backend, message.String); err != nil {
				return
			}
		case *pgproto3.Terminate:
			return
		case *pgproto3.Sync:
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := backend.Flush(); err != nil {
				return
			}
		default:
			return
		}
	}
}

func (s *fakePGServer) handleQuery(backend *pgproto3.Backend, query string) error {
	active := s.active.Add(1)
	for {
		current := s.maxActive.Load()
		if active <= current || s.maxActive.CompareAndSwap(current, active) {
			break
		}
	}
	defer s.active.Add(-1)
	select {
	case s.started <- query:
	default:
	}

	switch {
	case strings.Contains(query, "wire_hold"):
		s.sendRowDescription(backend)
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		if err := backend.Flush(); err != nil {
			return err
		}
		if err := s.gate("hold").Wait(s.stop); err != nil {
			return err
		}
		return s.finishQuery(backend)
	case strings.Contains(query, "wire_fail"):
		s.sendRowDescription(backend)
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		if err := backend.Flush(); err != nil {
			return err
		}
		backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "57014", Message: "wire iteration failure"})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		return backend.Flush()
	case strings.Contains(query, "wire_scan"):
		s.sendRowDescription(backend)
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("not-an-int")}})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		return backend.Flush()
	case strings.Contains(query, "wire_stall"):
		s.sendRowDescription(backend)
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		if err := backend.Flush(); err != nil {
			return err
		}
		if err := s.gate("stall").Wait(s.stop); err != nil {
			return err
		}
		// Terminating the connection is what makes a broken-cancellation
		// cleanup path return from a blocked rows.Next before rows.Close.
		return errors.New("fake PostgreSQL stall released")
	case strings.Contains(query, "wire_wait"):
		<-s.stop
		return nil
	case strings.Contains(query, "wire_slow"):
		if err := s.gate("slow").Wait(s.stop); err != nil {
			return err
		}
		s.sendRowDescription(backend)
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		return s.finishQuery(backend)
	case strings.Contains(query, "wire_fast"):
		if err := s.gate("fast").Wait(s.stop); err != nil {
			return err
		}
		s.sendRowDescription(backend)
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		return s.finishQuery(backend)
	case strings.Contains(query, "to_regclass"):
		state := s.planState()
		exists := false
		switch {
		case strings.Contains(query, "eregion_schema_migrations"):
			exists = state.schemaTableExists
		case strings.Contains(query, "river_migration"):
			exists = state.riverTableExists
		}
		s.sendBool(backend, exists)
		return s.finishQuery(backend)
	case strings.Contains(query, "eregion_schema_migrations") && strings.Contains(query, "checksum"):
		state := s.planState()
		s.sendSchemaMigrations(backend, state.schemaVersions)
		return s.finishQuery(backend)
	case strings.Contains(query, "information_schema.columns"):
		state := s.planState()
		s.sendBool(backend, state.riverLineColumn)
		return s.finishQuery(backend)
	case strings.Contains(query, "river_migration") && strings.Contains(query, "created_at"):
		state := s.planState()
		if err := s.gate("river_versions").Wait(s.stop); err != nil {
			return err
		}
		if state.riverFail {
			backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "57014", Message: "wire river version failure"})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			return backend.Flush()
		}
		if strings.Contains(query, "WHERE line =") {
			s.sendRiverVersionsByLine(backend, state.riverVersions)
		} else {
			s.sendRiverVersionsLegacy(backend, state.riverVersions)
		}
		return s.finishQuery(backend)
	default:
		s.sendRowDescription(backend)
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		return s.finishQuery(backend)
	}
}

func (s *fakePGServer) sendRowDescription(backend *pgproto3.Backend) {
	backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
		Name:         []byte("n"),
		DataTypeOID:  pgtype.Int4OID,
		DataTypeSize: 4,
		TypeModifier: -1,
		Format:       pgproto3.TextFormat,
	}}})
}

func (s *fakePGServer) sendBool(backend *pgproto3.Backend, value bool) {
	text := "f"
	if value {
		text = "t"
	}
	backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
		Name:         []byte("exists"),
		DataTypeOID:  pgtype.BoolOID,
		DataTypeSize: 1,
		TypeModifier: -1,
		Format:       pgproto3.TextFormat,
	}}})
	backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(text)}})
}

func (s *fakePGServer) sendSchemaMigrations(backend *pgproto3.Backend, versions []int64) {
	backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
		{Name: []byte("version"), DataTypeOID: pgtype.Int4OID, DataTypeSize: 4, TypeModifier: -1, Format: pgproto3.TextFormat},
		{Name: []byte("checksum"), DataTypeOID: pgtype.TextOID, DataTypeSize: -1, TypeModifier: -1, Format: pgproto3.TextFormat},
	}})
	for _, version := range versions {
		migration, ok := migrationByVersion(int(version))
		if !ok {
			continue
		}
		backend.Send(&pgproto3.DataRow{Values: [][]byte{
			[]byte(strconv.FormatInt(version, 10)),
			[]byte(migrationHash(migration)),
		}})
	}
}

func (s *fakePGServer) sendRiverVersionsByLine(backend *pgproto3.Backend, versions []int64) {
	backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
		{Name: []byte("line"), DataTypeOID: pgtype.TextOID, DataTypeSize: -1, TypeModifier: -1, Format: pgproto3.TextFormat},
		{Name: []byte("version"), DataTypeOID: pgtype.Int8OID, DataTypeSize: 8, TypeModifier: -1, Format: pgproto3.TextFormat},
		{Name: []byte("created_at"), DataTypeOID: pgtype.TimestamptzOID, DataTypeSize: 8, TypeModifier: -1, Format: pgproto3.TextFormat},
	}})
	for _, version := range versions {
		backend.Send(&pgproto3.DataRow{Values: [][]byte{
			[]byte("main"),
			[]byte(strconv.FormatInt(version, 10)),
			[]byte("2026-01-01 00:00:00+00"),
		}})
	}
}

func (s *fakePGServer) sendRiverVersionsLegacy(backend *pgproto3.Backend, versions []int64) {
	backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
		{Name: []byte("created_at"), DataTypeOID: pgtype.TimestamptzOID, DataTypeSize: 8, TypeModifier: -1, Format: pgproto3.TextFormat},
		{Name: []byte("version"), DataTypeOID: pgtype.Int8OID, DataTypeSize: 8, TypeModifier: -1, Format: pgproto3.TextFormat},
	}})
	for _, version := range versions {
		backend.Send(&pgproto3.DataRow{Values: [][]byte{
			[]byte("2026-01-01 00:00:00+00"),
			[]byte(strconv.FormatInt(version, 10)),
		}})
	}
}

func (s *fakePGServer) finishQuery(backend *pgproto3.Backend) error {
	backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	return backend.Flush()
}

func newWirePool(t *testing.T, server *fakePGServer, maxConns int32) *pgxpool.Pool {
	t.Helper()
	config, err := ParseDatabaseConfig(server.dsn())
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = maxConns
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Unblock fake handlers before pool.Close waits for every lease, then
		// wait for handlers after their sockets are closed.
		server.Stop()
		pool.Close()
		server.Close()
	})
	return pool
}

func newWireTraceContext(trace *readyTrace, stage string) (context.Context, func(error)) {
	return beginReadyStage(context.WithValue(context.Background(), readyTraceKey{}, trace), stage)
}

func finishWireTrace(trace *readyTrace, pool *pgxpool.Pool, failed bool) readyReport {
	trace.finish(snapshotReadyPool(pool), failed)
	return trace.report()
}

// pendingRowsNext owns one outstanding rows.Next call. Stop releases the
// server-side gate, joins the iterator, and only then closes rows. A forced
// connection close is the bounded backstop if the gate path is broken.
type pendingRowsNext struct {
	rows pgx.Rows
	gate *fakeGate
	done chan struct{}
	more bool
}

func startRowsNext(rows pgx.Rows, gate *fakeGate) *pendingRowsNext {
	pending := &pendingRowsNext{rows: rows, gate: gate, done: make(chan struct{})}
	go func() {
		pending.more = rows.Next()
		close(pending.done)
	}()
	return pending
}

func (p *pendingRowsNext) Stop(t *testing.T, server *fakePGServer) {
	t.Helper()
	p.gate.Release()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		server.CloseConnections()
		select {
		case <-p.done:
		case <-time.After(time.Second):
			t.Errorf("rows.Next did not stop after the connection was closed")
			return
		}
	}
	p.rows.Close()
}

func TestReadinessWireAcquireSaturationCancellation(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)

	t.Run("acquired lease is released", func(t *testing.T) {
		trace := newReadyTrace(nil)
		trace.poolBefore = snapshotReadyPool(pool)
		conn, err := pool.Acquire(context.WithValue(context.Background(), readyTraceKey{}, trace))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(conn.Release)
		conn.Release()
		report := finishWireTrace(trace, pool, false)
		if report.acquires != 1 || report.acquireErrs != 0 {
			t.Fatalf("acquire report = %+v", report)
		}
		if report.leases != 1 || report.released != 1 {
			t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
		}
		if report.poolAfter.acquired != 0 || report.poolAfter.idle != 1 {
			t.Fatalf("pool after release = %+v", report.poolAfter)
		}
	})

	t.Run("saturation cancellation", func(t *testing.T) {
		holder, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(holder.Release)

		trace := newReadyTrace(nil)
		trace.poolBefore = snapshotReadyPool(pool)
		traced, endStage := newWireTraceContext(trace, "wire_acquire")
		defer endStage(nil)
		ctx, cancel := context.WithTimeout(traced, 100*time.Millisecond)
		defer cancel()

		started := time.Now()
		conn, err := pool.Acquire(ctx)
		elapsed := time.Since(started)
		if err == nil {
			conn.Release()
			t.Fatal("saturated pool acquisition unexpectedly succeeded")
		}
		if elapsed < 80*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("saturation cancellation returned after %v", elapsed)
		}
		report := finishWireTrace(trace, pool, true)
		if report.acquires != 1 || report.acquireErrs != 1 {
			t.Fatalf("acquire report = %+v", report)
		}
		if report.acquireTime < 50*time.Millisecond {
			t.Fatalf("acquire time = %v, want the wait before cancellation", report.acquireTime)
		}
		if report.leases != 0 || report.released != 0 {
			t.Fatalf("canceled wait recorded a lease: leases %d, released %d", report.leases, report.released)
		}
		if report.poolBefore.acquired != 1 || report.poolAfter.acquired != 1 {
			t.Fatalf("holder lease not reflected: before %+v, after %+v", report.poolBefore, report.poolAfter)
		}
	})
}

func TestReadinessWireQueryRowLifetimeAndRelease(t *testing.T) {
	server := newFakePGServer(t)
	hold := newFakeGate()
	server.setGate("hold", hold)
	pool := newWirePool(t, server, 1)

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx, endStage := newWireTraceContext(trace, "wire_row_lifetime")
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := pool.Query(queryCtx, "SELECT wire_hold")
	if err != nil {
		endStage(err)
		t.Fatal(err)
	}
	t.Cleanup(rows.Close)
	t.Cleanup(hold.Release)
	if !rows.Next() {
		endStage(rows.Err())
		t.Fatalf("missing wire row: %v", rows.Err())
	}
	var value int
	if err := rows.Scan(&value); err != nil {
		endStage(err)
		t.Fatal(err)
	}
	if value != 1 {
		t.Fatalf("wire value = %d", value)
	}

	closed := make(chan struct{})
	go func() {
		rows.Close()
		close(closed)
	}()
	time.Sleep(50 * time.Millisecond)
	if report := trace.report(); report.queries != 0 {
		t.Fatalf("query ended before rows.Close finished: %+v", report)
	}
	hold.Release()
	<-closed
	endStage(rows.Err())
	report := finishWireTrace(trace, pool, false)

	if report.queries != 1 || report.queryErrs != 0 {
		t.Fatalf("query report = %+v", report)
	}
	if report.queryTime < 40*time.Millisecond {
		t.Fatalf("query time = %v, want the held row lifetime", report.queryTime)
	}
	if report.leases != 1 || report.released != 1 {
		t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
	}
	if report.poolAfter.acquired != 0 || report.poolAfter.idle != 1 {
		t.Fatalf("pool after rows.Close = %+v", report.poolAfter)
	}
}

func TestReadinessWireQueryFailureReleases(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)

	t.Run("iteration error", func(t *testing.T) {
		trace := newReadyTrace(nil)
		trace.poolBefore = snapshotReadyPool(pool)
		ctx, endStage := newWireTraceContext(trace, "wire_iteration_failure")
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		rows, err := pool.Query(queryCtx, "SELECT wire_fail")
		if err != nil {
			endStage(err)
			t.Fatal(err)
		}
		t.Cleanup(rows.Close)
		if !rows.Next() {
			endStage(rows.Err())
			t.Fatalf("missing first wire row: %v", rows.Err())
		}
		var value int
		if err := rows.Scan(&value); err != nil {
			endStage(err)
			t.Fatal(err)
		}
		if rows.Next() {
			endStage(nil)
			t.Fatal("iteration unexpectedly succeeded")
		}
		iterErr := rows.Err()
		rows.Close()
		endStage(iterErr)
		if iterErr == nil {
			t.Fatal("iteration failure was not reported")
		}
		report := finishWireTrace(trace, pool, true)
		if report.queries != 1 || report.queryErrs != 1 {
			t.Fatalf("query report = %+v", report)
		}
		if report.leases != 1 || report.released != 1 {
			t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
		}
		if report.poolAfter.acquired != 0 {
			t.Fatalf("iteration failure held a lease: %+v", report.poolAfter)
		}
	})

	t.Run("scan error", func(t *testing.T) {
		trace := newReadyTrace(nil)
		trace.poolBefore = snapshotReadyPool(pool)
		ctx, endStage := newWireTraceContext(trace, "wire_scan_failure")
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		rows, err := pool.Query(queryCtx, "SELECT wire_scan")
		if err != nil {
			endStage(err)
			t.Fatal(err)
		}
		t.Cleanup(rows.Close)
		if !rows.Next() {
			endStage(rows.Err())
			t.Fatalf("missing scan row: %v", rows.Err())
		}
		var value int
		scanErr := rows.Scan(&value)
		rows.Close()
		endStage(scanErr)
		if scanErr == nil {
			t.Fatal("invalid int4 text was scanned without an error")
		}
		report := finishWireTrace(trace, pool, true)
		if report.queries != 1 || report.queryErrs != 1 {
			t.Fatalf("query report = %+v", report)
		}
		if report.leases != 1 || report.released != 1 {
			t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		stall := newFakeGate()
		server.setGate("stall", stall)
		trace := newReadyTrace(nil)
		trace.poolBefore = snapshotReadyPool(pool)
		ctx, endStage := newWireTraceContext(trace, "wire_query_cancel")
		queryCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		rows, err := pool.Query(queryCtx, "SELECT wire_stall")
		if err != nil {
			endStage(err)
			t.Fatal(err)
		}
		t.Cleanup(rows.Close)
		t.Cleanup(stall.Release)
		if !rows.Next() {
			endStage(rows.Err())
			t.Fatalf("missing stall row: %v", rows.Err())
		}
		var value int
		if err := rows.Scan(&value); err != nil {
			endStage(err)
			t.Fatal(err)
		}

		pending := startRowsNext(rows, stall)
		t.Cleanup(func() { pending.Stop(t, server) })
		cancel()
		select {
		case <-pending.done:
			if pending.more {
				t.Fatal("stalled iteration returned another row after cancellation")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("query cancellation did not interrupt row iteration")
		}
		queryErr := rows.Err()
		pending.Stop(t, server)
		endStage(queryErr)
		if !errors.Is(queryErr, context.Canceled) {
			t.Fatalf("canceled query classification = %T %v, want context.Canceled", queryErr, queryErr)
		}
		report := finishWireTrace(trace, pool, true)
		if report.queries != 1 || report.queryErrs != 1 {
			t.Fatalf("query report = %+v", report)
		}
		if report.leases != 1 || report.released != 1 {
			t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
		}
	})
}

func TestReadinessWireStallCleanupStopsIteration(t *testing.T) {
	server := newFakePGServer(t)
	stall := newFakeGate()
	server.setGate("stall", stall)
	pool := newWirePool(t, server, 1)

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx, endStage := newWireTraceContext(trace, "wire_stall_cleanup")
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel() // Deliberately never canceled: this is the broken-cancel path.

	rows, err := pool.Query(queryCtx, "SELECT wire_stall")
	if err != nil {
		endStage(err)
		t.Fatal(err)
	}
	t.Cleanup(rows.Close)
	if !rows.Next() {
		endStage(rows.Err())
		t.Fatalf("missing stall row: %v", rows.Err())
	}
	var value int
	if err := rows.Scan(&value); err != nil {
		endStage(err)
		t.Fatal(err)
	}

	pending := startRowsNext(rows, stall)
	t.Cleanup(func() { pending.Stop(t, server) })
	select {
	case <-pending.done:
		t.Fatal("rows.Next returned before cleanup released the stall gate")
	case <-time.After(50 * time.Millisecond):
	}

	started := time.Now()
	pending.Stop(t, server)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stall cleanup did not converge: %v", elapsed)
	}
	if pending.more {
		t.Fatal("stalled iteration returned another row during cleanup")
	}
	queryErr := rows.Err()
	endStage(queryErr)
	if queryErr == nil {
		t.Fatal("stall cleanup terminated without a query error")
	}
	report := finishWireTrace(trace, pool, true)
	if report.queries != 1 || report.queryErrs != 1 {
		t.Fatalf("query report = %+v", report)
	}
	if report.leases != 1 || report.released != 1 {
		t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
	}
}

func TestReadinessWireDelayedEstablishment(t *testing.T) {
	server := newFakePGServer(t)
	establish := newFakeGate()
	server.setGate("establish", establish)
	pool := newWirePool(t, server, 1)
	t.Cleanup(establish.Release)

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	traced := context.WithValue(context.Background(), readyTraceKey{}, trace)
	acquired := make(chan struct{})
	var acquireErr error
	go func() {
		conn, err := pool.Acquire(traced)
		if err == nil {
			conn.Release()
		}
		acquireErr = err
		close(acquired)
	}()

	select {
	case <-server.accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server did not accept the connection")
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("acquire completed before the handshake gate was released")
	default:
	}
	establish.Release()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed establishment did not complete")
	}
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}

	report := finishWireTrace(trace, pool, false)
	if report.acquires != 1 || report.acquireErrs != 0 {
		t.Fatalf("acquire report = %+v", report)
	}
	if report.acquireTime < 40*time.Millisecond {
		t.Fatalf("acquire time = %v, want delayed establishment included", report.acquireTime)
	}
	if report.leases != 1 || report.released != 1 {
		t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
	}
	if report.poolAfter.acquired != 0 {
		t.Fatalf("delayed establishment held a lease: %+v", report.poolAfter)
	}
}

func TestReadinessWireUntracedTrafficIsNotAttributed(t *testing.T) {
	server := newFakePGServer(t)
	hold := newFakeGate()
	server.setGate("hold", hold)
	pool := newWirePool(t, server, 2)

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx, endStage := newWireTraceContext(trace, "wire_traced")
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := pool.Query(queryCtx, "SELECT wire_hold")
	if err != nil {
		endStage(err)
		t.Fatal(err)
	}
	t.Cleanup(rows.Close)
	t.Cleanup(hold.Release)
	if !rows.Next() {
		endStage(rows.Err())
		t.Fatalf("missing held row: %v", rows.Err())
	}
	var held int
	if err := rows.Scan(&held); err != nil {
		endStage(err)
		t.Fatal(err)
	}

	var ordinary int
	if err := pool.QueryRow(context.Background(), "SELECT wire_ordinary").Scan(&ordinary); err != nil {
		endStage(err)
		t.Fatalf("untraced ordinary query failed: %v", err)
	}
	if ordinary != 1 {
		endStage(nil)
		t.Fatalf("ordinary value = %d", ordinary)
	}
	if report := trace.report(); report.queries != 0 || report.acquires != 1 {
		t.Fatalf("untraced traffic was attributed: %+v", report)
	}

	hold.Release()
	rows.Close()
	endStage(rows.Err())
	report := finishWireTrace(trace, pool, false)
	if report.queries != 1 || report.acquires != 1 {
		t.Fatalf("traced totals = queries %d, acquires %d", report.queries, report.acquires)
	}
	if report.leases != 1 || report.released != 1 {
		t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
	}
}

func TestReadinessWirePlanMigrationsEarlyReturnReleases(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	plan, err := PlanMigrations(ctx, pool)
	trace.finish(snapshotReadyPool(pool), err != nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Required != requiredVersion() || plan.Current != 0 {
		t.Fatalf("schema plan = %+v", plan)
	}
	if plan.RiverRequired <= 0 || plan.RiverCurrent != 0 {
		t.Fatalf("River plan = %+v", plan)
	}
	report := trace.report()
	if len(report.stages) != 2 || report.stages[0].name != "schema_table" || report.stages[1].name != "river_versions" {
		t.Fatalf("early-return stages = %+v", report.stages)
	}
	if report.queries != 2 || report.acquires != 2 {
		t.Fatalf("early-return totals = queries %d, acquires %d", report.queries, report.acquires)
	}
	if report.leases != 2 || report.released != 2 {
		t.Fatalf("early-return leases = leases %d, released %d", report.leases, report.released)
	}
	if report.poolAfter.acquired != 0 {
		t.Fatalf("early-return held a lease: %+v", report.poolAfter)
	}
}

func riverRequiredVersion(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	m, err := migrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	versions := m.AllVersions()
	if len(versions) == 0 {
		t.Fatal("River migrator has no versions")
	}
	return versions[len(versions)-1].Version
}

func TestReadinessWirePlanMigrationsRiverExisting(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)
	required := riverRequiredVersion(t, pool)
	server.setPlanState(fakePlanState{riverTableExists: true, riverLineColumn: true, riverVersions: []int64{int64(required)}})

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	plan, err := PlanMigrations(ctx, pool)
	trace.finishContext(ctx, snapshotReadyPool(pool), err)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RiverCurrent != required || plan.RiverRequired != required {
		t.Fatalf("River plan = %+v", plan)
	}
	report := trace.report()
	if len(report.stages) != 2 || report.stages[0].name != "schema_table" || report.stages[1].name != "river_versions" {
		t.Fatalf("River stages = %+v", report.stages)
	}
	if report.stages[1].queries != 3 || report.queries != 4 {
		t.Fatalf("River query count = %+v, total %d", report.stages[1], report.queries)
	}
	if report.acquires != 4 || report.leases != report.released || report.poolAfter.acquired != 0 {
		t.Fatalf("River leases = acquires %d leases %d released %d after %+v", report.acquires, report.leases, report.released, report.poolAfter)
	}
	if len(report.stages[1].queryDetails) != 3 || report.stages[1].queryDetails[2].failed {
		t.Fatalf("River query ordinals = %+v", report.stages[1].queryDetails)
	}
}

func TestReadinessWirePlanMigrationsNormalExistingPathQueryCount(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)
	schemaRequired := requiredVersion()
	schemaVersions := make([]int64, 0, schemaRequired)
	for version := 1; version <= schemaRequired; version++ {
		schemaVersions = append(schemaVersions, int64(version))
	}
	riverRequired := riverRequiredVersion(t, pool)
	server.setPlanState(fakePlanState{
		schemaTableExists: true,
		schemaVersions:    schemaVersions,
		riverTableExists:  true,
		riverLineColumn:   true,
		riverVersions:     []int64{int64(riverRequired)},
	})

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	plan, err := PlanMigrations(ctx, pool)
	trace.finishContext(ctx, snapshotReadyPool(pool), err)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Current != schemaRequired || plan.RiverCurrent != riverRequired {
		t.Fatalf("normal plan = %+v", plan)
	}
	report := trace.report()
	if len(report.stages) != 3 || report.stages[0].name != "schema_table" ||
		report.stages[1].name != "schema_migrations" || report.stages[2].name != "river_versions" {
		t.Fatalf("normal stages = %+v", report.stages)
	}
	if report.queries != 5 || report.acquires != 5 {
		t.Fatalf("normal query count = queries %d acquires %d, want 5/5", report.queries, report.acquires)
	}
	if report.leases != report.released || report.poolAfter.acquired != 0 {
		t.Fatalf("normal leases = leases %d released %d after %+v", report.leases, report.released, report.poolAfter)
	}
}

func TestReadinessWirePlanMigrationsRiverLegacyLine(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)
	required := riverRequiredVersion(t, pool)
	server.setPlanState(fakePlanState{riverTableExists: true, riverLineColumn: false, riverVersions: []int64{int64(required)}})

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	plan, err := PlanMigrations(ctx, pool)
	trace.finishContext(ctx, snapshotReadyPool(pool), err)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RiverCurrent != required {
		t.Fatalf("legacy River plan = %+v", plan)
	}
	report := trace.report()
	if len(report.stages) != 2 || report.stages[1].name != "river_versions" || report.stages[1].queries != 3 {
		t.Fatalf("legacy River stages = %+v", report.stages)
	}
	if report.acquires != 4 || report.leases != report.released || report.poolAfter.acquired != 0 {
		t.Fatalf("legacy River leases = acquires %d leases %d released %d after %+v", report.acquires, report.leases, report.released, report.poolAfter)
	}
}

func TestReadinessWirePlanMigrationsRiverVersionFailure(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)
	required := riverRequiredVersion(t, pool)
	server.setPlanState(fakePlanState{riverTableExists: true, riverLineColumn: true, riverVersions: []int64{int64(required)}, riverFail: true})

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	_, err := PlanMigrations(ctx, pool)
	trace.finishContext(ctx, snapshotReadyPool(pool), err)
	if err == nil {
		t.Fatal("River version failure was accepted")
	}
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("River version failure code = %s", contract.Classify(err).Code)
	}
	report := trace.report()
	if report.failedStage != "river_versions" || report.queryErrs != 1 {
		t.Fatalf("River failure report = %+v", report)
	}
	if len(report.stages) != 2 || len(report.stages[1].queryDetails) != 3 || !report.stages[1].queryDetails[2].failed {
		t.Fatalf("River failure ordinals = %+v", report.stages)
	}
	if report.errorClass != readinessErrorPostgres {
		t.Fatalf("River failure class = %q, want postgres", report.errorClass)
	}
	if report.leases != report.released || report.poolAfter.acquired != 0 {
		t.Fatalf("River failure leases = leases %d released %d after %+v", report.leases, report.released, report.poolAfter)
	}
}

func TestReadinessWirePlanMigrationsRiverUnknownNewerFailsClosed(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)
	required := riverRequiredVersion(t, pool)
	server.setPlanState(fakePlanState{riverTableExists: true, riverLineColumn: true, riverVersions: []int64{int64(required + 1)}})

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx := context.WithValue(context.Background(), readyTraceKey{}, trace)
	_, err := PlanMigrations(ctx, pool)
	trace.finishContext(ctx, snapshotReadyPool(pool), err)
	if err == nil {
		t.Fatal("an unknown newer River version was accepted")
	}
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unknown newer River version code = %s", contract.Classify(err).Code)
	}
	report := trace.report()
	if report.failedStage != "river_versions" {
		t.Fatalf("unknown newer River report = %+v", report)
	}
	if report.leases != report.released || report.poolAfter.acquired != 0 {
		t.Fatalf("unknown newer River leases = leases %d released %d after %+v", report.leases, report.released, report.poolAfter)
	}
}

func TestReadinessWirePlanMigrationsRiverCancellation(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)
	required := riverRequiredVersion(t, pool)
	gate := newFakeGate()
	server.setGate("river_versions", gate)
	server.setPlanState(fakePlanState{riverTableExists: true, riverLineColumn: true, riverVersions: []int64{int64(required)}})
	t.Cleanup(gate.Release)

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, readyTraceKey{}, trace)

	done := make(chan error, 1)
	go func() {
		_, err := PlanMigrations(ctx, pool)
		done <- err
	}()

	started := false
	for !started {
		select {
		case query := <-server.started:
			if strings.Contains(query, "river_migration") && strings.Contains(query, "WHERE line =") {
				started = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("River version query did not start")
		}
	}

	// Cancel while the server query remains blocked on the gate. Wait for the
	// client result before releasing the gate so a successful response cannot
	// race the cancellation and make this test flaky. The gate is released on
	// the bounded failure path and by cleanup for the success path.
	cancel()
	var err error
	select {
	case err = <-done:
		gate.Release()
	case <-time.After(2 * time.Second):
		gate.Release()
		t.Fatal("River cancellation did not return")
	}
	trace.finishContext(ctx, snapshotReadyPool(pool), err)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("River cancellation = %v, want context.Canceled", err)
	}
	report := trace.report()
	if report.failedStage != "river_versions" || report.queryErrs != 1 || report.contextState != readinessContextCanceled {
		t.Fatalf("River cancellation report = %+v", report)
	}
	if report.leases != report.released {
		t.Fatalf("River cancellation leases = leases %d released %d", report.leases, report.released)
	}
}

func TestReadinessWireConcurrentAttribution(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 2)
	fastGate := newFakeGate()
	slowGate := newFakeGate()
	server.setGate("fast", fastGate)
	server.setGate("slow", slowGate)
	t.Cleanup(fastGate.Release)
	t.Cleanup(slowGate.Release)

	type queryResult struct {
		stage  string
		report readyReport
		err    error
	}
	run := func(query, stage string) queryResult {
		trace := newReadyTrace(nil)
		trace.poolBefore = snapshotReadyPool(pool)
		ctx, endStage := newWireTraceContext(trace, stage)
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rows, err := pool.Query(queryCtx, query)
		if err != nil {
			endStage(err)
			return queryResult{stage: stage, report: finishWireTrace(trace, pool, true), err: err}
		}
		defer rows.Close()
		var value int
		if rows.Next() {
			err = rows.Scan(&value)
		} else {
			err = rows.Err()
			if err == nil {
				err = errors.New("missing wire row")
			}
		}
		rows.Close()
		endStage(err)
		return queryResult{stage: stage, report: finishWireTrace(trace, pool, err != nil), err: err}
	}

	results := make(chan queryResult, 2)
	go func() { results <- run("SELECT wire_slow", "wire_slow") }()
	go func() { results <- run("SELECT wire_fast", "wire_fast") }()

	seen := map[string]bool{}
	var slowSeenAt time.Time
	for !seen["wire_slow"] || !seen["wire_fast"] {
		select {
		case query := <-server.started:
			for _, name := range []string{"wire_slow", "wire_fast"} {
				if strings.Contains(query, name) {
					seen[name] = true
					if name == "wire_slow" {
						slowSeenAt = time.Now()
					}
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("overlap gates not reached: %+v", seen)
		}
	}
	if got := server.maxActive.Load(); got < 2 {
		t.Fatalf("queries did not overlap concurrently: max active = %d", got)
	}

	fastGate.Release()
	fast := <-results
	if fast.stage != "wire_fast" || fast.err != nil {
		t.Fatalf("fast query failed: %+v", fast)
	}
	slowReleasedAt := time.Now()
	slowGate.Release()
	slow := <-results
	if slow.stage != "wire_slow" || slow.err != nil {
		t.Fatalf("slow query failed: %+v", slow)
	}

	if slow.report.queries != 1 || fast.report.queries != 1 {
		t.Fatalf("queries attributed across requests: slow %+v, fast %+v", slow.report, fast.report)
	}
	if slow.report.acquires != 1 || fast.report.acquires != 1 {
		t.Fatalf("acquires attributed across requests: slow %+v, fast %+v", slow.report, fast.report)
	}
	if slow.report.queryTime <= 0 || fast.report.queryTime <= 0 {
		t.Fatalf("query times not captured: slow %v, fast %v", slow.report.queryTime, fast.report.queryTime)
	}
	if slow.report.queryTime < slowReleasedAt.Sub(slowSeenAt)-10*time.Millisecond {
		t.Fatalf("slow gate wait was not attributed to its request: query %v, observed wait %v", slow.report.queryTime, slowReleasedAt.Sub(slowSeenAt))
	}
	if slow.report.stages[0].queryTime != slow.report.queryTime || fast.report.stages[0].queryTime != fast.report.queryTime {
		t.Fatalf("stage times are not request-scoped: slow %+v, fast %+v", slow.report.stages, fast.report.stages)
	}
	for name, report := range map[string]readyReport{"slow": slow.report, "fast": fast.report} {
		if report.leases != 1 || report.released != 1 {
			t.Fatalf("%s lease report = leases %d, released %d", name, report.leases, report.released)
		}
	}
}

func TestReadinessWireEarlierParentCancellation(t *testing.T) {
	server := newFakePGServer(t)
	pool := newWirePool(t, server, 1)
	warm, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(warm.Release)
	warm.Release()

	trace := newReadyTrace(nil)
	trace.poolBefore = snapshotReadyPool(pool)
	traced, endStage := newWireTraceContext(trace, "wire_earlier_parent")
	defer endStage(nil)

	parent, parentCancel := context.WithTimeout(traced, 100*time.Millisecond)
	defer parentCancel()
	child, childCancel := context.WithTimeout(parent, 5*time.Second)
	defer childCancel()

	started := time.Now()
	rows, err := pool.Query(child, "SELECT wire_wait")
	elapsed := time.Since(started)
	if err == nil {
		rows.Close()
		t.Fatal("query outlived the earlier parent deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("earlier parent classification = %T %v, want context.DeadlineExceeded", err, err)
	}
	if elapsed < 80*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("earlier parent cancellation returned after %v", elapsed)
	}
	report := finishWireTrace(trace, pool, true)
	if report.queries != 1 || report.queryErrs != 1 {
		t.Fatalf("query report = %+v", report)
	}
	if report.queryTime < 50*time.Millisecond || report.queryTime > 2*time.Second {
		t.Fatalf("query time = %v, want the earlier parent budget", report.queryTime)
	}
	if report.leases != 1 || report.released != 1 {
		t.Fatalf("lease report = leases %d, released %d", report.leases, report.released)
	}
}
